import { betterAuth, type BetterAuthOptions } from "better-auth";
import { jwt, organization, twoFactor } from "better-auth/plugins";
import { apiKey } from "@better-auth/api-key";
import { oauthProvider } from "@better-auth/oauth-provider";
import { passkey } from "@better-auth/passkey";
import { sso } from "@better-auth/sso";
import type { Pool } from "pg";

import type { Config } from "./config.js";

export const LOGIN_PATH = "/login";
export const CONSENT_PATH = "/consent";

/**
 * Scopes this issuer knows about. "openid" is what makes the discovery
 * document an OIDC one rather than a plain OAuth 2.0 authorization server.
 */
const SCOPES = ["openid", "profile", "email", "offline_access"] as const;

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
		trustedOrigins: [config.baseURL, ...config.trustedOrigins],
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
				// Clients are registered by the operator (and by admins), never
				// anonymously: registration requires a session, which the
				// operator gets from its API key.
				allowDynamicClientRegistration: true,
				allowUnauthenticatedClientRegistration: false,
				// The audiences a client may ask for a token for
				// (`resource=`), and nothing else — an unconstrained list is
				// the audience-confusion problem GHSA-p2fr-6hmx-4528 warns
				// about in the 1.6 line. The issuer is always one of them;
				// the operator API is added because it is a resource server
				// of its own, and a token for the API should say so rather
				// than be a token for everything.
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
			// stand in for a session so a machine can call the same endpoints a
			// signed-in administrator can — that is how the operator registers
			// OAuth clients.
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
