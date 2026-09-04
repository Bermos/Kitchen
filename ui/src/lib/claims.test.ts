import { describe, expect, it } from "vitest";
import { deletionGatedByName, destroysData, destroysDataRefusal, mayDestroyData } from "./claims";
import type { Caller } from "./policy";

// The one escalation on the claims surface (#320): `deletionPolicy: Delete`
// is the admin's at both ends, and the dashboard has to know it as well as
// the API does — a Delete option offered to somebody the API will refuse is
// the lie `policy.ts` exists to avoid.

const developer: Caller = { platform: "member", project: "developer", projectName: "shop" };
const admin: Caller = { platform: "member", project: "admin", projectName: "shop" };
const viewer: Caller = { platform: "member", project: "viewer", projectName: "shop" };
const operator: Caller = { platform: "operator", projectName: "shop" };
const stranger: Caller = { platform: "member", projectName: "shop" };

describe("what a claim's policy does to its data", () => {
  it("only Delete destroys it", () => {
    expect(destroysData({ deletionPolicy: "Delete" })).toBe(true);
    expect(destroysData({ deletionPolicy: "Retain" })).toBe(false);
    // No policy at all is the CRD's default, which is Retain.
    expect(destroysData({})).toBe(false);
  });
});

describe("who may destroy it", () => {
  it("is the admin, and the operator who holds admin everywhere", () => {
    expect(mayDestroyData(admin)).toBe(true);
    expect(mayDestroyData(operator)).toBe(true);
  });

  it("is not the developer whose day job the rest of the claim is", () => {
    expect(mayDestroyData(developer)).toBe(false);
    expect(mayDestroyData(viewer)).toBe(false);
    expect(mayDestroyData(stranger)).toBe(false);
  });

  it("says why in the words the API refuses in, naming the field and the role", () => {
    const refusal = destroysDataRefusal(developer, "asking for a claim that destroys its database");
    expect(refusal).toContain("you have developer on shop");
    expect(refusal).toContain("needs admin");
    expect(refusal).toContain("deletionPolicy Delete");
  });

  it("says nothing to somebody who may", () => {
    expect(destroysDataRefusal(admin, "asking for a claim that destroys its database")).toBeUndefined();
  });

  it("names no role for somebody who holds none", () => {
    expect(destroysDataRefusal(stranger, "deleting a claim that destroys its bucket")).toContain("no role on shop");
  });
});

describe("the delete confirmation", () => {
  it("is gated on typing the name exactly when the data goes with the claim", () => {
    expect(deletionGatedByName({ deletionPolicy: "Delete" })).toBe(true);
    expect(deletionGatedByName({ deletionPolicy: "Retain" })).toBe(false);
    expect(deletionGatedByName(null)).toBe(false);
  });
});
