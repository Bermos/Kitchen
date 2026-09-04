import { describe, expect, it } from "vitest";
import {
  awaitingContent,
  collidingPath,
  configFileDrafts,
  configFileWrites,
  nameProblem,
  newConfigFileDraft,
  pathProblem,
  renamedFile,
} from "./files";

describe("configuration file drafts", () => {
  it("carries a plain file's content over and never invents a secret one's", () => {
    const drafts = configFileDrafts([
      { name: "configuration", path: "/config/configuration.yaml", content: "logger: info\n" },
      { name: "app-ini", path: "/data/conf/app.ini", secret: true, contentHash: "abc123", size: 40 },
    ]);
    expect(drafts[0]).toMatchObject({ path: "/config/configuration.yaml", content: "logger: info\n" });
    // There is no content to carry, and inventing an empty one would send a
    // write that quietly empties the credential.
    expect(drafts[1].content).toBeUndefined();
    expect(drafts[1].contentHash).toBe("abc123");
  });

  it("sends a plain file's content and never a secret file's", () => {
    const drafts = configFileDrafts([
      { name: "configuration", path: "/config/configuration.yaml", content: "logger: info\n" },
      { name: "app-ini", path: "/data/conf/app.ini", secret: true, contentHash: "abc123" },
    ]);
    drafts[0].content = "logger: debug\n";
    expect(configFileWrites(drafts)).toEqual([
      { name: "configuration", path: "/config/configuration.yaml", content: "logger: debug\n" },
      { name: "app-ini", path: "/data/conf/app.ini", secret: true },
    ]);
  });

  it("carries the workloads a file names, and nothing at all when it names none", () => {
    const drafts = configFileDrafts([
      { name: "worker-conf", path: "/etc/worker.toml", content: "a", workloads: ["worker"] },
      { name: "configuration", path: "/config/app.yaml", content: "b" },
    ]);
    const writes = configFileWrites(drafts);
    expect(writes[0].workloads).toEqual(["worker"]);
    expect(writes[1].workloads).toBeUndefined();
  });

  it("knows a renamed file has left its content behind", () => {
    const drafts = configFileDrafts([{ name: "app-ini", path: "/data/app.ini", secret: true, contentHash: "a" }]);
    expect(renamedFile(drafts[0])).toBe(false);
    drafts[0].name = "gitea-ini";
    // Content is kept by name, so the rename lands a secret file with none —
    // which the form says out loud rather than saving quietly.
    expect(renamedFile(drafts[0])).toBe(true);
    expect(renamedFile(newConfigFileDraft())).toBe(false);
  });

  it("names a secret file the platform holds nothing for", () => {
    const [declared, written, plain] = configFileDrafts([
      { name: "app-ini", path: "/data/app.ini", secret: true },
      { name: "other-ini", path: "/data/other.ini", secret: true, contentHash: "abc" },
      { name: "configuration", path: "/config/app.yaml", content: "" },
    ]);
    // The workloads reading it will not start, so it is a state the screen
    // names rather than an empty cell.
    expect(awaitingContent(declared)).toBe(true);
    expect(awaitingContent(written)).toBe(false);
    expect(awaitingContent(plain)).toBe(false);
  });

  it("drops nameless rows and trims what it sends", () => {
    const draft = newConfigFileDraft();
    draft.name = " configuration ";
    draft.path = " /config/app.yaml ";
    draft.content = "logger: info\n";
    expect(configFileWrites([draft, newConfigFileDraft()])).toEqual([
      { name: "configuration", path: "/config/app.yaml", content: "logger: info\n" },
    ]);
  });

  it("says what is wrong with a path or a name before the save does", () => {
    expect(pathProblem("")).toBe("");
    expect(pathProblem("/config/app.yaml")).toBe("");
    expect(pathProblem("config/app.yaml")).not.toBe("");
    expect(pathProblem("/config/")).not.toBe("");
    expect(pathProblem("/config/../etc/passwd")).not.toBe("");
    expect(nameProblem("app-ini")).toBe("");
    expect(nameProblem("not a key")).not.toBe("");
  });

  it("catches two files landing on one path in one workload", () => {
    const drafts = configFileDrafts([
      { name: "one", path: "/config/app.yaml", content: "a" },
      { name: "two", path: "/config/app.yaml", content: "b" },
    ]);
    expect(collidingPath(drafts, 0)).toBe(true);

    // Two workloads that never share a pod can each have their own file at
    // the same path, which is a use rather than a mistake.
    drafts[0].workloads = ["web"];
    drafts[1].workloads = ["worker"];
    expect(collidingPath(drafts, 0)).toBe(false);
    expect(collidingPath(drafts, 1)).toBe(false);
  });
});
