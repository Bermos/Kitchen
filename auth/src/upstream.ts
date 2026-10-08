import { APIError } from "better-auth/api";

import type { Config } from "./config.js";
import { isServiceCredential } from "./keyscope.js";
import { log } from "./log.js";

/**
 * Who may register an upstream identity provider.
 *
 * The SSO plugin is mounted so that an organisation with an IdP of its own can
 * sign its people in through it, and the plugin's management endpoints are
 * behind nothing but a session — any signed-in person's browser is one. A
 * provider is an issuer this service believes about who somebody is, so a
 * person who could register one could sign in through an IdP they run, as any
 * address they liked, and an account would be created for it: sign-up being
 * closed is a rule about the issuer's own forms, and the plugin's implicit
 * sign-up is not one of them. An account holding an address before its owner
 * signs up is an account the API resolves that address to when somebody is
 * added as a member by it.
 *
 * So registering, changing, listing and removing providers is the platform's
 * own act — the operator's service credential, as for every other write that
 * shapes who this issuer trusts — and no Kitchen screen offers it to anyone
 * else. Signing in through a provider stays open, because every provider that
 * exists was then put there by the platform.
 */
const MANAGEMENT_PATHS = new Set([
	"/sso/register",
	"/sso/providers",
	"/sso/get-provider",
	"/sso/update-provider",
	"/sso/delete-provider",
	"/sso/request-domain-verification",
	"/sso/verify-domain",
]);

/** Whether a path manages the upstream providers rather than signing in through one. */
export function isProviderManagementPath(path: string): boolean {
	return MANAGEMENT_PATHS.has(path);
}

/** How the rule above reads when a refusal has to explain it. */
export const PROVIDER_MANAGEMENT_REFUSAL =
	"an upstream identity provider is registered by the platform, not by a signed-in person: " +
	"a provider decides who an account is, so registering one is the operator's";

/** Refuses the SSO plugin's management endpoints to everyone but the operator's service credential. */
export function guardProviderManagement(config: Config) {
	return async (ctx: { path: string; headers?: Headers | null }): Promise<void> => {
		if (!isProviderManagementPath(ctx.path)) {
			return;
		}
		if (isServiceCredential(config, ctx.headers?.get("x-api-key") ?? "")) {
			return;
		}
		log.warn("refused an identity provider endpoint at the issuer", { path: ctx.path });
		throw new APIError("FORBIDDEN", { message: PROVIDER_MANAGEMENT_REFUSAL });
	};
}
