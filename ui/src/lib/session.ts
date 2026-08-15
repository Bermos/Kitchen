import { claimsOf } from "./pkce";

// What the dashboard holds between requests, and the arithmetic around it.
// Deliberately pure: the storage, the timers and the network live in auth.ts,
// and this is the part worth testing without a browser.

export interface Session {
  /** The JWT the operator API is called with. */
  accessToken: string;
  /** Present when the sign-in asked for `offline_access`; single-use, because
   * the provider rotates it on every renewal. */
  refreshToken?: string;
}

/** Renew this long before the access token expires, so a request that starts
 * just before the deadline still arrives with a token the API accepts. */
export const RENEWAL_MARGIN_MS = 60_000;

/** setTimeout counts in a signed 32-bit integer and fires at once for
 * anything larger, which would turn a long-lived token into a renewal loop. */
const MAX_DELAY_MS = 2 ** 31 - 1;

/** Reads what was stored, treating anything unreadable as no session at all —
 * a half-parsed token is not something to send to the API. */
export function readSession(raw: string | null): Session | null {
  if (!raw) return null;
  try {
    const parsed = JSON.parse(raw) as Partial<Session>;
    if (typeof parsed?.accessToken !== "string" || !parsed.accessToken) return null;
    return {
      accessToken: parsed.accessToken,
      refreshToken: typeof parsed.refreshToken === "string" && parsed.refreshToken ? parsed.refreshToken : undefined,
    };
  } catch {
    return null;
  }
}

/**
 * When the token stops being accepted and how long before that to replace it,
 * both in milliseconds. Null for a token that names no deadline: the API is
 * the one that decides, and it says so with a 401.
 */
function deadline(token: string): { expiresAt: number; margin: number } | null {
  const { exp, iat } = claimsOf(token);
  if (typeof exp !== "number") return null;
  // A minute of head start, unless the token's whole life is shorter than
  // that. An issuer configured with very short access tokens would otherwise
  // be asked for a new one the moment it issued the last, over and over.
  const margin = typeof iat === "number" ? Math.min(RENEWAL_MARGIN_MS, ((exp - iat) * 1000) / 2) : RENEWAL_MARGIN_MS;
  return { expiresAt: exp * 1000, margin: Math.max(margin, 0) };
}

/** When the token stops being accepted, in epoch milliseconds. */
export function expiresAt(token: string): number | null {
  return deadline(token)?.expiresAt ?? null;
}

/** Whether the token is spent. What decides between "signed in" and "back to
 * the login page" when there is no refresh token to fall back on. */
export function isExpired(token: string, now = Date.now()): boolean {
  const at = deadline(token);
  return at !== null && at.expiresAt <= now;
}

/** Whether the token is worth sending: valid, and not so close to expiry that
 * it would be renewed a moment later anyway. */
export function isFresh(token: string, now = Date.now()): boolean {
  const at = deadline(token);
  return at === null || at.expiresAt - now > at.margin;
}

/** How long to wait before renewing in the background; null when the token
 * carries no expiry to schedule against. */
export function renewalDelay(token: string, now = Date.now()): number | null {
  const at = deadline(token);
  if (at === null) return null;
  return Math.min(Math.max(at.expiresAt - now - at.margin, 0), MAX_DELAY_MS);
}
