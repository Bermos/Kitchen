import type { Auth } from "./auth.js";
import { normalizeEmail, peopleOnly } from "./bootstrap.js";
import type { Config } from "./config.js";
import { log } from "./log.js";

/**
 * The operator's own corner of the identity provider.
 *
 * Everything under this prefix is Kitchen's, not better-auth's: it is mounted
 * ahead of the better-auth catch-all in src/server.ts and answers only to the
 * operator's service credential. It exists because the platform needs to
 * *read* the account table — who exists, and which account an address belongs
 * to — and OpenID Connect has no answer to either question. Discovery and
 * dynamic client registration remain the whole of the contract in
 * docs/AUTH.md; this is the operator talking to the identity provider the
 * chart ships, which is why it is a private prefix rather than an extension
 * of anything standard.
 *
 * It is deliberately narrow. It reads accounts, and it does not create,
 * change or delete one: an account nobody can enumerate is an operator list
 * nobody can seed (issue #104) and a people-picker that cannot resolve an
 * address (issue #106), and that is the whole of the need.
 */
export const KITCHEN_API_PREFIX = "/kitchen";

/** Header the operator's service credential arrives in. */
const SERVICE_KEY_HEADER = "x-api-key";

/**
 * One account, as the platform names accounts.
 *
 * `subject` is the issuer's `sub` and is what an access entry in a Kitchen
 * custom resource is written with; the rest is what makes a list of opaque
 * subjects readable. `emailVerified` travels because an access entry that
 * names an address instead of a `sub` is honoured only for a verified one —
 * the rule lives on AccessSubject in the API types.
 */
export interface Account {
	subject: string;
	email: string;
	name: string;
	emailVerified: boolean;
}

/** A request into the prefix, reduced to what the routes here read. */
export interface KitchenRequest {
	method: string;
	/** Path with the trailing slash already stripped, e.g. /kitchen/accounts. */
	path: string;
	query: URLSearchParams;
	apiKey: string | null;
}

/** What the caller should be answered with. */
export interface KitchenResponse {
	status: number;
	body: unknown;
}

/**
 * A route under the prefix. Issue #111 adds API-key endpoints here by adding
 * entries to the table below; the credential check in front of it is the
 * prefix's, so a route added later cannot forget to make it.
 */
interface KitchenRoute {
	method: string;
	path: string;
	handle: (auth: Auth, config: Config, request: KitchenRequest) => Promise<KitchenResponse>;
}

const routes: KitchenRoute[] = [
	{ method: "GET", path: `${KITCHEN_API_PREFIX}/accounts`, handle: getAccounts },
];

/** Whether a path belongs to this prefix at all. */
export function isKitchenPath(path: string): boolean {
	return path === KITCHEN_API_PREFIX || path.startsWith(`${KITCHEN_API_PREFIX}/`);
}

/**
 * Answers a request under the prefix, after establishing that it came from
 * the operator.
 */
export async function handleKitchenRequest(
	auth: Auth,
	config: Config,
	request: KitchenRequest,
): Promise<KitchenResponse> {
	const refusal = await authenticate(auth, config, request.apiKey);
	if (refusal) {
		return refusal;
	}

	const matching = routes.filter((route) => route.path === request.path);
	if (matching.length === 0) {
		return { status: 404, body: { error: `no such endpoint: ${request.path}` } };
	}
	const route = matching.find((candidate) => candidate.method === request.method);
	if (!route) {
		return {
			status: 405,
			body: { error: `${request.path} answers ${matching.map((r) => r.method).join(", ")}` },
		};
	}
	return route.handle(auth, config, request);
}

/**
 * Establishes that the caller is the operator, and returns the refusal to
 * send when it is not.
 *
 * Two things have to be true, and they fail differently on purpose. The key
 * has to be a key the identity provider issued — anything else is a 401 — and
 * the account that owns it has to be the service account, which is a 403: a
 * CI key is a perfectly valid credential belonging to somebody who may not
 * read the account table, and answering it with "invalid key" would send
 * whoever is holding it looking for the wrong fault. A human session does not
 * reach here at all, because nothing here looks at a session cookie.
 */
