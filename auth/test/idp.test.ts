import assert from "node:assert/strict";
import { createHash, randomBytes } from "node:crypto";
import { after, before, describe, it } from "node:test";

import { startHarness, type Harness } from "./support.js";

describe("the identity provider", () => {
	let kitchen: Harness;

	before(async () => {
		kitchen = await startHarness();
	});

	after(async () => {
		await kitchen.stop();
	});

	it("serves an OIDC discovery document at the issuer root", async () => {
		const response = await kitchen.fetch("/.well-known/openid-configuration");
		assert.equal(response.status, 200);

		const document = (await response.json()) as Record<string, string | string[]>;
		assert.equal(document.issuer, kitchen.url);
		assert.equal(document.authorization_endpoint, `${kitchen.url}/oauth2/authorize`);
		assert.equal(document.token_endpoint, `${kitchen.url}/oauth2/token`);
		assert.equal(document.userinfo_endpoint, `${kitchen.url}/oauth2/userinfo`);
		assert.equal(document.jwks_uri, `${kitchen.url}/jwks`);
		assert.equal(document.registration_endpoint, `${kitchen.url}/oauth2/register`);
		assert.ok(
			(document.code_challenge_methods_supported as string[]).includes("S256"),
			"the UI signs in with Authorization Code + PKCE",
		);
	});

	it("publishes a JWKS the operator API can validate tokens against", async () => {
		const response = await kitchen.fetch("/jwks");
		assert.equal(response.status, 200);

		const jwks = (await response.json()) as { keys: unknown[] };
		assert.ok(Array.isArray(jwks.keys) && jwks.keys.length > 0);
	});

	it("answers health and readiness probes", async () => {
		assert.equal((await kitchen.fetch("/healthz")).status, 200);
		assert.equal((await kitchen.fetch("/readyz")).status, 200);
	});

	it("refuses anonymous dynamic client registration", async () => {
		const response = await kitchen.fetch("/oauth2/register", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				client_name: "impostor",
				redirect_uris: ["https://impostor.example.com/callback"],
			}),
		});
		assert.equal(response.ok, false, "registration must require a credential");
		assert.ok([400, 401, 403].includes(response.status), `unexpected status ${response.status}`);
	});

	it("registers a client for the operator's service credential", async () => {
		const response = await kitchen.fetch("/oauth2/register", {
			method: "POST",
			headers: {
				"content-type": "application/json",
				"x-api-key": kitchen.serviceKey,
			},
			body: JSON.stringify({
				client_name: "kitchen-ui",
				redirect_uris: ["https://kitchen.apps.example.com/auth/callback"],
				grant_types: ["authorization_code", "refresh_token"],
			}),
		});
		assert.equal(response.status, 200, await response.clone().text());

		const client = (await response.json()) as Record<string, unknown>;
		assert.ok(client.client_id, "a client id is issued");
		assert.ok(client.client_secret, "a client secret is issued");
		assert.equal(client.client_name, "kitchen-ui");
	});

	it("rejects a wrong service credential", async () => {
		const response = await kitchen.fetch("/oauth2/register", {
			method: "POST",
			headers: {
				"content-type": "application/json",
				"x-api-key": `${kitchen.serviceKey}-nope`,
			},
			body: JSON.stringify({
				client_name: "impostor",
				redirect_uris: ["https://impostor.example.com/callback"],
			}),
		});
		assert.equal(response.ok, false);
	});

	it("serves the sign-in page the provider redirects to", async () => {
		const response = await kitchen.fetch("/login");
		assert.equal(response.status, 200);
		assert.match(await response.text(), /Sign in/);
	});

	it("keeps public sign-up closed", async () => {
		const response = await kitchen.fetch("/sign-up/email", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				email: "stranger@example.com",
				name: "Stranger",
				password: "correct horse battery staple",
			}),
		});
		assert.equal(response.ok, false, "anyone could otherwise create a platform account");
	});
});

