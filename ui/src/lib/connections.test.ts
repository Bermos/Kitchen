import { describe, expect, it } from "vitest";
import type { Connection } from "./api";
import { connectionChoices, connectionProvides, connectionReady, noteFor, selectableChoices } from "./connections";

// The picker reads two shapes: the operator's connection, and the thinned one
// everybody else is answered with. These are about the seam between them —
// the same connection has to read the same way whichever arrived.

/** As `GET /connections` answers an operator: provider, conditions, the lot. */
const operatorShape = (over: Partial<Connection> = {}): Connection => ({
  name: "ghcr",
  provider: "dockerRegistry",
  capabilities: ["imageStore"],
  createdAt: "2026-01-01T00:00:00Z",
  conditions: [{ type: "CredentialsValid", status: "True", lastTransitionTime: "2026-01-01T00:00:00Z" }],
  ...over,
});

/** As it answers everybody else: a name, what it can back, and whether the
 * platform has it working. */
const pickerShape = (over: Partial<Connection> = {}): Connection => ({
  name: "ghcr",
  capabilities: ["imageStore"],
  ready: true,
  ...over,
});

describe("whether the platform has a connection working", () => {
  it("takes the picker shape's own answer", () => {
    expect(connectionReady(pickerShape({ ready: true }))).toBe(true);
    expect(connectionReady(pickerShape({ ready: false }))).toBe(false);
  });

  it("reads the operator shape's off CredentialsValid", () => {
    expect(connectionReady(operatorShape())).toBe(true);
    expect(
      connectionReady(
        operatorShape({
          conditions: [{ type: "CredentialsValid", status: "False", lastTransitionTime: "2026-01-01T00:00:00Z" }],
        }),
      ),
    ).toBe(false);
  });

  it("says nothing when neither is there", () => {
    // Not the same answer as "no": a connection nothing has assessed is not a
    // connection that failed.
    expect(connectionReady({ name: "seeded" })).toBeUndefined();
    expect(connectionReady(operatorShape({ conditions: [] }))).toBeUndefined();
  });
});

describe("whether a connection can back a capability", () => {
  it("takes the reported capabilities at their word", () => {
    expect(connectionProvides(pickerShape({ capabilities: ["imageStore"] }), "imageStore")).toBe(true);
    expect(connectionProvides(pickerShape({ capabilities: ["gitSource"] }), "imageStore")).toBe(false);
  });

  it("admits one that has reported none", () => {
    // The API's own rule: requireConnection lets an unassessed connection
    // through, so the picker must not be stricter than the write it feeds.
    expect(connectionProvides(pickerShape({ capabilities: [] }), "imageStore")).toBe(true);
    expect(connectionProvides({ name: "fresh" }, "imageStore")).toBe(true);
  });
});

describe("the picker's entries", () => {
  it("names the provider when it is there and only the name when it is not", () => {
    expect(connectionChoices([operatorShape()], "imageStore")[0]!.label).toBe("ghcr · dockerRegistry");
    expect(connectionChoices([pickerShape()], "imageStore")[0]!.label).toBe("ghcr");
  });

  it("disables the wrong capability and says which one it wanted", () => {
    const [choice] = connectionChoices([pickerShape({ name: "github", capabilities: ["gitSource"] })], "imageStore");
    expect(choice!.disabled).toBe(true);
    expect(choice!.note).toContain("imageStore");
    expect(choice!.label).toContain("imageStore");
  });

  it("keeps an unassessed connection selectable, with the caveat", () => {
    // A fresh install's seeded registry connection is exactly this between
    // being created and being validated, and a project has to be creatable.
    const [choice] = connectionChoices([pickerShape({ capabilities: [] })], "imageStore");
    expect(choice!.disabled).toBeUndefined();
    expect(choice!.note).toBeTruthy();
  });

  it("keeps one the platform cannot get working selectable, and says whose problem it is", () => {
    const [choice] = connectionChoices([pickerShape({ ready: false })], "imageStore");
    expect(choice!.disabled).toBeUndefined();
    expect(choice!.note).toContain("operator");
  });

  it("says nothing about one that simply works", () => {
    expect(connectionChoices([pickerShape()], "imageStore")[0]!.note).toBe("");
  });

  it("lists every connection rather than dropping the ones that do not fit", () => {
    const choices = connectionChoices(
      [pickerShape({ name: "github", capabilities: ["gitSource"] }), pickerShape({ name: "ghcr" })],
      "imageStore",
    );
    expect(choices.map((c) => c.value)).toEqual(["github", "ghcr"]);
    expect(selectableChoices(choices).map((c) => c.value)).toEqual(["ghcr"]);
  });
});

describe("the caveat under the field", () => {
  it("is the chosen entry's, and empty when nothing is chosen", () => {
    const choices = connectionChoices([pickerShape({ ready: false }), pickerShape({ name: "other" })], "imageStore");
    expect(noteFor(choices, "ghcr")).toContain("operator");
    expect(noteFor(choices, "other")).toBe("");
    expect(noteFor(choices, undefined)).toBe("");
    expect(noteFor(choices, "gone")).toBe("");
  });
});
