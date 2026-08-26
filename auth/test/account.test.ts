import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

import { startHarness, type Harness } from "./support.js";

/**
 * Managing one's own account, as the dashboard does it.
 *
 * The endpoints these exercise are better-auth's and were mounted all along —
 * what was missing was any way to reach them. They are reached from the
 * *dashboard*, which is a different origin from this service, carrying the
 * session cookie this service set on its own login page. Three separate things
 * have to agree for that to work, and only the middle one is tested here
 * because only it is ours to get wrong:
 *
 * - the browser must be willing to send the cookie cross-origin, which is a
 *   property of where the two hostnames sit and not of any code;
 * - better-auth must *trust* the dashboard's origin, or it refuses every one
 *   of these as `INVALID_ORIGIN` — the whole of what a signed-in cookie is
 *   checked against on a state-changing request;
 * - the CORS headers must let the answer be read, which cors.test.ts covers.
 *
 * The middle one is easy to lose: the dashboard's origin is derived, so it
 * moves when the platform's API URL does, and nothing else on this service
 * posts from another origin at all.
 */

const DASHBOARD = "https://kitchen.example.com";
const STRANGER = "https://not-the-dashboard.example.com";

const EMAIL = "anna@example.com";
const FIRST_PASSWORD = "correct-horse-battery";
const SECOND_PASSWORD = "staple-battery-horse";
const THIRD_PASSWORD = "horse-staple-correct";

/** The cookies a response set, as a `cookie` header for the next request. */
function cookiesOf(response: Response): string {
	return response.headers
		.getSetCookie()
		.map((cookie) => cookie.split(";", 1)[0])
		.join("; ");
}

