import { createServer as createHTTPServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { toNodeHandler } from "better-auth/node";
import type { Pool } from "pg";

import type { Auth } from "./auth.js";
import { CONSENT_PATH, LOGIN_PATH } from "./auth.js";
import { bootstrapFirstUser, isBootstrapped, tokenMatches } from "./bootstrap.js";
import type { Config } from "./config.js";
import { log } from "./log.js";
import { bootstrapPage, consentPage, loginPage, messagePage } from "./pages.js";

const BOOTSTRAP_PATH = "/bootstrap";
/** Bodies on our own routes are tiny; anything larger is not a real request. */
const MAX_BODY_BYTES = 16 * 1024;

function send(res: ServerResponse, status: number, contentType: string, body: string): void {
	res.writeHead(status, {
		"content-type": contentType,
		"cache-control": "no-store",
		"x-content-type-options": "nosniff",
		"content-length": Buffer.byteLength(body),
	});
	res.end(body);
}

const sendHTML = (res: ServerResponse, status: number, body: string) =>
	send(res, status, "text/html; charset=utf-8", body);
const sendJSON = (res: ServerResponse, status: number, body: unknown) =>
	send(res, status, "application/json", JSON.stringify(body));

async function readBody(req: IncomingMessage): Promise<Record<string, string>> {
	const chunks: Buffer[] = [];
	let size = 0;
	for await (const chunk of req) {
		size += chunk.length;
		if (size > MAX_BODY_BYTES) {
			throw new Error("request body too large");
		}
		chunks.push(chunk as Buffer);
	}
	const raw = Buffer.concat(chunks).toString("utf8");
	if (!raw) {
		return {};
	}
	const contentType = req.headers["content-type"] ?? "";
	if (contentType.includes("application/x-www-form-urlencoded")) {
		return Object.fromEntries(new URLSearchParams(raw));
	}
	const parsed: unknown = JSON.parse(raw);
	if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
		throw new Error("request body must be a JSON object");
	}
	return parsed as Record<string, string>;
}

/**
 * Looks up the display name of the client asking for consent. A client
 * registered a second ago is not worth failing the consent screen over, so an
 * unknown client is shown by its id.
 */
async function clientName(auth: Auth, clientId: string): Promise<string> {
	if (!clientId) {
		return "an application";
	}
	try {
		const ctx = await auth.$context;
		const client = await ctx.adapter.findOne<{ name?: string | null }>({
			model: "oauthClient",
			where: [{ field: "clientId", value: clientId }],
		});
		return client?.name || clientId;
	} catch (error) {
		log.warn("could not resolve the client name", { clientId, error: String(error) });
		return clientId;
	}
}

