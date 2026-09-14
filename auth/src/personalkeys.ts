import { randomBytes } from "node:crypto";

import { defaultKeyHasher } from "@better-auth/api-key";

import type { Auth } from "./auth.js";
import type { Config } from "./config.js";
import { isPerson } from "./identity.js";
import { KeyExistsError } from "./keys.js";
import { log } from "./log.js";

/**
 * Personal keys: a credential that carries somebody's own identity (#593).
 *
 * Everything else in this service issues a credential with an account of its
 * own — a CI key's machine account, a platform credential's — precisely so
 * that the credential is *not* a copy of a person. This one is. It belongs to
 * an account somebody signs in to, so a token minted from it carries their
 * `sub`, and Kitchen resolves every role they hold from that: admin on the
 * projects they administer, the operator hat if they wear one.
 *
 * That is the thing src/keyscope.ts refuses at the plugin's own endpoints, and
 * it stays refused there. What changed is not the judgement about what such a
 * credential *is* — it is a copy of a person's access, and this file will not
 * pretend otherwise — but the judgement about the alternative. The automation
 * people want is "do the thing I would do", and everything they had was a CI
 * key bounded to one project with no admin. What they did instead was take the
 * dashboard's access token out of the browser: an hour-long copy of exactly
 * the same access, with no name on it, no list it appears in, no expiry
 * anybody chose and nothing to revoke. A credential the platform issued,
 * named, lists and can take back is strictly better than the one people were
 * already using.
 *
 * Three properties are what make it defensible, and none of them is optional:
 *
 * - **It expires.** Every personal key carries `expiresAt`, which the api-key
 *   plugin enforces on presentation and which deletes the row when it lapses.
 *   The ceiling is Kitchen's (internal/api/personalkeys.go); nothing here
 *   issues one without a date.
 * - **It cannot mint its own successor.** A key reaches `/token` and
 *   `/get-session` and nothing else (src/keyscope.ts), and the Kitchen route
 *   that creates one admits only a token an OAuth client of the platform's
 *   issued — which is to say, somebody who actually signed in. A credential
 *   can use a personal key; it cannot make one.
 * - **It is visible.** It is listed by name, prefix, creation and last use,
 *   beside the person's sessions, and deleting it is one call.
 *
 * Unlike a CI key, one account owns *many* of these — a laptop, a script, a
 * scheduled job are three credentials, revoked separately — so the name is
 * unique per account rather than per account address, and the row's own `name`
 * column is what holds it.
 */

/** 32 bytes as hex is the api-key plugin's `defaultKeyLength`. @see src/keys.ts */
const KEY_BYTES = 32;

/** How many leading characters are kept so a key is recognisable in a list. */
const KEY_START_LENGTH = 6;

/** One personal key, as everything outside this service reads it. */
export interface PersonalKey {
	name: string;
	/** The account it belongs to: the person's own `sub`. */
	subject: string;
	/** That account's address, so a list of subjects still reads. */
	email: string;
	/** The key's first few characters, for telling two keys apart. */
	prefix: string;
	created: string;
	/** When it stops working. Every personal key has one. */
	expires: string;
	lastUsed?: string;
}

/** A key as it is answered exactly once, at creation. */
export interface IssuedPersonalKey extends PersonalKey {
	/** The key itself. It is not stored in a form anything can read back. */
	key: string;
}

/**
 * Thrown when the account a key was asked for is not a person's — a machine
 * account, a platform credential's account, or the service account.
 *
 * It is a refusal rather than an impossibility because the subject arrives in
 * a request: Kitchen asks for a key for the caller it authenticated, and a
 * caller is whoever the token says. The rule that a credential may not mint a
 * credential is enforced twice for that reason, once at each end.
 */
export class NotAPersonError extends Error {}

/** The columns this module reads off an `apikey` row. */
interface KeyRow {
	id: string;
	name?: string | null;
	start?: string | null;
	referenceId: string;
	expiresAt?: Date | string | null;
	createdAt?: Date | string | null;
	lastRequest?: Date | string | null;
}

function asISO(value: Date | string | null | undefined): string | undefined {
	if (!value) {
		return undefined;
	}
	const date = value instanceof Date ? value : new Date(value);
	return Number.isNaN(date.getTime()) ? undefined : date.toISOString();
}

