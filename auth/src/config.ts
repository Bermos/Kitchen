/**
 * Configuration comes from the environment only: the chart renders it from
 * values and secrets, and nothing about a running instance is read from disk.
 */

export interface GitHubConfig {
	clientId: string;
	clientSecret: string;
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
	/** Upstream GitHub OAuth app, when one is configured. */
	github?: GitHubConfig;
	/** Extra origins allowed to call the API from a browser. */
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

	const config: Config = {
		port: collect(() => number(env, "PORT", 8080), 8080),
		baseURL: baseURL.replace(/\/$/, ""),
		secret,
		databaseURL,
		databaseWaitSeconds: collect(() => number(env, "KITCHEN_AUTH_DATABASE_WAIT_SECONDS", 120), 120),
		serviceKey: optional(env, "KITCHEN_AUTH_SERVICE_KEY"),
		serviceAccountEmail: optional(env, "KITCHEN_AUTH_SERVICE_ACCOUNT_EMAIL") ?? "operator@kitchen.local",
		bootstrapToken: optional(env, "KITCHEN_AUTH_BOOTSTRAP_TOKEN"),
		github: clientId && clientSecret ? { clientId, clientSecret } : undefined,
		trustedOrigins: (optional(env, "KITCHEN_AUTH_TRUSTED_ORIGINS") ?? "")
			.split(",")
			.map((origin) => origin.trim())
			.filter(Boolean),
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
