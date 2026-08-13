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
