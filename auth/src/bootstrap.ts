import { timingSafeEqual } from "node:crypto";

import type { Auth } from "./auth.js";
import type { Config } from "./config.js";
import { log } from "./log.js";

/**
 * The first administrator.
 *
 * `helm install` generates a token into the release's auth Secret and prints
 * the link that carries it. The link works exactly until the platform has its
 * first account: after that the endpoint is gone, whether or not the token is
 * still lying around in the Secret. That makes the token one-time without
 * needing state of its own — the account it creates is the state.
 *
 * The service account that owns the operator's API key is not a person and
 * does not count.
 */
export async function isBootstrapped(auth: Auth, config: Config): Promise<boolean> {
	const ctx = await auth.$context;
	const people = await ctx.adapter.count({
		model: "user",
		where: [{ field: "email", operator: "ne", value: config.serviceAccountEmail }],
	});
	return people > 0;
}

export function tokenMatches(expected: string | undefined, provided: string | null): boolean {
	if (!expected || !provided) {
		return false;
	}
	const a = Buffer.from(expected);
	const b = Buffer.from(provided);
	return a.length === b.length && timingSafeEqual(a, b);
}

export interface BootstrapRequest {
	token: string | null;
	email: string;
	name: string;
	password: string;
}

export type BootstrapResult =
	| { ok: true }
	| { ok: false; status: number; error: string };

/**
 * Creates the first administrator. It writes the user and its credential
 * account directly instead of going through `/sign-up/email`, which stays
 * disabled: this is the one account that may exist without an invitation.
 */
export async function bootstrapFirstUser(
	auth: Auth,
	config: Config,
	request: BootstrapRequest,
): Promise<BootstrapResult> {
	if (!config.bootstrapToken) {
		return { ok: false, status: 404, error: "bootstrap is not enabled on this installation" };
	}
	if (!tokenMatches(config.bootstrapToken, request.token)) {
		return { ok: false, status: 401, error: "invalid bootstrap token" };
	}
	if (await isBootstrapped(auth, config)) {
		return { ok: false, status: 410, error: "this installation already has an account" };
	}

	const email = request.email.trim().toLowerCase();
	const name = request.name.trim();
	if (!email.includes("@")) {
		return { ok: false, status: 400, error: "a valid email address is required" };
	}
	if (!name) {
		return { ok: false, status: 400, error: "a name is required" };
	}

	const ctx = await auth.$context;
	const minLength = ctx.password.config.minPasswordLength;
	const maxLength = ctx.password.config.maxPasswordLength;
	if (request.password.length < minLength || request.password.length > maxLength) {
		return {
			ok: false,
			status: 400,
			error: `the password must be between ${minLength} and ${maxLength} characters`,
		};
	}

	const hash = await ctx.password.hash(request.password);
	const user = await ctx.internalAdapter.createUser({
		email,
		name,
		// Nobody can send this account a verification mail yet, and it is
		// created from a secret only a cluster administrator can read.
		emailVerified: true,
	});
	await ctx.internalAdapter.linkAccount({
		userId: user.id,
		providerId: "credential",
		accountId: user.id,
		password: hash,
	});

	log.info("bootstrapped the first administrator", { email });
	return { ok: true };
}
