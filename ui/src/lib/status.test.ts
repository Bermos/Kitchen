import { describe, expect, it } from "vitest";
import { phaseTone, statusDetail, unhealthyConditions } from "./status";

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
});
