/**
 * The readings behind the six project screens.
 *
 * Three of these are things the screens got wrong before there were six of
 * them, and each is here for its own reason:
 *
 * - **the artifact a process deploys**, because "no artifact of its own" and
 *   "not in this build" are the same shape and opposite answers, and a filter
 *   that confused them would silently empty the timeline of every project whose
 *   workers share the project's image — which is most of them;
 * - **the auto-rollback column**, because its name says something the platform
 *   does not do, and a column that promised it would be believed;
 * - **"addressed from"**, because the rows that answer "nobody" are the whole
 *   point of the column: they are what makes a worker's empty request chart
 *   correct rather than broken.
 */
import { describe, expect, it } from "vitest";
import type { Build, Claim, Environment, Exception, Process, Project } from "./api";
import {
  addressedFrom,
  artifactFor,
  artifactNames,
  autoRollbackFor,
  claimPlan,
  claimUsedBy,
  deployEntries,
  deployedProcesses,
  hasRoute,
  host,
  noRouteReason,
  processRows,
  settingsSection,
  SETTINGS_SECTIONS,
  WEB_PROCESS,
} from "./project";

const project = (over: Partial<Project> = {}): Project =>
  ({
    name: "shop",
    repo: "acme/shop",
    productionEnvironment: "shop-production",
    replicas: 2,
    createdAt: "2026-01-01T00:00:00Z",
    ...over,
  }) as Project;

const environment = (over: Partial<Environment> = {}): Environment =>
  ({
    name: "shop-production",
    project: "shop",
    type: "production",
    release: "shop-rel-42",
    createdAt: "2026-01-01T00:00:00Z",
    ...over,
  }) as Environment;

const build = (over: Partial<Build> = {}): Build =>
  ({
    name: "shop-1",
    project: "shop",
    git: { sha: "abc", branch: "main" },
    createdAt: "2026-02-01T00:00:00Z",
    ...over,
  }) as Build;

const grant = (over: Partial<Exception> = {}): Exception =>
  ({
    name: "shop-glass-1",
    project: "shop",
    environment: "shop-production",
    ruleIDs: ["attestation.required"],
    reason: "incident",
    requestedBy: "a",
    approvedBy: "b",
    expiresAt: "2026-03-01T00:00:00Z",
    autoRollback: true,
    phase: "Active",
    createdAt: "2026-02-28T00:00:00Z",
    ...over,
  }) as Exception;

describe("the processes table", () => {
  const process = (over: Partial<Process>): Process => ({ name: "x", type: "worker", healthy: true, ...over });
  const worker = process({ name: "mailer", type: "worker", replicas: 3 });
  const service = process({ name: "api", type: "service", port: 8080 });
  const nightly = process({ name: "nightly", type: "cron", schedule: "0 2 * * *" });

  it("puts the published process first, and it is not one of the declared ones", () => {
    const rows = processRows(project({ processes: [worker] }), environment());
    expect(rows.map((row) => row.name)).toEqual([WEB_PROCESS, "mailer"]);
    // `spec.runtime` is singular because the URL is, so `web` is prepended
    // rather than read off a list it was never in.
    expect(rows[0].type).toBe(WEB_PROCESS);
  });

  it("carries one release across every row, because the unit deploys as one", () => {
    const rows = processRows(project({ processes: [worker, service] }), environment());
    expect(new Set(rows.map((row) => row.release))).toEqual(new Set(["shop-rel-42"]));
  });

  it("reads replicas off what is running, and asks for none where nothing runs continuously", () => {
    const live: Process[] = [process({ name: "mailer", type: "worker", replicas: 3, readyReplicas: 1 })];
    const rows = processRows(project({ processes: [worker, nightly] }), environment(), live);
    expect(rows.find((row) => row.name === "mailer")?.replicas).toBe("1/3");
    // A scheduled job has no replicas at all — how a firing went is its exit
    // status, and "0/0" would read as an outage.
    expect(rows.find((row) => row.name === "nightly")?.replicas).toBe("");
  });

  it("says what nothing addresses, rather than leaving the cell empty", () => {
    expect(addressedFrom("worker")).toContain("nothing");
    expect(addressedFrom("cron")).toContain("nothing");
    expect(addressedFrom("task")).toContain("nothing");
    // A service is addressed, and from inside this project alone.
    expect(addressedFrom("service", "api.shop:8080")).toContain("api.shop:8080");
    expect(addressedFrom(WEB_PROCESS)).toBe("the internet");
  });

  it("is honest about a project that has never deployed", () => {
    const rows = processRows(project({ processes: [worker] }));
    expect(rows[0].addressedFrom).toBe("not published yet");
    expect(rows[0].release).toBe("");
  });

  it("only the web process has a route, and everything else says why not", () => {
    expect(hasRoute(WEB_PROCESS)).toBe(true);
    for (const type of ["service", "worker", "cron", "task"]) {
      expect(hasRoute(type)).toBe(false);
      expect(noRouteReason(type)).toMatch(/^no route: /);
    }
    // The sentence #470 asks for by name.
    expect(noRouteReason("service")).toBe("no route: addressed only from web");
  });
});

