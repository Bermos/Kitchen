/**
 * What the band says, and about what.
 *
 * The band is the screen's answer to "is anything wrong", so the two things
 * worth pinning are that it hoists exactly what the table marks with an error
 * dot — no more, so the inventory below is not dimmed at random, and no fewer,
 * so nothing broken is left in the table alone — and that the sentences it
 * puts next to a failure are true of the failure.
 */
import { beforeEach, describe, expect, it } from "vitest";
import type { Build, Environment, Project } from "./api";
import {
  ATTENTION_CAP,
  clearDismissals,
  dismiss,
  incidentsFrom,
  isDismissed,
  undismissed,
} from "./attention";

function project(over: Partial<Project> = {}): Project {
  return {
    name: "werkverzeichnis",
    role: "developer",
    repo: "Bermos/werkverzeichnis",
    connection: "github",
    registry: "kitchen",
    productionBranch: "main",
    requirePullRequest: false,
    previews: true,
    previewsProtected: false,
    productionEnvironment: "werkverzeichnis-production",
    createdAt: "2026-01-01T00:00:00Z",
    ...over,
  } as Project;
}

function environment(over: Partial<Environment> = {}): Environment {
  return {
    name: "werkverzeichnis-production",
    project: "werkverzeichnis",
    type: "production",
    release: "rel-000042",
    phase: "Live",
    url: "https://werkverzeichnis.bermos.dev",
    createdAt: "2026-01-01T00:00:00Z",
    ...over,
  } as Environment;
}

function build(over: Partial<Build> = {}): Build {
  return {
    name: "bld-0aa3ce2",
    project: "werkverzeichnis",
    phase: "Failed",
    git: { sha: "0aa3ce2f5b1d9e7c", branch: "chore/sharp" },
    failure: {
      message: "sharp@0.34 requires libvips ≥ 8.15, image has 8.14",
      container: "build",
      exitCode: 1,
      log: ["#8 8.421 npm error code 1", "ERROR: process \"/bin/sh -c npm ci\" exited with code 1"],
    },
    conditions: [{ type: "Ready", status: "False", reason: "BuildFailed", message: "the build failed" }],
    createdAt: "2026-08-24T22:00:00Z",
    completedAt: "2026-08-24T22:12:00Z",
    ...over,
  } as Build;
}

beforeEach(() => clearDismissals());

