import { describe, expect, it } from "vitest";
import { PLATFORM_ROLES, POLICY, PROJECT_ROLES, REQUIREMENT_KINDS } from "./policy.generated";
import type { Requirement, Route } from "./policy.generated";
import {
  effectiveProjectRole,
  may,
  narrowsAnswer,
  platformAtLeast,
  projectAtLeast,
  refusal,
  requirementFor,
} from "./policy";

const rows = Object.entries(POLICY) as [Route, Requirement][];

// The generated half. These are invariants of the API's table rather than a
// second copy of it: a row's role may move, and this still holds. What it
// catches is a generator that emitted something incomplete — a row with no
// kind, a project requirement with no role, a refusal with no words.
describe("the generated table", () => {
  it("carries every route the API serves", () => {
    // The catch-all is a row like any other, and it is the last one: anything
    // else under the API prefix is a 404 rather than a fall-through.
    expect(rows.length).toBeGreaterThan(1);
    expect(POLICY["/"]).toEqual({ kind: "authenticated" });
  });

  it("gives every row a kind the module knows", () => {
    for (const [route, requirement] of rows) {
      expect(REQUIREMENT_KINDS, route).toContain(requirement.kind);
    }
  });

  it("gives every project requirement a role and every refusal its words", () => {
    for (const [route, requirement] of rows) {
      if (requirement.kind === "projectRole") {
        expect(PROJECT_ROLES, route).toContain(requirement.role);
        expect(requirement.doing, route).toBeTruthy();
      }
      if (requirement.kind === "operator") {
        expect(requirement.role, route).toBeUndefined();
        expect(requirement.doing, route).toBeTruthy();
      }
    }
  });

  it("leaves the kinds that never refuse without a role", () => {
    for (const [route, requirement] of rows) {
      if (requirement.kind === "projectRole") continue;
      expect(requirement.role, route).toBeUndefined();
    }
  });

  it("orders the roles the way the API compares them", () => {
    expect(PLATFORM_ROLES).toEqual(["member", "operator"]);
    expect(PROJECT_ROLES).toEqual(["viewer", "developer", "admin"]);
  });

  // The one row pinned by name: it is the example the model is written around
  // — moving an environment to another release is the whole of promotion and
  // rollback — so if the generator ever stopped carrying the role or the
  // words, this is where it should be noticed.
  it("carries the role and the words of the row it exists for", () => {
    expect(requirementFor("PATCH /api/v1/environments/{name}")).toEqual({
      kind: "projectRole",
      role: "developer",
      doing: "redeploying",
    });
  });
});

describe("comparing roles", () => {
  it("reads the ordering, not the spelling", () => {
    expect(platformAtLeast("operator", "member")).toBe(true);
    expect(platformAtLeast("member", "operator")).toBe(false);
    expect(projectAtLeast("admin", "developer")).toBe(true);
    expect(projectAtLeast("developer", "developer")).toBe(true);
    expect(projectAtLeast("viewer", "developer")).toBe(false);
  });

  it("satisfies nothing for a role it has never heard of", () => {
    // The zero value's rule from internal/access: an absent grant, an
    // unparsed string and a caller nobody resolved all sit below every real
    // role rather than at the bottom of the ordering.
    expect(projectAtLeast(undefined, "viewer")).toBe(false);
    expect(projectAtLeast("", "viewer")).toBe(false);
    expect(projectAtLeast("owner", "viewer")).toBe(false);
    expect(platformAtLeast(undefined, "member")).toBe(false);
  });

  it("gives an operator admin on every project", () => {
    expect(effectiveProjectRole({ platform: "operator" })).toBe("admin");
    expect(effectiveProjectRole({ platform: "member", project: "viewer" })).toBe("viewer");
    expect(effectiveProjectRole({})).toBeUndefined();
  });
});

describe("what a caller may do", () => {
  it("admits any valid token where the token is the whole requirement", () => {
    expect(may("POST /api/v1/projects")).toBe(true);
    expect(may("GET /api/v1/me")).toBe(true);
    expect(may("GET /api/v1/projects", { platform: "member" })).toBe(true);
    expect(may("GET /api/v1/status", { platform: "member" })).toBe(true);
  });

  it("keeps the platform's own surface to operators", () => {
    expect(may("GET /api/v1/settings", { platform: "operator" })).toBe(true);
    expect(may("GET /api/v1/settings", { platform: "member" })).toBe(false);
    expect(may("PATCH /api/v1/settings", {})).toBe(false);
    // A developer holding admin on a project is still not an operator.
    expect(may("GET /api/v1/environments/{name}/objects", { platform: "member", project: "admin" })).toBe(false);
  });

  it("weighs a project role against the role the row wants", () => {
    const redeploy: Route = "PATCH /api/v1/environments/{name}";
    expect(may(redeploy, { platform: "member", project: "developer" })).toBe(true);
    expect(may(redeploy, { platform: "member", project: "admin" })).toBe(true);
    expect(may(redeploy, { platform: "member", project: "viewer" })).toBe(false);
    expect(may(redeploy, { platform: "member" })).toBe(false);
    // The operator's hat reaches every project, including one they hold no
    // grant on.
    expect(may(redeploy, { platform: "operator" })).toBe(true);
  });

  it("lets a viewer read and not write", () => {
    const viewer = { platform: "member", project: "viewer" };
    expect(may("GET /api/v1/projects/{name}", viewer)).toBe(true);
    expect(may("DELETE /api/v1/projects/{name}", viewer)).toBe(false);
    expect(may("POST /api/v1/projects/{name}/builds", viewer)).toBe(false);
  });

  it("marks the routes that answer everybody with less rather than refusing", () => {
    expect(narrowsAnswer("GET /api/v1/logs")).toBe(true);
    expect(narrowsAnswer("GET /api/v1/status")).toBe(true);
    expect(narrowsAnswer("GET /api/v1/settings")).toBe(false);
    expect(narrowsAnswer("PATCH /api/v1/environments/{name}")).toBe(false);
  });
});

describe("saying why not", () => {
  it("says nothing at all when the caller may", () => {
    expect(refusal("PATCH /api/v1/settings", { platform: "operator" })).toBeUndefined();
    expect(refusal("GET /api/v1/me")).toBeUndefined();
  });

  it("uses the table's own words for the platform's surface", () => {
    expect(refusal("PATCH /api/v1/settings", { platform: "member" })).toBe(
      "changing the platform's settings needs the operator role; you are a member",
    );
    // Nobody resolved: the sentence still names the operation rather than
    // guessing at a role the caller does not hold.
    expect(refusal("PATCH /api/v1/settings")).toBe("changing the platform's settings needs the operator role");
  });

  it("names the role held and the role wanted on a project", () => {
    expect(
      refusal("PATCH /api/v1/environments/{name}", { platform: "member", project: "viewer", projectName: "shop" }),
    ).toBe("you have viewer on shop; redeploying needs developer");
    expect(refusal("PATCH /api/v1/environments/{name}", { platform: "member" })).toBe(
      "redeploying needs developer; you have no role",
    );
  });
});
