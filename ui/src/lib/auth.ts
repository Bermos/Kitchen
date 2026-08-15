import { ref, computed } from "vue";
import { loadConfig } from "./config";
import { challengeFor, claimsOf, randomVerifier } from "./pkce";
import { isExpired, isFresh, readSession, renewalDelay, type Session } from "./session";

// OIDC Authorization Code + PKCE against the platform's identity provider.
// The access token is requested *for the API* (RFC 8707 `resource`) — without
// that the provider issues an opaque token the operator cannot validate.
//
// The sign-in also asks for `offline_access`, so the provider hands back a
// refresh token and the dashboard renews in the background instead of bouncing
// through the identity provider once per access-token lifetime. That round
// trip needs no interaction while the provider still has a session, but it
// reloads the SPA and loses whatever the page was in the middle of.
//
// The provider rotates refresh tokens and treats a replayed one as a
// compromised family (RFC 9700 §4.14), tearing down every token it issued for
// the account. A refresh token is therefore strictly single-use, which is why
// only one renewal may ever be in flight — across tabs, not just this one.

interface Discovery {
  authorization_endpoint: string;
  token_endpoint: string;
  revocation_endpoint?: string;
  end_session_endpoint?: string;
}

// The session lives in localStorage, so it is shared by every tab of the same
// browser: a second tab is not a second trip through the identity provider,
// and signing out of one signs out of all of them. The deliberate cost is that
// a refresh token now outlives the tab that obtained it — bounded by rotation,
// by revoking it on sign-out, and by the provider's refresh-token lifetime.
//
// The in-flight sign-in stays in sessionStorage, where it belongs: one redirect
// round trip, owned by the tab that started it, and meaningless to any other.
const STORAGE_SESSION = "kitchen.session";
const STORAGE_FLOW = "kitchen.authflow";
/** Name of the cross-tab lock renewal is serialised on. */
const RENEWAL_LOCK = "kitchen.renewal";

const session = ref<Session | null>(readSession(localStorage.getItem(STORAGE_SESSION)));

let renewalTimer: ReturnType<typeof setTimeout> | undefined;
let renewing: Promise<string | null> | null = null;

export const isAuthenticated = computed(() => {
  const current = session.value;
  if (!current) return false;
  // An expired access token is not a signed-out browser while a refresh token
  // can still be traded for a new one: the route renders and the first request
  // renews, rather than the router sending everyone to the login page.
  return Boolean(current.refreshToken) || !isExpired(current.accessToken);
});

export const user = computed(() => {
  const current = session.value;
  if (!current) return null;
  const claims = claimsOf(current.accessToken);
  return {
    name: (claims.name as string) || (claims.email as string) || (claims.sub as string) || "",
    email: (claims.email as string) || "",
  };
});

/** Hold a session and make every tab agree about it. */
function remember(next: Session | null): void {
  session.value = next;
  if (next) localStorage.setItem(STORAGE_SESSION, JSON.stringify(next));
  else localStorage.removeItem(STORAGE_SESSION);
  schedule();
}

/** Renew a little before the access token runs out, so nothing ever waits on
 * a token exchange it could have made earlier. */
function schedule(): void {
  clearTimeout(renewalTimer);
  renewalTimer = undefined;
  const current = session.value;
  if (!current?.refreshToken) return;
  const delay = renewalDelay(current.accessToken);
  if (delay === null) return;
  renewalTimer = setTimeout(() => {
    // A renewal the network refused is not fatal here: the next request
    // renews on demand, and a 401 is still the backstop.
    void renew().catch(() => undefined);
  }, delay);
}

let discovered: Promise<Discovery> | null = null;

/** The issuer's endpoints, fetched once: renewal and revocation ask for them
 * too, and they do not move while the tab is open. */
function discover(issuer: string): Promise<Discovery> {
  discovered ??= (async () => {
    const res = await fetch(`${issuer.replace(/\/$/, "")}/.well-known/openid-configuration`);
    if (!res.ok) throw new Error(`the identity provider at ${issuer} did not answer discovery (${res.status})`);
    return (await res.json()) as Discovery;
  })().catch((error: unknown) => {
    discovered = null;
    throw error;
  });
  return discovered;
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
  // `offline_access` is what asks for the refresh token; without it the
  // dashboard is back to one redirect per access-token lifetime.
  url.searchParams.set("scope", "openid profile email offline_access");
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
  const tokens = (await res.json()) as { access_token: string; refresh_token?: string };
  remember({ accessToken: tokens.access_token, refreshToken: tokens.refresh_token });
  return flow.returnTo || "/";
}

