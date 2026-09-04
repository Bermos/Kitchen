import { createAuth, authOptions } from "./auth.js";
import { loadConfig } from "./config.js";
import { createPool, runMigrations, waitForDatabase } from "./db.js";
import { isBootstrapped } from "./bootstrap.js";
import { log } from "./log.js";
import { seedServiceCredential, seedUIClient } from "./seed.js";
import { createServer } from "./server.js";

async function main(): Promise<void> {
	const config = loadConfig();
	const pool = createPool(config.databaseURL);

	await waitForDatabase(pool, config.databaseWaitSeconds);
	await runMigrations(authOptions(config, pool), pool);

	const auth = createAuth(config, pool);
	await seedServiceCredential(auth, config);
	await seedUIClient(auth, config);

	if (config.bootstrapToken && !(await isBootstrapped(auth, config))) {
		log.info("waiting for the first administrator", { url: `${config.baseURL}/bootstrap?token=<token>` });
	}

	// Two listeners, because only one of them is published. The chart routes
	// the issuer's hostname at the public port; the operator's `/kitchen`
	// prefix is served on the private one, which no HTTPRoute names.
	const server = createServer(auth, config, pool, "public");
	const internal = createServer(auth, config, pool, "private");
	await new Promise<void>((resolve) => server.listen(config.port, resolve));
	await new Promise<void>((resolve) => internal.listen(config.internalPort, resolve));
	log.info("listening", {
		port: config.port,
		internalPort: config.internalPort,
		issuer: config.baseURL,
		github: Boolean(config.github),
	});

	const shutdown = (signal: string) => {
		log.info("shutting down", { signal });
		internal.close();
		server.close(() => {
			void pool.end().finally(() => process.exit(0));
		});
		// Kubernetes sends SIGKILL after terminationGracePeriodSeconds anyway;
		// this only keeps a stuck connection from holding the pod open.
		setTimeout(() => process.exit(0), 10_000).unref();
	};
	process.on("SIGTERM", () => shutdown("SIGTERM"));
	process.on("SIGINT", () => shutdown("SIGINT"));
}

main().catch((error) => {
	log.error("failed to start", { error: error instanceof Error ? error.stack : String(error) });
	process.exit(1);
});
