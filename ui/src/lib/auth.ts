import { ref, computed } from "vue";
import { loadConfig } from "./config";
import { challengeFor, claimsOf, randomVerifier } from "./pkce";

// OIDC Authorization Code + PKCE against the platform's identity provider.
// The access token is requested *for the API* (RFC 8707 `resource`) — without
// that the provider issues an opaque token the operator cannot validate.

interface Discovery {
  authorization_endpoint: string;
  token_endpoint: string;
  end_session_endpoint?: string;
}

const STORAGE_TOKEN = "kitchen.token";
const STORAGE_FLOW = "kitchen.authflow";

const accessToken = ref<string | null>(sessionStorage.getItem(STORAGE_TOKEN));

export const isAuthenticated = computed(() => {
  const token = accessToken.value;
  if (!token) return false;
  const exp = claimsOf(token).exp;
  return typeof exp !== "number" || exp * 1000 > Date.now();
});

export const user = computed(() => {
  if (!accessToken.value) return null;
  const claims = claimsOf(accessToken.value);
  return {
    name: (claims.name as string) || (claims.email as string) || (claims.sub as string) || "",
    email: (claims.email as string) || "",
  };
});

export function token(): string | null {
  return isAuthenticated.value ? accessToken.value : null;
}

async function discover(issuer: string): Promise<Discovery> {
  const res = await fetch(`${issuer.replace(/\/$/, "")}/.well-known/openid-configuration`);
  if (!res.ok) throw new Error(`the identity provider at ${issuer} did not answer discovery (${res.status})`);
  return res.json();
}

function callbackURL(): string {
  return `${window.location.origin}/auth/callback`;
}

/** Send the browser to the identity provider. `returnTo` is the in-app path
 * to come back to once the round trip is done. */
export async function signIn(returnTo: string): Promise<void> {
  const config = await loadConfig();
  if (!config.issuer) {
    throw new Error(
      "no identity provider is configured — the operator serves /config.json with the issuer, or set VITE_ISSUER for development",
    );
  }
  const discovery = await discover(config.issuer);
  const verifier = randomVerifier();
  const state = randomVerifier();
  sessionStorage.setItem(STORAGE_FLOW, JSON.stringify({ verifier, state, returnTo }));

  const url = new URL(discovery.authorization_endpoint);
  url.searchParams.set("response_type", "code");
  url.searchParams.set("client_id", config.clientId);
  url.searchParams.set("redirect_uri", callbackURL());
  url.searchParams.set("scope", "openid profile email");
  url.searchParams.set("state", state);
  url.searchParams.set("code_challenge", await challengeFor(verifier));
  url.searchParams.set("code_challenge_method", "S256");
  url.searchParams.set("resource", config.apiURL);
  window.location.assign(url.toString());
}

/** Finish the round trip: exchange the code for a token. Returns the in-app
 * path the flow started from. */
export async function completeSignIn(query: URLSearchParams): Promise<string> {
  const raw = sessionStorage.getItem(STORAGE_FLOW);
  sessionStorage.removeItem(STORAGE_FLOW);
  if (!raw) throw new Error("no sign-in in progress — start again from the login page");
  const flow = JSON.parse(raw) as { verifier: string; state: string; returnTo: string };

  if (query.get("error")) {
    throw new Error(query.get("error_description") || query.get("error") || "the identity provider refused the sign-in");
  }
  if (query.get("state") !== flow.state) {
    throw new Error("the sign-in answer does not match the sign-in that was started");
  }
  const code = query.get("code");
  if (!code) throw new Error("the identity provider answered without a code");

  const config = await loadConfig();
  const discovery = await discover(config.issuer);
  const body = new URLSearchParams({
    grant_type: "authorization_code",
    client_id: config.clientId,
    code,
    code_verifier: flow.verifier,
    redirect_uri: callbackURL(),
    // What makes the access token a JWT with the API as its audience.
    resource: config.apiURL,
  });
  const res = await fetch(discovery.token_endpoint, {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body,
  });
  if (!res.ok) {
    let detail = `${res.status}`;
    try {
      const err = await res.json();
      detail = err.error_description || err.error || detail;
    } catch {
      // keep the status
    }
    throw new Error(`the code exchange failed: ${detail}`);
  }
  const tokens = await res.json();
  accessToken.value = tokens.access_token;
  sessionStorage.setItem(STORAGE_TOKEN, tokens.access_token);
  return flow.returnTo || "/";
}

export function signOut(): void {
  accessToken.value = null;
  sessionStorage.removeItem(STORAGE_TOKEN);
}