/** A bearer token for the API, renewing first when the one held is spent. */
export async function token(): Promise<string | null> {
  const current = session.value;
  if (current && isFresh(current.accessToken)) return current.accessToken;
  return renew();
}

/**
 * Trade the refresh token for a fresh access token, returning null when the
 * session is over rather than throwing — a caller that gets nothing back is
 * already signed out.
 *
 * Single-flight in this tab and, where the browser has the Web Locks API,
 * across all of them: rotation makes a refresh token single-use, and two tabs
 * racing would replay one and cost the account every token it holds. A network
 * failure still propagates, because that is not the session ending.
 */
export function renew(): Promise<string | null> {
  renewing ??= withLock(RENEWAL_LOCK, exchangeRefreshToken).finally(() => {
    renewing = null;
  });
  return renewing;
}

function withLock<T>(name: string, run: () => Promise<T>): Promise<T> {
  // Without Web Locks the renewal is only single-flight per tab, which is
  // where the dashboard stood before renewal existed at all.
  if (!navigator.locks) return run();
  return navigator.locks.request(name, run) as Promise<T>;
}

async function exchangeRefreshToken(): Promise<string | null> {
  // Another tab may have renewed while this one waited for the lock. Its
  // result is in storage, and the token it spent getting there is not one to
  // spend again.
  const stored = readSession(localStorage.getItem(STORAGE_SESSION));
  if (stored?.accessToken !== session.value?.accessToken) {
    session.value = stored;
    if (stored && isFresh(stored.accessToken)) {
      schedule();
      return stored.accessToken;
    }
  }
  if (!stored?.refreshToken) {
    forget();
    return null;
  }

  const config = await loadConfig();
  const discovery = await discover(config.issuer);
  const res = await fetch(discovery.token_endpoint, {
    method: "POST",
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({
      grant_type: "refresh_token",
      client_id: config.clientId,
      refresh_token: stored.refreshToken,
      // The same audience the sign-in asked for: a renewed token that did not
      // name the API would come back opaque, and the operator would refuse it.
      resource: config.apiURL,
    }),
  });
  if (!res.ok) {
    // A refresh token the provider will not take is a session that is over —
    // expired, revoked, or replayed somewhere. There is nothing left to
    // revoke, so this is not a round trip worth making.
    forget();
    return null;
  }
  const tokens = (await res.json()) as { access_token?: string; refresh_token?: string };
  if (!tokens.access_token) {
    forget();
    return null;
  }
  // Kitchen's provider rotates, so there is always a new refresh token here;
  // keeping the old one when an issuer sends none is what RFC 6749 §6 asks a
  // client to do.
  remember({ accessToken: tokens.access_token, refreshToken: tokens.refresh_token ?? stored.refreshToken });
  return tokens.access_token;
}

/** Drop the session here and in every other tab, without telling the issuer. */
function forget(): void {
  remember(null);
}

/** Drop the session everywhere, here and at the issuer. Resolves once the
 * issuer has been told, so a caller that is about to navigate away can wait
 * for it; nothing breaks if it does not. */
export async function signOut(): Promise<void> {
  const refreshToken = session.value?.refreshToken;
  forget();
  if (refreshToken) await revoke(refreshToken);
}

/**
 * Best-effort revocation: a refresh token that outlived the sign-out would
 * keep the session renewable for its whole lifetime, and it is sitting in
 * storage the sign-out just cleared. `keepalive` is what lets the request
 * survive a navigation that starts before it lands.
 */
async function revoke(refreshToken: string): Promise<void> {
  try {
    const config = await loadConfig();
    const discovery = await discover(config.issuer);
    if (!discovery.revocation_endpoint) return;
    await fetch(discovery.revocation_endpoint, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      keepalive: true,
      body: new URLSearchParams({
        client_id: config.clientId,
        token: refreshToken,
        token_type_hint: "refresh_token",
      }),
    });
  } catch {
    // Being signed out locally is what matters; the token expires on its own.
  }
}

// Another tab signed in, renewed, or signed out. A null key means the whole
// storage was cleared, which counts as all three.
window.addEventListener("storage", (event) => {
  if (event.key !== null && event.key !== STORAGE_SESSION) return;
  session.value = readSession(localStorage.getItem(STORAGE_SESSION));
  schedule();
});

// Timers are throttled in a background tab and stop altogether while the
// machine sleeps, so a scheduled renewal can be arbitrarily late. Coming back
// to the tab is the cue to catch up — `schedule()` fires at once when the
// deadline has already passed.
document.addEventListener("visibilitychange", () => {
  if (document.visibilityState === "visible") schedule();
});

schedule();