describe("an account managing itself from the dashboard", () => {
	let kitchen: Harness;
	/** A signed-in browser: what `/login` would have left in one. */
	let browser: string;

	/** Signs in with a password the way the issuer's own login page does: from
	 * the issuer's own origin, which is what that page is served from. */
	async function signIn(password: string): Promise<Response> {
		return kitchen.fetch("/sign-in/email", {
			method: "POST",
			headers: { "content-type": "application/json", origin: kitchen.url },
			body: JSON.stringify({ email: EMAIL, password }),
		});
	}

	/** A call the dashboard makes: its origin, and the browser's cookie. */
	function fromDashboard(path: string, init: RequestInit = {}, origin = DASHBOARD): Promise<Response> {
		return kitchen.fetch(path, {
			...init,
			headers: { origin, cookie: browser, ...init.headers },
		});
	}

	before(async () => {
		kitchen = await startHarness({ apiURL: DASHBOARD });

		const created = await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				token: kitchen.bootstrapToken,
				email: EMAIL,
				name: "Anna",
				password: FIRST_PASSWORD,
			}),
		});
		assert.equal(created.status, 201, await created.clone().text());

		const session = await signIn(FIRST_PASSWORD);
		assert.equal(session.status, 200, await session.clone().text());
		browser = cookiesOf(session);
		assert.ok(browser, "signing in leaves a session cookie");
	});

	after(async () => {
		await kitchen.stop();
	});

	it("tells the dashboard who the browser is signed in as", async () => {
		const response = await fromDashboard("/get-session");
		assert.equal(response.status, 200, await response.clone().text());

		const answer = (await response.json()) as { user: { email: string }; session: { token: string } };
		assert.equal(answer.user.email, EMAIL);
		assert.ok(answer.session.token, "the current session names itself, so the table can mark it");
	});

	it("says how this account can sign in", async () => {
		const response = await fromDashboard("/list-accounts");
		assert.equal(response.status, 200);

		const accounts = (await response.json()) as Array<{ providerId: string }>;
		assert.deepEqual(
			accounts.map((account) => account.providerId),
			["credential"],
			"a bootstrap account has a password and nothing else",
		);
	});

	it("lists sessions however long ago this one began", async () => {
		// better-auth guards this endpoint on a session created within the last
		// day unless the freshness window is off, and nothing here can make a
		// session fresher without ending it — so a session in its second day
		// would otherwise leave its owner unable to see the others at all.
		await kitchen.pool.query(`UPDATE "session" SET "createdAt" = now() - interval '3 days'`);

		const response = await fromDashboard("/list-sessions");
		assert.equal(response.status, 200, await response.clone().text());

		const sessions = (await response.json()) as Array<{ token: string }>;
		assert.equal(sessions.length, 1);
	});

	it("refuses the same call from an origin the platform does not know", async () => {
		// Not a CORS failure — this one is refused on the wire, and it is what
		// a dashboard missing from the derived origin list would get.
		const response = await fromDashboard(
			"/change-password",
			{
				method: "POST",
				headers: { "content-type": "application/json" },
				body: JSON.stringify({ currentPassword: FIRST_PASSWORD, newPassword: SECOND_PASSWORD }),
			},
			STRANGER,
		);

		assert.equal(response.status, 403);
		const body = (await response.json()) as { code?: string };
		assert.equal(body.code, "INVALID_ORIGIN");
	});

	it("will not change a password without the current one", async () => {
		const response = await fromDashboard("/change-password", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ currentPassword: "not-it", newPassword: SECOND_PASSWORD }),
		});

		assert.equal(response.ok, false);
		const body = (await response.json()) as { code?: string };
		assert.equal(body.code, "INVALID_PASSWORD");
	});

	it("changes the password, and it is the new one that works", async () => {
		const changed = await fromDashboard("/change-password", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				currentPassword: FIRST_PASSWORD,
				newPassword: SECOND_PASSWORD,
				revokeOtherSessions: false,
			}),
		});
		assert.equal(changed.status, 200, await changed.clone().text());

		assert.equal((await signIn(FIRST_PASSWORD)).ok, false, "the old password stops working");

		const again = await signIn(SECOND_PASSWORD);
		assert.equal(again.status, 200);
		browser = cookiesOf(again);
	});

	it("signs one other browser out and leaves this one alone", async () => {
		const other = await signIn(SECOND_PASSWORD);
		assert.equal(other.status, 200);
		const otherToken = ((await other.json()) as { token: string }).token;

		const before = (await (await fromDashboard("/list-sessions")).json()) as unknown[];
		assert.ok(before.length >= 2, "two browsers are signed in");

		const revoked = await fromDashboard("/revoke-session", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ token: otherToken }),
		});
		assert.equal(revoked.status, 200, await revoked.clone().text());

		const left = (await (await fromDashboard("/list-sessions")).json()) as Array<{ token: string }>;
		assert.equal(
			left.some((session) => session.token === otherToken),
			false,
			"the browser that was signed out is gone",
		);
		assert.equal((await fromDashboard("/get-session")).status, 200, "and this one is still signed in");
	});
	it("signs every other browser out with the password, and keeps this one", async () => {
		// better-auth implements this by deleting every session and minting a
		// new one for the caller, so "keeps this one" is a claim worth checking
		// rather than an obvious consequence — the screen says it in as many
		// words before anyone ticks the box.
		const other = await signIn(SECOND_PASSWORD);
		assert.equal(other.status, 200);
		const otherCookie = cookiesOf(other);

		const changed = await fromDashboard("/change-password", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				currentPassword: SECOND_PASSWORD,
				newPassword: THIRD_PASSWORD,
				revokeOtherSessions: true,
			}),
		});
		assert.equal(changed.status, 200, await changed.clone().text());
		// The new session arrives as a cookie, which is what keeps the browser
		// that made the change signed in.
		browser = cookiesOf(changed);
		assert.ok(browser, "the answer carries the replacement session");

		const stranded = await kitchen.fetch("/get-session", {
			headers: { origin: DASHBOARD, cookie: otherCookie },
		});
		assert.equal(await stranded.json(), null, "the other browser has no session left");

		assert.equal((await fromDashboard("/get-session")).status, 200);
		const left = (await (await fromDashboard("/list-sessions")).json()) as unknown[];
		assert.equal(left.length, 1, "one browser signed in: this one");
	});
});
