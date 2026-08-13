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

/**
 * The Kitchen UI's OAuth client.
 *
 * It is the platform's own front end, so it is seeded rather than registered
 * through the dynamic-registration endpoint: the UI is configured with its
 * client id at build time, and a generated one would have to be discovered
 * from somewhere. Everything else about it follows from being a browser
 * application — no client secret it could not keep, and therefore PKCE, which
 * the provider enforces for public clients without being asked.
 *
 * Consent is skipped. Asking someone to authorise the dashboard of the
 * platform they just signed in to is a dialog with one sensible answer;
 * third-party clients registered later still get the consent screen.
 *
 * Seeding is idempotent, and the redirect URIs are refreshed on every start,
 * because they follow the platform's base domain — which is the chart's to
 * change.
 */
export async function seedUIClient(auth: Auth, config: Config): Promise<void> {
	if (!config.ui) {
		log.debug("no redirect URIs configured for the Kitchen UI: no client seeded", {
			env: "KITCHEN_AUTH_UI_REDIRECT_URIS",
		});
		return;
	}

	const { clientId, redirectURIs } = config.ui;
	const ctx = await auth.$context;
	const now = new Date();

	const existing = await ctx.adapter.findOne<{ id: string; redirectUris?: string[] | string }>({
		model: "oauthClient",
		where: [{ field: "clientId", value: clientId }],
	});

	if (existing) {
		const current = Array.isArray(existing.redirectUris)
			? existing.redirectUris
			: String(existing.redirectUris ?? "").split(",").filter(Boolean);
		if (current.length === redirectURIs.length && current.every((uri, i) => uri === redirectURIs[i])) {
			log.debug("the Kitchen UI client is already registered", { clientId });
			return;
		}
		await ctx.adapter.update({
			model: "oauthClient",
			where: [{ field: "id", value: existing.id }],
			update: { redirectUris: redirectURIs, updatedAt: now },
		});
		log.info("updated the Kitchen UI client's redirect URIs", { clientId, redirectURIs });
		return;
	}

	await ctx.adapter.create({
		model: "oauthClient",
		data: {
			clientId,
			name: "Kitchen",
			redirectUris: redirectURIs,
			grantTypes: ["authorization_code", "refresh_token"],
			responseTypes: ["code"],
			// A public client: no secret, so PKCE is mandatory.
			tokenEndpointAuthMethod: "none",
			public: true,
			type: "user-agent-based",
			skipConsent: true,
			disabled: false,
			createdAt: now,
			updatedAt: now,
		},
	});
	log.info("registered the Kitchen UI as an OAuth client", { clientId, redirectURIs });
}
