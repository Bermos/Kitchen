import { describe, expect, it } from "vitest";
import type { Build } from "./api";
import { buildFailureLine } from "./builds";

const build = (over: Partial<Build> = {}): Build => ({
  name: "shop-bld-0f8ed150b919",
  project: "shop",
  git: { sha: "0f8ed150b9190000", branch: "main" },
  createdAt: "2026-08-25T10:00:00Z",
  ...over,
});

describe("buildFailureLine", () => {
  it("says nothing about a build that did not fail", () => {
    expect(buildFailureLine(build({ phase: "Succeeded", failure: { message: "stale" } }))).toBe("");
    expect(buildFailureLine(build({ phase: "Running" }))).toBe("");
  });

  it("prefers the failure the operator wrote down", () => {
    const failed = build({
      phase: "Failed",
      failure: { container: "creator", exitCode: 51, reason: "Error", message: "creator exited 51" },
      conditions: [{ type: "Ready", status: "False", message: "Job has reached the specified backoff limit", lastTransitionTime: "" }],
    });
    expect(buildFailureLine(failed)).toBe("creator exited 51");
  });

  it("falls back to the container when there is no message", () => {
    expect(buildFailureLine(build({ phase: "Failed", failure: { container: "clone", exitCode: 128 } }))).toBe(
      "clone exited 128",
    );
    expect(buildFailureLine(build({ phase: "Failed", failure: { container: "creator", reason: "ImagePullBackOff" } }))).toBe(
      "creator did not run",
    );
  });

  it("falls back to the condition for a build that never had a pod", () => {
    const refused = build({
      phase: "Failed",
      conditions: [
        { type: "Ready", status: "False", reason: "SourceUnreviewed", message: "the commit was not reviewed", lastTransitionTime: "" },
      ],
    });
    expect(buildFailureLine(refused)).toBe("the commit was not reviewed");
  });

  it("says nothing rather than guessing", () => {
    expect(buildFailureLine(build({ phase: "Failed" }))).toBe("");
  });
});