describe("the first-administrator bootstrap", () => {
	let kitchen: Harness;

	before(async () => {
		kitchen = await startHarness();
	});

	after(async () => {
		await kitchen.stop();
	});

	const admin = {
		name: "Ada",
		email: "ada@example.com",
		password: "correct horse battery staple",
	};

	it("refuses a link without the token", async () => {
		assert.equal((await kitchen.fetch("/bootstrap")).status, 401);
		assert.equal((await kitchen.fetch("/bootstrap?token=wrong")).status, 401);
	});

	it("serves the form for the token in the release secret", async () => {
		const response = await kitchen.fetch(`/bootstrap?token=${kitchen.bootstrapToken}`);
		assert.equal(response.status, 200);
		assert.match(await response.text(), /Create the first administrator/);
	});

	it("creates the first administrator", async () => {
		const response = await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ token: kitchen.bootstrapToken, ...admin }),
		});
		assert.equal(response.status, 201, await response.clone().text());
	});

	it("lets that administrator sign in", async () => {
		const response = await kitchen.fetch("/sign-in/email", {
			method: "POST",
			// better-auth rejects cookie-setting requests without an origin;
			// the sign-in page is served from the issuer, so a browser sends one.
			headers: { "content-type": "application/json", origin: kitchen.url },
			body: JSON.stringify({ email: admin.email, password: admin.password }),
		});
		assert.equal(response.status, 200, await response.clone().text());
		assert.ok(response.headers.getSetCookie().some((cookie) => cookie.includes("session_token")));
	});

	it("closes the link once an account exists", async () => {
		assert.equal((await kitchen.fetch(`/bootstrap?token=${kitchen.bootstrapToken}`)).status, 410);

		const response = await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				token: kitchen.bootstrapToken,
				name: "Second",
				email: "second@example.com",
				password: "correct horse battery staple",
			}),
		});
		assert.equal(response.status, 410);
	});
});

describe("the authorization code flow", () => {
	let kitchen: Harness;
	let cookie = "";
	let client: { client_id: string; client_secret: string };

	const admin = {
		name: "Grace",
		email: "grace@example.com",
		password: "correct horse battery staple",
	};
	const redirectURI = "https://kitchen.apps.example.com/auth/callback";

	before(async () => {
		kitchen = await startHarness();

		await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ token: kitchen.bootstrapToken, ...admin }),
		});
		const signIn = await kitchen.fetch("/sign-in/email", {
			method: "POST",
			headers: { "content-type": "application/json", origin: kitchen.url },
			body: JSON.stringify({ email: admin.email, password: admin.password }),
		});
		cookie = signIn.headers
			.getSetCookie()
			.map((value) => value.split(";")[0])
			.join("; ");

		const registration = await kitchen.fetch("/oauth2/register", {
			method: "POST",
			headers: { "content-type": "application/json", "x-api-key": kitchen.serviceKey },
			body: JSON.stringify({
				client_name: "kitchen-ui",
				redirect_uris: [redirectURI],
				grant_types: ["authorization_code", "refresh_token"],
			}),
		});
		client = (await registration.json()) as typeof client;
	});

	after(async () => {
		await kitchen.stop();
	});

	it("issues an ID token to a PKCE client the operator registered", async () => {
		const verifier = randomBytes(32).toString("base64url");
		const challenge = createHash("sha256").update(verifier).digest("base64url");
		const state = randomBytes(8).toString("hex");

		const authorize = await kitchen.fetch(
			`/oauth2/authorize?${new URLSearchParams({
				response_type: "code",
				client_id: client.client_id,
				redirect_uri: redirectURI,
				scope: "openid profile email",
				state,
				code_challenge: challenge,
				code_challenge_method: "S256",
			})}`,
			{ headers: { cookie }, redirect: "manual" },
		);

		// A first-time client sends the user to the consent screen, carrying
		// the signed authorization request along. A browser navigation gets a
		// 302; Node's fetch marks its requests as CORS, which the provider
		// answers with the same redirect as JSON.
		const target =
			authorize.status === 302
				? (authorize.headers.get("location") ?? "")
				: ((await authorize.clone().json().catch(() => ({}))) as { url?: string }).url;
		assert.ok(target, `authorize did not redirect: ${authorize.status} ${await authorize.text()}`);
		const consentLocation = new URL(target, kitchen.url);
		assert.equal(consentLocation.pathname, "/consent");

		const consent = await kitchen.fetch("/oauth2/consent", {
			method: "POST",
			headers: { "content-type": "application/json", origin: kitchen.url, cookie },
			body: JSON.stringify({ accept: true, oauth_query: consentLocation.search.slice(1) }),
		});
		assert.equal(consent.status, 200, await consent.clone().text());

		const decision = (await consent.json()) as { url?: string; redirect_uri?: string };
		const callback = decision.url ?? decision.redirect_uri;
		assert.ok(callback, "consent hands back the client's callback");
		const callbackURL = new URL(callback);
		assert.equal(`${callbackURL.origin}${callbackURL.pathname}`, redirectURI);
		assert.equal(callbackURL.searchParams.get("state"), state);

		const code = callbackURL.searchParams.get("code");
		assert.ok(code, "the redirect carries an authorization code");

		const token = await kitchen.fetch("/oauth2/token", {
			method: "POST",
			headers: {
				"content-type": "application/x-www-form-urlencoded",
				authorization: `Basic ${Buffer.from(`${client.client_id}:${client.client_secret}`).toString("base64")}`,
			},
			body: new URLSearchParams({
				grant_type: "authorization_code",
				code,
				redirect_uri: redirectURI,
				code_verifier: verifier,
			}),
		});
		assert.equal(token.status, 200, await token.clone().text());

		const tokens = (await token.json()) as { access_token: string; id_token: string };
		assert.ok(tokens.access_token);
		assert.ok(tokens.id_token);

		const claims = JSON.parse(
			Buffer.from(tokens.id_token.split(".")[1] ?? "", "base64url").toString("utf8"),
		) as Record<string, unknown>;
		assert.equal(claims.iss, kitchen.url);
		assert.equal(claims.aud, client.client_id);
		assert.equal(claims.email, admin.email);

		const userinfo = await kitchen.fetch("/oauth2/userinfo", {
			headers: { authorization: `Bearer ${tokens.access_token}` },
		});
		assert.equal(userinfo.status, 200, await userinfo.clone().text());
		assert.equal(((await userinfo.json()) as { email: string }).email, admin.email);
	});
});

