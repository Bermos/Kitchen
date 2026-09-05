import { describe, expect, it } from "vitest";
import type { Process } from "./api";
import {
  newWorkloadDraft,
  originOf,
  processWrites,
  volumeInitDrafts,
  volumeInitProblems,
  volumeInitWrites,
  workloadDrafts,
  workloadProblems,
} from "./workloads";

function read(...processes: Partial<Process>[]) {
  return workloadDrafts(processes.map((p) => ({ name: "x", type: "worker", healthy: true, ...p }) as Process));
}

describe("workload drafts", () => {
  it("reads a workload back the way the project declared it", () => {
    const [draft] = read({
      name: "worker",
      type: "worker",
      command: ["node", "worker.js"],
      replicas: 2,
      memory: "512Mi",
      singleton: true,
      previews: true,
    });
    expect(draft.name).toBe("worker");
    expect(draft.command).toBe("node\nworker.js");
    expect(draft.replicas).toBe("2");
    expect(draft.memory).toBe("512Mi");
    expect(draft.singleton).toBe(true);
    expect(draft.previews).toBe("yes");
    expect(draft.origin).toBe("project");
  });

  // The two fields that used not to survive the round trip, which is why the
  // API reports them at all: a parked workload's zero, and a declaration of
  // previews that is absent rather than false.
  it("keeps a parked workload parked", () => {
    const [draft] = read({ name: "worker", type: "worker", replicas: 0 });
    expect(draft.replicas).toBe("0");
    expect(processWrites([draft])[0].replicas).toBe(0);
  });

  it("keeps an undeclared previews undeclared, and a declared false false", () => {
    const [absent, declared] = read(
      { name: "worker", type: "worker" },
      { name: "api", type: "service", port: 8080, previews: false },
    );
    expect(absent.previews).toBe("default");
    expect(declared.previews).toBe("no");
    const writes = processWrites([absent, declared]);
    expect(writes[0].previews).toBeUndefined();
    expect(writes[1].previews).toBe(false);
  });

  it("knows where each workload's image comes from", () => {
    const workloads = [
      { name: "worker", type: "worker" },
      { name: "api", type: "service", port: 8080, build: { strategy: "auto", rootDirectory: "services/api" } },
      {
        name: "cache",
        type: "service",
        port: 6379,
        imageSource: { repository: "docker.io/library/redis", tag: "7.4", reference: "docker.io/library/redis:7.4" },
      },
    ] as Process[];
    expect(workloads.map(originOf)).toEqual(["project", "build", "image"]);
    const writes = processWrites(workloadDrafts(workloads));
    expect(writes[0].build).toBeUndefined();
    expect(writes[0].image).toBeUndefined();
    expect(writes[1].build).toEqual({ strategy: "auto", rootDirectory: "services/api" });
    expect(writes[2].image).toEqual({ repository: "docker.io/library/redis", tag: "7.4" });
  });
});

describe("what the write carries", () => {
  it("sends a workload back with only the fields its type has", () => {
    const [worker] = read({ name: "worker", type: "worker", replicas: 3 });
    // The form still holds a port and a schedule from whatever the row was
    // before somebody changed its type; neither may reach a worker.
    worker.port = "8080";
    worker.schedule = "0 3 * * *";
    worker.timeout = "30m";
    const [write] = processWrites([worker]);
    expect(write).toEqual({ name: "worker", type: "worker", replicas: 3 });
  });

  it("gives a scheduled job its schedule and its bound, and no replicas", () => {
    const draft = newWorkloadDraft();
    draft.name = "nightly";
    draft.type = "cron";
    draft.schedule = "0 3 * * *";
    draft.timeout = "30m";
    draft.concurrencyPolicy = "Replace";
    expect(processWrites([draft])[0]).toEqual({
      name: "nightly",
      type: "cron",
      schedule: "0 3 * * *",
      concurrencyPolicy: "Replace",
      timeout: "30m",
    });
  });

  it("drops a nameless row rather than sending one the API would refuse", () => {
    expect(processWrites([newWorkloadDraft()])).toEqual([]);
  });

  it("never sends a probe a scheduled job would be refused", () => {
    const draft = newWorkloadDraft();
    draft.name = "nightly";
    draft.type = "cron";
    draft.schedule = "0 3 * * *";
    draft.health = true;
    draft.healthPort = "9000";
    expect(processWrites([draft])[0].health).toBeUndefined();
  });

  it("sends a worker's probe with the port it names", () => {
    const draft = newWorkloadDraft();
    draft.name = "worker";
    draft.health = true;
    draft.healthPath = "/healthz";
    draft.healthPort = "9000";
    expect(processWrites([draft])[0].health).toEqual({ path: "/healthz", port: 9000 });
  });
});

