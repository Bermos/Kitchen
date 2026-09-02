import { describe, expect, it } from "vitest";
import type { Build, ConfigDiff, Environment, Release, RequestSummary, Workload } from "./api";
import {
  changeDetail,
  changeSign,
  commitsBetween,
  gatedByName,
  lastServedStint,
  deployTasksThatRunAgain,
  movedProcesses,
  movedRuntime,
  movedVariables,
  previouslyServed,
  releaseRows,
  servingSince,
  swapLanded,
  unchangedVariables,
  variableCounts,
  verificationChecks,
} from "./rollback";

// The rollback panel's reasoning (#181). The screen is a rendering of this
// module, so the questions it has to answer before a production write —
// what stops being served, what changes about the configuration, and whether
// the write is one somebody should have to type the environment's name for —
// are all answered here.

const release = (name: string, createdAt: string, build = `${name}-bld`): Release => ({
  name,
  project: "shop",
  build,
  image: `registry.example.com/shop@sha256:${name}`,
  createdAt,
});

const releases = [
  release("rel-42", "2026-08-25T09:00:00Z"),
  release("rel-41", "2026-08-25T04:00:00Z"),
  release("rel-40", "2026-08-24T09:00:00Z"),
  release("rel-39", "2026-08-23T09:00:00Z"),
];

const build = (name: string, sha: string, message: string): Build =>
  ({
    name,
    project: "shop",
    phase: "Succeeded",
    git: { sha, branch: "main", message, author: "bermos" },
    createdAt: "2026-08-25T09:00:00Z",
  }) as Build;

const builds = [
  build("rel-42-bld", "8f3a2c1", "Cache artwork thumbnails at build"),
  build("rel-41-bld", "77c0b12", "Fix pagination on /works"),
  build("rel-40-bld", "c19be40", "Bump Next.js to 15.4"),
];

const environment = (over: Partial<Environment> = {}): Environment =>
  ({
    name: "shop-production",
    project: "shop",
    type: "production",
    release: "rel-42",
    observedRelease: "rel-42",
    createdAt: "2026-08-01T00:00:00Z",
    history: [
      { release: "rel-41", from: "2026-08-25T04:10:00Z", to: "2026-08-25T09:05:00Z", reason: "promoted" },
      { release: "rel-40", from: "2026-08-24T09:10:00Z", to: "2026-08-25T04:10:00Z", reason: "rolledBack" },
    ],
    ...over,
  }) as Environment;

const diff = (over: Partial<ConfigDiff> = {}): ConfigDiff => ({
  release: "rel-41",
  against: "rel-42",
  project: "shop",
  variables: [
    { name: "NEXT_PUBLIC_CDN", change: "changed", source: "value", againstSource: "value" },
    { name: "FEATURE_BULK_IMPORT", change: "removed", againstSource: "value" },
    { name: "NODE_ENV", change: "unchanged", source: "value", againstSource: "value" },
    {
      name: "DATABASE_URL",
      change: "unchanged",
      source: "claim",
      againstSource: "claim",
      ref: { name: "shop-db", key: "url" },
      againstRef: { name: "shop-db", key: "url" },
    },
  ],
  runtime: [
    { field: "replicas", from: "3", to: "2", changed: true },
    { field: "port", from: "3000", to: "3000", changed: false },
  ],
  processes: [
    { name: "nightly", change: "changed", type: "cron", schedule: "0 2 * * *" },
    { name: "mailer", change: "unchanged", type: "worker" },
  ],
  ...over,
});

describe("releaseRows", () => {
  const rows = releaseRows(releases, builds, environment());

  it("marks what is live and what was live before it", () => {
    expect(rows.map((r) => r.release.name)).toEqual(["rel-42", "rel-41", "rel-40", "rel-39"]);
    expect(rows[0].live).toBe(true);
    expect(rows[1].lastServed).toBe(true);
    expect(rows[0].lastServed).toBe(false);
  });

  it("counts the distance from the live release, negative for a promotion", () => {
    expect(rows.map((r) => r.distance)).toEqual([0, 1, 2, 3]);
    const ahead = releaseRows(releases, builds, environment({ release: "rel-40" }));
    expect(ahead.map((r) => r.distance)).toEqual([-2, -1, 0, 1]);
  });

  it("says when a release has been rolled back off before", () => {
    expect(rows.find((r) => r.release.name === "rel-40")?.rolledBackBefore).toBe(true);
    expect(rows.find((r) => r.release.name === "rel-41")?.rolledBackBefore).toBe(false);
  });

  it("joins the build without inventing one that has aged out", () => {
    expect(rows[0].build?.git.sha).toBe("8f3a2c1");
    expect(rows[3].build).toBeUndefined();
  });
});