describe("the Kitchen UI client and the operator API's audience", () => {
	let kitchen: Harness;
	let cookie = "";

	const apiURL = "https://kitchen.apps.example.com";
	const redirectURI = `${apiURL}/auth/callback`;
	const admin = {
		name: "Ada",
		email: "ada@example.com",
		password: "correct horse battery staple",
	};

	/** The claims of a JWT, without checking a signature the tests do not own. */
	const claimsOf = (token: string): Record<string, unknown> =>
		JSON.parse(Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8")) as Record<string, unknown>;

	before(async () => {
		kitchen = await startHarness({
			apiURL,
			ui: { clientId: "kitchen-ui", redirectURIs: [redirectURI] },
		});

		await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ token: kitchen.bootstrapToken, ...admin }),
		});
		const signIn = await kitchen.fetch("/sign-in/email", {
			method: "POST",
			headers: { "content-type": "application/json", origin: kitchen.url },
			body: JSON.stringify({ email: admin.email, password: admin.password }),
		});
		cookie = signIn.headers
			.getSetCookie()
			.map((value) => value.split(";")[0])
			.join("; ");
	});

	after(async () => {
		await kitchen.stop();
	});

	it("issues the UI a token for the operator API, with PKCE and no client secret", async () => {
		const verifier = randomBytes(32).toString("base64url");
		const challenge = createHash("sha256").update(verifier).digest("base64url");

		// The seeded client skips consent, so authorize hands back the
		// callback directly instead of routing through the consent screen.
		const authorize = await kitchen.fetch(
			`/oauth2/authorize?${new URLSearchParams({
				response_type: "code",
				client_id: "kitchen-ui",
				redirect_uri: redirectURI,
				scope: "openid profile email",
				state: "opaque",
				code_challenge: challenge,
				code_challenge_method: "S256",
			})}`,
			{ headers: { cookie }, redirect: "manual" },
		);
		const target =
			authorize.status === 302
				? (authorize.headers.get("location") ?? "")
				: ((await authorize.clone().json().catch(() => ({}))) as { url?: string }).url;
		assert.ok(target, `authorize did not redirect: ${authorize.status} ${await authorize.text()}`);

		const callbackURL = new URL(target, kitchen.url);
		assert.equal(`${callbackURL.origin}${callbackURL.pathname}`, redirectURI);
		const code = callbackURL.searchParams.get("code");
		assert.ok(code, `the redirect carries an authorization code: ${callbackURL.search}`);

		const token = await kitchen.fetch("/oauth2/token", {
			method: "POST",
			headers: { "content-type": "application/x-www-form-urlencoded" },
			body: new URLSearchParams({
				grant_type: "authorization_code",
				// A public client authenticates with nothing but its id and
				// the verifier for the challenge it sent.
				client_id: "kitchen-ui",
				code,
				redirect_uri: redirectURI,
				code_verifier: verifier,
				// The resource indicator: "a token for the operator API,
				// please", which is what makes the access token a JWT the
				// operator can validate against the JWKS.
				resource: apiURL,
			}),
		});
		assert.equal(token.status, 200, await token.clone().text());

		const tokens = (await token.json()) as { access_token: string };
		const claims = claimsOf(tokens.access_token);
		assert.equal(claims.iss, kitchen.url);
		// With `openid` in the scopes the provider adds the userinfo endpoint
		// to the audience, so this is a list. The operator accepts a token
		// whose audience *contains* the API or the issuer.
		const audience = Array.isArray(claims.aud) ? claims.aud : [claims.aud];
		assert.ok(
			audience.includes(apiURL),
			`the operator API only accepts tokens minted for it or for the issuer: ${JSON.stringify(audience)}`,
		);
		assert.equal(claims.azp, "kitchen-ui");
		assert.ok(claims.sub, "the token names the account it belongs to");
		// Neither the UI nor the operator API calls /oauth2/userinfo, so the
		// account's name and address have to travel on the access token — it is
		// what both of them show the person signed in, and what the API records
		// as the author of everything it writes.
		assert.equal(claims.name, admin.name, "the granted `profile` scope puts the account's name on the token");
		assert.equal(claims.email, admin.email, "the granted `email` scope puts the address on the token");
	});

	it("refuses a token without the PKCE verifier the client committed to", async () => {
		const challenge = createHash("sha256").update(randomBytes(32).toString("base64url")).digest("base64url");
		const authorize = await kitchen.fetch(
			`/oauth2/authorize?${new URLSearchParams({
				response_type: "code",
				client_id: "kitchen-ui",
				redirect_uri: redirectURI,
				scope: "openid",
				state: "opaque",
				code_challenge: challenge,
				code_challenge_method: "S256",
			})}`,
			{ headers: { cookie }, redirect: "manual" },
		);
		const target =
			authorize.status === 302
				? (authorize.headers.get("location") ?? "")
				: ((await authorize.clone().json().catch(() => ({}))) as { url?: string }).url;
		const code = new URL(target ?? "", kitchen.url).searchParams.get("code");
		assert.ok(code);

		const token = await kitchen.fetch("/oauth2/token", {
			method: "POST",
			headers: { "content-type": "application/x-www-form-urlencoded" },
			body: new URLSearchParams({
				grant_type: "authorization_code",
				client_id: "kitchen-ui",
				code,
				redirect_uri: redirectURI,
				code_verifier: randomBytes(32).toString("base64url"),
			}),
		});
		assert.equal(token.ok, false, "a stolen code must be useless without the verifier");
	});

	it("refuses a token for a resource that is not the API or the issuer", async () => {
		const verifier = randomBytes(32).toString("base64url");
		const challenge = createHash("sha256").update(verifier).digest("base64url");
		const authorize = await kitchen.fetch(
			`/oauth2/authorize?${new URLSearchParams({
				response_type: "code",
				client_id: "kitchen-ui",
				redirect_uri: redirectURI,
				scope: "openid",
				state: "opaque",
				code_challenge: challenge,
				code_challenge_method: "S256",
			})}`,
			{ headers: { cookie }, redirect: "manual" },
		);
		const target =
			authorize.status === 302
				? (authorize.headers.get("location") ?? "")
				: ((await authorize.clone().json().catch(() => ({}))) as { url?: string }).url;
		const code = new URL(target ?? "", kitchen.url).searchParams.get("code");
		assert.ok(code);

		const response = await kitchen.fetch("/oauth2/token", {
			method: "POST",
			headers: { "content-type": "application/x-www-form-urlencoded" },
			body: new URLSearchParams({
				grant_type: "authorization_code",
				client_id: "kitchen-ui",
				code,
				redirect_uri: redirectURI,
				code_verifier: verifier,
				resource: "https://somewhere-else.example.com",
			}),
		});
		assert.equal(response.ok, false, "any audience a client asks for would otherwise be minted");
		assert.match(JSON.stringify(await response.json()), /resource/i);
	});

	it("exchanges an API key for a token the operator API accepts", async () => {
		// How CI authenticates: the key is a credential at the issuer, and
		// the issuer turns it into the short-lived JWT the operator
		// validates. The operator never sees the key, and keeps no session.
		const response = await kitchen.fetch("/token", {
			headers: { "x-api-key": kitchen.serviceKey },
		});
		assert.equal(response.status, 200, await response.clone().text());

		const { token } = (await response.json()) as { token: string };
		const claims = claimsOf(token);
		assert.equal(claims.iss, kitchen.url);
		assert.equal(claims.aud, kitchen.url, "a session token is minted for the issuer itself");
		assert.ok(claims.exp, "the token expires");
	});
});

