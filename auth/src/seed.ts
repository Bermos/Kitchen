import { defaultKeyHasher } from "@better-auth/api-key";

import type { Auth } from "./auth.js";
import type { Config } from "./config.js";
import { log } from "./log.js";

/** Name of the seeded API key, so it is recognisable in the UI later. */
const SERVICE_KEY_NAME = "kitchen-operator";

/**
 * Makes the operator's credential usable: an account that owns it, and the
 * key itself stored the way the api-key plugin expects (hashed).
 *
 * The key value comes from the chart, which is the only place that can put it
 * in a Kubernetes Secret the operator can read. Seeding is idempotent — the
 * service starts with the same environment on every rollout — and rotating the
 * key in the Secret is enough to replace it, because the previous row is
 * removed here.
 */
export async function seedServiceCredential(auth: Auth, config: Config): Promise<void> {
	if (!config.serviceKey) {
		log.warn("no service credential configured: client registration needs an interactive session", {
			env: "KITCHEN_AUTH_SERVICE_KEY",
		});
		return;
	}

	const ctx = await auth.$context;
	const email = config.serviceAccountEmail;

	let user = await ctx.internalAdapter.findUserByEmail(email);
	if (!user) {
		await ctx.internalAdapter.createUser({
			email,
			name: "Kitchen operator",
			emailVerified: true,
		});
		user = await ctx.internalAdapter.findUserByEmail(email);
		if (!user) {
			throw new Error(`failed to create the service account ${email}`);
		}
		log.info("created the service account", { email });
	}

	const hashed = await defaultKeyHasher(config.serviceKey);
	const existing = await ctx.adapter.findMany<{ id: string; key: string }>({
		model: "apikey",
		where: [{ field: "referenceId", value: user.user.id }],
	});

	if (existing.some((key) => key.key === hashed)) {
		log.debug("service credential already seeded");
		return;
	}

	// A key that no longer matches the Secret is a rotated key: dropping it
	// keeps exactly one credential valid at a time.
	for (const stale of existing) {
		await ctx.adapter.delete({ model: "apikey", where: [{ field: "id", value: stale.id }] });
	}

	const now = new Date();
	await ctx.adapter.create({
		model: "apikey",
		data: {
			name: SERVICE_KEY_NAME,
			start: config.serviceKey.slice(0, 6),
			referenceId: user.user.id,
			key: hashed,
			enabled: true,
			// The operator registers a client per environment and refreshes
			// redirect URIs as previews come and go; the plugin's default of a
			// few requests a day would throttle its own control loop.
			rateLimitEnabled: false,
			createdAt: now,
			updatedAt: now,
		},
	});
	log.info("seeded the operator's service credential", { name: SERVICE_KEY_NAME, email });
}
