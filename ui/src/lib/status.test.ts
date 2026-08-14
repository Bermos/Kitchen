import { describe, expect, it } from "vitest";
import type { Environment, ReleaseHistoryEntry } from "./api";
import { phaseTone, releaseHistoryLabel, statusDetail, unhealthyConditions } from "./status";

describe("status", () => {
  it("maps every documented phase to a tone", () => {
    expect(phaseTone("Succeeded")).toBe("success");
    expect(phaseTone("Live")).toBe("success");
    expect(phaseTone("Running")).toBe("warning");
    expect(phaseTone("Deploying")).toBe("warning");
    expect(phaseTone("Failed")).toBe("error");
    expect(phaseTone("Degraded")).toBe("error");
    expect(phaseTone("Queued")).toBe("neutral");
    expect(phaseTone("SomethingNew")).toBe("neutral");
    expect(phaseTone(undefined)).toBe("neutral");
  });

  it("surfaces the condition that is not where it should be", () => {
    const conditions = [
      { type: "Ready", status: "True", lastTransitionTime: "" },
      { type: "RouteProgrammed", status: "False", reason: "NoGate", message: "previews need the gate", lastTransitionTime: "" },
    ];
    expect(unhealthyConditions(conditions)).toHaveLength(1);
    expect(statusDetail(conditions)).toBe("previews need the gate");
    expect(statusDetail([])).toBe("");
    expect(statusDetail(undefined)).toBe("");
  });

  it("labels past releases from the environment's history", () => {
    const entry = (release: string, reason: ReleaseHistoryEntry["reason"]): ReleaseHistoryEntry => ({
      release,
      from: "2026-08-13T09:00:00Z",
      to: "2026-08-14T09:00:00Z",
      reason,
    });
    // Newest first, like the API sends it: rel-3 left most recently.
    const environment = {
      history: [entry("rel-3", "promoted"), entry("rel-2", "rolledBack"), entry("rel-1", "superseded")],
    } as Environment;

    expect(releaseHistoryLabel("rel-3", environment)).toBe("Previous");
    expect(releaseHistoryLabel("rel-2", environment)).toBe("Rolled back");
    expect(releaseHistoryLabel("rel-1", environment)).toBe("Superseded");
    // Never current as far as the history knows — no label.
    expect(releaseHistoryLabel("rel-0", environment)).toBe("");
    expect(releaseHistoryLabel("rel-3", undefined)).toBe("");
  });

  it("does not call a rolled-back release Previous, even in first position", () => {
    const environment = {
      history: [
        { release: "rel-2", from: "", to: "", reason: "rolledBack" },
        { release: "rel-1", from: "", to: "", reason: "promoted" },
      ],
    } as Environment;
    expect(releaseHistoryLabel("rel-2", environment)).toBe("Rolled back");
    expect(releaseHistoryLabel("rel-1", environment)).toBe("Superseded");
  });
});
