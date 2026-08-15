import { describe, expect, it } from "vitest";
import { RENEWAL_MARGIN_MS, expiresAt, isExpired, isFresh, readSession, renewalDelay } from "./session";

/** A JWT-shaped string with the given claims; nothing here verifies one. */
function tokenWith(claims: Record<string, unknown>): string {
  const payload = btoa(JSON.stringify(claims))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");
  return `header.${payload}.signature`;
}

const NOW = 1_700_000_000_000;
const expiringIn = (ms: number) => tokenWith({ exp: Math.floor((NOW + ms) / 1000) });

describe("the stored session", () => {
  it("reads back what was written", () => {
    const stored = JSON.stringify({ accessToken: "a", refreshToken: "r" });
    expect(readSession(stored)).toEqual({ accessToken: "a", refreshToken: "r" });
  });

  it("is nothing when it is absent, unreadable, or has no access token", () => {
    expect(readSession(null)).toBeNull();
    expect(readSession("")).toBeNull();
    expect(readSession("{not json")).toBeNull();
    expect(readSession(JSON.stringify({ refreshToken: "r" }))).toBeNull();
    expect(readSession(JSON.stringify({ accessToken: "" }))).toBeNull();
  });

  it("has no refresh token when the sign-in did not get one", () => {
    expect(readSession(JSON.stringify({ accessToken: "a" }))?.refreshToken).toBeUndefined();
    // A non-string is the same as none: it is never going to a token endpoint.
    expect(readSession(JSON.stringify({ accessToken: "a", refreshToken: 7 }))?.refreshToken).toBeUndefined();
  });
});

describe("when a token runs out", () => {
  it("reads the deadline off `exp`, in milliseconds", () => {
    expect(expiresAt(tokenWith({ exp: 1_700_000_000 }))).toBe(1_700_000_000_000);
  });

  it("has no deadline for a token that carries none", () => {
    expect(expiresAt(tokenWith({ sub: "abc" }))).toBeNull();
    expect(expiresAt("not-a-token")).toBeNull();
  });

  it("counts a token as spent only once `exp` has passed", () => {
    expect(isExpired(expiringIn(1_000), NOW)).toBe(false);
    expect(isExpired(expiringIn(-1_000), NOW)).toBe(true);
    // Nothing the dashboard can see says this one is over; the API decides.
    expect(isExpired(tokenWith({ sub: "abc" }), NOW)).toBe(false);
  });

  it("stops calling a token fresh a margin before it expires", () => {
    expect(isFresh(expiringIn(RENEWAL_MARGIN_MS * 2), NOW)).toBe(true);
    expect(isFresh(expiringIn(RENEWAL_MARGIN_MS), NOW)).toBe(false);
    expect(isFresh(expiringIn(-1), NOW)).toBe(false);
  });
});

describe("scheduling the renewal", () => {
  it("wakes up a margin before the token expires", () => {
    expect(renewalDelay(expiringIn(10 * 60_000), NOW)).toBe(10 * 60_000 - RENEWAL_MARGIN_MS);
  });

  it("renews at once when the deadline has already gone by", () => {
    // What a tab that was asleep across the whole token lifetime comes back to.
    expect(renewalDelay(expiringIn(-60 * 60_000), NOW)).toBe(0);
  });

  it("has nothing to schedule against without an `exp`", () => {
    expect(renewalDelay(tokenWith({ sub: "abc" }), NOW)).toBeNull();
  });

  it("renews halfway through a token too short-lived for the full margin", () => {
    // An issuer minting 30-second access tokens would otherwise be asked for a
    // new one the instant it issued the last, forever.
    const issued = Math.floor(NOW / 1000);
    const brief = tokenWith({ iat: issued, exp: issued + 30 });
    expect(renewalDelay(brief, NOW)).toBe(15_000);
    expect(isFresh(brief, NOW)).toBe(true);
    expect(isFresh(brief, NOW + 16_000)).toBe(false);
  });

  it("never asks setTimeout for a delay it would round down to zero", () => {
    // Delays above 2^31-1 ms fire immediately, which would be a renewal loop.
    const delay = renewalDelay(expiringIn(400 * 24 * 60 * 60_000), NOW);
    expect(delay).toBeLessThanOrEqual(2 ** 31 - 1);
    expect(delay).toBeGreaterThan(0);
  });
});