describe("what the form can refuse on its own", () => {
  it("names the workload each problem is about", () => {
    const drafts = [newWorkloadDraft(), newWorkloadDraft(), newWorkloadDraft(), newWorkloadDraft("image")];
    drafts[0].name = "";
    drafts[1].name = "api";
    drafts[1].type = "service";
    drafts[2].name = "nightly";
    drafts[2].type = "cron";
    drafts[3].name = "cache";
    drafts[3].type = "service";
    drafts[3].port = "6379";
    expect(workloadProblems(drafts)).toEqual([
      "A workload needs a name.",
      "api is addressed by the rest of the unit, so it has to say which port it listens on.",
      "nightly runs on a schedule, so it needs one.",
      "cache runs an image somebody else built, so it has to say which.",
      "cache needs a tag or a digest: a vendored image is pinned to a version.",
    ]);
  });

  it("catches the two names the API would refuse", () => {
    const drafts = [newWorkloadDraft(), newWorkloadDraft(), newWorkloadDraft()];
    drafts[0].name = "worker";
    drafts[1].name = "worker";
    drafts[2].name = "web";
    expect(workloadProblems(drafts)).toEqual([
      "Two workloads are called worker.",
      "The web process is the project's own runtime, not a workload in this list.",
    ]);
  });

  it("is quiet about a list the platform would accept", () => {
    const drafts = read(
      { name: "worker", type: "worker" },
      { name: "api", type: "service", port: 8080 },
      { name: "nightly", type: "cron", schedule: "0 3 * * *" },
      { name: "migrate", type: "task" },
    );
    expect(workloadProblems(drafts)).toEqual([]);
  });
});

// What a workload prepares inside the volumes it mounts before it starts
// (#348). The editor holds the two step lists as text — a list is a list of
// lines, the way the command and the arguments already are — so the round trip
// through that text is the thing worth pinning: a workload nobody touched has
// to be written back exactly as it came.
describe("preparing a volume before the workload starts", () => {
  it("reads a declaration back as lines and writes the same declaration", () => {
    const drafts = volumeInitDrafts([
      {
        volume: "config",
        directories: [{ path: "custom_components" }, { path: "secrets", mode: "0700" }],
        seed: [{ file: "configuration", path: "configuration.yaml" }],
      },
    ]);
    expect(drafts).toHaveLength(1);
    expect(drafts[0].volume).toBe("config");
    expect(drafts[0].directories).toBe("custom_components\nsecrets 0700");
    expect(drafts[0].seed).toBe("configuration configuration.yaml");

    expect(volumeInitWrites(drafts)).toEqual([
      {
        volume: "config",
        directories: [{ path: "custom_components" }, { path: "secrets", mode: "0700" }],
        seed: [{ file: "configuration", path: "configuration.yaml" }],
      },
    ]);
  });

  it("carries a workload's declaration through the workloads editor untouched", () => {
    const [draft] = read({
      name: "hass",
      type: "worker",
      init: [{ volume: "config", directories: [{ path: "data", mode: "0750" }] }],
    });
    expect(draft.init).toHaveLength(1);
    const [write] = processWrites([draft]);
    expect(write.init).toEqual([{ volume: "config", directories: [{ path: "data", mode: "0750" }] }]);
  });

  it("leaves the field out for a workload that prepares nothing", () => {
    const [write] = processWrites([{ ...newWorkloadDraft(), name: "worker" }]);
    expect(write.init).toBeUndefined();
  });

  it("says what is wrong rather than sending it", () => {
    expect(volumeInitProblems([{ key: "k", volume: "", directories: "data", seed: "" }], "worker")).toEqual([
      "worker prepares a volume without saying which.",
    ]);
    expect(volumeInitProblems([{ key: "k", volume: "config", directories: "", seed: "" }], "worker")).toEqual([
      "worker prepares config and says nothing to do to it.",
    ]);
    expect(
      volumeInitProblems(
        [
          { key: "a", volume: "config", directories: "data", seed: "" },
          { key: "b", volume: "config", directories: "more", seed: "" },
        ],
        "worker",
      ),
    ).toContain("worker prepares config twice: one volume is one entry, with all of its steps in it.");
    expect(
      volumeInitProblems([{ key: "k", volume: "config", directories: "", seed: "configuration" }], "worker")[0],
    ).toContain("says which file and where it goes");
  });
});
