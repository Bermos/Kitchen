import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

import { startHarness, type Harness } from "./support.js";

/**
 * The operator's client management: the half of `ResourceClaim` type
 * `oidcClient` that lives at the identity provider.
 *
 * Registration is standard and covered in idp.test.ts. What is tested here is
 * what the OAuth provider plugin does not offer at all — changing a
 * registered client's redirect list, and taking the client away — because
 * that is what keeps an application's sign-in working as previews come and
 * go, and it is a Kitchen endpoint rather than an RFC 7592 one.
 */
describe("the operator's OAuth clients", () => {
	let kitchen: Harness;

	before(async () => {
		kitchen = await startHarness();
	});

	after(async () => {
		await kitchen.stop();
	});

	/** Registers a client the way the operator does, and answers its id. */
	async function register(name: string, redirectURIs: string[]): Promise<string> {
		const response = await kitchen.fetch("/oauth2/register", {
			method: "POST",
			headers: { "content-type": "application/json", "x-api-key": kitchen.serviceKey },
			body: JSON.stringify({
				client_name: name,
				redirect_uris: redirectURIs,
				grant_types: ["authorization_code", "refresh_token"],
			}),
		});
		assert.equal(response.status, 200, await response.clone().text());
		const client = (await response.json()) as { client_id: string };
		return client.client_id;
	}

	function setRedirectURIs(clientId: string, redirectURIs: string[], key = kitchen.serviceKey) {
		return kitchen.fetch("/kitchen/clients", {
			method: "PUT",
			headers: { "content-type": "application/json", "x-api-key": key },
			body: JSON.stringify({ clientId, redirectURIs }),
		});
	}

	it("replaces the redirect list of a client it registered", async () => {
		const clientId = await register("shop", ["https://shop.apps.example.com/auth/callback"]);

		const response = await setRedirectURIs(clientId, [
			"https://shop.apps.example.com/auth/callback",
			"https://shop-pr-42.apps.example.com/auth/callback",
		]);
		assert.equal(response.status, 200, await response.clone().text());

		const client = (await response.json()) as { clientId: string; redirectURIs: string[] };
		assert.equal(client.clientId, clientId);
		assert.deepEqual(client.redirectURIs, [
			"https://shop.apps.example.com/auth/callback",
			"https://shop-pr-42.apps.example.com/auth/callback",
		]);
	});

	it("lets a preview's callback be taken away again", async () => {
		const clientId = await register("blog", [
			"https://blog.apps.example.com/auth/callback",
			"https://blog-pr-7.apps.example.com/auth/callback",
		]);

		await setRedirectURIs(clientId, ["https://blog.apps.example.com/auth/callback"]);

		// The issuer is what enforces the list, so the test asks the issuer:
		// an authorization request for the closed preview's callback must not
		// be sent back to it.
		const authorize = await kitchen.fetch(
			`/oauth2/authorize?response_type=code&client_id=${clientId}` +
				"&redirect_uri=https%3A%2F%2Fblog-pr-7.apps.example.com%2Fauth%2Fcallback" +
				"&scope=openid&code_challenge=abcabcabcabcabcabcabcabcabcabcabcabcabcabca&code_challenge_method=S256",
			{ redirect: "manual" },
		);
		const location = authorize.headers.get("location") ?? "";
		assert.ok(
			!location.startsWith("https://blog-pr-7.apps.example.com/"),
			`a deregistered callback was still honoured: ${authorize.status} ${location}`,
		);
	});

	it("refuses an empty redirect list", async () => {
		const clientId = await register("empty", ["https://empty.apps.example.com/auth/callback"]);
		const response = await setRedirectURIs(clientId, []);
		assert.equal(response.status, 400);
	});

	it("deregisters a client with its claim", async () => {
		const clientId = await register("gone", ["https://gone.apps.example.com/auth/callback"]);

		const removal = await kitchen.fetch(`/kitchen/clients?clientId=${clientId}`, {
			method: "DELETE",
			headers: { "x-api-key": kitchen.serviceKey },
		});
		assert.equal(removal.status, 200, await removal.clone().text());

		// Gone means gone: the same call again is a 404, which is what the
		// operator's finalizer reads as "already removed".
		const again = await kitchen.fetch(`/kitchen/clients?clientId=${clientId}`, {
			method: "DELETE",
			headers: { "x-api-key": kitchen.serviceKey },
		});
		assert.equal(again.status, 404);
	});

	it("answers only to the operator's credential", async () => {
		const clientId = await register("guarded", ["https://guarded.apps.example.com/auth/callback"]);

		const anonymous = await kitchen.fetch("/kitchen/clients", {
			method: "PUT",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ clientId, redirectURIs: ["https://impostor.example.com/callback"] }),
		});
		assert.equal(anonymous.status, 401);

		const wrongKey = await setRedirectURIs(
			clientId,
			["https://impostor.example.com/callback"],
			`${kitchen.serviceKey}-nope`,
		);
		assert.equal(wrongKey.status, 401);
	});
});

/**
 * The dashboard's own client is seeded straight into the table at start-up
 * and belongs to no account, which is what makes it a client the operator did
 * not register. Rewriting its redirect URIs would sign every person out of
 * the platform with no way back in, so this is the one refusal in the file
 * worth its own harness.
 */
describe("the dashboard's own client", () => {
	const uiClientId = "kitchen-ui";
	let kitchen: Harness;

	before(async () => {
		kitchen = await startHarness({
			ui: { clientId: uiClientId, redirectURIs: ["https://kitchen.apps.example.com/auth/callback"] },
		});
	});

	after(async () => {
		await kitchen.stop();
	});

	it("is not the operator's to change", async () => {
		const response = await kitchen.fetch("/kitchen/clients", {
			method: "PUT",
			headers: { "content-type": "application/json", "x-api-key": kitchen.serviceKey },
			body: JSON.stringify({
				clientId: uiClientId,
				redirectURIs: ["https://impostor.example.com/callback"],
			}),
		});
		assert.equal(response.status, 403, await response.clone().text());
	});

	it("is not the operator's to remove", async () => {
		const response = await kitchen.fetch(`/kitchen/clients?clientId=${uiClientId}`, {
			method: "DELETE",
			headers: { "x-api-key": kitchen.serviceKey },
		});
		assert.equal(response.status, 403);
	});
});
