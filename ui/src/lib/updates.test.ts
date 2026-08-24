import { describe, expect, it } from "vitest";
import type { ComponentStatus, PlatformUpdate } from "./api";
import { checklist, componentDetail, frozen, inFlight, moving, settled, stageOf, unreachable, versionLabel } from "./updates";

/** What the client throws when the API answered: an Error carrying the status.
 *  The class itself lives in the API client, which no test here imports. */
const answered = (status: number, message = `${status}`) => Object.assign(new Error(message), { status });

const update = (over: Partial<PlatformUpdate> = {}): PlatformUpdate => ({
  name: "update-0-13-1",
  version: "0.13.1",
  phase: "Running",
  fromVersion: "0.13.0",
  ...over,
});

const component = (over: Partial<ComponentStatus> = {}): ComponentStatus => ({
  name: "auth",
  kind: "Deployment",
  healthy: true,
  available: 1,
  desired: 1,
  ...over,
});

describe("what a failed read of /updates means", () => {
  it("reads a dead socket as the platform being absent", () => {
    // What fetch throws when there is nothing on the other end. There is no
    // response, so there is no status to judge it by.
    expect(unreachable(new TypeError("Failed to fetch"))).toBe(true);
  });

  it("reads the codes in front of a workload with no pods as absence", () => {
    for (const status of [0, 502, 503, 504, 522, 523]) {
      expect(unreachable(answered(status))).toBe(true);
    }
  });

  it("keeps an answered refusal an error", () => {
    // The API answered, and what it said is true whether or not an upgrade is
    // running. Swallowing these is how a screen goes quiet about a real fault.
    expect(unreachable(answered(403, "not an operator"))).toBe(false);
    expect(unreachable(answered(404, "no such update"))).toBe(false);
    expect(unreachable(answered(500, "the store refused the query"))).toBe(false);
  });
});

describe("where an upgrade is", () => {
  it("walks the sequence an operator actually sees", () => {
    expect(stageOf({ phase: "Pending", reachable: true, landed: false })).toBe("waiting");
    expect(stageOf({ phase: "Running", reachable: true, landed: false })).toBe("applying");
    // The manager is replaced: the API stops answering, and that is the
    // upgrade going normally rather than an error.
    expect(stageOf({ phase: "Running", reachable: false, landed: false })).toBe("restarting");
    // /config.json is public and comes back first — the new operator serving.
    expect(stageOf({ phase: "Running", reachable: false, landed: true })).toBe("landed");
    expect(stageOf({ phase: "Running", reachable: true, landed: true })).toBe("reconnected");
    expect(stageOf({ phase: "Succeeded", reachable: true, landed: true })).toBe("succeeded");
  });

  it("lets a terminal phase outrank everything", () => {
    // A real failure of the upgrade arrives as a phase once the API is back,
    // and it is the answer whatever the blackout looked like getting there.
    expect(stageOf({ phase: "Failed", reachable: true, landed: false })).toBe("failed");
    expect(stageOf({ phase: "Failed", reachable: false, landed: false })).toBe("failed");
  });

  it("says which stages are frozen and which are over", () => {
    expect(frozen("restarting")).toBe(true);
    expect(frozen("landed")).toBe(true);
    expect(frozen("applying")).toBe(false);
    expect(settled("succeeded")).toBe(true);
    expect(settled("failed")).toBe(true);
    expect(settled("restarting")).toBe(false);
  });
});

describe("the update in flight", () => {
  it("is the pending or running one", () => {
    const items = [update({ name: "b", phase: "Succeeded" }), update({ name: "a", phase: "Running" })];
    expect(inFlight(items)?.name).toBe("a");
    expect(inFlight([update({ phase: "Failed" })])).toBeUndefined();
    expect(inFlight(undefined)).toBeUndefined();
  });

  it("is moving until the record settles", () => {
    expect(moving(update({ phase: "Running" }))).toBe(true);
    expect(moving(update({ phase: "Pending" }))).toBe(true);
    expect(moving(update({ phase: "Succeeded" }))).toBe(false);
    expect(moving(null)).toBe(false);
  });
});

describe("the component checklist", () => {
  it("puts what the upgrade is still waiting for first", () => {
    const ordered = checklist([
      component({ name: "clickhouse" }),
      component({ name: "collector", healthy: false, available: 2, desired: 3 }),
      component({ name: "auth" }),
    ]);
    expect(ordered.map((c) => c.name)).toEqual(["collector", "auth", "clickhouse"]);
  });

  it("does not sort the survey it was handed", () => {
    const components = [component({ name: "z" }), component({ name: "a", healthy: false })];
    checklist(components);
    expect(components.map((c) => c.name)).toEqual(["z", "a"]);
  });

  it("says what a component is waiting on", () => {
    expect(componentDetail(component({ message: "pods refused at admission" }))).toBe("pods refused at admission");
    expect(componentDetail(component({ healthy: false, available: 0, desired: 1 }))).toBe("0 of 1 pod available");
    expect(componentDetail(component({ healthy: false, available: 2, desired: 3 }))).toBe("2 of 3 pods available");
  });
});

describe("version labels", () => {
  it("adds the v the API leaves off, and leaves dev alone", () => {
    expect(versionLabel("0.13.1")).toBe("v0.13.1");
    expect(versionLabel("dev")).toBe("dev");
    expect(versionLabel(undefined)).toBe("—");
  });
});