describe("the deploys process filter", () => {
  const unit = build({
    artifact: { attested: true, repository: "registry/shop", digest: "sha256:web" },
    workloads: [{ name: "mailer", artifact: { attested: true, repository: "registry/shop-mailer" } }],
  });

  it("gives a workload with its own build its own artifact", () => {
    expect(artifactFor(unit, "mailer")?.name).toBe("mailer");
    expect(artifactFor(unit, "mailer")?.artifact?.repository).toBe("registry/shop-mailer");
  });

  it("gives a process with no build of its own the web artifact rather than nothing", () => {
    // The whole of decision 3: it *shares* the project's timeline instead of
    // disappearing from it.
    const shared = artifactFor(unit, "poller");
    expect(shared?.name).toBe(WEB_PROCESS);
    expect(shared?.artifact?.digest).toBe("sha256:web");
  });

  it("treats the empty name and `web` as the same thing", () => {
    expect(artifactFor(unit, "")).toEqual(artifactFor(unit, WEB_PROCESS));
  });

  it("names every artifact a build produced, web first", () => {
    expect(artifactNames(unit)).toEqual([WEB_PROCESS, "mailer"]);
    expect(artifactNames(build())).toEqual([WEB_PROCESS]);
  });

  it("offers a workload the project has since stopped declaring", () => {
    // A process removed last week still has a history, and a filter that
    // dropped it would make that history unreachable.
    const offered = deployedProcesses(
      project({ processes: [{ name: "api", type: "service", healthy: true }] }),
      [unit],
    );
    expect(offered).toContain("api");
    expect(offered).toContain("mailer");
    expect(offered[0]).toBe(WEB_PROCESS);
  });
});

describe("the deploy timeline", () => {
  it("puts builds and promotions in one order, newest first", () => {
    const entries = deployEntries(
      [build({ name: "shop-1", createdAt: "2026-02-01T00:00:00Z" }), build({ name: "shop-2", createdAt: "2026-02-03T00:00:00Z" })],
      [
        {
          name: "shop-promo-1",
          project: "shop",
          environment: "shop-production",
          release: "shop-rel-1",
          requestedBy: "a",
          trigger: "automatic",
          phase: "Applied",
          createdAt: "2026-02-02T00:00:00Z",
        },
      ],
    );
    expect(entries.map((entry) => entry.key)).toEqual(["build/shop-2", "promotion/shop-promo-1", "build/shop-1"]);
  });

  it("is stable for two things that happened at the same instant", () => {
    const at = "2026-02-01T00:00:00Z";
    const once = deployEntries([build({ name: "b", createdAt: at }), build({ name: "a", createdAt: at })], []);
    const twice = deployEntries([build({ name: "a", createdAt: at }), build({ name: "b", createdAt: at })], []);
    expect(once.map((e) => e.key)).toEqual(twice.map((e) => e.key));
  });
});

