import { randomBytes } from "node:crypto";

import { defaultKeyHasher } from "@better-auth/api-key";

import type { Auth } from "./auth.js";
import {
	MACHINE_PROVISIONING,
	PLATFORM_ACCOUNT_DOMAIN,
	platformAddress,
	platformIdentity,
} from "./identity.js";
import { KeyExistsError } from "./keys.js";
import { log } from "./log.js";

/**
 * Platform credentials, as the platform stores them (issue #349).
 *
 * Everything src/keys.ts says about a CI key is true here — a key is not an
 * identity of its own, so it gets an account of its own to be granted
 * something with, and one account owns exactly one key so that "revoke that
 * credential" is unambiguous. What differs is the *grant*: a CI key's account
 * is named in a Project's `spec.access` and holds a role there, while a
 * platform credential's account is named in `spec.access.credentials` on the
 * Kitchen singleton and holds scopes — operations, not a role, and never the
 * operator hat.
 *
 * Nothing about the scopes is stored here, deliberately. This service issues
 * and revokes the credential; what the platform will honour it for is Kitchen's
 * own state, which is what makes narrowing a credential an edit to a custom
 * resource rather than a write to somebody's identity provider, and what keeps
 * `kubectl get kitchen -o yaml` the whole truth about who may do what.
 *
 * The two kinds of credential are told apart by the domain their account sits
 * under and by nothing else — see src/identity.ts for why that is a second
 * reserved domain rather than a reserved project under the first.
 */

/** 32 bytes as hex is the api-key plugin's `defaultKeyLength`. @see src/keys.ts */
const KEY_BYTES = 32;

/** How many leading characters are kept so a credential is recognisable in a list. */
const KEY_START_LENGTH = 6;

/** One platform credential, as everything outside this service reads it. */
export interface PlatformKey {
	name: string;
	/** The account's `sub`: what `spec.access.credentials` names. */
	subject: string;
	/** The account's address, so a list of subjects still reads. */
	email: string;
	/** The credential's first few characters, for telling two apart. */
	prefix: string;
	created: string;
	lastUsed?: string;
}

/** A credential as it is answered exactly once, at creation. */
export interface IssuedPlatformKey extends PlatformKey {
	/** The credential itself. It is not stored in a form anything can read back. */
	key: string;
}

/** The columns this module reads off an `apikey` row. */
interface KeyRow {
	id: string;
	name?: string | null;
	start?: string | null;
	referenceId: string;
	createdAt?: Date | string | null;
	lastRequest?: Date | string | null;
}

/** The columns this module reads off a credential account's `user` row. */
interface AccountRow {
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
 * Creates a platform credential and the account that owns it.
 *
 * The account is written first because the key's `referenceId` has to name it,
 * and it is removed again if the key cannot be written — an account with no key
 * is not a credential, but it does hold the name.
 */
export async function createPlatformKey(auth: Auth, name: string): Promise<IssuedPlatformKey> {
	const ctx = await auth.$context;
	const email = platformAddress(name);

	if (await ctx.internalAdapter.findUserByEmail(email)) {
		throw new KeyExistsError(`the platform already has a credential called ${name}`);
	}

	const user = await ctx.internalAdapter.createUser(
		{
			email,
			name: `${name} (platform)`,
			// Nothing can receive mail here, and an unverified address is one no
			// access entry naming an address will ever resolve to — see
			// src/identity.ts.
			emailVerified: false,
		},
		MACHINE_PROVISIONING,
	);

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
				// Rate limiting is left to the plugin's configured window, as it
				// is for a CI key and unlike the operator's own credential,
				// which turns it off. A scheduled job is not a control loop, and
				// a credential that has gone wrong is exactly the case a window
				// is for.
				createdAt: now,
				updatedAt: now,
			},
		});
	} catch (error) {
		await ctx.adapter.delete({ model: "user", where: [{ field: "id", value: user.id }] });
		throw error;
	}

	log.info("issued a platform credential", { name, subject: user.id });
	return {
		name,
		subject: user.id,
		email,
		prefix: value.slice(0, KEY_START_LENGTH),
		created: now.toISOString(),
		key: value,
	};
}

/** Every platform credential, oldest first. */
export async function listPlatformKeys(auth: Auth): Promise<PlatformKey[]> {
	const ctx = await auth.$context;
	const accounts = (
		await ctx.adapter.findMany<AccountRow>({
			model: "user",
			where: [{ field: "email", operator: "ends_with", value: `@${PLATFORM_ACCOUNT_DOMAIN}` }],
			sortBy: { field: "createdAt", direction: "asc" },
		})
	).filter((account) => platformIdentity(account.email) !== null);

	if (accounts.length === 0) {
		return [];
	}

	const rows = await ctx.adapter.findMany<KeyRow>({
		model: "apikey",
		where: [{ field: "referenceId", operator: "in", value: accounts.map((a) => a.id) }],
	});
	const byOwner = new Map(rows.map((row) => [row.referenceId, row]));

	return accounts.map((account) => {
		const row = byOwner.get(account.id);
		return {
			// The address is the authority on the name, not the key row: the
			// address is what makes the name unique, and the row's copy of it is
			// only there so the plugin's own screens can show one.
			name: platformIdentity(account.email) ?? "",
			subject: account.id,
			email: account.email,
			prefix: row?.start ?? "",
			created: asISO(row?.createdAt ?? account.createdAt) ?? new Date(0).toISOString(),
			...(asISO(row?.lastRequest) ? { lastUsed: asISO(row?.lastRequest) } : {}),
		};
	});
}

/**
 * Revokes a platform credential and removes the account that owned it,
 * answering what was removed so the caller can take the grant off the
 * singleton.
 *
 * The key row goes first, as it does for a CI key: an account left behind holds
 * a name, a key left behind is a credential that still works.
 */
export async function deletePlatformKey(auth: Auth, name: string): Promise<PlatformKey | null> {
	const ctx = await auth.$context;
	const email = platformAddress(name);
	const owner = await ctx.internalAdapter.findUserByEmail(email);
	if (!owner) {
		return null;
	}

	const existing = await listPlatformKeys(auth);
	const removed = existing.find((key) => key.subject === owner.user.id) ?? {
		name,
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

	log.info("revoked a platform credential", { name, subject: owner.user.id });
	return removed;
}
