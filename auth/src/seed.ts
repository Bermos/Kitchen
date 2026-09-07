import { defaultKeyHasher } from "@better-auth/api-key";

import type { Auth } from "./auth.js";
import { platformResources, type Config } from "./config.js";
import { MACHINE_PROVISIONING } from "./identity.js";
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
		await ctx.internalAdapter.createUser(
			{
				email,
				name: "Kitchen operator",
				emailVerified: true,
			},
			MACHINE_PROVISIONING,
		);
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
		await linkPlatformResources(auth, config, clientId);
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
	await linkPlatformResources(auth, config, clientId);
	log.info("registered the Kitchen UI as an OAuth client", { clientId, redirectURIs });
}

/**
 * Says that the dashboard's client may ask for a token for the platform's own
 * resources — the issuer and the operator API (`platformResources`).
 *
 * RFC 8707 §3 is enforced per client: a resource being registered says the
 * audience exists, and a link says *this* client may name it. Without one the
 * dashboard's sign-in fails at the token endpoint with `invalid_target`,
 * because it asks for a token for the API rather than for the issuer.
 *
 * It is the same rule `platformClients` states in front of the token endpoint,
 * kept by the provider's own table rather than only by Kitchen's guard — an
 * application's client is registered through `/oauth2/register` and is linked
 * to nothing, so it is refused twice.
 *
 * Re-linking on every start is deliberate and idempotent: the resource list
 * follows the platform's external URLs, which are the chart's to change, so a
 * link that does not exist yet is created and one that does is left alone.
 */
async function linkPlatformResources(auth: Auth, config: Config, clientId: string): Promise<void> {
	const ctx = await auth.$context;
	for (const resourceId of platformResources(config)) {
		const linked = await ctx.adapter.findOne({
			model: "oauthClientResource",
			where: [
				{ field: "clientId", value: clientId },
				{ field: "resourceId", value: resourceId },
			],
		});
		if (linked) {
			continue;
		}
		await ctx.adapter.create({
			model: "oauthClientResource",
			data: { clientId, resourceId, createdAt: new Date() },
		});
		log.info("linked the Kitchen UI client to a platform resource", { clientId, resourceId });
	}
}
