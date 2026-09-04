import { timingSafeEqual } from "node:crypto";

import { APIError } from "better-auth/api";

import type { Config } from "./config.js";
import { log } from "./log.js";

/**
 * What an API key may reach at the identity provider.
 *
 * `enableSessionForAPIKeys` is what makes a key usable at all — it is how a
 * machine is handed a session for the account its key belongs to, and so how
 * `GET /token` can mint that account's platform token. What it does *not*
 * mean, and what this file exists to stop it meaning, is that a key is a
 * signed-in administrator. Left alone the plugin's session is a session on
 * every better-auth endpoint, so a CI key could register OAuth clients with
 * redirects of its choosing, mint further keys for its own machine account
 * with no expiry, and change the account it belongs to — none of which is
 * anything a pipeline needs, and all of which is a credential minting its own
 * successors (issue #318).
 *
 * So the reach of a key is stated here rather than left to be whatever
 * better-auth happens to mount, and there are exactly two kinds of key:
 *
 * - **The operator's service credential** — the one value the chart generates
 *   into the release's auth Secret and seeds as the service account's key. It
 *   is the platform talking to its own issuer, and it registers a client per
 *   application (`ResourceClaim` type `oidcClient`), so it is unrestricted
 *   here and bounded instead by *who it is*: `clientPrivileges` in
 *   src/auth.ts admits the service account (`isServiceAccount` in
 *   src/identity.ts, where the line between an account and a credential is
 *   drawn) and nobody else.
 * - **Every other key** — a project's CI key, which is a machine account's
 *   credential (docs/AUTH.md, "Machine accounts"). It may do the one thing
 *   `kitchen login` and CI do with it and nothing more.
 *
 * The two are told apart by the presented value rather than by looking the
 * key's owner up, which is what keeps this guard free: it runs in front of
 * every request that carries the header, and the answer costs a comparison
 * rather than a query. The pair it compares against is seeded as a pair
 * (src/seed.ts writes exactly `config.serviceKey`, owned by exactly
 * `config.serviceAccountEmail`, and deletes any earlier key of that account),
 * so "the presented key is the service key" and "the caller is the service
 * account" are the same statement.
 *
 * A key that is *widened* later — issue #349's platform-scoped credential — is
 * a third answer to the same question rather than a second mechanism: what
 * changes is which paths `mayReach` returns true for, decided from the key's
 * own scope. The shape here is deliberately one function taking a path.
 */

/**
 * The paths an ordinary key may reach.
 *
 * `GET /token` is the exchange the CLI (`kitchen login`) and CI make: a key in,
 * a short-lived platform token for its machine account out. `/get-session` is
 * the api-key plugin's own answer to "whose credential is this", which reads
 * the account the key already belongs to and reveals nothing a token would
 * not.
 *
 * Kitchen's own prefix, `/kitchen/*`, is reachable by a key too — but it never
 * arrives here. src/server.ts answers it ahead of the better-auth handler, and
 * its own check refuses every credential but the operator's with a 403.
 */
const KEY_SESSION_PATHS: ReadonlySet<string> = new Set(["/token", "/get-session"]);

/** Whether an ordinary key may reach this endpoint. */
export function mayReach(path: string): boolean {
	return KEY_SESSION_PATHS.has(path);
}

/** How the rule above reads when a refusal has to explain it. */
export const KEY_SESSION_REFUSAL =
	"an API key is a CI credential, not a session: it may be exchanged for a token at /token and " +
	"read its own session at /get-session, and nothing else. Registering an OAuth client, issuing " +
	"another key and changing an account are a person's or the operator's — sign in, or ask the " +
	"Kitchen API for a key";

/**
 * Whether the presented key is the operator's service credential.
 *
 * Compared in constant time, and only after the lengths match — `timingSafeEqual`
 * throws on buffers of different lengths, and a length is not a secret worth
 * the exception.
 */
export function isServiceCredential(config: Config, presented: string): boolean {
	if (!config.serviceKey || !presented) {
		return false;
	}
	const a = Buffer.from(presented, "utf8");
	const b = Buffer.from(config.serviceKey, "utf8");
	return a.length === b.length && timingSafeEqual(a, b);
}

/**
 * Refuses a key-authenticated request to anything outside a key's reach.
 *
 * It runs ahead of the api-key plugin's own hook — better-auth runs the
 * options' `before` before every plugin's — which is why it verifies nothing
 * and looks nothing up: the question it answers is "may a credential of this
 * kind be here at all", and that is settled before anyone establishes whether
 * the key is valid. An invalid key is refused a moment later by the plugin,
 * with its own answer.
 */
export function guardKeySession(config: Config) {
	return async (ctx: { path: string; headers?: Headers | null }): Promise<void> => {
		const presented = ctx.headers?.get("x-api-key") ?? "";
		if (!presented) {
			return;
		}
		if (isServiceCredential(config, presented) || mayReach(ctx.path)) {
			return;
		}
		log.warn("refused an API key outside what a key may reach", { path: ctx.path });
		throw new APIError("FORBIDDEN", { message: KEY_SESSION_REFUSAL });
	};
}
