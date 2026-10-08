import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

import { startHarness, type Harness } from "./support.js";

/**
 * Who may register an upstream identity provider (src/upstream.ts).
 *
 * The SSO plugin's management endpoints are behind a session, and a signed-in
 * person's browser is one. A provider is an issuer this service believes about
 * who somebody is, so one registered by a person they run would sign them in
 * as any address they liked — an account created for an address before its
 * owner ever signs up, which is the account the API resolves that address to
 * when somebody is added as a member by it. Registering one is the platform's.
 */
describe("who may register an upstream identity provider", () => {
	let kitchen: Harness;
	let browser: string;

	const ADMIN = "anna@example.com";
	const PASSWORD = "correct-horse-battery";

	before(async () => {
		kitchen = await startHarness();

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

	it("refuses a signed-in person a provider of their own", async () => {
		const registered = await kitchen.fetch("/sso/register", {
			method: "POST",
			headers: { "content-type": "application/json", origin: kitchen.url, cookie: browser },
			body: JSON.stringify({
				providerId: "mine",
				issuer: "https://idp.attacker.example",
				domain: "example.com",
				oidcConfig: {
					clientId: "kitchen",
					clientSecret: "secret",
					skipDiscovery: true,
					authorizationEndpoint: "https://idp.attacker.example/authorize",
					tokenEndpoint: "https://idp.attacker.example/token",
					jwksEndpoint: "https://idp.attacker.example/jwks",
				},
			}),
		});
		assert.equal(registered.status, 403, await registered.clone().text());

		const ctx = await kitchen.auth.$context;
		const providers = await ctx.adapter.findMany({ model: "ssoProvider" });
		assert.equal(providers.length, 0, "no provider was written");
	});

	it("refuses a signed-in person the plugin's other management endpoints too", async () => {
		for (const [path, init] of [
			["/sso/providers", {}],
			["/sso/get-provider?providerId=mine", {}],
			["/sso/update-provider", { method: "POST", body: JSON.stringify({ providerId: "mine" }) }],
			["/sso/delete-provider", { method: "POST", body: JSON.stringify({ providerId: "mine" }) }],
		] as [string, RequestInit][]) {
			const response = await kitchen.fetch(path, {
				...init,
				headers: { "content-type": "application/json", origin: kitchen.url, cookie: browser },
			});
			assert.equal(response.status, 403, `${path} is not a person's: ${await response.clone().text()}`);
		}
	});

	it("leaves the platform's own credential able to manage providers", async () => {
		const listed = await kitchen.fetch("/sso/providers", {
			headers: { "x-api-key": kitchen.serviceKey },
		});
		assert.notEqual(listed.status, 403, await listed.clone().text());
	});
});
