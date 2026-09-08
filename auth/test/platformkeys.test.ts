import assert from "node:assert/strict";
import { randomBytes } from "node:crypto";
import { after, before, beforeEach, describe, it } from "node:test";

import type { Account } from "../src/directory.js";
import { MACHINE_ACCOUNT_DOMAIN, PLATFORM_ACCOUNT_DOMAIN } from "../src/identity.js";
import type { IssuedPlatformKey, PlatformKey } from "../src/platformkeys.js";
import { startHarness, type Harness } from "./support.js";

/**
 * Platform credentials, and the accounts that own them (issue #349).
 *
 * Everything the CI-key tests establish holds here — a credential has no
 * subject of its own, so it gets an account of its own — and what these add is
 * the part that is specific to a credential that reaches the *platform*:
 *
 * - It lives under its own reserved domain, so no CI key's address can ever be
 *   shaped like one and no platform credential's can be shaped like a CI key's.
 *   That matters because the domain is what the operator tells them apart by.
 * - It is not a person, with more at stake than for a CI key: an installation
 *   seeds its operator list from the accounts that exist when nobody has named
 *   any, so an account counted as a person here would be a credential that
 *   becomes an operator by upgrade.
 * - It is not widened at the issuer. What it may do is Kitchen's state, so a
 *   credential that reaches more of the platform reaches no more of the issuer
 *   than a CI key does — which is the shape #349 was written to get.
 */
