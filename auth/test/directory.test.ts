import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { after, before, describe, it } from "node:test";

import type { Account } from "../src/directory.js";
import { startHarness, type Harness } from "./support.js";

/**
 * The account directory is what makes an operator list something the platform
 * can seed and the dashboard can offer a picker over: nothing else can
 * enumerate accounts or turn an address into the `sub` an access entry is
 * written with.
 *
 * The interesting half of it is who may ask. It is the operator's service
 * credential and nothing else — not a signed-in administrator, and not a CI
 * key, which is an ordinary account's credential and would otherwise be able
 * to read the whole team's addresses out of the identity provider.
 */
describe("the account directory", () => {
	let kitchen: Harness;
	let anna: { id: string; email: string };
	let bo: { id: string; email: string };

	const asOperator = (path: string) =>
		kitchen.internal(path, { headers: { "x-api-key": kitchen.serviceKey } });

	before(async () => {
		kitchen = await startHarness();

		// Two people, one of them with the address written in mixed case: an
		// account created as Bo@Example.com is the account a lookup for
		// bo@example.com means.
		anna = await createAccount(kitchen, "anna@example.com", "Anna");
		bo = await createAccount(kitchen, "Bo@Example.com", "Bo");
	});

	after(async () => {
		await kitchen.stop();
	});

	it("lists every account, and no service account", async () => {
		const response = await asOperator("/kitchen/accounts");
		assert.equal(response.status, 200, await response.clone().text());

		const { accounts } = (await response.json()) as { accounts: Account[] };
		assert.deepEqual(
			accounts.map((account) => account.email),
			[anna.email, bo.email],
			"oldest account first, so the bootstrap account leads the operator list it seeds",
		);
		assert.equal(
			accounts.some((account) => account.email === kitchen.config.serviceAccountEmail),
			false,
			"the operator's own service account is a credential, not a person",
		);

		const found = accounts.find((account) => account.email === anna.email);
		assert.equal(found?.subject, anna.id, "the subject is the issuer's sub");
		assert.equal(found?.name, "Anna");
		assert.equal(found?.emailVerified, true);
	});

	it("resolves an address to the account that holds it, whatever its case", async () => {
		const response = await asOperator("/kitchen/accounts?email=BO%40EXAMPLE.COM");
		assert.equal(response.status, 200, await response.clone().text());

		const account = (await response.json()) as Account;
		assert.equal(account.subject, bo.id);
		assert.equal(account.email, bo.email);
	});

	it("answers 404 for an address nobody has", async () => {
		const response = await asOperator("/kitchen/accounts?email=stranger@example.com");
		assert.equal(response.status, 404);
	});

	it("will not resolve the service account, which is not a person", async () => {
		const response = await asOperator(
			`/kitchen/accounts?email=${encodeURIComponent(kitchen.config.serviceAccountEmail)}`,
		);
		assert.equal(response.status, 404);
	});

	it("refuses a caller with no credential at all", async () => {
		const response = await kitchen.internal("/kitchen/accounts");
		assert.equal(response.status, 401);
	});

	it("refuses a credential the issuer never handed out", async () => {
		const response = await kitchen.internal("/kitchen/accounts", {
			headers: { "x-api-key": randomBytes(32).toString("hex") },
		});
		assert.equal(response.status, 401);
	});

	it("refuses a valid key that is not the operator's, and says which", async () => {
		const key = await issueKeyFor(kitchen, anna.id);

		const response = await kitchen.internal("/kitchen/accounts", { headers: { "x-api-key": key } });
		assert.equal(response.status, 403, "a CI key is a valid credential belonging to somebody else");
		const { error } = (await response.json()) as { error: string };
		assert.match(error, /operator/);
	});

	it("does not answer a browser session", async () => {
		// No api key, cookie or not: the directory reads no session at all, so
		// a signed-in administrator's browser gets the same refusal a stranger
		// does.
		const response = await kitchen.internal("/kitchen/accounts", {
			headers: { cookie: "better-auth.session_token=whatever" },
		});
		assert.equal(response.status, 401);
	});

	it("keeps the prefix to itself: an unknown endpoint under it is a 404, not better-auth's", async () => {
		const response = await asOperator("/kitchen/nothing-here");
		assert.equal(response.status, 404);
		const { error } = (await response.json()) as { error: string };
		assert.match(error, /no such endpoint/);
	});

	it("names the methods an endpoint answers", async () => {
		const response = await kitchen.internal("/kitchen/accounts", {
			method: "POST",
			headers: { "x-api-key": kitchen.serviceKey, "content-type": "application/json" },
			body: "{}",
		});
		assert.equal(response.status, 405);
	});
});

/** Creates an account the way the bootstrap flow does, minus the token. */
async function createAccount(
	kitchen: Harness,
	email: string,
	name: string,
): Promise<{ id: string; email: string }> {
	const ctx = await kitchen.auth.$context;
	const user = await ctx.internalAdapter.createUser({ email, name, emailVerified: true });
	return { id: user.id, email: user.email };
}

/** A CI key: an ordinary account's credential, not the operator's. */
async function issueKeyFor(kitchen: Harness, userId: string): Promise<string> {
	const { defaultKeyHasher } = await import("@better-auth/api-key");
	const value = randomBytes(32).toString("hex");
	const ctx = await kitchen.auth.$context;
	const now = new Date();
	await ctx.adapter.create({
		model: "apikey",
		data: {
			name: "ci",
			start: value.slice(0, 6),
			referenceId: userId,
			key: await defaultKeyHasher(value),
			enabled: true,
			rateLimitEnabled: false,
			createdAt: now,
			updatedAt: now,
		},
	});
	return value;
}
