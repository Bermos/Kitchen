import { existsSync } from "node:fs";

import { getMigrations } from "better-auth/db/migration";
import type { BetterAuthOptions } from "better-auth";
import pg from "pg";

import { log } from "./log.js";

/**
 * Arbitrary but stable key for the Postgres advisory lock guarding schema
 * migrations. Replicas start at the same time; only one may migrate.
 */
const MIGRATION_LOCK_KEY = 4_015_071_986;

/**
 * What the connection string asks of the server.
 *
 * `sslmode` and `sslrootcert` are libpq's spelling, and node-postgres reads
 * both out of a connection string — but it does not mean the same things by
 * them. Without `uselibpqcompat`, which the driver does not pass, `require`
 * and `verify-ca` verify the certificate exactly as `verify-full` does; the
 * only mode that does not verify is `no-verify`, which libpq has never heard
 * of. `verify-full` is therefore the one mode that means the same thing to
 * this service and to the operator's libpq-compatible driver, and it is what
 * the chart writes for the database it deploys.
 *
 * A DSN in libpq's other form (`host=... sslmode=...`) is not a URL and is
 * left alone: an installation that hands one over gets whatever the driver
 * makes of it, which is the same as before this existed.
 */
export function connectionSecurity(databaseURL: string): { sslmode: string; caFile: string } {
	try {
		const parameters = new URL(databaseURL).searchParams;
		return {
			sslmode: parameters.get("sslmode") ?? "",
			caFile: parameters.get("sslrootcert") ?? "",
		};
	} catch {
		return { sslmode: "", caFile: "" };
	}
}

export function createPool(databaseURL: string): pg.Pool {
	const { sslmode, caFile } = connectionSecurity(databaseURL);
	// The driver reads this file when it parses the connection string, which
	// happens at the first connection rather than here — so a CA that is not
	// there yet surfaces as an ENOENT from inside a query, with nothing to
	// say which file or why. The platform mounts the CA into this pod from a
	// ConfigMap the operator publishes, and the mount is deliberately not
	// optional so that the pod waits for it; if it is somehow missing anyway,
	// this is the sentence that says so.
	if (caFile && !existsSync(caFile)) {
		throw new Error(
			`the database connection verifies the server against ${caFile}, and that file is not ` +
				"there. It is the platform's internal CA, published as the ConfigMap " +
				"kitchen-internal-ca and mounted into this pod; the connection is not made " +
				"without it, because an unverified one is what this exists to prevent.",
		);
	}
	log.info("connecting to the database", {
		// Never the DSN itself: it carries the password. `none` is a
		// connection in the clear, which the platform's own database refuses —
		// so seeing it here means an external one nobody asked for TLS from.
		sslmode: sslmode || "none",
		verifiedAgainst: caFile || (sslmode ? "the host's roots" : "nothing"),
	});
	return new pg.Pool({ connectionString: databaseURL, max: 10 });
}

/**
 * Blocks until Postgres answers. The chart starts the auth Deployment and the
 * Postgres StatefulSet at the same time, so the first start regularly races
 * the database being ready.
 */
export async function waitForDatabase(pool: pg.Pool, timeoutSeconds: number): Promise<void> {
	const deadline = Date.now() + timeoutSeconds * 1000;
	let lastError: unknown;
	for (;;) {
		try {
			await pool.query("SELECT 1");
			return;
		} catch (error) {
			lastError = error;
			if (Date.now() >= deadline) {
				break;
			}
			log.info("waiting for the database", { error: String(error) });
			await new Promise((resolve) => setTimeout(resolve, 2000));
		}
	}
	throw new Error(`database was not reachable within ${timeoutSeconds}s: ${String(lastError)}`);
}

/**
 * Brings the schema up to date for the configured plugin set. better-auth
 * derives the schema from the options, so this runs on every start and is a
 * no-op once nothing has changed.
 */
export async function runMigrations(options: BetterAuthOptions, pool: pg.Pool): Promise<void> {
	const client = await pool.connect();
	try {
		await client.query("SELECT pg_advisory_lock($1)", [MIGRATION_LOCK_KEY]);
		const { toBeCreated, toBeAdded, runMigrations: apply } = await getMigrations(options);
		if (toBeCreated.length === 0 && toBeAdded.length === 0) {
			log.info("database schema is up to date");
			return;
		}
		log.info("migrating the database schema", {
			create: toBeCreated.map((table) => table.table),
			alter: toBeAdded.map((table) => table.table),
		});
		await apply();
		log.info("database schema migrated");
	} finally {
		await client.query("SELECT pg_advisory_unlock($1)", [MIGRATION_LOCK_KEY]).catch(() => undefined);
		client.release();
	}
}