describe("the auto-rollback column", () => {
  it("says none where no grant asks for one, and refuses to promise anything", () => {
    const answer = autoRollbackFor(environment(), []);
    expect(answer.armed).toBe(false);
    expect(answer.label).toBe("none");
    // The correction #470 makes: this column is not "rolls back on a bad
    // deploy", and the sentence has to say so.
    expect(answer.detail).toContain("not a promise");
  });

  it("ignores a grant that does not ask for a rollback", () => {
    expect(autoRollbackFor(environment(), [grant({ autoRollback: false })]).armed).toBe(false);
  });

  it("ignores a grant for another environment, and one for another release", () => {
    expect(autoRollbackFor(environment(), [grant({ environment: "shop-staging" })]).armed).toBe(false);
    expect(autoRollbackFor(environment(), [grant({ release: "shop-rel-7" })]).armed).toBe(false);
    // A grant naming the release the environment is on does cover it.
    expect(autoRollbackFor(environment(), [grant({ release: "shop-rel-42" })]).armed).toBe(true);
  });

  it("ignores a grant that was resolved before it expired", () => {
    expect(autoRollbackFor(environment(), [grant({ phase: "Resolved" })]).armed).toBe(false);
  });

  it("says on expiry while the grant is live, and warns once it has expired", () => {
    const active = autoRollbackFor(environment(), [grant()]);
    expect(active.label).toBe("on expiry");
    expect(active.tone).toBe("neutral");

    const expired = autoRollbackFor(environment(), [grant({ phase: "Expired" })]);
    expect(expired.label).toBe("expired grant");
    expect(expired.tone).toBe("warning");
    // Narrowly, the way `internal/controller/rescan.go` acts on it: only if a
    // rule the grant waived is still firing.
    expect(expired.detail).toContain("still firing");
  });
});

describe("the attached resources table", () => {
  const claim = (over: Partial<Claim>): Claim =>
    ({ name: "db", project: "shop", connection: "neon", type: "postgres", ...over }) as Claim;

  it("names the one process a volume is mounted into", () => {
    expect(claimUsedBy(claim({ type: "volume", volume: { process: "mailer", mountPath: "/data" } } as Partial<Claim>))).toBe(
      "mailer",
    );
  });

  it("says how everything else reaches a claim, and says when nothing does yet", () => {
    expect(claimUsedBy(claim({ secret: "shop-db" }))).toContain("variables");
    expect(claimUsedBy(claim({}))).toBe("not bound yet");
  });

  it("falls back to the connection when the claim asked for nothing in particular", () => {
    expect(claimPlan(claim({}))).toBe("neon");
    expect(claimPlan(claim({ postgres: { version: "16" } } as Partial<Claim>))).toBe("pg 16");
  });
});

describe("the settings sub-navigation", () => {
  it("has a pane for every part of the page it replaces", () => {
    const ids = SETTINGS_SECTIONS.map((section) => section.id);
    for (const id of [
      "source",
      "processes",
      "resources",
      "variables",
      "files",
      "secrets",
      "domains",
      "members",
      "keys",
      "notifications",
      "runtime",
      "security",
      "continuity",
      "danger",
    ]) {
      expect(ids, `${id} is a pane`).toContain(id);
    }
  });

  it("gives every pane a sentence saying what it answers", () => {
    for (const section of SETTINGS_SECTIONS) {
      expect(section.description.length, `${section.id} says what it is for`).toBeGreaterThan(20);
    }
  });

  it("opens the first pane for a name it does not have", () => {
    // A bookmark to a renamed pane opens Settings rather than nothing.
    expect(settingsSection("nonsense").id).toBe(SETTINGS_SECTIONS[0].id);
    expect(settingsSection(undefined).id).toBe(SETTINGS_SECTIONS[0].id);
    expect(settingsSection("danger").id).toBe("danger");
  });
});

describe("host", () => {
  it("is the host of a URL, the string itself for anything else, and empty for nothing", () => {
    expect(host("https://shop.example.com/x")).toBe("shop.example.com");
    expect(host("not a url")).toBe("not a url");
    expect(host()).toBe("");
  });
});
