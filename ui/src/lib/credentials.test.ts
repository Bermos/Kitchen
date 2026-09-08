import { describe, expect, it } from "vitest";
import {
  credentialNameProblem,
  holdsNothing,
  LIFETIME_OPTIONS,
  operationsOf,
  scopeReach,
  scopeSummary,
} from "./credentials";
import { PLATFORM_SCOPES, POLICY, type Route } from "./policy.generated";

// The Credentials screen's own reasoning, which is mostly one claim: what a
// scope reaches is read out of the API's table rather than written down twice.

describe("what a scope reaches", () => {
  it("offers every scope the API knows, and only those", () => {
    expect(scopeReach().map((entry) => entry.scope)).toEqual([...PLATFORM_SCOPES]);
  });

  it("names each scope's operations in the API's own words", () => {
    for (const entry of scopeReach()) {
      expect(entry.operations.length).toBeGreaterThan(0);
      // Every operation is some row's `doing`, which is what a refusal is
      // built from — so the screen and the 403 say the same thing.
      for (const operation of entry.operations) {
        const named = (Object.keys(POLICY) as Route[]).some(
          (route) => POLICY[route].scope === entry.scope && POLICY[route].doing === operation,
        );
        expect(named, `${operation} is not any row's words`).toBe(true);
      }
    }
  });

  it("reaches nothing for a word that is not a scope", () => {
    expect(operationsOf("platform.write" as never)).toEqual([]);
    expect(scopeSummary("platform.write")).toBe("not a scope this platform knows");
  });

  it("keeps the operator's routes out of every scope", () => {
    // The property the whole design rests on, asserted against the generated
    // table: a route that names no scope is unreachable by any credential.
    const scoped = (Object.keys(POLICY) as Route[]).filter((route) => POLICY[route].scope);
    for (const route of scoped) {
      expect(route).not.toContain("/platform/credentials");
      expect(route).not.toContain("/settings");
      expect(route).not.toContain("/updates");
    }
  });
});

describe("issuing one", () => {
  it("refuses a name the API would refuse", () => {
    expect(credentialNameProblem("nightly")).toBe("");
    expect(credentialNameProblem("backup-agent")).toBe("");
    expect(credentialNameProblem("")).toContain("needs a name");
    expect(credentialNameProblem("Nightly")).toContain("lowercase");
    expect(credentialNameProblem("-leading")).toContain("lowercase");
    expect(credentialNameProblem("a".repeat(33))).toContain("at most 32");
  });

  it("never offers a lifetime the API caps out", () => {
    for (const option of LIFETIME_OPTIONS) {
      expect(option.value).toBeGreaterThan(0);
      expect(option.value).toBeLessThanOrEqual(90);
    }
  });
});

describe("a credential that holds nothing", () => {
  it("is one that has lapsed, or one whose grant was removed", () => {
    expect(holdsNothing({ expired: false, scopes: ["platform.read"] })).toBe(false);
    expect(holdsNothing({ expired: true, scopes: ["platform.read"] })).toBe(true);
    expect(holdsNothing({ expired: false, scopes: [] })).toBe(true);
  });
});
