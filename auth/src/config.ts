/**
 * Configuration comes from the environment only: the chart renders it from
 * values and secrets, and nothing about a running instance is read from disk.
 */

export interface GitHubConfig {
	clientId: string;
	clientSecret: string;
}

/**
 * The Kitchen UI: the platform's own OAuth client, seeded rather than
 * registered at runtime so that the client id is a known constant the UI can
 * be configured with.
 */
export interface UIClientConfig {
	clientId: string;
	redirectURIs: string[];
}

export interface Config {
	/** Port the HTTP server binds to. */
	port: number;
	/** Public issuer URL, e.g. https://auth.apps.example.com. */
	baseURL: string;
	/** Signing secret for sessions, tokens and the OAuth query signature. */
	secret: string;
	/** Postgres connection string. */
	databaseURL: string;
	/** Seconds to wait for Postgres to accept connections on start. */
	databaseWaitSeconds: number;
	/**
	 * API key handed to the operator so it can register OAuth clients. Seeded
	 * on start together with the service account that owns it.
	 */
	serviceKey?: string;
	/** Email of the service account owning the service key. */
	serviceAccountEmail: string;
	/**
	 * One-time token for creating the first administrator. It only works
	 * while no account exists — see src/bootstrap.ts.
	 */
	bootstrapToken?: string;
	/**
	 * Public base URL of the operator's REST API, e.g.
	 * https://kitchen.apps.example.com. The API is a resource server of its
	 * own: a client that asks for a token for this URL gets one whose
	 * audience is the API rather than the issuer.
	 */
	apiURL?: string;
	/** The Kitchen UI's OAuth client, when redirect URIs are configured. */
	ui?: UIClientConfig;
	/** Upstream GitHub OAuth app, when one is configured. */
	github?: GitHubConfig;
	/**
	 * Extra origins a browser may call this service from, beyond the ones
	 * `allowedOrigins` already derives. They reach both the CORS headers and
	 * better-auth's origin check, so an entry here is trusted to make a
	 * signed-in write, not merely to read an answer.
	 */
	trustedOrigins: string[];
	/**
	 * Whether an unknown upstream account may create a Kitchen account. Off by
	 * default: everyone with a GitHub account would otherwise be a platform user.
	 */
	allowSocialSignUp: boolean;
}

class ConfigError extends Error {}

function required(env: NodeJS.ProcessEnv, name: string): string {
	const value = env[name]?.trim();
	if (!value) {
		throw new ConfigError(`${name} is required`);
	}
	return value;
}

function optional(env: NodeJS.ProcessEnv, name: string): string | undefined {
	const value = env[name]?.trim();
	return value ? value : undefined;
}

function number(env: NodeJS.ProcessEnv, name: string, fallback: number): number {
	const raw = optional(env, name);
	if (raw === undefined) {
		return fallback;
	}
	const value = Number(raw);
	if (!Number.isInteger(value) || value < 0) {
		throw new ConfigError(`${name} must be a non-negative integer (got ${raw})`);
	}
	return value;
}

function boolean(env: NodeJS.ProcessEnv, name: string, fallback: boolean): boolean {
	const raw = optional(env, name)?.toLowerCase();
	if (raw === undefined) {
		return fallback;
	}
	if (["1", "true", "yes", "on"].includes(raw)) {
		return true;
	}
	if (["0", "false", "no", "off"].includes(raw)) {
		return false;
	}
	throw new ConfigError(`${name} must be a boolean (got ${raw})`);
}

/** Splits a comma-separated list, dropping the empties. */
function list(raw: string | undefined): string[] {
	return (raw ?? "")
		.split(",")
		.map((item) => item.trim())
		.filter(Boolean);
}

function isAbsoluteURL(value: string): boolean {
	try {
		return Boolean(new URL(value).protocol);
	} catch {
		return false;
	}
}

/**
 * Reads the configuration, collecting every problem before failing so a
 * misconfigured Deployment reports all of them in one crash loop.
 */
