import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { after, before, beforeEach, describe, it } from "node:test";

import { MACHINE_ACCOUNT_DOMAIN } from "../src/identity.js";
import type { IssuedProjectKey, ProjectKey } from "../src/keys.js";
import type { Account } from "../src/directory.js";
import { startHarness, type Harness } from "./support.js";

/**
 * CI keys, and the machine accounts that own them.
 *
 * The premise the whole design rests on is worth stating where it is tested:
 * the api-key plugin's `enableSessionForAPIKeys` mints a session for the
 * account a key's `referenceId` points at, so a key has no `sub` of its own.
 * Giving every key an owner of its own is what turns "grant the key's subject
 * a project role" from a grant to whoever created it into a grant to the key.
 *
 * The rest is what has to stay true around that: a machine account is not a
 * person, so it must not make an installation look bootstrapped and must not
 * appear in the people directory the operator seeds its operator list from.
 */
describe("CI keys", () => {
	let kitchen: Harness;

	const asOperator = (path: string, init: RequestInit = {}) =>
		kitchen.fetch(path, { ...init, headers: { ...init.headers, "x-api-key": kitchen.serviceKey } });

	const issue = async (project: string, name: string): Promise<IssuedProjectKey> => {
		const response = await asOperator("/kitchen/keys", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ project, name }),
		});
		assert.equal(response.status, 201, await response.clone().text());
		return (await response.json()) as IssuedProjectKey;
	};

	const keysOf = async (project: string): Promise<ProjectKey[]> => {
		const response = await asOperator(`/kitchen/keys?project=${project}`);
		assert.equal(response.status, 200, await response.clone().text());
		return ((await response.json()) as { keys: ProjectKey[] }).keys;
	};

	before(async () => {
		kitchen = await startHarness();
	});

	beforeEach(async () => {
		// Each test starts from a project with no keys, so that the listing
		// assertions are about what the test itself created.
		for (const key of await keysOf("shop")) {
			await asOperator(`/kitchen/keys?project=shop&name=${key.name}`, { method: "DELETE" });
		}
	});

	after(async () => {
		await kitchen.stop();
	});

	it("hands the key value back exactly once, with the subject to grant a role to", async () => {
		const issued = await issue("shop", "nightly");

		assert.equal(issued.project, "shop");
		assert.equal(issued.name, "nightly");
		assert.equal(issued.email, `shop.nightly@${MACHINE_ACCOUNT_DOMAIN}`);
		assert.ok(issued.subject, "the grant is written against the machine account's sub");
		assert.equal(issued.key.length, 64, "the api-key plugin refuses anything shorter");
		assert.ok(issued.key.startsWith(issued.prefix));

		const listed = await keysOf("shop");
		assert.deepEqual(
			listed.map((key) => key.name),
			["nightly"],
		);
		assert.equal(listed[0]?.subject, issued.subject);
		assert.equal(listed[0]?.prefix, issued.prefix);
		assert.equal(
			JSON.stringify(listed).includes(issued.key),
			false,
			"the value exists in the creation response and nowhere else",
		);
	});

	it("mints a session for the machine account, not for whoever created the key", async () => {
		const issued = await issue("shop", "deploy");

		// The whole premise: what the key stands in for. `/token` is what CI
		// exchanges a key for, and the token it gets is the platform token for
		// the account the key belongs to.
		const response = await kitchen.fetch("/token", { headers: { "x-api-key": issued.key } });
		assert.equal(response.status, 200, await response.clone().text());

		const { token } = (await response.json()) as { token: string };
		const claims = JSON.parse(
			Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8"),
		) as { sub: string; email?: string };
		assert.equal(
			claims.sub,
			issued.subject,
			"the sub in a key's token is its owner's — which is why every key gets an owner of its own",
		);
		assert.equal(claims.email, issued.email);
	});

	it("keeps one key per name per project, so revoking one is unambiguous", async () => {
		await issue("shop", "nightly");

		const again = await asOperator("/kitchen/keys", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ project: "shop", name: "nightly" }),
		});
		assert.equal(again.status, 409);
	});

	it("keeps a project's keys to that project", async () => {
		await issue("shop", "nightly");
		await issue("blog", "nightly");

		assert.deepEqual((await keysOf("shop")).map((key) => key.name), ["nightly"]);
		assert.deepEqual((await keysOf("blog")).map((key) => key.subject).length, 1);
		assert.notEqual(
			(await keysOf("shop"))[0]?.subject,
			(await keysOf("blog"))[0]?.subject,
			"two projects' keys are two accounts, so one grant cannot cover both",
		);

		for (const key of await keysOf("blog")) {
			await asOperator(`/kitchen/keys?project=blog&name=${key.name}`, { method: "DELETE" });
		}
	});

	it("revokes a key and removes the account that owned it", async () => {
		const issued = await issue("shop", "nightly");

		const removed = await asOperator("/kitchen/keys?project=shop&name=nightly", { method: "DELETE" });
		assert.equal(removed.status, 200, await removed.clone().text());
		assert.equal(((await removed.json()) as ProjectKey).subject, issued.subject);

		assert.deepEqual(await keysOf("shop"), []);

		// The credential itself stops working, which is the half that matters:
		// revocation lives here and only here.
		const exchanged = await kitchen.fetch("/token", { headers: { "x-api-key": issued.key } });
		assert.notEqual(exchanged.status, 200, "a revoked key must not be exchangeable for a token");

		// And a second delete is a 404 rather than a silent success.
		const again = await asOperator("/kitchen/keys?project=shop&name=nightly", { method: "DELETE" });
		assert.equal(again.status, 404);
	});

	it("refuses a project or key name that is not a label", async () => {
		for (const body of [
			{ project: "shop", name: "Nightly Build" },
			{ project: "Shop", name: "nightly" },
			{ project: "shop", name: "" },
			{ project: "shop", name: "a.b" },
		]) {
			const response = await asOperator("/kitchen/keys", {
				method: "POST",
				headers: { "content-type": "application/json" },
				body: JSON.stringify(body),
			});
			assert.equal(response.status, 400, `${JSON.stringify(body)} should be refused`);
		}
	});

	it("answers only the operator's own credential", async () => {
		assert.equal((await kitchen.fetch("/kitchen/keys?project=shop")).status, 401);

		const issued = await issue("shop", "nightly");
		const asKey = await kitchen.fetch("/kitchen/keys?project=shop", {
			headers: { "x-api-key": issued.key },
		});
		assert.equal(asKey.status, 403, "a CI key is a valid credential belonging to somebody else");

		const stranger = await kitchen.fetch("/kitchen/keys?project=shop", {
			headers: { "x-api-key": randomBytes(32).toString("hex") },
		});
		assert.equal(stranger.status, 401);
	});

	it("is not a person: it does not bootstrap the platform and is not in the directory", async () => {
		// An installation whose only accounts are credentials still has nobody
		// who can sign in, so the bootstrap link is still live.
		const issued = await issue("shop", "nightly");

		const bootstrap = await kitchen.fetch(`/bootstrap?token=${kitchen.bootstrapToken}`);
		assert.equal(bootstrap.status, 200, "a machine account must not count as the first administrator");

		const directory = await asOperator("/kitchen/accounts");
		const { accounts } = (await directory.json()) as { accounts: Account[] };
		assert.equal(
			accounts.some((account) => account.subject === issued.subject),
			false,
			"a machine account is a credential, not somebody the people picker should offer",
		);

		// Nor can it be resolved by address, which is what stops a grant
		// naming the address from ever being written for one.
		const resolved = await asOperator(`/kitchen/accounts?email=${encodeURIComponent(issued.email)}`);
		assert.equal(resolved.status, 404);
	});
});