/**
 * The account a personal key may be issued for, or a refusal.
 *
 * A subject nothing resolves is a 404's worth of information and a credential
 * account is a 400's, so the two are told apart here rather than both reading
 * as "no".
 */
async function personBehind(auth: Auth, config: Config, subject: string) {
	const ctx = await auth.$context;
	const user = await ctx.internalAdapter.findUserById(subject);
	if (!user) {
		return null;
	}
	if (!isPerson(config, user.email)) {
		throw new NotAPersonError(
			"a personal key belongs to somebody who signs in, and this account is a credential's: " +
				"a credential that could issue one would be minting a copy of a person",
		);
	}
	return user;
}

/**
 * Issues a personal key for an account somebody signs in to.
 *
 * There is no account to create — that is the whole difference from
 * src/keys.ts — so the only write is the key row, and the only thing that can
 * be half done is nothing.
 */
export async function createPersonalKey(
	auth: Auth,
	config: Config,
	subject: string,
	name: string,
	expiresAt: Date,
): Promise<IssuedPersonalKey | null> {
	const person = await personBehind(auth, config, subject);
	if (!person) {
		return null;
	}

	const existing = await listPersonalKeys(auth, config, subject);
	if (existing?.some((key) => key.name === name)) {
		throw new KeyExistsError(`you already have a personal key called ${name}`);
	}

	const ctx = await auth.$context;
	const value = randomBytes(KEY_BYTES).toString("hex");
	const now = new Date();
	await ctx.adapter.create({
		model: "apikey",
		data: {
			name,
			start: value.slice(0, KEY_START_LENGTH),
			referenceId: person.id,
			key: await defaultKeyHasher(value),
			enabled: true,
			// The one field that is not on a CI key. The plugin refuses an
			// expired key on presentation and deletes the row, so a forgotten
			// personal key stops working without anybody having had to run
			// anything.
			expiresAt,
			createdAt: now,
			updatedAt: now,
		},
	});

	log.info("issued a personal key", { subject: person.id, name, expires: expiresAt.toISOString() });
	return {
		name,
		subject: person.id,
		email: person.email,
		prefix: value.slice(0, KEY_START_LENGTH),
		created: now.toISOString(),
		expires: expiresAt.toISOString(),
		key: value,
	};
}

/**
 * Every personal key of one account, oldest first — and null for a subject
 * that is nobody.
 *
 * Every key the account owns is a personal key: a person's account owns
 * nothing else, because the plugin's own create endpoint is refused to
 * everyone but the operator (`guardKeyIssuance` in src/keyscope.ts) and every
 * other credential this service issues belongs to an account of its own.
 */
export async function listPersonalKeys(
	auth: Auth,
	config: Config,
	subject: string,
): Promise<PersonalKey[] | null> {
	const person = await personBehind(auth, config, subject);
	if (!person) {
		return null;
	}

	const ctx = await auth.$context;
	const rows = await ctx.adapter.findMany<KeyRow>({
		model: "apikey",
		where: [{ field: "referenceId", value: person.id }],
		sortBy: { field: "createdAt", direction: "asc" },
	});

	return rows.map((row) => ({
		name: row.name ?? "",
		subject: person.id,
		email: person.email,
		prefix: row.start ?? "",
		created: asISO(row.createdAt) ?? new Date(0).toISOString(),
		expires: asISO(row.expiresAt) ?? "",
		...(asISO(row.lastRequest) ? { lastUsed: asISO(row.lastRequest) } : {}),
	}));
}

/**
 * Revokes one personal key of one account, answering what was removed.
 *
 * The account stays: it is a person's, and it was here before the key was.
 * That is the other half of what makes revocation cheap — nothing else has to
 * be reasoned about, because nothing else was created.
 */
export async function deletePersonalKey(
	auth: Auth,
	config: Config,
	subject: string,
	name: string,
): Promise<PersonalKey | null> {
	const keys = await listPersonalKeys(auth, config, subject);
	const removed = keys?.find((key) => key.name === name);
	if (!removed) {
		return null;
	}

	const ctx = await auth.$context;
	await ctx.adapter.deleteMany({
		model: "apikey",
		where: [
			{ field: "referenceId", value: removed.subject },
			{ field: "name", value: name },
		],
	});

	log.info("revoked a personal key", { subject: removed.subject, name });
	return removed;
}
