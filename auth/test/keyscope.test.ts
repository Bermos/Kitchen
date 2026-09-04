import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

import type { IssuedProjectKey } from "../src/keys.js";
import { startHarness, type Harness } from "./support.js";

/**
 * What an API key may reach at the identity provider.
 *
 * `enableSessionForAPIKeys` is what makes a key an identity at all, and left
 * alone it makes a key an identity *everywhere*: a session on every
 * better-auth endpoint, which is a CI credential that can register OAuth
 * clients, mint further keys for its own machine account and change the
 * account it belongs to (issue #318). None of that is anything a pipeline
 * asks for, and all of it is a credential minting its own successors.
 *
 * These tests pin both halves of the answer. A CI key keeps the one exchange
 * `kitchen login` and CI make — and nothing else. The operator's own service
 * credential keeps everything, because registering a client per application
 * is what it is for, and it is bounded by *who it is* instead:
 * `clientPrivileges` admits the service account and refuses even a signed-in
 * administrator.
 */
describe("what an API key may reach", () => {
	let kitchen: Harness;
	/** A CI key: a machine account's credential, issued the way an admin issues one. */
	let ci: IssuedProjectKey;

	const ADMIN = "anna@example.com";
	const PASSWORD = "correct-horse-battery";

	/** A signed-in browser's cookie: what the issuer's login page leaves. */
	let browser: string;

	const withKey = (path: string, key: string, init: RequestInit = {}) =>
		kitchen.fetch(path, { ...init, headers: { ...init.headers, "x-api-key": key } });

	/** A client registration, as the operator makes it. */
	const registration = {
		client_name: "shop",
		redirect_uris: ["https://shop.apps.example.com/auth/callback"],
		grant_types: ["authorization_code", "refresh_token"],
	};

	before(async () => {
		kitchen = await startHarness();

		const issued = await withKey("/kitchen/keys", kitchen.serviceKey, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ project: "shop", name: "nightly" }),
		});
		assert.equal(issued.status, 201, await issued.clone().text());
		ci = (await issued.json()) as IssuedProjectKey;

		const created = await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				token: kitchen.bootstrapToken,
				email: ADMIN,
				name: "Anna",
				password: PASSWORD,
			}),
		});
		assert.equal(created.status, 201, await created.clone().text());

		const session = await kitchen.fetch("/sign-in/email", {
			method: "POST",
			headers: { "content-type": "application/json", origin: kitchen.url },
			body: JSON.stringify({ email: ADMIN, password: PASSWORD }),
		});
		assert.equal(session.status, 200, await session.clone().text());
		browser = session.headers
			.getSetCookie()
			.map((cookie) => cookie.split(";", 1)[0])
			.join("; ");
		assert.ok(browser, "signing in leaves a session cookie");
	});

	after(async () => {
		await kitchen.stop();
	});

	it("exchanges a CI key for a token, which is the whole of what CI needs", async () => {
		const exchanged = await withKey("/token", ci.key);
		assert.equal(exchanged.status, 200, await exchanged.clone().text());

		const { token } = (await exchanged.json()) as { token: string };
		const claims = JSON.parse(
			Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8"),
		) as { sub: string };
		assert.equal(claims.sub, ci.subject, "the token is the machine account's");
	});

	it("lets a CI key read the session it stands for", async () => {
		const session = await withKey("/get-session", ci.key);
		assert.equal(session.status, 200, await session.clone().text());

		const answer = (await session.json()) as { user?: { email?: string } } | null;
		assert.equal(answer?.user?.email, ci.email);
	});

	it("refuses a CI key the client registration endpoint", async () => {
		const registered = await withKey("/oauth2/register", ci.key, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(registration),
		});
		assert.equal(
			registered.status,
			403,
			"a key that can register a client can publish a confidential one the operator cannot see",
		);
	});

	it("refuses a CI key another key, so it cannot mint its own successors", async () => {
		const minted = await withKey("/api-key/create", ci.key, {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ name: "second" }),
		});
		assert.equal(minted.status, 403, await minted.clone().text());

		// The refusal is what matters, but so is what it leaves behind: one key
		// per machine account is what makes revoking a key unambiguous, and a
		// second key on the same account is invisible to `GET /kitchen/keys`,
		// which lists one row per account.
		const ctx = await kitchen.auth.$context;
		const keys = await ctx.adapter.findMany({
			model: "apikey",
			where: [{ field: "referenceId", value: ci.subject }],
		});
		assert.equal(keys.length, 1, "a machine account owns exactly the one key it was created with");
	});

	it("refuses a CI key everything else a session would reach", async () => {
		for (const [path, init] of [
			["/update-user", { method: "POST", body: JSON.stringify({ name: "not anna" }) }],
			["/change-password", { method: "POST", body: JSON.stringify({ newPassword: "x", currentPassword: "y" }) }],
			["/list-sessions", {}],
			["/api-key/list", {}],
		] as [string, RequestInit][]) {
			const response = await withKey(path, ci.key, {
				...init,
				headers: { "content-type": "application/json" },
			});
			assert.equal(response.status, 403, `${path} should be refused to a key: ${await response.clone().text()}`);
		}
	});

	it("keeps the operator's own credential unrestricted", async () => {
		const registered = await kitchen.fetch("/oauth2/register", {
			method: "POST",
			headers: { "content-type": "application/json", "x-api-key": kitchen.serviceKey },
			body: JSON.stringify(registration),
		});
		assert.equal(registered.status, 200, await registered.clone().text());

		const accounts = await withKey("/kitchen/accounts", kitchen.serviceKey);
		assert.equal(accounts.status, 200, await accounts.clone().text());
	});

	it("refuses a signed-in administrator a client registration too", async () => {
		// A client an application signs in with is `ResourceClaim` type
		// `oidcClient`: the operator registers it and keeps its redirect list in
		// step with the project's environments. One registered by hand is one
		// nothing maintains, and the door that admitted it is the same one every
		// CI key was walking through.
		const registered = await kitchen.fetch("/oauth2/register", {
			method: "POST",
			headers: { "content-type": "application/json", origin: kitchen.url, cookie: browser },
			body: JSON.stringify(registration),
		});
		assert.equal(
			registered.status,
			401,
			`client registration is the service account's: ${await registered.clone().text()}`,
		);
	});
});