/**
 * What keeps a dashboard tab signed in without the redirect it used to make
 * once per access-token lifetime (issue #25): the sign-in asks for
 * `offline_access`, and the refresh token it gets back buys new access tokens
 * for the API in the background.
 */
describe("renewing the dashboard's session", () => {
	let kitchen: Harness;
	let cookie = "";

	const apiURL = "https://kitchen.apps.example.com";
	const redirectURI = `${apiURL}/auth/callback`;
	const admin = {
		name: "Katherine",
		email: "katherine@example.com",
		password: "correct horse battery staple",
	};

	const claimsOf = (token: string): Record<string, unknown> =>
		JSON.parse(Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8")) as Record<string, unknown>;

	interface Tokens {
		access_token?: string;
		refresh_token?: string;
		error?: string;
	}

	/** One sign-in the way the dashboard makes it: the seeded public client,
	 * PKCE instead of a secret, and a token minted for the operator API. */
	async function signIn(scope: string): Promise<Tokens> {
		const verifier = randomBytes(32).toString("base64url");
		const challenge = createHash("sha256").update(verifier).digest("base64url");
		const authorize = await kitchen.fetch(
			`/oauth2/authorize?${new URLSearchParams({
				response_type: "code",
				client_id: "kitchen-ui",
				redirect_uri: redirectURI,
				scope,
				state: "opaque",
				code_challenge: challenge,
				code_challenge_method: "S256",
				resource: apiURL,
			})}`,
			{ headers: { cookie }, redirect: "manual" },
		);
		const target =
			authorize.status === 302
				? (authorize.headers.get("location") ?? "")
				: ((await authorize.clone().json().catch(() => ({}))) as { url?: string }).url;
		assert.ok(target, `authorize did not redirect: ${authorize.status} ${await authorize.text()}`);
		const code = new URL(target, kitchen.url).searchParams.get("code");
		assert.ok(code, "the redirect carries an authorization code");

		const response = await kitchen.fetch("/oauth2/token", {
			method: "POST",
			headers: { "content-type": "application/x-www-form-urlencoded" },
			body: new URLSearchParams({
				grant_type: "authorization_code",
				client_id: "kitchen-ui",
				code,
				redirect_uri: redirectURI,
				code_verifier: verifier,
				resource: apiURL,
			}),
		});
		assert.equal(response.status, 200, await response.clone().text());
		return (await response.json()) as Tokens;
	}

	/** The renewal itself: no interaction, no redirect, and the same resource
	 * indicator, because a token that did not name the API comes back opaque. */
	const refresh = (refreshToken: string): Promise<Response> =>
		kitchen.fetch("/oauth2/token", {
			method: "POST",
			headers: { "content-type": "application/x-www-form-urlencoded" },
			body: new URLSearchParams({
				grant_type: "refresh_token",
				client_id: "kitchen-ui",
				refresh_token: refreshToken,
				resource: apiURL,
			}),
		});

	before(async () => {
		kitchen = await startHarness({
			apiURL,
			ui: { clientId: "kitchen-ui", redirectURIs: [redirectURI] },
		});

		await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ token: kitchen.bootstrapToken, ...admin }),
		});
		const signedIn = await kitchen.fetch("/sign-in/email", {
			method: "POST",
			headers: { "content-type": "application/json", origin: kitchen.url },
			body: JSON.stringify({ email: admin.email, password: admin.password }),
		});
		cookie = signedIn.headers
			.getSetCookie()
			.map((value) => value.split(";")[0])
			.join("; ");
	});

	after(async () => {
		await kitchen.stop();
	});

	it("hands out a refresh token only to a sign-in that asked for one", async () => {
		assert.equal(
			(await signIn("openid profile email")).refresh_token,
			undefined,
			"a session that cannot be renewed is what `offline_access` opts out of",
		);
		assert.ok((await signIn("openid profile email offline_access")).refresh_token);
	});

	it("renews the token for the API without another trip through the login", async () => {
		const first = await signIn("openid profile email offline_access");
		assert.ok(first.refresh_token);
		assert.ok(first.access_token);

		// `iat` and `exp` count in whole seconds, and the signature is
		// deterministic: a renewal inside the same second as the sign-in mints
		// the same token byte for byte, and proves nothing about the deadline
		// moving. Crossing a second boundary is what makes the assertion real.
		await new Promise((resolve) => setTimeout(resolve, 1_100));

		const response = await refresh(first.refresh_token);
		assert.equal(response.status, 200, await response.clone().text());
		const renewed = (await response.json()) as Tokens;
		assert.ok(renewed.access_token);
		assert.ok(
			Number(claimsOf(renewed.access_token).exp) > Number(claimsOf(first.access_token).exp),
			"the whole point: the renewed token outlives the one it replaces",
		);

		// Everything the operator API and the dashboard read off the first
		// token has to survive the renewal, or the tab renews into a session
		// the API refuses and an account menu showing an opaque id.
		const claims = claimsOf(renewed.access_token);
		const audience = Array.isArray(claims.aud) ? claims.aud : [claims.aud];
		assert.ok(audience.includes(apiURL), `the renewed token is still for the API: ${JSON.stringify(audience)}`);
		assert.equal(claims.azp, "kitchen-ui");
		assert.equal(claims.email, admin.email);
		assert.equal(claims.name, admin.name);
	});

	it("rotates the refresh token on every renewal", async () => {
		const first = await signIn("openid profile email offline_access");
		assert.ok(first.refresh_token);

		const renewed = (await (await refresh(first.refresh_token)).json()) as Tokens;
		assert.ok(renewed.refresh_token);
		assert.notEqual(renewed.refresh_token, first.refresh_token, "a refresh token is single-use");

		const again = await refresh(renewed.refresh_token);
		assert.equal(again.status, 200, await again.clone().text());
	});

	// Last, deliberately: replaying a spent token tears down every refresh
	// token the provider issued for this account and client (RFC 9700 §4.14),
	// so nothing after it would have a session left to renew.
	it("treats a replayed refresh token as a compromised session", async () => {
		const first = await signIn("openid profile email offline_access");
		assert.ok(first.refresh_token);
		const renewed = (await (await refresh(first.refresh_token)).json()) as Tokens;
		assert.ok(renewed.refresh_token);

		const replay = await refresh(first.refresh_token);
		assert.equal(replay.ok, false, "a spent refresh token must not buy a second access token");

		// The dashboard keeps its refresh token in localStorage, so a stolen
		// one is the threat this answers: the moment both copies are used, the
		// family goes, and the real tab is asked to sign in again.
		const after = await refresh(renewed.refresh_token);
		assert.equal(after.ok, false, "the replay takes the whole family with it");
	});
});

