import { describe, expect, it } from "vitest";
import { modeFor } from "./mode";

// Mode is derived, not stored, and these are the rules it is derived by. The
// third describes an installation that already exists — every browser that
// ever flipped the old free switch has "operator" under `kitchen.mode`, and
// the account behind it may now be a member.

describe("the mode a role and a preference add up to", () => {
  it("puts an operator with no preference in operator mode", () => {
    expect(modeFor(null, "operator")).toBe("operator");
  });

  it("keeps an operator who picked the developer's view in it", () => {
    expect(modeFor("developer", "operator")).toBe("developer");
    expect(modeFor("operator", "operator")).toBe("operator");
  });

  it("ignores a stored operator preference for a member", () => {
    // The case this function exists for: before the split the switch was free
    // and flipping it was the only way to see a conditions table, so a member
    // upgrading into enforcement arrives with exactly this stored.
    expect(modeFor("operator", "member")).toBe("developer");
  });

  it("puts a member in developer mode whatever is stored", () => {
    for (const stored of [null, "", "developer", "operator", "nonsense"]) {
      expect(modeFor(stored, "member"), String(stored)).toBe("developer");
    }
  });

  it("treats an unknown or missing role as no role at all", () => {
    // The same direction policy.ts takes: a role this build has never heard
    // of, or a /me that has not answered, satisfies nothing.
    expect(modeFor("operator", undefined)).toBe("developer");
    expect(modeFor("operator", "superuser")).toBe("developer");
    expect(modeFor("operator", "")).toBe("developer");
  });

  it("reads an unrecognised preference as the role's default", () => {
    // Only "developer" turns an operator's mode off. Anything else — a value
    // from a future version, or one somebody typed into devtools — leaves
    // them where their role puts them.
    expect(modeFor("nonsense", "operator")).toBe("operator");
    expect(modeFor("", "operator")).toBe("operator");
  });
});
