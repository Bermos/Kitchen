import { describe, expect, it } from "vitest";
import { challengeFor, claimsOf, randomVerifier } from "./pkce";

describe("pkce", () => {
  it("verifiers are base64url and long enough for RFC 7636", () => {
    const verifier = randomVerifier();
    expect(verifier).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(randomVerifier()).not.toEqual(verifier);
  });

  it("challenges are the S256 of the verifier", async () => {
    // RFC 7636 appendix B's worked example.
    const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk";
    expect(await challengeFor(verifier)).toBe("E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM");
  });

  it("claims of a JWT decode without verifying", () => {
    const payload = Buffer.from(JSON.stringify({ sub: "abc", exp: 123 })).toString("base64url");
    expect(claimsOf(`header.${payload}.signature`)).toEqual({ sub: "abc", exp: 123 });
  });

  it("claims of garbage are empty rather than a crash", () => {
    expect(claimsOf("not-a-token")).toEqual({});
    expect(claimsOf("")).toEqual({});
  });
});