/**
 * Issue #314: every client this issuer knows could ask for a token for the
 * operator API, and the API took it as the person who consented.
 *
 * An `oidcClient` claim registers a client for whatever a project developer
 * is deploying — that is the feature. Its consent screen lists scopes and has
 * never listed an audience, so somebody signing in to a colleague's app was
 * asked about `profile` and `email` and got, if the app asked for it, a token
 * carrying every role they hold on this platform. The resource indicator is
 * the platform's own clients' alone until third-party API access is a feature
 * with a consent screen that says so.
 */
describe("the resource indicator belongs to the platform's own clients", () => {
	let kitchen: Harness;
	let cookie = "";
	/** The client an oidcClient claim would have registered for an app. */
	let app = { client_id: "", client_secret: "" };

	const apiURL = "https://kitchen.apps.example.com";
	const dashboardRedirectURI = `${apiURL}/auth/callback`;
	const appRedirectURI = "https://shop-pr-7.apps.example.com/auth/callback";
	const admin = {
		name: "Radia",
		email: "radia@example.com",
		password: "correct horse battery staple",
	};

	const claimsOf = (token: string): Record<string, unknown> =>
		JSON.parse(Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8")) as Record<string, unknown>;

	/**
	 * An authorization code for the application's client, consent and all —
	 * the whole of what somebody pressing "Allow" on its screen produces.
	 */
	async function appCode(verifier: string): Promise<string> {
		const challenge = createHash("sha256").update(verifier).digest("base64url");
		const authorize = await kitchen.fetch(
			`/oauth2/authorize?${new URLSearchParams({
				response_type: "code",
				client_id: app.client_id,
				redirect_uri: appRedirectURI,
				scope: "openid profile email",
				state: "opaque",
				code_challenge: challenge,
				code_challenge_method: "S256",
			})}`,
			{ headers: { cookie }, redirect: "manual" },
		);
		const target =
			authorize.status === 302
				? (authorize.headers.get("location") ?? "")
				: ((await authorize.clone().json().catch(() => ({}))) as { url?: string }).url;
		assert.ok(target, `authorize did not redirect: ${authorize.status} ${await authorize.text()}`);

		const location = new URL(target, kitchen.url);
		let callback = location.href;
		if (location.pathname === "/consent") {
			const consent = await kitchen.fetch("/oauth2/consent", {
				method: "POST",
				headers: { "content-type": "application/json", origin: kitchen.url, cookie },
				body: JSON.stringify({ accept: true, oauth_query: location.search.slice(1) }),
			});
			assert.equal(consent.status, 200, await consent.clone().text());
			const decision = (await consent.json()) as { url?: string; redirect_uri?: string };
			const accepted = decision.url ?? decision.redirect_uri;
			assert.ok(accepted, "consent hands back the client's callback");
			callback = accepted;
		}
		const code = new URL(callback, kitchen.url).searchParams.get("code");
		assert.ok(code, "the redirect carries an authorization code");
		return code;
	}

	/** The application's code exchange, which authenticates with HTTP Basic. */
	const appToken = (code: string, verifier: string, resource?: string): Promise<Response> =>
		kitchen.fetch("/oauth2/token", {
			method: "POST",
			headers: {
				"content-type": "application/x-www-form-urlencoded",
				authorization: `Basic ${Buffer.from(`${app.client_id}:${app.client_secret}`).toString("base64")}`,
			},
			body: new URLSearchParams({
				grant_type: "authorization_code",
				code,
				redirect_uri: appRedirectURI,
				code_verifier: verifier,
				...(resource ? { resource } : {}),
			}),
		});

	before(async () => {
		kitchen = await startHarness({
			apiURL,
			ui: { clientId: "kitchen-ui", redirectURIs: [dashboardRedirectURI] },
		});

		await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ token: kitchen.bootstrapToken, ...admin }),
		});
		const signIn = await kitchen.fetch("/sign-in/email", {
			method: "POST",
			headers: { "content-type": "application/json", origin: kitchen.url },
			body: JSON.stringify({ email: admin.email, password: admin.password }),
		});
		cookie = signIn.headers
			.getSetCookie()
			.map((value) => value.split(";")[0])
			.join("; ");

		// Registered exactly the way the operator registers an oidcClient
		// claim's client: RFC 7591, under the operator's service credential.
		const registration = await kitchen.fetch("/oauth2/register", {
			method: "POST",
			headers: { "content-type": "application/json", "x-api-key": kitchen.serviceKey },
			body: JSON.stringify({
				client_name: "shop preview",
				redirect_uris: [appRedirectURI],
				grant_types: ["authorization_code", "refresh_token"],
			}),
		});
		assert.equal(registration.status, 200, await registration.clone().text());
		app = (await registration.json()) as typeof app;
	});

	after(async () => {
		await kitchen.stop();
	});

	it("refuses an application's client a token for the operator API", async () => {
		const verifier = randomBytes(32).toString("base64url");
		const response = await appToken(await appCode(verifier), verifier, apiURL);

		assert.equal(response.ok, false, "an app's client must not be able to act on the API as its user");
		const body = (await response.json()) as { error?: string };
		assert.equal(body.error, "invalid_target");
	});

	// The issuer itself is an audience too, and the operator API accepts a
	// token for it — that is the CI path. So "any resource but the API" is not
	// the rule; the rule is that a resource indicator is not this client's to
	// use at all.
	it("refuses an application's client a token for the issuer", async () => {
		const verifier = randomBytes(32).toString("base64url");
		const response = await appToken(await appCode(verifier), verifier, kitchen.url);

		assert.equal(response.ok, false);
		assert.equal(((await response.json()) as { error?: string }).error, "invalid_target");
	});

	// The feature an oidcClient claim exists for, untouched: the app signs
	// somebody in and learns who they are. What it does not get is a token
	// anything else in the platform will honour.
	it("still signs somebody in to the application", async () => {
		const verifier = randomBytes(32).toString("base64url");
		const response = await appToken(await appCode(verifier), verifier);
		assert.equal(response.status, 200, await response.clone().text());

		const tokens = (await response.json()) as { access_token: string; id_token: string };
		assert.ok(tokens.id_token, "an ID token is the whole of what the app asked for");
		assert.equal(claimsOf(tokens.id_token).email, admin.email);
	});

	it("still issues the dashboard a token for the operator API", async () => {
		const verifier = randomBytes(32).toString("base64url");
		const challenge = createHash("sha256").update(verifier).digest("base64url");
		const authorize = await kitchen.fetch(
			`/oauth2/authorize?${new URLSearchParams({
				response_type: "code",
				client_id: "kitchen-ui",
				redirect_uri: dashboardRedirectURI,
				scope: "openid profile email",
				state: "opaque",
				code_challenge: challenge,
				code_challenge_method: "S256",
			})}`,
			{ headers: { cookie }, redirect: "manual" },
		);
		const target =
			authorize.status === 302
				? (authorize.headers.get("location") ?? "")
				: ((await authorize.clone().json().catch(() => ({}))) as { url?: string }).url;
		const code = new URL(target ?? "", kitchen.url).searchParams.get("code");
		assert.ok(code);

		const response = await kitchen.fetch("/oauth2/token", {
			method: "POST",
			headers: { "content-type": "application/x-www-form-urlencoded" },
			body: new URLSearchParams({
				grant_type: "authorization_code",
				client_id: "kitchen-ui",
				code,
				redirect_uri: dashboardRedirectURI,
				code_verifier: verifier,
				resource: apiURL,
			}),
		});
		assert.equal(response.status, 200, await response.clone().text());

		const claims = claimsOf(((await response.json()) as { access_token: string }).access_token);
		const audience = Array.isArray(claims.aud) ? claims.aud : [claims.aud];
		assert.ok(audience.includes(apiURL), `the dashboard's token is still for the API: ${JSON.stringify(audience)}`);
		assert.equal(claims.azp, "kitchen-ui");
	});

	// The CI path names no client and asks for no resource, so nothing above
	// applies to it — and it is what `kitchen` on a developer's machine and in
	// a pipeline authenticates with.
	it("still exchanges an API key for a token the operator API accepts", async () => {
		const response = await kitchen.fetch("/token", { headers: { "x-api-key": kitchen.serviceKey } });
		assert.equal(response.status, 200, await response.clone().text());

		const claims = claimsOf(((await response.json()) as { token: string }).token);
		assert.equal(claims.aud, kitchen.url);
		assert.equal(claims.azp, undefined, "a session token names no client, which is what the API reads as CI");
	});
});
