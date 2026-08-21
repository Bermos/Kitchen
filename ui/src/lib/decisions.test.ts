import { describe, expect, it } from "vitest";
import { firedSummary, shortDigest, unmetRules, verdictTone } from "./decisions";

describe("verdictTone", () => {
  it("maps the three verdicts and tolerates a fourth", () => {
    expect(verdictTone("allowed")).toBe("text-success");
    expect(verdictTone("allowed-with-exception")).toBe("text-warning");
    expect(verdictTone("blocked")).toBe("text-error");
    // A newer server may say something this build has not heard of; it must
    // read as something rather than vanish.
    expect(verdictTone("quarantined")).toBe("text-toned");
  });
});

describe("unmetRules", () => {
  it("keeps what fired and was not waived", () => {
    const unmet = unmetRules([
      { rule: "require-sbom" },
      { rule: "max-severity", waived: true, exception: "incident-441" },
    ]);
    expect(unmet.map((rule) => rule.rule)).toEqual(["require-sbom"]);
    expect(unmetRules(undefined)).toEqual([]);
  });
});

describe("firedSummary", () => {
  it("counts, and always says the waived half", () => {
    expect(firedSummary(undefined)).toBe("no rules fired");
    expect(firedSummary([])).toBe("no rules fired");
    expect(firedSummary([{ rule: "a" }])).toBe("1 rule fired");
    expect(firedSummary([{ rule: "a" }, { rule: "b", waived: true }])).toBe("2 rules fired, 1 waived");
  });
});

describe("shortDigest", () => {
  it("drops the algorithm and keeps enough hex to tell two apart", () => {
    expect(shortDigest(`sha256:${"ab".repeat(32)}`)).toBe("abababababab");
    expect(shortDigest(undefined)).toBe("—");
    expect(shortDigest("")).toBe("—");
  });
});
