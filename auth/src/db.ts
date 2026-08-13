import { getMigrations } from "better-auth/db/migration";
import type { BetterAuthOptions } from "better-auth";
import pg from "pg";

import { log } from "./log.js";

/**
 * Arbitrary but stable key for the Postgres advisory lock guarding schema
 * migrations. Replicas start at the same time; only one may migrate.
 */
const MIGRATION_LOCK_KEY = 4_015_071_986;

export function createPool(databaseURL: string): pg.Pool {
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
