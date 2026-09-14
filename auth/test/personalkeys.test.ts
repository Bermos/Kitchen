import assert from "node:assert/strict";
import { after, before, beforeEach, describe, it } from "node:test";

import type { IssuedPersonalKey, PersonalKey } from "../src/personalkeys.js";
import type { IssuedPlatformKey } from "../src/platformkeys.js";
import { startHarness, type Harness } from "./support.js";

/**
 * Personal keys: a credential that carries somebody's own identity (#593).
 *
 * This is the one credential in the service that does *not* get an account of
 * its own, and these tests are mostly about what follows from that. It belongs
 * to the account somebody signs in to, so a token minted from it carries their
 * `sub` and every role Kitchen resolves from it. What keeps that defensible is
 * here rather than assumed: it expires, a credential cannot make one, and the
 * account it belongs to is a person's.
 */
describe("personal keys", () => {
	let kitchen: Harness;
	let anna: string;

	const EMAIL = "anna@example.com";
	const PASSWORD = "correct-horse-battery";

	/** A month out, which is what Kitchen asks for when nobody says. */
	const soon = () => new Date(Date.now() + 30 * 24 * 60 * 60 * 1000).toISOString();

	const asOperator = (path: string, init: RequestInit = {}) =>
		kitchen.internal(path, { ...init, headers: { ...init.headers, "x-api-key": kitchen.serviceKey } });

	const post = (body: unknown) =>
		asOperator("/kitchen/personal-keys", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify(body),
		});

	const issue = async (name: string, expires = soon()): Promise<IssuedPersonalKey> => {
		const response = await post({ subject: anna, name, expires });
		assert.equal(response.status, 201, await response.clone().text());
		return (await response.json()) as IssuedPersonalKey;
	};

	const keys = async (subject = anna): Promise<PersonalKey[]> => {
		const response = await asOperator(`/kitchen/personal-keys?subject=${encodeURIComponent(subject)}`);
		assert.equal(response.status, 200, await response.clone().text());
		return ((await response.json()) as { keys: PersonalKey[] }).keys;
	};

	before(async () => {
		kitchen = await startHarness();

		const created = await kitchen.fetch("/bootstrap", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({
				token: kitchen.bootstrapToken,
				email: EMAIL,
				name: "Anna",
				password: PASSWORD,
			}),
		});
		assert.equal(created.status, 201, await created.clone().text());

		const directory = await asOperator(`/kitchen/accounts?email=${encodeURIComponent(EMAIL)}`);
		assert.equal(directory.status, 200, await directory.clone().text());
		anna = ((await directory.json()) as { subject: string }).subject;
	});

	beforeEach(async () => {
		for (const key of await keys()) {
			await asOperator(
				`/kitchen/personal-keys?subject=${encodeURIComponent(anna)}&name=${key.name}`,
				{ method: "DELETE" },
			);
		}
	});

	after(async () => {
		await kitchen.stop();
	});

	it("hands the value back exactly once, and carries the person's own subject", async () => {
		const issued = await issue("laptop");

		assert.equal(issued.name, "laptop");
		assert.equal(issued.subject, anna, "this is the whole point: it is *their* identity");
		assert.equal(issued.email, EMAIL);
		assert.equal(issued.key.length, 64, "the api-key plugin refuses anything shorter");
		assert.ok(issued.key.startsWith(issued.prefix));
		assert.ok(Date.parse(issued.expires) > Date.now(), "every personal key expires");

		const listed = await keys();
		assert.deepEqual(
			listed.map((key) => key.name),
			["laptop"],
		);
		assert.equal(
			JSON.stringify(listed).includes(issued.key),
			false,
			"the value exists in the creation response and nowhere else",
		);
	});

	it("is exchanged for a token that is the person's, not a machine's", async () => {
		const issued = await issue("script");

		const response = await kitchen.fetch("/token", { headers: { "x-api-key": issued.key } });
		assert.equal(response.status, 200, await response.clone().text());

		const { token } = (await response.json()) as { token: string };
		const claims = JSON.parse(
			Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8"),
		) as { sub: string; email?: string };
		assert.equal(claims.sub, anna, "Kitchen resolves every role this person holds from this sub");
		assert.equal(claims.email, EMAIL);
	});

	it("holds several at once, because a laptop and a scheduled job are two credentials", async () => {
		await issue("laptop");
		await issue("nightly");

		assert.deepEqual(
			(await keys()).map((key) => key.name),
			["laptop", "nightly"],
		);

		const again = await post({ subject: anna, name: "laptop", expires: soon() });
		assert.equal(again.status, 409, "one name is one key, so revoking one is unambiguous");
	});

	it("revokes one key and leaves the account and its siblings alone", async () => {
		const laptop = await issue("laptop");
		const nightly = await issue("nightly");

		const removed = await asOperator(
			`/kitchen/personal-keys?subject=${encodeURIComponent(anna)}&name=laptop`,
			{ method: "DELETE" },
		);
		assert.equal(removed.status, 200, await removed.clone().text());
		assert.deepEqual(
			(await keys()).map((key) => key.name),
			["nightly"],
		);

		const revoked = await kitchen.fetch("/token", { headers: { "x-api-key": laptop.key } });
		assert.notEqual(revoked.status, 200, "a revoked key must not be exchangeable for a token");

		const survivor = await kitchen.fetch("/token", { headers: { "x-api-key": nightly.key } });
		assert.equal(survivor.status, 200, "and revoking one must not touch the account or the others");

		const again = await asOperator(
			`/kitchen/personal-keys?subject=${encodeURIComponent(anna)}&name=laptop`,
			{ method: "DELETE" },
		);
		assert.equal(again.status, 404);
	});

	it("stops working when it lapses, without anything having had to run", async () => {
		// The plugin enforces `expiresAt` on presentation, which is what makes
		// the expiry a property of the credential rather than of a sweep
		// somebody has to keep running. Issued a second out to prove it.
		const issued = await issue("brief", new Date(Date.now() + 1000).toISOString());
		assert.equal(
			(await kitchen.fetch("/token", { headers: { "x-api-key": issued.key } })).status,
			200,
			"and it works until it lapses",
		);

		await new Promise((resolve) => setTimeout(resolve, 1200));
		const lapsed = await kitchen.fetch("/token", { headers: { "x-api-key": issued.key } });
		assert.notEqual(lapsed.status, 200, "an expired personal key is refused at the issuer");
	});

	it("is not issued without an expiry, or with one in the past", async () => {
		for (const expires of ["", "not a date", new Date(Date.now() - 1000).toISOString()]) {
			const response = await post({ subject: anna, name: "forever", expires });
			assert.equal(response.status, 400, `${JSON.stringify(expires)} should be refused`);
		}
	});

	it("reaches no more of the issuer than any other key, so it cannot mint a successor", async () => {
		const issued = await issue("laptop");

		// The one exchange it is for. Everything else a key could do at the
		// issuer stays refused: this is a copy of a person's access, and a copy
		// that could make further copies is one nobody could account for.
		assert.equal((await kitchen.fetch("/token", { headers: { "x-api-key": issued.key } })).status, 200);

		const minted = await kitchen.fetch("/api-key/create", {
			method: "POST",
			headers: { "x-api-key": issued.key, "content-type": "application/json" },
			body: JSON.stringify({ name: "successor" }),
		});
		assert.equal(minted.status, 403);

		const listed = await kitchen.internal(
			`/kitchen/personal-keys?subject=${encodeURIComponent(anna)}`,
			{ headers: { "x-api-key": issued.key } },
		);
		assert.equal(listed.status, 403, "nor reach this prefix, which is the operator's");
	});

	it("belongs to a person, so no credential's account can be given one", async () => {
		const credential = await asOperator("/kitchen/platform-keys", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ name: "agent" }),
		});
		assert.equal(credential.status, 201, await credential.clone().text());
		const agent = (await credential.json()) as IssuedPlatformKey;

		const refused = await post({ subject: agent.subject, name: "successor", expires: soon() });
		assert.equal(refused.status, 400, "a credential must not be handed a copy of a person");

		const missing = await post({ subject: "nobody", name: "laptop", expires: soon() });
		assert.equal(missing.status, 404);

		await asOperator("/kitchen/platform-keys?name=agent", { method: "DELETE" });
	});

	it("answers only the operator's own credential", async () => {
		assert.equal(
			(await kitchen.internal(`/kitchen/personal-keys?subject=${encodeURIComponent(anna)}`)).status,
			401,
		);
	});
});
