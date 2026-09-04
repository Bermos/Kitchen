import assert from "node:assert/strict";
import { after, before, describe, it } from "node:test";

import { startHarness, type Harness } from "./support.js";

/**
 * The `/kitchen` prefix is the operator's, and it is not on the internet.
 *
 * The chart routes `PathPrefix /` of the issuer's hostname at the public
 * Service, which is what put the prefix on the public hostname in the first
 * place: whoever held the service key could, from anywhere, enumerate every
 * account, mint a CI key for any project and rewrite an OAuth client's
 * redirect list. The fix is two listeners on two ports, only one of which any
 * HTTPRoute names — so these tests are about which port answers what, not
 * about who holds which credential (that is directory.test.ts's).
 */
describe("the two listeners", () => {
	let kitchen: Harness;

	before(async () => {
		kitchen = await startHarness();
	});

	after(async () => {
		await kitchen.stop();
	});

	it("does not serve the Kitchen prefix on the published listener, even to the operator", async () => {
		for (const path of [
			"/kitchen",
			"/kitchen/accounts",
			"/kitchen/keys?project=shop",
			"/kitchen/clients",
		]) {
			const response = await kitchen.fetch(path, {
				headers: { "x-api-key": kitchen.serviceKey },
			});
			assert.equal(
				response.status,
				404,
				`${path} is served on the hostname the Gateway publishes: ${await response.clone().text()}`,
			);
		}
	});

	it("says what an issuer without the prefix would say, rather than that a key is wrong", async () => {
		const response = await kitchen.fetch("/kitchen/accounts");
		assert.equal(response.status, 404);

		const { error } = (await response.json()) as { error: string };
		assert.match(error, /no such endpoint/);
		assert.doesNotMatch(error, /api key|x-api-key/i, "a 404 that asks for a header is an invitation");
	});

	it("serves the prefix on the private listener", async () => {
		const response = await kitchen.internal("/kitchen/accounts", {
			headers: { "x-api-key": kitchen.serviceKey },
		});
		assert.equal(response.status, 200, await response.clone().text());

		const anonymous = await kitchen.internal("/kitchen/accounts");
		assert.equal(anonymous.status, 401, "reaching the port is not the same as holding the credential");
	});

	it("keeps sign-in, discovery and the hosted pages off the private listener", async () => {
		for (const path of ["/.well-known/openid-configuration", "/login", "/jwks", "/"]) {
			const response = await kitchen.internal(path);
			assert.equal(response.status, 404, `the private listener serves ${path}`);
		}
	});

	it("answers both probes on both listeners, so each port can be probed on its own", async () => {
		assert.equal((await kitchen.fetch("/healthz")).status, 200);
		assert.equal((await kitchen.fetch("/readyz")).status, 200);
		assert.equal((await kitchen.internal("/healthz")).status, 200);
		assert.equal((await kitchen.internal("/readyz")).status, 200);
	});
});

/**
 * The prefix is Kitchen's rather than better-auth's, so better-auth's rate
 * limiter has never seen a request to it. This is the platform's own.
 */
describe("the Kitchen prefix's rate limit", () => {
	let kitchen: Harness;

	before(async () => {
		kitchen = await startHarness({ kitchenRatePerMinute: 3 });
	});

	after(async () => {
		await kitchen.stop();
	});

	it("refuses a caller that asks faster than the limit, credential or not", async () => {
		const statuses: number[] = [];
		let refusal: Response | undefined;
		for (let attempt = 0; attempt < 5; attempt += 1) {
			// The operator's own credential on the last two: the limit is per
			// address, so holding the key is not a way around it.
			const headers = attempt >= 3 ? { "x-api-key": kitchen.serviceKey } : undefined;
			const response = await kitchen.internal("/kitchen/accounts", { headers });
			statuses.push(response.status);
			refusal ??= response.status === 429 ? response : undefined;
			await response.text();
		}

		assert.deepEqual(
			statuses,
			[401, 401, 401, 429, 429],
			"three requests a minute, and the fourth is refused before the key is even looked at",
		);
		assert.equal(refusal?.headers.get("retry-after"), "60", "a caller that is not attacking can back off");
	});
});