describe("platform credentials", () => {
	let kitchen: Harness;

	const asOperator = (path: string, init: RequestInit = {}) =>
		kitchen.internal(path, { ...init, headers: { ...init.headers, "x-api-key": kitchen.serviceKey } });

	const issue = async (name: string): Promise<IssuedPlatformKey> => {
		const response = await asOperator("/kitchen/platform-keys", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ name }),
		});
		assert.equal(response.status, 201, await response.clone().text());
		return (await response.json()) as IssuedPlatformKey;
	};

	const credentials = async (): Promise<PlatformKey[]> => {
		const response = await asOperator("/kitchen/platform-keys");
		assert.equal(response.status, 200, await response.clone().text());
		return ((await response.json()) as { keys: PlatformKey[] }).keys;
	};

	before(async () => {
		kitchen = await startHarness();
	});

	beforeEach(async () => {
		for (const credential of await credentials()) {
			await asOperator(`/kitchen/platform-keys?name=${credential.name}`, { method: "DELETE" });
		}
	});

	after(async () => {
		await kitchen.stop();
	});

	it("hands the value back exactly once, with the subject to grant scopes to", async () => {
		const issued = await issue("nightly");

		assert.equal(issued.name, "nightly");
		assert.equal(issued.email, `nightly@${PLATFORM_ACCOUNT_DOMAIN}`);
		assert.ok(issued.subject, "the grant on the singleton is written against this sub");
		assert.equal(issued.key.length, 64, "the api-key plugin refuses anything shorter");
		assert.ok(issued.key.startsWith(issued.prefix));

		const listed = await credentials();
		assert.deepEqual(
			listed.map((credential) => credential.name),
			["nightly"],
		);
		assert.equal(
			JSON.stringify(listed).includes(issued.key),
			false,
			"the value exists in the creation response and nowhere else",
		);
	});

	it("mints a session for its own account, so the platform can grant scopes to it", async () => {
		const issued = await issue("agent");

		const response = await kitchen.fetch("/token", { headers: { "x-api-key": issued.key } });
		assert.equal(response.status, 200, await response.clone().text());

		const { token } = (await response.json()) as { token: string };
		const claims = JSON.parse(
			Buffer.from(token.split(".")[1] ?? "", "base64url").toString("utf8"),
		) as { sub: string; email?: string };
		assert.equal(claims.sub, issued.subject);
		assert.equal(
			claims.email,
			issued.email,
			"the address is how the operator tells a platform credential from a CI key",
		);
	});

	it("cannot be shaped like a CI key's address, or the reverse", async () => {
		const issued = await issue("nightly");

		assert.ok(issued.email.endsWith(`@${PLATFORM_ACCOUNT_DOMAIN}`));
		assert.equal(
			issued.email.endsWith(`@${MACHINE_ACCOUNT_DOMAIN}`),
			false,
			"a platform credential must never read as a project's key",
		);

		// And the local part is one label, where a CI key's is two — which is
		// what makes the two unambiguous without a list of reserved names.
		const local = issued.email.slice(0, issued.email.indexOf("@"));
		assert.equal(local.includes("."), false);
	});

	it("reaches no more of the issuer than a CI key does", async () => {
		const issued = await issue("agent");

		// The one exchange it is for, and the one read of itself.
		assert.equal((await kitchen.fetch("/token", { headers: { "x-api-key": issued.key } })).status, 200);

		// And nothing else. A credential that reaches more of the *platform*
		// must not thereby reach more of the identity provider: minting its own
		// successors here is exactly what the platform's own issuance path
		// exists to prevent.
		const minted = await kitchen.fetch("/api-key/create", {
			method: "POST",
			headers: { "x-api-key": issued.key, "content-type": "application/json" },
			body: JSON.stringify({ name: "successor" }),
		});
		assert.equal(minted.status, 403, "a platform credential must not be able to mint another key");

		const listed = await kitchen.internal("/kitchen/platform-keys", {
			headers: { "x-api-key": issued.key },
		});
		assert.equal(listed.status, 403, "nor list or revoke the platform's credentials");
	});

	it("keeps one credential per name, so revoking one is unambiguous", async () => {
		await issue("nightly");

		const again = await asOperator("/kitchen/platform-keys", {
			method: "POST",
			headers: { "content-type": "application/json" },
			body: JSON.stringify({ name: "nightly" }),
		});
		assert.equal(again.status, 409);
	});

	it("revokes a credential and removes the account that owned it", async () => {
		const issued = await issue("nightly");

		const removed = await asOperator("/kitchen/platform-keys?name=nightly", { method: "DELETE" });
		assert.equal(removed.status, 200, await removed.clone().text());
		assert.equal(((await removed.json()) as PlatformKey).subject, issued.subject);
		assert.deepEqual(await credentials(), []);

		const exchanged = await kitchen.fetch("/token", { headers: { "x-api-key": issued.key } });
		assert.notEqual(exchanged.status, 200, "a revoked credential must not be exchangeable for a token");

		const again = await asOperator("/kitchen/platform-keys?name=nightly", { method: "DELETE" });
		assert.equal(again.status, 404);
	});

	it("refuses a name that is not a label", async () => {
		for (const name of ["Nightly Build", "", "a.b", "-leading"]) {
			const response = await asOperator("/kitchen/platform-keys", {
				method: "POST",
				headers: { "content-type": "application/json" },
				body: JSON.stringify({ name }),
			});
			assert.equal(response.status, 400, `${JSON.stringify(name)} should be refused`);
		}
	});

	it("answers only the operator's own credential", async () => {
		assert.equal((await kitchen.internal("/kitchen/platform-keys")).status, 401);

		const stranger = await kitchen.internal("/kitchen/platform-keys", {
			headers: { "x-api-key": randomBytes(32).toString("hex") },
		});
		assert.equal(stranger.status, 401);
	});

	it("is not a person: it does not bootstrap the platform and is not in the directory", async () => {
		const issued = await issue("agent");

		// This is the sharper version of the CI key's own rule. An installation
		// that has never named its operators seeds the list from the accounts
		// that exist, so a credential counted as a person here would become an
		// operator by upgrade — the credential granting itself the hat the
		// scopes exist to avoid handing out.
		const bootstrap = await kitchen.fetch(`/bootstrap?token=${kitchen.bootstrapToken}`);
		assert.equal(bootstrap.status, 200, "a platform credential must not count as the first administrator");

		const directory = await asOperator("/kitchen/accounts");
		const { accounts } = (await directory.json()) as { accounts: Account[] };
		assert.equal(
			accounts.some((account) => account.subject === issued.subject),
			false,
			"a platform credential is a credential, not somebody the people picker should offer",
		);

		const byAddress = await asOperator(`/kitchen/accounts?email=${encodeURIComponent(issued.email)}`);
		assert.equal(
			byAddress.status,
			404,
			"and it cannot be resolved by address, which is what stops a grant naming one",
		);
	});
});
