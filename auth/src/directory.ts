import type { Auth } from "./auth.js";
import {
	deleteClient,
	type ManagedClient,
	NotOurClientError,
	updateClient,
} from "./clients.js";
import type { Config } from "./config.js";
import { isLabel, LABEL_RULE, listPeople, normalizeEmail } from "./identity.js";
import {
	createProjectKey,
	deleteProjectKey,
	KeyExistsError,
	listProjectKeys,
	type ProjectKey,
} from "./keys.js";
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
 * It is deliberately narrow. Over accounts it only reads — who exists, and
 * who holds an address — because an account nobody can enumerate is an
 * operator list nobody can seed (issue #104) and a people-picker that cannot
 * resolve an address (issue #106). The one thing it writes is a CI key and
 * the machine account that owns it (issue #111), which the operator cannot
 * do for itself: a key has to exist at the issuer, because the issuer is
 * where a key is verified and where revoking one takes effect.
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
	/** The decoded JSON body, or an empty object for a request without one. */
	body: Record<string, unknown>;
	apiKey: string | null;
	/**
	 * The account the credential belongs to, filled in by the prefix's own
	 * check before any route runs. It is the service account — that is what
	 * the check establishes — and a route needs it when what it may do
	 * depends on what the operator itself created.
	 */
	operator?: string;
}

/** What the caller should be answered with. */
export interface KitchenResponse {
	status: number;
	body: unknown;
}

/**
 * A route under the prefix. New endpoints are entries in the table below; the
 * credential check in front of it is the prefix's, so a route added later
 * cannot forget to make it.
 */
interface KitchenRoute {
	method: string;
	path: string;
	handle: (auth: Auth, config: Config, request: KitchenRequest) => Promise<KitchenResponse>;
}

const routes: KitchenRoute[] = [
	{ method: "GET", path: `${KITCHEN_API_PREFIX}/accounts`, handle: getAccounts },
	{ method: "GET", path: `${KITCHEN_API_PREFIX}/keys`, handle: getKeys },
	{ method: "POST", path: `${KITCHEN_API_PREFIX}/keys`, handle: postKey },
	{ method: "DELETE", path: `${KITCHEN_API_PREFIX}/keys`, handle: deleteKey },
	{ method: "PUT", path: `${KITCHEN_API_PREFIX}/clients`, handle: putClient },
	{ method: "DELETE", path: `${KITCHEN_API_PREFIX}/clients`, handle: removeClient },
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
	const caller = await authenticate(auth, config, request.apiKey);
	if ("refusal" in caller) {
		return caller.refusal;
	}
	request.operator = caller.operator;

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
): Promise<{ refusal: KitchenResponse } | { operator: string }> {
	if (!apiKey) {
		return {
			refusal: {
				status: 401,
				body: { error: `this endpoint requires the ${SERVICE_KEY_HEADER} header` },
			},
		};
	}

	const verified = await apiKeys(auth).verifyApiKey({ body: { key: apiKey } });
	if (!verified.valid || !verified.key) {
		return { refusal: { status: 401, body: { error: "invalid api key" } } };
	}

	const ownerId = ownerOf(verified.key);
	const ctx = await auth.$context;
	const owner = ownerId ? await ctx.internalAdapter.findUserById(ownerId) : null;
	if (!owner || normalizeEmail(owner.email) !== normalizeEmail(config.serviceAccountEmail)) {
		log.warn("refused a Kitchen API call from a key that is not the operator's");
		return {
			refusal: {
				status: 403,
				body: { error: "this endpoint is for the Kitchen operator's service credential only" },
			},
		};
	}
	return { operator: owner.id };
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
	const users = await listPeople(auth, config);
	return users.map((user) => ({
		subject: user.id,
		email: user.email,
		name: user.name ?? "",
		emailVerified: Boolean(user.emailVerified),
	}));
}

/**
 * The CI keys a project has, the one it is given, and the one it takes away.
 *
 * All three answer on `/kitchen/keys` and address a key by the two things
 * that name it — its project and its name — rather than by the machine
 * account's `sub`, which is opaque and which the operator would have to have
 * looked up first to create anything at all.
 *
 * Only the creation reveals a key value, and only in that one response. There
 * is no read that returns it and no way to recover it: the key is stored
 * hashed, exactly as the api-key plugin stores every other one.
 */
async function getKeys(auth: Auth, _config: Config, request: KitchenRequest): Promise<KitchenResponse> {
	const project = (request.query.get("project") ?? "").trim();
	const refusal = badLabel("project", project);
	if (refusal) {
		return refusal;
	}
	return { status: 200, body: { keys: await listProjectKeys(auth, project) } };
}

async function postKey(auth: Auth, _config: Config, request: KitchenRequest): Promise<KitchenResponse> {
	const project = text(request.body.project);
	const name = text(request.body.name);
	const refusal = badLabel("project", project) ?? badLabel("name", name);
	if (refusal) {
		return refusal;
	}

	try {
		return { status: 201, body: await createProjectKey(auth, project, name) };
	} catch (error) {
		if (error instanceof KeyExistsError) {
			return { status: 409, body: { error: error.message } };
		}
		throw error;
	}
}

async function deleteKey(auth: Auth, _config: Config, request: KitchenRequest): Promise<KitchenResponse> {
	const project = (request.query.get("project") ?? "").trim();
	const name = (request.query.get("name") ?? "").trim();
	const refusal = badLabel("project", project) ?? badLabel("name", name);
	if (refusal) {
		return refusal;
	}

	const removed: ProjectKey | null = await deleteProjectKey(auth, project, name);
	if (!removed) {
		return { status: 404, body: { error: `${project} has no key called ${name}` } };
	}
	return { status: 200, body: removed };
}

/**
 * PUT /kitchen/clients — what the operator maintains about a client it
 * registered, and DELETE /kitchen/clients — taking that client away.
 *
 * Both address the client by its `client_id`, which is the only name it has,
 * and both are here rather than at a standard endpoint because there is no
 * standard endpoint: the OAuth provider plugin implements RFC 7591's
 * registration and not RFC 7592's management, so a registered client cannot
 * be changed or removed through anything the discovery document names. See
 * src/clients.ts for what that costs an issuer this prefix is not on.
 *
 * The redirect list is the field that moves. The operator keeps it in step
 * with the URLs of a project's environments — a preview appears with a URL,
 * a merged pull request takes it away — which is the whole of what
 * `ResourceClaim` type `oidcClient` promises an application: sign-in that
 * works on every environment without anybody visiting an OAuth console.
 */
async function putClient(auth: Auth, _config: Config, request: KitchenRequest): Promise<KitchenResponse> {
	const clientId = text(request.body.clientId);
	if (!clientId) {
		return { status: 400, body: { error: "clientId is required" } };
	}
	const redirectURIs = list(request.body.redirectURIs);
	if (redirectURIs && redirectURIs.length === 0) {
		// An empty list would leave a client nothing can sign in to, and the
		// operator never asks for one: a claim with no environments to point
		// at keeps the redirect URIs it has until it has better ones.
		return { status: 400, body: { error: "redirectURIs cannot be empty" } };
	}

	return manage(async () => {
		const updated = await updateClient(auth, request.operator ?? "", clientId, {
			clientName: text(request.body.clientName) || undefined,
			redirectURIs,
			grantTypes: list(request.body.grantTypes),
			scopes: list(request.body.scopes),
		});
		return [updated, clientId];
	});
}

async function removeClient(auth: Auth, _config: Config, request: KitchenRequest): Promise<KitchenResponse> {
	const clientId = (request.query.get("clientId") ?? "").trim();
	if (!clientId) {
		return { status: 400, body: { error: "clientId is required" } };
	}
	return manage(async () => [await deleteClient(auth, request.operator ?? "", clientId), clientId]);
}

/**
 * The two answers a client operation has that are not the client itself: it
 * is not there (404, which is also what an issuer without this prefix says),
 * and it is there but belongs to somebody else (403, because retrying cannot
 * help and the operator has to be told which client it is being kept away
 * from).
 */
async function manage(
	operation: () => Promise<[ManagedClient | null, string]>,
): Promise<KitchenResponse> {
	try {
		const [client, clientId] = await operation();
		if (!client) {
			return { status: 404, body: { error: `no client with the id ${clientId}` } };
		}
		return { status: 200, body: client };
	} catch (error) {
		if (error instanceof NotOurClientError) {
			return { status: 403, body: { error: error.message } };
		}
		throw error;
	}
}

/**
 * A string array field of a request body, and undefined for a field that is
 * absent — which is how a caller says "leave this as it is" rather than "set
 * it to nothing".
 */
function list(value: unknown): string[] | undefined {
	if (!Array.isArray(value)) {
		return undefined;
	}
	return value.filter((entry): entry is string => typeof entry === "string" && entry.trim() !== "");
}

/** One string field of a request body, and the empty string for anything else. */
function text(value: unknown): string {
	return typeof value === "string" ? value.trim() : "";
}

/**
 * The refusal for a project or key name that is not a label, or null when it
 * is one. Both halves of a machine account's address have to be labels for
 * the address to stay parseable — see src/identity.ts.
 */
function badLabel(field: string, value: string): KitchenResponse | null {
	if (isLabel(value)) {
		return null;
	}
	return {
		status: 400,
		body: { error: `${field} must be ${LABEL_RULE} (got ${JSON.stringify(value)})` },
	};
}