async function authenticate(
	auth: Auth,
	config: Config,
	apiKey: string | null,
): Promise<KitchenResponse | null> {
	if (!apiKey) {
		return { status: 401, body: { error: `this endpoint requires the ${SERVICE_KEY_HEADER} header` } };
	}

	const verified = await apiKeys(auth).verifyApiKey({ body: { key: apiKey } });
	if (!verified.valid || !verified.key) {
		return { status: 401, body: { error: "invalid api key" } };
	}

	const ownerId = ownerOf(verified.key);
	const ctx = await auth.$context;
	const owner = ownerId ? await ctx.internalAdapter.findUserById(ownerId) : null;
	if (!owner || normalizeEmail(owner.email) !== normalizeEmail(config.serviceAccountEmail)) {
		log.warn("refused a Kitchen API call from a key that is not the operator's");
		return {
			status: 403,
			body: { error: "this endpoint is for the Kitchen operator's service credential only" },
		};
	}
	return null;
}

/**
 * The api-key plugin's server-side verification.
 *
 * `authOptions` is declared as the widened `BetterAuthOptions`, so that the
 * migration runner and the instance share one object; the cost is that
 * `betterAuth` cannot infer the plugins' endpoints onto `auth.api`, and a
 * plugin endpoint has to be named for what it is. Naming only the two fields
 * this file reads keeps the cast honest — a plugin that changed either would
 * still fail here rather than somewhere further down.
 */
interface ApiKeyVerification {
	valid: boolean;
	key: Record<string, unknown> | null;
}

function apiKeys(auth: Auth): { verifyApiKey(input: { body: { key: string } }): Promise<ApiKeyVerification> } {
	return auth.api as unknown as {
		verifyApiKey(input: { body: { key: string } }): Promise<ApiKeyVerification>;
	};
}

/**
 * The account a key belongs to. The api-key plugin calls the column
 * `referenceId`; older rows and other spellings are read too, since being
 * unable to tell who owns a key has to fail closed rather than mismatch.
 */
function ownerOf(key: Record<string, unknown>): string | null {
	for (const field of ["referenceId", "userId"]) {
		const value = key[field];
		if (typeof value === "string" && value) {
			return value;
		}
	}
	return null;
}

/**
 * GET /kitchen/accounts — every account, or the one with a given address.
 *
 * Both answers come off the same read. The account table is a team's worth of
 * rows, and doing the address match here rather than in the database keeps
 * the two endpoints on one code path: the same "is this a person" rule, and a
 * comparison between normalised addresses rather than one at the mercy of the
 * column's collation.
 */
async function getAccounts(auth: Auth, config: Config, request: KitchenRequest): Promise<KitchenResponse> {
	const accounts = await listAccounts(auth, config);

	const wanted = request.query.get("email");
	if (wanted === null) {
		return { status: 200, body: { accounts } };
	}

	const address = normalizeEmail(wanted);
	const found = accounts.find((account) => normalizeEmail(account.email) === address);
	if (!found) {
		return { status: 404, body: { error: `no account with the address ${address}` } };
	}
	return { status: 200, body: found };
}

/** Every account that belongs to a person, oldest first. */
async function listAccounts(auth: Auth, config: Config): Promise<Account[]> {
	const ctx = await auth.$context;
	const users = await ctx.adapter.findMany<{
		id: string;
		email: string;
		name?: string | null;
		emailVerified?: boolean | null;
	}>({
		model: "user",
		where: peopleOnly(config),
		sortBy: { field: "createdAt", direction: "asc" },
	});

	return users.map((user) => ({
		subject: user.id,
		email: user.email,
		name: user.name ?? "",
		emailVerified: Boolean(user.emailVerified),
	}));
}