describe("the environment's own history", () => {
  it("reads what was running before, and when the live one took over", () => {
    expect(previouslyServed(environment())).toBe("rel-41");
    expect(servingSince(environment())).toBe("2026-08-25T09:05:00Z");
    expect(lastServedStint(environment(), "rel-41")).toEqual({
      from: "2026-08-25T04:10:00Z",
      to: "2026-08-25T09:05:00Z",
    });
    expect(lastServedStint(environment(), "rel-39")).toBeUndefined();
  });

  it("falls back to the environment's creation when it has only held one release", () => {
    expect(servingSince(environment({ history: [] }))).toBe("2026-08-01T00:00:00Z");
    expect(previouslyServed(environment({ history: [] }))).toBe("");
  });
});

describe("commitsBetween", () => {
  const rows = releaseRows(releases, builds, environment());

  it("names the commits that stop being served, the live one included", () => {
    const stopping = commitsBetween(rows, releases[2]); // back to rel-40
    expect(stopping.direction).toBe("rollback");
    expect(stopping.builds.map((b) => b.git.sha)).toEqual(["8f3a2c1", "77c0b12"]);
  });

  it("reads the same range the other way for a move forward", () => {
    const behind = releaseRows(releases, builds, environment({ release: "rel-40", observedRelease: "rel-40" }));
    const starting = commitsBetween(behind, releases[0]); // forward to rel-42
    expect(starting.direction).toBe("promotion");
    expect(starting.builds.map((b) => b.git.sha)).toEqual(["8f3a2c1", "77c0b12"]);
  });

  it("says nothing about a move to where the environment already is", () => {
    expect(commitsBetween(rows, releases[0]).builds).toEqual([]);
    expect(commitsBetween(rows, undefined).builds).toEqual([]);
  });

  it("skips a release whose build no longer exists rather than guessing", () => {
    const far = commitsBetween(rows, releases[3]); // back to rel-39
    expect(far.builds.map((b) => b.git.sha)).toEqual(["8f3a2c1", "77c0b12", "c19be40"]);
  });
});

describe("the variable diff", () => {
  it("counts what moves and lists it apart from what does not", () => {
    expect(variableCounts(diff())).toEqual({ changed: 1, removed: 1, added: 0, unchanged: 2 });
    expect(movedVariables(diff()).map((v) => v.name)).toEqual(["NEXT_PUBLIC_CDN", "FEATURE_BULK_IMPORT"]);
    expect(unchangedVariables(diff()).map((v) => v.name)).toEqual(["NODE_ENV", "DATABASE_URL"]);
    expect(variableCounts(undefined)).toEqual({ changed: 0, removed: 0, added: 0, unchanged: 0 });
  });

  it("marks each row in the diff's own vocabulary", () => {
    expect(changeSign("changed")).toBe("~");
    expect(changeSign("removed")).toBe("−");
    expect(changeSign("added")).toBe("+");
    expect(changeSign("unchanged")).toBe("=");
  });

  // The panel never shows a value, because the API never reads one back. What
  // it shows instead is what kind of change it is — and where the *source*
  // moved, which is the change no diff of values would have explained.
  it("says what changed without ever having a value to say", () => {
    expect(changeDetail({ name: "A", change: "removed", againstSource: "value" })).toBe("a value → unset");
    expect(changeDetail({ name: "A", change: "added", source: "secret" })).toBe("unset → a secret");
    expect(changeDetail({ name: "A", change: "changed", source: "value", againstSource: "value" })).toBe(
      "the value differs",
    );
    expect(changeDetail({ name: "A", change: "changed", source: "claim", againstSource: "value" })).toBe(
      "a value → a claim binding",
    );
    expect(
      changeDetail({ name: "A", change: "changed", source: "value", againstSource: "value", previewOnly: true }),
    ).toBe("only the preview override differs");
    expect(
      changeDetail({
        name: "A",
        change: "changed",
        source: "secret",
        againstSource: "secret",
        ref: { name: "shop", key: "old" },
        againstRef: { name: "shop", key: "new" },
      }),
    ).toBe("shop/new → shop/old");
  });

  it("carries the runtime and the processes that move", () => {
    expect(movedRuntime(diff()).map((f) => f.field)).toEqual(["replicas"]);
    expect(movedProcesses(diff()).map((p) => p.name)).toEqual(["nightly"]);
  });

  // A rollback runs the work the release it goes back to declares, so the
  // migration runs again whether or not anything about it differs — and the
  // one that does not run is the one only the release being left behind has.
  it("names the deploy tasks that run again, changed or not", () => {
    const withTasks = diff({
      processes: [
        { name: "migrate", change: "unchanged", type: "task" },
        { name: "seed", change: "changed", type: "task" },
        { name: "backfill", change: "removed", type: "task" },
        { name: "nightly", change: "changed", type: "cron", schedule: "0 2 * * *" },
      ],
    });
    expect(deployTasksThatRunAgain(withTasks).map((p) => p.name)).toEqual(["migrate", "seed"]);
    expect(deployTasksThatRunAgain(diff())).toEqual([]);
    expect(deployTasksThatRunAgain(undefined)).toEqual([]);
  });
});

