import type { Auth } from "./auth.js";
import { log } from "./log.js";

/**
 * The OAuth clients the operator registered, as the operator manages them
 * afterwards.
 *
 * Registering a client is standard and needs nothing from this file: RFC 7591
 * is what the OAuth provider plugin serves at `/oauth2/register`, and it is
 * the whole of the contract docs/AUTH.md asks an issuer for. Changing one
 * afterwards is RFC 7592, a separate specification the plugin does not
 * implement — its registration answer names no client configuration endpoint
 * and hands out no registration access token, so there is nothing standard
 * for the operator to call.
 *
 * That matters because the redirect list of an application's client is not
 * written once. It follows the project's environments: a preview appears with
 * a URL nobody could have written down in advance, and a merged pull request
 * takes that URL away again. Keeping the list in step is the point of
 * `ResourceClaim` type `oidcClient` (issue #8), and the operator is the only
 * component that knows when a URL comes or goes.
 *
 * So this rides where the account directory rides — the operator's own prefix
 * on the identity provider the chart ships, authenticated by its service
 * credential — for the same reason the directory does: the platform needs
 * something OpenID Connect never offered. An installation federated to an
 * issuer of its own keeps registration and loses the maintenance, which the
 * operator reports on the claim rather than papering over.
 *
 * **The operator may only manage what the operator registered.** Every client
 * the dynamic-registration endpoint creates records the account whose session
 * created it, which for the operator is the service account. The seeded
 * Kitchen UI client records nobody, because it is written straight into the
 * table at start-up. Requiring the owner to match therefore refuses a call
 * that would rewrite the dashboard's own redirect URIs or take its client
 * away — the one mistake in this file that would sign every person out of the
 * platform and leave nobody able to sign back in.
 */

/** One client, as the operator reads it back. Never its secret. */
export interface ManagedClient {
	clientId: string;
	name: string;
	redirectURIs: string[];
}

/** What a client may have changed about it. */
export interface ClientUpdate {
	clientName?: string;
	redirectURIs?: string[];
	grantTypes?: string[];
	scopes?: string[];
}

/** Thrown when the client exists but was not registered by the operator. */
export class NotOurClientError extends Error {}

/** The columns this module reads off an `oauthClient` row. */
interface ClientRow {
	id: string;
	clientId: string;
	name?: string | null;
	userId?: string | null;
	redirectUris?: string[] | string | null;
}

/**
 * A `string[]` column, whichever way the adapter hands it back. The Postgres
 * adapter answers with an array; some store it joined, and a client whose
 * redirect list read back as the characters of a string would be a client
 * nobody could sign in to.
 */
function asList(value: string[] | string | null | undefined): string[] {
	if (Array.isArray(value)) {
		return value;
	}
	return String(value ?? "")
		.split(",")
		.filter(Boolean);
}

function asManagedClient(row: ClientRow): ManagedClient {
	return {
		clientId: row.clientId,
		name: row.name ?? "",
		redirectURIs: asList(row.redirectUris),
	};
}

/** The client with this id, or null when there is none. */
async function findClient(auth: Auth, clientId: string): Promise<ClientRow | null> {
	const ctx = await auth.$context;
	return ctx.adapter.findOne<ClientRow>({
		model: "oauthClient",
		where: [{ field: "clientId", value: clientId }],
	});
}

/**
 * The client, established to be one this operator registered.
 *
 * Null means there is no such client — which the caller answers with a 404,
 * the same answer an issuer that had never heard of this endpoint would give,
 * and the same one the operator reads as "it is not there".
 */
async function ourClient(auth: Auth, operatorId: string, clientId: string): Promise<ClientRow | null> {
	const client = await findClient(auth, clientId);
	if (!client) {
		return null;
	}
	if (!client.userId || client.userId !== operatorId) {
		throw new NotOurClientError(
			`the client ${clientId} was not registered by the operator: it is managed where it was created`,
		);
	}
	return client;
}

/**
 * Replaces what the operator maintains about one of its clients.
 *
 * Only the named fields move. The client id, the secret and everything the
 * registration decided stay as they are: this is the redirect list following
 * the project's environments, not a re-registration, and a re-registration
 * would hand out credentials the running application does not have.
 */
export async function updateClient(
	auth: Auth,
	operatorId: string,
	clientId: string,
	update: ClientUpdate,
): Promise<ManagedClient | null> {
	const client = await ourClient(auth, operatorId, clientId);
	if (!client) {
		return null;
	}

	const changes: Record<string, unknown> = { updatedAt: new Date() };
	if (update.redirectURIs) {
		changes.redirectUris = update.redirectURIs;
	}
	if (update.clientName) {
		changes.name = update.clientName;
	}
	if (update.grantTypes?.length) {
		changes.grantTypes = update.grantTypes;
	}
	if (update.scopes?.length) {
		changes.scopes = update.scopes;
	}

	const ctx = await auth.$context;
	const updated = await ctx.adapter.update<ClientRow>({
		model: "oauthClient",
		where: [{ field: "id", value: client.id }],
		update: changes,
	});
	log.info("updated an operator-registered OAuth client", {
		clientId,
		redirectURIs: update.redirectURIs?.length ?? asList(client.redirectUris).length,
	});
	return asManagedClient(updated ?? { ...client, ...(update.redirectURIs ? { redirectUris: update.redirectURIs } : {}) });
}

/**
 * Deregisters one of the operator's clients.
 *
 * The consents and tokens that reference it are the plugin's rows, and they
 * are removed with it: a client id nothing can be issued for is exactly what
 * "the claim is gone" has to mean, since anything still holding a token would
 * otherwise keep signing people in with credentials the platform no longer
 * knows about.
 */
export async function deleteClient(
	auth: Auth,
	operatorId: string,
	clientId: string,
): Promise<ManagedClient | null> {
	const client = await ourClient(auth, operatorId, clientId);
	if (!client) {
		return null;
	}

	const ctx = await auth.$context;
	for (const model of ["oauthAccessToken", "oauthRefreshToken", "oauthConsent"]) {
		try {
			await ctx.adapter.deleteMany({
				model,
				where: [{ field: "clientId", value: clientId }],
			});
		} catch (error) {
			// A model this plugin version does not have is not a reason to
			// leave the client registered; the client row is what makes the
			// credentials usable, and it goes below either way.
			log.debug("nothing to remove for the client", { model, clientId, error: String(error) });
		}
	}
	await ctx.adapter.delete({ model: "oauthClient", where: [{ field: "id", value: client.id }] });
	log.info("deregistered an operator-registered OAuth client", { clientId });
	return asManagedClient(client);
}
