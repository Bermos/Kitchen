import { createServer as createHTTPServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";
import { toNodeHandler } from "better-auth/node";
import type { Pool } from "pg";

import type { Auth } from "./auth.js";
import { CONSENT_PATH, LOGIN_PATH } from "./auth.js";
import { bootstrapFirstUser, isBootstrapped, tokenMatches } from "./bootstrap.js";
import { allowedOrigins, type Config } from "./config.js";
import { handleKitchenRequest, isKitchenPath } from "./directory.js";
import { log } from "./log.js";
import { bootstrapPage, consentPage, loginPage, messagePage } from "./pages.js";
import { rateLimiter } from "./ratelimit.js";

const BOOTSTRAP_PATH = "/bootstrap";
/**
 * The header a proxy records the original caller in. It is read for the log
 * line on a refusal only — never to decide anything — because on the public
 * listener the socket's peer is the Gateway and the address worth reporting
 * is the one behind it.
 */
const FORWARDED_FOR = "x-forwarded-for";

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

/** One request header, taking the first when a header arrives repeated. */
function headerValue(req: IncomingMessage, name: string): string | null {
	const value = req.headers[name];
	if (Array.isArray(value)) {
		return value[0] ?? null;
	}
	return value ?? null;
}

/**
 * The address a request is attributed to: the socket's peer, or the first
 * entry of `x-forwarded-for` when something in front recorded one.
 *
 * It keys the rate limiter and it names the caller in a refusal. Behind the
 * Gateway the peer is the Gateway, so without the forwarded header every
 * request from the internet would share one bucket and one log line would
 * name every one of them.
 */
function sourceOf(req: IncomingMessage): string {
	const forwarded = req.headers[FORWARDED_FOR];
	const first = (Array.isArray(forwarded) ? forwarded[0] : forwarded)?.split(",")[0]?.trim();
	return first || req.socket.remoteAddress || "unknown";
}

/** What a refusal says about where it came from. */
function sourceFields(req: IncomingMessage): Record<string, unknown> {
	const fields: Record<string, unknown> = { source: sourceOf(req) };
	const forwarded = req.headers[FORWARDED_FOR];
	if (forwarded) {
		fields.forwardedFor = Array.isArray(forwarded) ? forwarded.join(", ") : forwarded;
	}
	if (req.socket.remoteAddress) {
		fields.peer = req.socket.remoteAddress;
	}
	return fields;
}

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
 * Which of the two listeners a server is.
 *
 * `public` is the one the chart publishes on the shared Gateway: the whole of
 * OpenID Connect, the hosted pages, the bootstrap link — and a 404 for
 * `/kitchen`, which is what an issuer that never had the prefix would say.
 * `private` is the one only the cluster can reach: `/kitchen` and the health
 * endpoints, and a 404 for everything else, so that a listener bound for the
 * operator cannot be talked into serving a login page or minting a token.
 *
 * The split is the fix for the prefix having been reachable from the
 * internet. Whoever holds the service key could, from anywhere, enumerate
 * every account, mint a CI key for any project and rewrite an OAuth client's
 * redirect list; a leaked key is now a leaked key inside the cluster.
 */
export type Listener = "public" | "private";

export function createServer(
	auth: Auth,
	config: Config,
	pool: Pool,
	listener: Listener = "public",
): Server {
	const authHandler = toNodeHandler(auth);
	const allowed = allowedOrigins(config);
	const kitchenLimiter = rateLimiter(config.kitchenRatePerMinute);

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

		// The private listener is the operator's, and serves only what the
		// operator asks for. Everything else — sign-in, the token endpoint,
		// discovery — belongs to the published listener and is not answered
		// twice.
		if (listener === "private") {
			if (await health(res, path)) {
				return;
			}
			if (isKitchenPath(path)) {
				await handleKitchen(req, res, url, path);
				return;
			}
			sendJSON(res, 404, { error: `no such endpoint: ${path}` });
			return;
		}

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

		if (await health(res, path)) {
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

		// Kitchen's own endpoints are not this listener's. This one is
		// published on the shared Gateway, and the prefix answers to a single
		// header that is worth an account takeover; it is served on the
		// private listener instead and refused here, in the words an issuer
		// that never had the prefix would use.
		if (isKitchenPath(path)) {
			refuseKitchen(req, path);
			sendJSON(res, 404, { error: `no such endpoint: ${path}` });
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

	/**
	 * Liveness and readiness, which both listeners answer: a probe is
	 * configured against a port, and the private one has to be able to say
	 * whether it is up without the public one being asked on its behalf.
	 *
	 * Liveness is that the process answers. Readiness additionally requires
	 * the database, because an instance that cannot reach Postgres can
	 * neither sign anyone in nor issue a token.
	 */
	async function health(res: ServerResponse, path: string): Promise<boolean> {
		if (path === "/healthz") {
			send(res, 200, "text/plain; charset=utf-8", "ok\n");
			return true;
		}
		if (path === "/readyz") {
			try {
				await pool.query("SELECT 1");
				send(res, 200, "text/plain; charset=utf-8", "ok\n");
			} catch (error) {
				log.warn("readiness check failed", { error: String(error) });
				send(res, 503, "text/plain; charset=utf-8", "database unavailable\n");
			}
			return true;
		}
		return false;
	}

	/**
	 * Kitchen's own endpoints, mounted ahead of the better-auth catch-all so
	 * the prefix belongs to the platform rather than to whatever better-auth
	 * might route there later. They answer to the operator's service
	 * credential and to nothing else — see src/directory.ts.
	 */
	async function handleKitchen(
		req: IncomingMessage,
		res: ServerResponse,
		url: URL,
		path: string,
	): Promise<void> {
		const source = sourceOf(req);
		if (!kitchenLimiter.allow(source)) {
			log.warn("rate-limited a call to the Kitchen prefix", {
				path,
				...sourceFields(req),
				limit: config.kitchenRatePerMinute,
			});
			res.setHeader("retry-after", "60");
			sendJSON(res, 429, { error: "too many requests to the Kitchen prefix" });
			return;
		}

		// A body is read only for the methods that carry one, so a GET or a
		// DELETE under the prefix cannot be refused over a body it never
		// sent. Unreadable JSON is the caller's fault and is said so here,
		// before the routing table is consulted at all.
		let body: Record<string, unknown> = {};
		if (req.method === "POST" || req.method === "PUT" || req.method === "PATCH") {
			try {
				body = await readBody(req);
			} catch (error) {
				sendJSON(res, 400, { error: String(error instanceof Error ? error.message : error) });
				return;
			}
		}
		const answer = await handleKitchenRequest(auth, config, {
			method: req.method ?? "GET",
			path,
			query: url.searchParams,
			body,
			apiKey: headerValue(req, "x-api-key"),
			source,
		});
		sendJSON(res, answer.status, answer.body);
	}

	/**
	 * Records a call to the prefix on the listener that does not serve it.
	 *
	 * It is a warning rather than a debug line on purpose: nothing legitimate
	 * asks the published hostname for `/kitchen`, so one of these is somebody
	 * looking — and the address it names is the only thing an installation
	 * has to go on. The limiter is consulted so that whoever is looking
	 * cannot turn the log into the denial of service.
	 */
	function refuseKitchen(req: IncomingMessage, path: string): void {
		if (!kitchenLimiter.allow(sourceOf(req))) {
			return;
		}
		log.warn("refused a call to the Kitchen prefix on the published listener", {
			path,
			method: req.method,
			...sourceFields(req),
		});
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
