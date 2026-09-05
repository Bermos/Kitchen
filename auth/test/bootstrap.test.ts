import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

import { listPeople } from "../src/identity.js";
import { startHarness, type Harness } from "./support.js";

/**
 * The bootstrap link: the one account that may exist without an invitation.
 *
 * Two properties are worth a test of their own, and both were findings of the
 * 2026-09-03 security review:
 *
 * - **An unauthenticated probe learns nothing.** GET /bootstrap answers 410
 *   once the platform has an account and 401 without the token. Checking the
 *   installation first made the pair of answers a public readout of whether
 *   this installation had been set up yet, which is exactly the fact the link
 *   protects. The token is checked first, so a caller with the wrong one gets
 *   401 either way.
 * - **Two concurrent valid POSTs create one administrator.** The check and
 *   the write are two round trips against a table with no constraint that
 *   would refuse the second account; the advisory lock is what makes them one
 *   step.
 */

const NAME = "Anna Ops";
const PASSWORD = "correct-horse-battery";

describe("the bootstrap link", () => {
	let harness: Harness;

	before(async () => {
		harness = await startHarness();
	});

	after(async () => {
		await harness.stop();
	});

	it("refuses a GET without the token before it says whether it is set up", async () => {
		const missing = await harness.fetch("/bootstrap");
		assert.equal(missing.status, 401);
		const wrong = await harness.fetch("/bootstrap?token=not-the-token");
		assert.equal(wrong.status, 401);
	});

	it("serves the page to the token while nobody has an account", async () => {
		const response = await harness.fetch(`/bootstrap?token=${harness.bootstrapToken}`);
		assert.equal(response.status, 200);
	});

	it("creates exactly one administrator when two valid POSTs race", async () => {
		const post = (email: string) =>
			harness.fetch("/bootstrap", {
				method: "POST",
				headers: { "content-type": "application/json" },
				body: JSON.stringify({
					token: harness.bootstrapToken,
					email,
					name: NAME,
					password: PASSWORD,
				}),
			});

		const [first, second] = await Promise.all([post("anna@example.com"), post("bea@example.com")]);
		const statuses = [first.status, second.status].sort();
		assert.deepEqual(statuses, [201, 410], "one POST creates the account and the other is told it is too late");

		const people = await listPeople(harness.auth, harness.config);
		assert.equal(people.length, 1, "exactly one person exists");
	});

	it("still answers 401 rather than 410 to a wrong token once it is set up", async () => {
		const wrong = await harness.fetch("/bootstrap?token=not-the-token");
		assert.equal(wrong.status, 401, "a caller without the token is told nothing about the installation");

		const right = await harness.fetch(`/bootstrap?token=${harness.bootstrapToken}`);
		assert.equal(right.status, 410, "the token is what buys the answer");
	});
});
