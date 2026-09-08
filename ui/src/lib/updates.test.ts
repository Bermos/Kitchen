import { describe, expect, it } from "vitest";
import type { ComponentStatus, K8sEvent, PlatformUpdate } from "./api";
import {
  PROBE_GRACE,
  checklist,
  componentDetail,
  frozen,
  inFlight,
  moving,
  partitionWarnings,
  settled,
  stageOf,
  unreachable,
  versionLabel,
  warmingUpLine,
} from "./updates";

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

/** The kubelet's own wording, which is what the rule below reads. */
const probe = (over: Partial<K8sEvent> = {}): K8sEvent => ({
  timestamp: "2026-09-08T10:00:00Z",
  namespace: "kitchen-system",
  kind: "Pod",
  name: "kitchen-auth-8fff9ddbf-htzln",
  reason: "Unhealthy",
  message: 'Startup probe failed: Get "http://10.244.0.156:8080/healthz": dial tcp 10.244.0.156:8080: connect: connection refused',
  count: 2,
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

describe("the grace a restarting pod gets", () => {
  it("holds back the probes of a container that has not come up yet", () => {
    // The whole of the reported bug: applying the chart restarts every platform
    // workload, and every one of them refuses connections on its own port for
    // its first few seconds. Rendering that as what is going wrong reports a
    // fault at the one moment there is none.
    const { wrong, starting } = partitionWarnings(
      [
        probe({ count: 2 }),
        probe({ name: "kitchen-auth-fc54fc64b-ndg5t", count: 5 }),
        probe({
          name: "kitchen-clickhouse-0",
          count: 1,
          message: 'Readiness probe failed: Get "https://10.244.0.77:8443/ping": dial tcp: connect: connection refused',
        }),
      ],
      "applying",
    );
    expect(wrong).toEqual([]);
    expect(starting).toHaveLength(3);
  });

  it("ends the grace when the probe keeps going unanswered", () => {
    // A probe that never answers is exactly what a stuck upgrade looks like
    // from the outside, so the same event becomes a warning on its own.
    expect(partitionWarnings([probe({ count: PROBE_GRACE })], "applying").wrong).toEqual([]);
    expect(partitionWarnings([probe({ count: PROBE_GRACE + 1 })], "applying").starting).toEqual([]);
  });

  it("never holds back a liveness probe, which kills the container", () => {
    const dying = probe({ message: 'Liveness probe failed: Get "http://10.244.0.156:8080/healthz": connection refused' });
    expect(partitionWarnings([dying], "applying").wrong).toEqual([dying]);
  });

  it("holds back nothing else the cluster complains about", () => {
    const events = [
      probe({ reason: "FailedScheduling", message: "0/3 nodes are available: insufficient memory" }),
      probe({ reason: "Failed", message: "Error: ImagePullBackOff" }),
      probe({ reason: "BackOff", message: "Back-off restarting failed container" }),
      probe({ reason: "FailedMount", message: "MountVolume.SetUp failed for volume \"data\"" }),
    ];
    expect(partitionWarnings(events, "applying").wrong).toEqual(events);
  });

  it("gives a failed upgrade no grace at all", () => {
    // Once helm has given up, the probes that never answered are not noise
    // around the failure — they are the account of it.
    const events = [probe({ count: 1 })];
    expect(partitionWarnings(events, "failed")).toEqual({ wrong: events, starting: [] });
  });

  it("says what it held back rather than going silent", () => {
    const line = warmingUpLine([probe({ count: 2 }), probe({ count: 5 }), probe({ name: "kitchen-clickhouse-0" })]);
    // One entry per pod, not per event: two rounds of the same pod's probe is
    // one pod that has not come up.
    expect(line).toContain("2 pods are not answering their probes yet");
    expect(line).toContain("kitchen-auth-8fff9ddbf-htzln, kitchen-clickhouse-0");
    expect(warmingUpLine([probe()])).toContain("1 pod is not answering its probes yet");
    expect(warmingUpLine([])).toBe("");
  });
});

describe("version labels", () => {
  it("adds the v the API leaves off, and leaves dev alone", () => {
    expect(versionLabel("0.13.1")).toBe("v0.13.1");
    expect(versionLabel("dev")).toBe("dev");
    expect(versionLabel(undefined)).toBe("—");
  });
});
