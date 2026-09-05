import assert from "node:assert/strict";
import { test } from "node:test";

import { connectionSecurity, createPool } from "../src/db.js";

/**
 * #382: the accounts database used to answer in the clear inside the platform
 * namespace, and the chart now hands this service a DSN that verifies it
 * against the platform's own CA. What is checked here is the half that is this
 * service's: that it reads what the DSN asks for, and that a CA it cannot read
 * is a sentence rather than an ENOENT from inside the first query.
 */
test("reads what the connection string asks of the server", () => {
	const security = connectionSecurity(
		"postgres://kitchen:pw@kitchen-postgres.kitchen-system.svc:5432/kitchen_auth" +
			"?sslmode=verify-full&sslrootcert=/etc/kitchen/internal-ca/ca.crt",
	);
	assert.equal(security.sslmode, "verify-full");
	assert.equal(security.caFile, "/etc/kitchen/internal-ca/ca.crt");
});

test("asks for nothing where the connection string says nothing", () => {
	assert.deepEqual(connectionSecurity("postgres://kitchen:pw@localhost:5432/kitchen_auth"), {
		sslmode: "",
		caFile: "",
	});
	// libpq's other form is not a URL. It is left to the driver rather than
	// guessed at, which is what this service did before any of this existed.
	assert.deepEqual(connectionSecurity("host=localhost dbname=kitchen_auth sslmode=require"), {
		sslmode: "",
		caFile: "",
	});
});

test("refuses to build a pool whose CA is not there, naming the file", () => {
	assert.throws(
		() =>
			createPool(
				"postgres://kitchen:pw@kitchen-postgres.kitchen-system.svc:5432/kitchen_auth" +
					"?sslmode=verify-full&sslrootcert=/etc/kitchen/internal-ca/absent.crt",
			),
		/absent\.crt/,
		"a missing CA has to name itself: the driver reads it at the first connection, where " +
			"the failure is an ENOENT with no file in it",
	);
});