/**
 * Origins the browser may call this service from.
 *
 * The dashboard and the identity provider sit on different hostnames by
 * design, so every call the dashboard's JavaScript makes here — fetching the
 * discovery document, exchanging the authorization code — is cross-origin and
 * unreadable without these headers.
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

export function createServer(auth: Auth, config: Config, pool: Pool): Server {
	const authHandler = toNodeHandler(auth);
	const allowed = allowedOrigins(config);

	return createHTTPServer((req, res) => {
		void handle(req, res).catch((error) => {
			log.error("request failed", { path: req.url, error: String(error) });
			if (!res.headersSent) {
				sendJSON(res, 500, { error: "internal server error" });
			} else {
				res.end();
			}
		});
	});

	async function handle(req: IncomingMessage, res: ServerResponse): Promise<void> {
		const url = new URL(req.url ?? "/", config.baseURL);
		const path = url.pathname.replace(/\/+$/, "") || "/";

		// Cross-origin access for the dashboard. Without these headers the
		// browser refuses to let its script read anything this service returns,
		// which surfaces as an unexplained load failure on the sign-in screen
		// rather than as an error anyone can act on.
		const origin = req.headers.origin;
		const originAllowed = typeof origin === "string" && allowed.has(origin);
		if (originAllowed) {
			res.setHeader("access-control-allow-origin", origin);
			res.setHeader("access-control-allow-credentials", "true");
			res.setHeader("vary", "Origin");
		}

		// Preflight. better-auth answers no OPTIONS of its own, so without this
		// the token endpoint's preflight falls through to a 405 and the code
		// exchange never happens.
		if (req.method === "OPTIONS") {
			if (!originAllowed) {
				send(res, 403, "text/plain; charset=utf-8", "origin not allowed\n");
				return;
			}
			res.writeHead(204, {
				"access-control-allow-methods": "GET, POST, OPTIONS",
				"access-control-allow-headers":
					req.headers["access-control-request-headers"] ?? "content-type, authorization",
				"access-control-max-age": "600",
				"cache-control": "no-store",
			});
			res.end();
			return;
		}

		// Liveness: the process answers. Readiness additionally requires the
		// database, because an instance that cannot reach Postgres can neither
		// sign anyone in nor issue a token.
		if (path === "/healthz") {
			send(res, 200, "text/plain; charset=utf-8", "ok\n");
			return;
		}
		if (path === "/readyz") {
			try {
				await pool.query("SELECT 1");
				send(res, 200, "text/plain; charset=utf-8", "ok\n");
			} catch (error) {
				log.warn("readiness check failed", { error: String(error) });
				send(res, 503, "text/plain; charset=utf-8", "database unavailable\n");
			}
			return;
		}

		if (path === LOGIN_PATH && req.method === "GET") {
			sendHTML(res, 200, loginPage({ github: Boolean(config.github) }));
			return;
		}

		if (path === CONSENT_PATH && req.method === "GET") {
			const scopes = (url.searchParams.get("scope") ?? "").split(" ").filter(Boolean);
			const name = await clientName(auth, url.searchParams.get("client_id") ?? "");
			sendHTML(res, 200, consentPage({ clientName: name, scopes }));
			return;
		}

		if (path === BOOTSTRAP_PATH) {
			await handleBootstrap(req, res, url);
			return;
		}

		if (path === "/") {
			res.writeHead(302, { location: LOGIN_PATH, "cache-control": "no-store" });
			res.end();
			return;
		}

		// Everything else is better-auth: it is mounted at the root so the
		// discovery document sits at <issuer>/.well-known/openid-configuration.
		await authHandler(req, res);
	}

	async function handleBootstrap(req: IncomingMessage, res: ServerResponse, url: URL): Promise<void> {
		if (!config.bootstrapToken) {
			sendHTML(res, 404, messagePage("Bootstrap disabled", "This installation was configured without a bootstrap token."));
			return;
		}

		if (req.method === "GET") {
			const token = url.searchParams.get("token");
			if (await isBootstrapped(auth, config)) {
				sendHTML(res, 410, messagePage("Already set up", "This installation already has an account. Sign in instead."));
				return;
			}
			if (!tokenMatches(config.bootstrapToken, token)) {
				sendHTML(res, 401, messagePage("Invalid link", "This bootstrap link is missing or carries the wrong token."));
				return;
			}
			sendHTML(res, 200, bootstrapPage(token ?? ""));
			return;
		}

		if (req.method !== "POST") {
			res.writeHead(405, { allow: "GET, POST" });
			res.end();
			return;
		}

		let body: Record<string, string>;
		try {
			body = await readBody(req);
		} catch (error) {
			sendJSON(res, 400, { error: String(error instanceof Error ? error.message : error) });
			return;
		}

		const result = await bootstrapFirstUser(auth, config, {
			token: body.token ?? url.searchParams.get("token"),
			email: body.email ?? "",
			name: body.name ?? "",
			password: body.password ?? "",
		});
		if (result.ok) {
			sendJSON(res, 201, { ok: true });
		} else {
			sendJSON(res, result.status, { error: result.error });
		}
	}
}
