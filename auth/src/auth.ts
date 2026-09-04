import { betterAuth, type BetterAuthOptions } from "better-auth";
import { APIError, createAuthMiddleware } from "better-auth/api";
import { jwt, organization, twoFactor } from "better-auth/plugins";
import { apiKey } from "@better-auth/api-key";
import { oauthProvider } from "@better-auth/oauth-provider";
import { passkey } from "@better-auth/passkey";
import { sso } from "@better-auth/sso";
import type { Pool } from "pg";

import { allowedOrigins, platformClients, type Config } from "./config.js";
import { isServiceAccount } from "./identity.js";
import { guardKeySession } from "./keyscope.js";
import { log } from "./log.js";

export const LOGIN_PATH = "/login";
export const CONSENT_PATH = "/consent";

/** The provider's token endpoint, where a resource indicator is honoured. */
const TOKEN_PATH = "/oauth2/token";

/**
 * Scopes this issuer knows about. "openid" is what makes the discovery
 * document an OIDC one rather than a plain OAuth 2.0 authorization server.
 */
const SCOPES = ["openid", "profile", "email", "offline_access"] as const;

/**
 * Which client is asking for this token.
 *
 * A public client sends its id in the body; a confidential one authenticates
 * with HTTP Basic and sends nothing in the body at all, so both have to be
 * read. The header is split exactly the way the provider's own client lookup
 * splits it — first colon, no unescaping — so that the id checked here and the
 * id authenticated a moment later cannot be two different strings. Anything
 * unreadable is nobody, which the caller below refuses rather than guesses at.
 */
function requestingClient(body: Record<string, unknown>, authorization: string | null): string {
	const fromBody = typeof body.client_id === "string" ? body.client_id.trim() : "";
	if (fromBody) {
		return fromBody;
	}
	if (!authorization?.startsWith("Basic ")) {
		return "";
	}
	const decoded = Buffer.from(authorization.slice("Basic ".length), "base64").toString("utf8");
	const separator = decoded.indexOf(":");
	return separator === -1 ? "" : decoded.slice(0, separator);
}

/**
 * Refuses a resource indicator to any client that is not the platform's own.
 *
 * `validAudiences` on the provider bounds *what* may be asked for; nothing
 * bounded *who* could ask. Every client this issuer knows — the dashboard and
 * every application an `oidcClient` claim registered alike — could exchange a
 * code with `resource=<the operator API>` and receive a JWT the API accepts as
 * the person who authorized it, holding every role that person holds. See
 * `platformClients` in config.ts for why the answer is configuration rather
 * than something a registration claims about itself.
 *
 * It refuses rather than strips, because a client that asked for an audience
 * and silently got another one is the audience confusion the resource
 * indicator exists to prevent. `invalid_target` is RFC 8707's word for it.
 * Signing in is untouched: without a `resource` an application's client gets
 * exactly what it got before — an ID token, an opaque access token, and
 * `/oauth2/userinfo`.
 */
function guardResourceIndicator(config: Config) {
	const platform = platformClients(config);
	return async (ctx: { path: string; body?: unknown; request?: Request }): Promise<void> => {
		if (ctx.path !== TOKEN_PATH) {
			return;
		}
		const body: Record<string, unknown> =
			ctx.body && typeof ctx.body === "object" ? (ctx.body as Record<string, unknown>) : {};
		const resource = typeof body.resource === "string" ? body.resource.trim() : "";
		if (!resource) {
			return;
		}
		const clientId = requestingClient(body, ctx.request?.headers.get("authorization") ?? null);
		if (clientId && platform.has(clientId)) {
			return;
		}
		log.warn("refused a resource indicator to a client that is not the platform's own", {
			clientId: clientId || "(none)",
			resource,
		});
		throw new APIError("BAD_REQUEST", {
			error: "invalid_target",
			error_description:
				"this client may not request a token for another resource: the resource indicator is " +
				"the platform's own clients' alone, and a token for the Kitchen API is not something " +
				"an application may be granted on a person's behalf",
		});
	};
}

/**
 * Everything that runs in front of every endpoint, in one hook because
 * better-auth's options take one.
 *
 * They are two questions asked in the order they can be answered. *May this
 * credential be here at all* comes first and is settled from the request
 * alone (src/keyscope.ts); *may this client ask for this audience* is about
 * one endpoint's body. A guard added later belongs in this list rather than
 * inside one of them.
 */