export function loadConfig(env: NodeJS.ProcessEnv = process.env): Config {
	const problems: string[] = [];
	const collect = <T>(read: () => T, fallback: T): T => {
		try {
			return read();
		} catch (error) {
			problems.push(error instanceof ConfigError ? error.message : String(error));
			return fallback;
		}
	};

	const baseURL = collect(() => required(env, "KITCHEN_AUTH_BASE_URL"), "");
	if (baseURL) {
		try {
			const url = new URL(baseURL);
			if (url.pathname !== "/") {
				problems.push("KITCHEN_AUTH_BASE_URL must not contain a path: the issuer is served from the root");
			}
		} catch {
			problems.push(`KITCHEN_AUTH_BASE_URL must be an absolute URL (got ${baseURL})`);
		}
	}

	const secret = collect(() => required(env, "BETTER_AUTH_SECRET"), "");
	if (secret && secret.length < 16) {
		problems.push("BETTER_AUTH_SECRET must be at least 16 characters");
	}

	const databaseURL = collect(() => required(env, "DATABASE_URL"), "");

	const clientId = optional(env, "GITHUB_CLIENT_ID");
	const clientSecret = optional(env, "GITHUB_CLIENT_SECRET");
	if (Boolean(clientId) !== Boolean(clientSecret)) {
		problems.push("GITHUB_CLIENT_ID and GITHUB_CLIENT_SECRET must be set together");
	}

	const apiURL = optional(env, "KITCHEN_AUTH_API_URL")?.replace(/\/$/, "");
	if (apiURL && !isAbsoluteURL(apiURL)) {
		problems.push(`KITCHEN_AUTH_API_URL must be an absolute URL (got ${apiURL})`);
	}

	const redirectURIs = list(optional(env, "KITCHEN_AUTH_UI_REDIRECT_URIS"));
	const invalidRedirects = redirectURIs.filter((uri) => !isAbsoluteURL(uri));
	if (invalidRedirects.length > 0) {
		problems.push(`KITCHEN_AUTH_UI_REDIRECT_URIS must be absolute URLs (got ${invalidRedirects.join(", ")})`);
	}

	const config: Config = {
		port: collect(() => number(env, "PORT", 8080), 8080),
		baseURL: baseURL.replace(/\/$/, ""),
		secret,
		databaseURL,
		databaseWaitSeconds: collect(() => number(env, "KITCHEN_AUTH_DATABASE_WAIT_SECONDS", 120), 120),
		serviceKey: optional(env, "KITCHEN_AUTH_SERVICE_KEY"),
		serviceAccountEmail: optional(env, "KITCHEN_AUTH_SERVICE_ACCOUNT_EMAIL") ?? "operator@kitchen.local",
		bootstrapToken: optional(env, "KITCHEN_AUTH_BOOTSTRAP_TOKEN"),
		apiURL,
		// Without a redirect URI there is nowhere to send anyone back to, so
		// there is no client worth creating.
		ui:
			redirectURIs.length > 0
				? {
						clientId: optional(env, "KITCHEN_AUTH_UI_CLIENT_ID") ?? "kitchen-ui",
						redirectURIs,
					}
				: undefined,
		github: clientId && clientSecret ? { clientId, clientSecret } : undefined,
		trustedOrigins: list(optional(env, "KITCHEN_AUTH_TRUSTED_ORIGINS")),
		allowSocialSignUp: collect(() => boolean(env, "KITCHEN_AUTH_ALLOW_SOCIAL_SIGNUP", false), false),
	};

	// The api-key plugin rejects anything shorter than its default key length
	// before it even looks the key up, so a short key would fail at the point
	// of use rather than here.
	if (config.serviceKey && config.serviceKey.length < 64) {
		problems.push("KITCHEN_AUTH_SERVICE_KEY must be at least 64 characters: it authenticates client registration");
	}
	if (config.bootstrapToken && config.bootstrapToken.length < 16) {
		problems.push("KITCHEN_AUTH_BOOTSTRAP_TOKEN must be at least 16 characters");
	}

	if (problems.length > 0) {
		throw new Error(`invalid configuration:\n  - ${problems.join("\n  - ")}`);
	}
	return config;
}

/**
 * Origins a browser may call this service from.
 *
 * The dashboard and the identity provider sit on different hostnames by
 * design, so every call the dashboard's JavaScript makes here — fetching the
 * discovery document, exchanging the authorization code, changing a
 * password — is cross-origin. Two separate things turn on this one list, and
 * they must not drift apart:
 *
 * - the CORS headers (`src/server.ts`), without which the browser refuses to
 *   let the dashboard's script read anything this service returns;
 * - better-auth's own origin check, which refuses any cookie-bearing POST
 *   whose `Origin` is not trusted. That is the CSRF defence on every endpoint
 *   the account screen calls, and an origin missing from here is a `403
 *   INVALID_ORIGIN` rather than a browser-side failure.
 *
 * The list is derived rather than configured: the platform already tells this
 * service where its UI lives, through the API URL and the UI client's redirect
 * URIs. Only known origins are reflected, never `*`, because a wildcard cannot
 * be combined with credentials.
 */
export function allowedOrigins(config: Config): ReadonlySet<string> {
	const origins = new Set<string>();
	const add = (value: string | undefined): void => {
		if (!value) {
			return;
		}
		try {
			origins.add(new URL(value).origin);
		} catch {
			// A malformed entry is the config validator's problem, not ours.
		}
	};

	add(config.baseURL);
	add(config.apiURL);
	for (const uri of config.ui?.redirectURIs ?? []) {
		add(uri);
	}
	for (const origin of config.trustedOrigins) {
		add(origin);
	}
	return origins;
}

/**
 * The OAuth clients that are the platform's own — the only ones this issuer
 * will mint a token for a named resource for.
 *
 * This issuer serves two kinds of client from one registry. Some are the
 * platform's: the dashboard, seeded here with an id the chart chooses. The
 * rest belong to *applications* — an `oidcClient` claim registers one for
 * whatever a project developer is deploying, and it exists so that their app
 * can sign people in.
 *
 * `validAudiences` says which audiences may be asked for; it never said *by
 * whom*. So an application's client could exchange its code with
 * `resource=<the operator API>` and be handed a JWT the API accepts as the
 * person who pressed "Allow" — with every role that person holds, for an hour,
 * renewable for a week. The consent screen lists scopes and has never listed
 * an audience, so there was nothing there to refuse either. Third-party API
 * access is not a feature Kitchen has; until it is, `resource` belongs to the
 * platform's own clients, and `src/auth.ts` refuses it to anybody else.
 *
 * It is derived from configuration rather than from anything a registration
 * says about itself: a client id is issued by the provider, so a developer
 * cannot choose one, and nothing a developer can reach writes this list. The
 * operator API keeps the same rule against `azp` on its side
 * (`internal/api/auth.go`, PlatformClientIDs), so neither service depends on
 * the other's enforcement.
 *
 * The CLI is deliberately absent: it holds an API key and exchanges it here
 * for a session-minted token, which asks for no resource and names no client.
 */
export function platformClients(config: Config): ReadonlySet<string> {
	const clients = new Set<string>();
	if (config.ui?.clientId) {
		clients.add(config.ui.clientId);
	}
	return clients;
}