describe("gatedByName", () => {
  const rows = releaseRows(releases, builds, environment());
  const clean = diff({ variables: [{ name: "NODE_ENV", change: "unchanged", source: "value" }] });
  const rowFor = (name: string) => rows.find((r) => r.release.name === name);

  it("leaves the on-call undo a single click", () => {
    expect(gatedByName(environment(), rowFor("rel-41"), clean)).toBe(false);
  });

  it("asks for the name once the configuration moves too, however near", () => {
    expect(gatedByName(environment(), rowFor("rel-41"), diff())).toBe(true);
  });

  it("asks for the name past one release back, whatever the configuration says", () => {
    expect(gatedByName(environment(), rowFor("rel-40"), clean)).toBe(true);
    expect(gatedByName(environment(), rowFor("rel-39"), clean)).toBe(true);
  });

  it("never gates a preview, which is disposable", () => {
    const preview = environment({ type: "preview" });
    expect(gatedByName(preview, releaseRows(releases, builds, preview).find((r) => r.release.name === "rel-39"), diff())).toBe(
      false,
    );
  });

  it("gates nothing it has not been given", () => {
    expect(gatedByName(undefined, rowFor("rel-39"), diff())).toBe(false);
    expect(gatedByName(environment(), undefined, diff())).toBe(false);
  });
});

describe("the verification step", () => {
  const workload = (over: Partial<Workload> = {}): Workload =>
    ({
      environment: "shop-production",
      namespace: "shop",
      deployment: "shop-production",
      replicas: { desired: 3, ready: 3, available: 3, updated: 3 },
      restarts: 0,
      ...over,
    }) as Workload;

  const summary = (over: Partial<RequestSummary> = {}): RequestSummary =>
    ({
      since: "2026-08-25T09:05:00Z",
      until: "2026-08-25T09:20:00Z",
      requests: 214,
      requestsPerSecond: 0.2,
      errors: 0,
      errorRate: 0,
      p50Ms: 40,
      p95Ms: 131,
      p99Ms: 300,
      rollup: "1m",
      environment: "shop-production",
      ...over,
    }) as RequestSummary;

  it("knows the swap has landed only once the operator has applied it", () => {
    expect(swapLanded(environment({ release: "rel-41", observedRelease: "rel-41" }), "rel-41")).toBe(true);
    expect(swapLanded(environment({ release: "rel-41", observedRelease: "rel-42" }), "rel-41")).toBe(false);
  });

  it("answers all four questions from what the screen already polls", () => {
    const checks = verificationChecks({
      environment: environment({
        release: "rel-41",
        observedRelease: "rel-41",
        conditions: [
          { type: "RouteProgrammed", status: "True", message: "HTTPRoute applied", lastTransitionTime: "" },
        ],
      }),
      target: "rel-41",
      workload: workload(),
      since: summary(),
      baseline: summary({ p95Ms: 146 }),
    });
    expect(checks.map((c) => [c.label, c.state])).toEqual([
      ["Replicas updated", "ok"],
      ["Route programmed", "ok"],
      ["5xx since the swap", "ok"],
      ["p95 recovered", "ok"],
    ]);
    expect(checks[0].detail).toBe("3 of 3 · 0 restarts");
    expect(checks[3].detail).toBe("146 ms → 131 ms");
  });

  // Not knowing yet is its own state. Colouring it red would teach somebody
  // to ignore red at the exact moment it matters.
  it("waits rather than failing while it does not know", () => {
    const checks = verificationChecks({ environment: environment(), target: "rel-41" });
    expect(checks.every((c) => c.state === "pending")).toBe(true);
  });

  it("is red about a 5xx and about a route that did not program", () => {
    const checks = verificationChecks({
      environment: environment({
        release: "rel-41",
        observedRelease: "rel-41",
        conditions: [
          {
            type: "RouteProgrammed",
            status: "False",
            message: "the preview gate is unavailable",
            lastTransitionTime: "",
          },
        ],
      }),
      target: "rel-41",
      workload: workload(),
      since: summary({ errors: 12 }),
      baseline: summary(),
    });
    expect(checks[1].state).toBe("bad");
    expect(checks[1].detail).toBe("the preview gate is unavailable");
    expect(checks[2].state).toBe("bad");
    expect(checks[2].detail).toBe("12 in 214 requests");
  });

  // A slower p95 right after a swap is a cold cache, not a failure — it
  // settles rather than fails, and keeps the panel watching.
  it("keeps watching a p95 that has not come back yet", () => {
    const checks = verificationChecks({
      environment: environment({ release: "rel-41", observedRelease: "rel-41" }),
      target: "rel-41",
      workload: workload(),
      since: summary({ p95Ms: 300 }),
      baseline: summary({ p95Ms: 131 }),
    });
    expect(checks[3].label).toBe("p95 still settling");
    expect(checks[3].state).toBe("pending");
  });

  it("waits on replicas the operator has not rolled yet", () => {
    const checks = verificationChecks({
      environment: environment({ release: "rel-41", observedRelease: "rel-41" }),
      target: "rel-41",
      workload: workload({ replicas: { desired: 3, ready: 1, available: 1, updated: 1 } }),
    });
    expect(checks[0].state).toBe("pending");
    expect(checks[0].detail).toBe("1 of 3 · 0 restarts");
  });
});
