import { randomBytes } from "node:crypto";

import { defaultKeyHasher } from "@better-auth/api-key";

import type { Auth } from "./auth.js";
import { MACHINE_ACCOUNT_DOMAIN, machineAddress, machineIdentity } from "./identity.js";
import { log } from "./log.js";

/**
 * CI keys, as the platform stores them.
 *
 * A key is not an identity of its own, whatever `enableSessionForAPIKeys`
 * suggests: the plugin turns a key into a session for the account the key's
 * `referenceId` points at, so the `sub` in the token it is exchanged for is
 * the *owner's*. Granting "the key's subject" a project role would therefore
 * grant it to whoever created the key, on their own account — which is not a
 * scoped CI credential, it is a copy of a person's.
 *
 * So every key gets an owner of its own: a machine account, created for it,
 * holding nothing but this one key. That account's `sub` is what the project
 * grants a role to, which is what makes a key a member of exactly one project
 * (docs/AUTH.md, "Machine accounts"). The convention that recognises such an
 * account, and ties it to its project, is in src/identity.ts.
 *
 * One machine account owns exactly one key. Rotation is therefore delete and
 * create rather than a second key on the same account: two credentials behind
 * one grant would make "revoke that key" ambiguous, and revocation being
 * unambiguous and in one place is the property this whole design keeps.
 */

/**
 * 32 bytes as hex is 64 characters, which is the api-key plugin's
 * `defaultKeyLength` — anything shorter is refused before the key is even
 * looked up, so a shorter key would fail at the point of use rather than here.
 */
const KEY_BYTES = 32;

/** How many leading characters are kept so a key is recognisable in a list. */
const KEY_START_LENGTH = 6;

/** One key, as everything outside this service reads it — never its value. */
export interface ProjectKey {
	name: string;
	project: string;
	/** The machine account's `sub`: what the project's `spec.access` names. */
	subject: string;
	/** The machine account's address, so a list of subjects still reads. */
	email: string;
	/** The key's first few characters, for telling two keys apart. */
	prefix: string;
	created: string;
	lastUsed?: string;
}

/** A key as it is answered exactly once, at creation. */
export interface IssuedProjectKey extends ProjectKey {
	/** The key itself. It is not stored in a form anything can read back. */
	key: string;
}

/** Thrown when the project already has a key by that name. */
export class KeyExistsError extends Error {}

/** The columns this module reads off an `apikey` row. */
interface KeyRow {
	id: string;
	name?: string | null;
	start?: string | null;
	referenceId: string;
	createdAt?: Date | string | null;
	lastRequest?: Date | string | null;
}

/** The columns this module reads off a machine account's `user` row. */
interface MachineRow {
	id: string;
	email: string;
	createdAt?: Date | string | null;
}

function asISO(value: Date | string | null | undefined): string | undefined {
	if (!value) {
		return undefined;
	}
	const date = value instanceof Date ? value : new Date(value);
	return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}

/**
 * Creates a machine account and the one key it owns.
 *
 * The account is written first because the key's `referenceId` has to name
 * it. If the key cannot be written the account is removed again: an account
 * with no key is not a credential, but it does hold the name, and a name held
 * by nothing is a key nobody can create and nobody can delete.
 */
export async function createProjectKey(
	auth: Auth,
	project: string,
	name: string,
): Promise<IssuedProjectKey> {
	const ctx = await auth.$context;
	const email = machineAddress(project, name);

	if (await ctx.internalAdapter.findUserByEmail(email)) {
		throw new KeyExistsError(`${project} already has a key called ${name}`);
	}

	const user = await ctx.internalAdapter.createUser({
		email,
		name: `${name} (${project})`,
		// Nothing can receive mail here, and an unverified address is one no
		// access entry naming an address will ever resolve to — see
		// src/identity.ts.
		emailVerified: false,
	});

	const value = randomBytes(KEY_BYTES).toString("hex");
	const now = new Date();
	try {
		await ctx.adapter.create({
			model: "apikey",
			data: {
				name,
				start: value.slice(0, KEY_START_LENGTH),
				referenceId: user.id,
				key: await defaultKeyHasher(value),
				enabled: true,
				// Rate limiting is left to the plugin's configured window,
				// unlike the operator's own credential, which turns it off:
				// the operator runs a control loop and CI runs a pipeline.
				createdAt: now,
				updatedAt: now,
			},
		});
	} catch (error) {
		await ctx.adapter.delete({ model: "user", where: [{ field: "id", value: user.id }] });
		throw error;
	}

	log.info("issued a CI key", { project, name, subject: user.id });
	return {
		name,
		project,
		subject: user.id,
		email,
		prefix: value.slice(0, KEY_START_LENGTH),
		created: now.toISOString(),
		key: value,
	};
}

/** Every key belonging to one project's machine accounts, oldest first. */
export async function listProjectKeys(auth: Auth, project: string): Promise<ProjectKey[]> {
	const ctx = await auth.$context;
	const accounts = (
		await ctx.adapter.findMany<MachineRow>({
			model: "user",
			where: [{ field: "email", operator: "ends_with", value: `@${MACHINE_ACCOUNT_DOMAIN}` }],
			sortBy: { field: "createdAt", direction: "asc" },
		})
	).filter((account) => machineIdentity(account.email)?.project === project);

	if (accounts.length === 0) {
		return [];
	}

	const rows = await ctx.adapter.findMany<KeyRow>({
		model: "apikey",
		where: [{ field: "referenceId", operator: "in", value: accounts.map((a) => a.id) }],
	});
	const byOwner = new Map(rows.map((row) => [row.referenceId, row]));

	return accounts.map((account) => {
		const identity = machineIdentity(account.email);
		const row = byOwner.get(account.id);
		return {
			// The address is the authority on the name, not the key row: the
			// address is what makes the name unique, and the row's copy of it
			// is only there so the plugin's own screens can show one.
			name: identity?.key ?? "",
			project,
			subject: account.id,
			email: account.email,
			prefix: row?.start ?? "",
			created: asISO(row?.createdAt ?? account.createdAt) ?? new Date(0).toISOString(),
			...(asISO(row?.lastRequest) ? { lastUsed: asISO(row?.lastRequest) } : {}),
		};
	});
}

/**
 * Revokes a key and removes the account that owned it, answering what was
 * removed so the caller can take the grant off the project by `subject`.
 *
 * The key row goes first. Revocation is the half that matters: an account
 * left behind holds a name, a key left behind is a credential that still
 * works.
 */
export async function deleteProjectKey(
	auth: Auth,
	project: string,
	name: string,
): Promise<ProjectKey | null> {
	const ctx = await auth.$context;
	const email = machineAddress(project, name);
	const owner = await ctx.internalAdapter.findUserByEmail(email);
	if (!owner) {
		return null;
	}

	const existing = await listProjectKeys(auth, project);
	const removed = existing.find((key) => key.subject === owner.user.id) ?? {
		name,
		project,
		subject: owner.user.id,
		email,
		prefix: "",
		created: new Date(0).toISOString(),
	};

	await ctx.adapter.deleteMany({
		model: "apikey",
		where: [{ field: "referenceId", value: owner.user.id }],
	});
	await ctx.adapter.delete({ model: "user", where: [{ field: "id", value: owner.user.id }] });

	log.info("revoked a CI key", { project, name, subject: owner.user.id });
	return removed;
}
