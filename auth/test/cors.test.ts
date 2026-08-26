import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

import { authOptions } from "../src/auth.js";
import { allowedOrigins, type Config } from "../src/config.js";
import { startHarness, type Harness } from "./support.js";

/**
 * The dashboard is served from another hostname than the issuer, so every call
 * it makes here is cross-origin. These are browser-only failures: without the
 * headers below the requests still succeed on the wire, and only the browser
 * refuses to hand the response to the script — which is why curl-based checks
 * cannot catch a regression here.
 */
const DASHBOARD = "https://kitchen.example.com";
const STRANGER = "https://not-the-dashboard.example.com";

describe("cross-origin access for the dashboard", () => {
	let kitchen: Harness;

	before(async () => {
		kitchen = await startHarness({ apiURL: DASHBOARD });
	});

	after(async () => {
		await kitchen.stop();
	});

	it("lets the dashboard read the discovery document", async () => {
		const response = await kitchen.fetch("/.well-known/openid-configuration", {
			headers: { origin: DASHBOARD },
		});

		assert.equal(response.status, 200);
		assert.equal(response.headers.get("access-control-allow-origin"), DASHBOARD);
		assert.equal(response.headers.get("access-control-allow-credentials"), "true");
		assert.match(response.headers.get("vary") ?? "", /origin/i);
	});

	it("answers the token endpoint's preflight rather than rejecting the method", async () => {
		const response = await kitchen.fetch("/oauth2/token", {
			method: "OPTIONS",
			headers: {
				origin: DASHBOARD,
				"access-control-request-method": "POST",
				"access-control-request-headers": "content-type",
			},
		});

		assert.equal(response.status, 204);
		assert.equal(response.headers.get("access-control-allow-origin"), DASHBOARD);
		assert.match(response.headers.get("access-control-allow-methods") ?? "", /POST/);
		assert.match(response.headers.get("access-control-allow-headers") ?? "", /content-type/);
	});

	it("reflects nothing back to an origin the platform does not know", async () => {
		const response = await kitchen.fetch("/.well-known/openid-configuration", {
			headers: { origin: STRANGER },
		});

		// The document itself is public; what an unknown origin must not get is
		// permission to read it from script.
		assert.equal(response.status, 200);
		assert.equal(response.headers.get("access-control-allow-origin"), null);
	});

	it("refuses a preflight from an unknown origin", async () => {
		const response = await kitchen.fetch("/oauth2/token", {
			method: "OPTIONS",
			headers: { origin: STRANGER, "access-control-request-method": "POST" },
		});

		assert.equal(response.status, 403);
	});

});

/*
 * The allow-list itself is worth checking without a server: a second harness
 * would reset the shared schema underneath every other suite.
 */
describe("the origins a browser may call the issuer from", () => {
	const base = {
		port: 0,
		baseURL: "https://auth.example.com",
		secret: "secret",
		databaseURL: "",
		databaseWaitSeconds: 0,
		serviceAccountEmail: "operator@kitchen.local",
		trustedOrigins: [],
		allowSocialSignUp: false,
	} as unknown as Config;

	it("takes the dashboard's origin from the UI client's redirect URIs", () => {
		const origins = allowedOrigins({
			...base,
			ui: { clientId: "kitchen-ui", redirectURIs: [`${DASHBOARD}/auth/callback`] },
		});

		assert.ok(origins.has(DASHBOARD), "the UI signs in from where it redirects back to");
	});

	it("includes the issuer and the API, and drops anything unparseable", () => {
		const origins = allowedOrigins({
			...base,
			apiURL: DASHBOARD,
			trustedOrigins: ["not a url"],
		});

		assert.ok(origins.has(DASHBOARD));
		assert.ok(origins.has("https://auth.example.com"));
		assert.equal(origins.has("not a url"), false);
	});
});

/*
 * And the same list has to reach better-auth, which refuses a cookie-bearing
 * POST from an origin it does not trust. That is every write the account
 * screen makes, so a dashboard missing from here is a 403 nothing in the
 * browser can explain — and it is not the CORS failure above, which the two
 * lists having one source is what rules out.
 */
describe("the origins better-auth accepts a signed-in write from", () => {
	const config = {
		port: 0,
		baseURL: "https://auth.example.com",
		secret: "secret",
		databaseURL: "",
		databaseWaitSeconds: 0,
		serviceAccountEmail: "operator@kitchen.local",
		apiURL: DASHBOARD,
		trustedOrigins: [],
		allowSocialSignUp: false,
	} as unknown as Config;

	it("trusts the dashboard, and nowhere else", () => {
		// The pool is never touched: `authOptions` only hands it on.
		const trusted = authOptions(config, {} as never).trustedOrigins as string[];

		assert.ok(trusted.includes(DASHBOARD), "the account screen posts from here");
		assert.ok(trusted.includes("https://auth.example.com"), "so do the issuer's own pages");
		assert.equal(trusted.includes(STRANGER), false);
	});

	it("asks nobody to sign in again before they may see their own sessions", () => {
		// better-auth guards /list-sessions on a session created within the last
		// day, and nothing here can make one fresher without ending it.
		assert.equal(authOptions(config, {} as never).session?.freshAge, 0);
	});
});