describe("what the band hoists", () => {
  it("takes a failed build, with the error at its full length and the log that produced it", () => {
    const [incident] = incidentsFrom([project()], [environment()], [build()]);
    expect(incident.kind).toBe("build");
    expect(incident.error).toBe("sharp@0.34 requires libvips ≥ 8.15, image has 8.14");
    expect(incident.log.length, "the failing step's output rides on the build already").toBe(2);
    expect(incident.conditions).toHaveLength(1);
    expect(incident.facts).toEqual(["chore/sharp", "0aa3ce2", "bld-0aa3ce2"]);
    expect(incident.build?.name).toBe("bld-0aa3ce2");
  });

  it("says what a failure costs, and a branch build costs production nothing", () => {
    const [preview] = incidentsFrom([project()], [environment()], [build()]);
    expect(preview.blastRadius).toBe("preview only — production still serving rel-000042");

    const [onMain] = incidentsFrom(
      [project()],
      [environment()],
      [build({ git: { sha: "0aa3ce2f", branch: "main" } })],
    );
    expect(onMain.blastRadius).toBe("production still serving rel-000042 — this commit has not shipped");

    const [nothingLive] = incidentsFrom([project()], [], [build()]);
    expect(nothingLive.blastRadius).toBe("preview only — nothing is published for this project yet");
  });

  it("takes a degraded production, and says what it is still serving", () => {
    const degraded = environment({
      phase: "Degraded",
      conditions: [
        {
          type: "Ready",
          status: "False",
          reason: "ReplicasUnavailable",
          message: "14 × 5xx in the last 5 min — ECONNRESET at pg/client.js:412",
          lastTransitionTime: "2026-08-22T09:00:00Z",
        },
      ],
    });
    const [incident] = incidentsFrom([project()], [degraded], []);
    expect(incident.kind).toBe("environment");
    expect(incident.error).toBe("14 × 5xx in the last 5 min — ECONNRESET at pg/client.js:412");
    expect(incident.blastRadius).toBe("werkverzeichnis.bermos.dev degraded — still serving rel-000042");
    expect(incident.since).toBe("2026-08-22T09:00:00Z");
    expect(incident.environment?.name).toBe("werkverzeichnis-production");
  });

  it("says when what is running is not what was asked for", () => {
    const stuck = environment({ phase: "Degraded", release: "rel-000042", observedRelease: "rel-000041" });
    const [incident] = incidentsFrom([project()], [stuck], []);
    expect(incident.blastRadius).toBe(
      "werkverzeichnis.bermos.dev degraded — asked for rel-000042, still running rel-000041",
    );
  });

  it("takes a project's own trouble only when nothing more specific said it", () => {
    const unhappy = project({
      conditions: [
        {
          type: "SourceConnected",
          status: "False",
          reason: "TokenRejected",
          message: "the connection's token was refused",
          lastTransitionTime: "2026-08-24T21:00:00Z",
        },
      ],
    });
    const alone = incidentsFrom([unhappy], [environment()], []);
    expect(alone).toHaveLength(1);
    expect(alone[0].kind).toBe("project");
    expect(alone[0].error).toBe("the connection's token was refused");

    // With a failed build as well, the build is the specific answer and the
    // project's condition is not repeated under it.
    const withBuild = incidentsFrom([unhappy], [environment()], [build()]);
    expect(withBuild.map((i) => i.kind)).toEqual(["build"]);
  });

  it("hoists exactly the projects the table marks, and leaves healthy ones alone", () => {
    const healthy = project({ name: "paste", productionEnvironment: "paste-production" });
    const incidents = incidentsFrom(
      [project(), healthy],
      [environment(), environment({ name: "paste-production", project: "paste" })],
      [build(), build({ name: "bld-99", project: "paste", phase: "Succeeded", failure: undefined })],
    );
    expect(incidents.map((i) => i.project)).toEqual(["werkverzeichnis"]);
  });

  it("is newest first", () => {
    const older = project({ name: "status", productionEnvironment: "status-production" });
    const incidents = incidentsFrom(
      [project(), older],
      [
        environment(),
        environment({
          name: "status-production",
          project: "status",
          phase: "Degraded",
          conditions: [{ type: "Ready", status: "False", lastTransitionTime: "2026-08-22T22:00:00Z" }],
        }),
      ],
      [build()],
    );
    expect(incidents.map((i) => i.project)).toEqual(["werkverzeichnis", "status"]);
  });
});

describe("dismissal", () => {
  it("stays out until the condition changes", () => {
    const [incident] = incidentsFrom([project()], [environment()], [build()]);
    dismiss(incident);
    expect(isDismissed(incident)).toBe(true);
    expect(undismissed([incident])).toEqual([]);

    // The same failure on the next poll is the same incident, and stays down.
    const [again] = incidentsFrom([project()], [environment()], [build()]);
    expect(undismissed([again])).toEqual([]);

    // A different failure of the same build is not what was waved off.
    const [changed] = incidentsFrom(
      [project()],
      [environment()],
      [build({ failure: { message: "the registry refused the push" } })],
    );
    expect(undismissed([changed])).toHaveLength(1);
  });

  it("is forgotten when there is nothing left to be about", () => {
    const [incident] = incidentsFrom([project()], [environment()], [build()]);
    dismiss(incident);
    clearDismissals();
    expect(isDismissed(incident)).toBe(false);
  });
});

describe("the cap", () => {
  it("is five, and the rest are counted rather than dropped", () => {
    const many = Array.from({ length: 7 }, (_, i) =>
      project({ name: `p${i}`, productionEnvironment: `p${i}-production` }),
    );
    const builds = many.map((p, i) =>
      build({ name: `bld-${i}`, project: p.name, completedAt: `2026-08-24T2${i}:00:00Z` }),
    );
    const incidents = incidentsFrom(many, [], builds);
    expect(incidents).toHaveLength(7);
    expect(ATTENTION_CAP).toBe(5);
    // The band shows the cap and says how many it is holding; nothing here
    // throws the rest away, which is what makes "2 more" honest.
    expect(incidents.slice(ATTENTION_CAP)).toHaveLength(2);
  });
});