function guards(config: Config): BetterAuthOptions["hooks"] {
	const keySession = guardKeySession(config);
	const resourceIndicator = guardResourceIndicator(config);
	return {
		before: createAuthMiddleware(async (ctx) => {
			await keySession(ctx);
			await resourceIndicator(ctx);
		}),
	};
}

/**
 * The better-auth configuration, kept separate from the instance because the
 * migration runner needs the same options object.
 *
 * The service is mounted at the root (`basePath: "/"`) so that the discovery
 * document lands on `<issuer>/.well-known/openid-configuration`, where every
 * OIDC client looks for it.
 */
export function authOptions(config: Config, database: Pool): BetterAuthOptions {
	const rpID = new URL(config.baseURL).hostname;

	return {
		appName: "Kitchen",
		baseURL: config.baseURL,
		basePath: "/",
		secret: config.secret,
		database,
		// Every origin a browser may reach this service from, derived in one
		// place so that the CORS allow-list and this list cannot disagree —
		// `allowedOrigins` says what turns on it. What is new here is the
		// dashboard: it holds the account screen, and every write that screen
		// makes is a cookie-bearing cross-origin POST, which better-auth refuses
		// with `INVALID_ORIGIN` unless the dashboard is trusted. Listing the
		// issuer alone was enough only while nothing but the issuer's own pages
		// posted here.
		trustedOrigins: [...allowedOrigins(config)],
		// What a credential may reach here at all, and who may ask for a token
		// for another resource — the operator API above all. Both run ahead of
		// the endpoint they guard, and ahead of every plugin's own hooks, which
		// better-auth runs after the options'.
		hooks: guards(config),
		session: {
			// No freshness window. better-auth guards `/list-sessions` on a session
			// created within the last day by default, and nothing in Kitchen can
			// make a session fresher without ending it: the dashboard's sign-out
			// revokes its own OAuth tokens and leaves the session here alone, and a
			// new sign-in round trip reuses it rather than replacing it. So the
			// default would leave the account screen unable to list sessions for
			// six of the seven days one lasts, with nothing anyone could do about
			// it but wait to be signed out. What the window would have protected is
			// reading one's own sessions and revoking them; the one genuinely
			// sensitive operation, changing a password, asks for the current
			// password instead — which is the check that actually re-authenticates.
			freshAge: 0,
		},
		telemetry: { enabled: false },
		emailAndPassword: {
			enabled: true,
			// Kitchen is not a public sign-up service. The first administrator
			// comes from the bootstrap flow; everyone after that is invited or
			// arrives through a configured upstream provider.
			disableSignUp: true,
		},
		socialProviders: config.github
			? {
					github: {
						clientId: config.github.clientId,
						clientSecret: config.github.clientSecret,
						disableSignUp: !config.allowSocialSignUp,
					},
				}
			: {},
		plugins: [
			// Signs ID tokens and serves the JWKS the operator API validates
			// bearer tokens against. The OAuth provider builds on it.
			jwt(),
			oauthProvider({
				loginPage: LOGIN_PATH,
				consentPage: CONSENT_PATH,
				scopes: [...SCOPES],
				// Clients are registered by the operator and by nobody else:
				// registration requires a session, which the operator gets
				// from its API key, and `clientPrivileges` below decides whose
				// session that may be.
				allowDynamicClientRegistration: true,
				allowUnauthenticatedClientRegistration: false,
				// Who may register or manage an OAuth client: the service
				// account, and only it.
				//
				// The plugin asks this at the one chokepoint every client
				// mutation goes through — create, read, update, delete, list
				// and rotate alike — so it is the whole of the answer rather
				// than a check on one route. It is set because "registration
				// requires a session" turned out to be a much wider door than
				// it reads as: `enableSessionForAPIKeys` makes every CI key a
				// session, so any key could register a confidential client
				// with redirects of its choosing and the `client_credentials`
				// grant, and the operator's `/kitchen/clients` — which manages
				// only what the operator itself registered — could not even
				// see it (issue #318).
				//
				// It refuses a signed-in administrator too. Registering a
				// client is not something a person does here: an application's
				// client is `ResourceClaim` type `oidcClient`, which the
				// operator registers and then keeps in step with the project's
				// environments, and the dashboard's own client is seeded
				// (src/seed.ts). A client a person registered by hand would be
				// one nothing maintains.
				clientPrivileges: ({ user }) => isServiceAccount(config, user?.email),
				// The audiences a client may ask for a token for
				// (`resource=`), and nothing else — an unconstrained list is
				// the audience-confusion problem GHSA-p2fr-6hmx-4528 warns
				// about in the 1.6 line. The issuer is always one of them;
				// the operator API is added because it is a resource server
				// of its own, and a token for the API should say so rather
				// than be a token for everything.
				//
				// It is half the rule: this list is issuer-wide, so it says
				// what may be asked for and not by whom. `hooks.before`
				// above is the other half — only the platform's own clients
				// may name a resource at all.
				validAudiences: config.apiURL ? [config.baseURL, config.apiURL] : undefined,
				//
				//
				// The shape of a dashboard session, spelled out rather than
				// left to the plugin's defaults because it is now a decision:
				// an hour of API access per token, and a week before anyone is
				// asked to sign in again. The dashboard trades its refresh
				// token for a new access token in the background, so the hour
				// is invisible; the week is how long a browser that was closed
				// with a session still has one when it comes back.
				//
				// The plugin rotates refresh tokens and tears down the whole
				// family when a spent one is replayed (RFC 9700 §4.14), which
				// is what makes it defensible to keep one in the browser at
				// all — and why the shorter-than-default lifetime is worth
				// having.
				accessTokenExpiresIn: 60 * 60,
				refreshTokenExpiresIn: 60 * 60 * 24 * 7,
				//
				// Registration is rate limited per minute; the operator creates
				// a client per environment, so the plugin's default of five is
				// too tight for a burst of preview deployments.
				rateLimit: { register: { window: 60, max: 30 } },
				// Who the token belongs to, in words.
				//
				// The access token is the only thing the UI and the operator API
				// ever see: neither holds a session, and neither calls
				// `/oauth2/userinfo`. Left alone the provider puts the account's
				// identity in the ID token and names it in the access token with
				// `sub` alone — an opaque id — so the UI's account menu shows a
				// random string and everything the API records as created by a
				// person is attributed to one too. The claims follow the granted
				// scopes, the same rule the ID token and userinfo apply.
				customAccessTokenClaims: ({ user, scopes }) => {
					if (!user) {
						return {};
					}
					return {
						...(scopes.includes("profile") ? { name: user.name, picture: user.image ?? undefined } : {}),
						...(scopes.includes("email") ? { email: user.email, email_verified: user.emailVerified } : {}),
					};
				},
				silenceWarnings: { oauthAuthServerConfig: true },
			}),
			// Upstream identity providers registered at runtime (OIDC or SAML),
			// for organisations that already have an IdP of their own.
			sso(),
			// Organisations, for installations that model their people that
			// way. Deliberately *not* where Kitchen's own roles live: putting
			// membership here would widen the contract this service has to
			// meet from "an OIDC issuer with dynamic client registration" to
			// one that also exposes an organisation model as claims, and a
			// role inside an hour-long access token is a stale snapshot of
			// who may do what. Kitchen records that on its own objects
			// instead — docs/AUTH.md, "Where membership lives".
			organization(),
			passkey({ rpID, rpName: "Kitchen", origin: config.baseURL }),
			twoFactor({ issuer: "Kitchen" }),
			// CI credentials and the operator's own service credential. Keys
			// stand in for a session so that a machine has an identity at all:
			// what `GET /token` mints is a token for the account the key
			// belongs to.
			//
			// That session is *not* a signed-in administrator, and what stops
			// it becoming one is `guardKeySession` above — a key reaches the
			// token exchange and its own session, and the operator's own
			// credential reaches everything (src/keyscope.ts).
			apiKey({
				enableSessionForAPIKeys: true,
				// The plugin's default budget (10 requests a day) is meant to be
				// configured; a minute-long window fits interactive CI use.
				rateLimit: { enabled: true, timeWindow: 60_000, maxRequests: 60 },
			}),
		],
	};
}

export type Auth = ReturnType<typeof betterAuth>;

export function createAuth(config: Config, database: Pool): Auth {
	return betterAuth(authOptions(config, database));
}
