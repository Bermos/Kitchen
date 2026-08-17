import { describe, expect, it } from "vitest";
import type { Finding, SignalsAnswer } from "./api";
import {
  evidenceLabel,
  evidenceLocation,
  findingHeadline,
  firstClause,
  hasSomethingToSay,
  problemCount,
  problemsSentence,
  scopeLabel,
  scopePath,
  severityLabel,
  severityRank,
  severityTone,
  sortFindings,
  unreadableSentence,
} from "./signals";

const finding = (over: Partial<Finding> = {}): Finding => ({
  signal: "workload.crashloop",
  severity: "critical",
  scope: { kind: "environment", project: "shop", environment: "shop-production", name: "app" },
  fingerprint: "workload.crashloop/shop/shop-production/app",
  title: "crash-looping",
  detail: "12 restarts in 30m; CrashLoopBackOff: back-off 5m0s restarting failed container",
  since: "2026-08-16T09:31:00Z",
  evidence: "/environments/shop-production?section=workload",
  ...over,
});

describe("severityTone", () => {
  it("never renders a rule that could not be evaluated as health or as alarm", () => {
    expect(severityTone("unknown")).toBe("neutral");
    expect(severityTone("unknown")).not.toBe("success");
    expect(severityTone("unknown")).not.toBe("error");
    expect(severityLabel("unknown")).toBe("Could not evaluate");
  });

  it("keeps the three that did evaluate apart", () => {
    expect(severityTone("critical")).toBe("error");
    expect(severityTone("warning")).toBe("warning");
    expect(severityTone("info")).toBe("info");
  });
});

describe("severityRank", () => {
  it("puts “I could not tell you” below a real warning and above a note", () => {
    expect(severityRank("critical")).toBeGreaterThan(severityRank("warning"));
    expect(severityRank("warning")).toBeGreaterThan(severityRank("unknown"));
    expect(severityRank("unknown")).toBeGreaterThan(severityRank("info"));
  });
});

describe("sortFindings", () => {
  it("orders worst first, then by signal and fingerprint", () => {
    const rows = sortFindings([
      finding({ severity: "info", signal: "edge.unrouted-hosts", fingerprint: "b" }),
      finding({ severity: "critical", signal: "node.silent", fingerprint: "c" }),
      finding({ severity: "warning", signal: "pvc.filling", fingerprint: "a" }),
      finding({ severity: "unknown", signal: "store.disk", fingerprint: "d" }),
    ]);
    expect(rows.map((row) => row.severity)).toEqual(["critical", "warning", "unknown", "info"]);
  });

  it("is stable for two evaluations of an unchanged cluster", () => {
    const rows = sortFindings([
      finding({ signal: "a.b", fingerprint: "a.b/two" }),
      finding({ signal: "a.b", fingerprint: "a.b/one" }),
    ]);
    expect(rows.map((row) => row.fingerprint)).toEqual(["a.b/one", "a.b/two"]);
  });

  it("has nothing to sort when the API sent nothing", () => {
    expect(sortFindings(undefined)).toEqual([]);
    expect(sortFindings(null)).toEqual([]);
  });
});

describe("firstClause", () => {
  it("is the headline number the strip renders in parentheses", () => {
    expect(firstClause("12 restarts in 30m; CrashLoopBackOff: back-off 5m0s")).toBe("12 restarts in 30m");
    expect(findingHeadline(finding())).toBe("crash-looping (12 restarts in 30m)");
  });

  it("leaves a one-clause detail whole", () => {
    expect(firstClause("nothing received for 34m")).toBe("nothing received for 34m");
  });

  it("has nothing to add where a finding carried no detail", () => {
    expect(firstClause(undefined)).toBe("");
    expect(findingHeadline(finding({ detail: "" }))).toBe("crash-looping");
  });
});

describe("scopePath", () => {
  it("joins the fields that are set, in the order the fingerprint uses", () => {
    expect(scopePath({ kind: "environment", project: "shop", environment: "pr-41", name: "web" })).toBe(
      "shop/pr-41/web",
    );
    expect(scopePath({ kind: "node", node: "node-b" })).toBe("node-b");
    expect(scopePath({ kind: "workload", namespace: "kitchen-system", name: "kitchen-collector" })).toBe(
      "kitchen-system/kitchen-collector",
    );
  });

  it("names the platform itself with no path at all", () => {
    expect(scopePath({ kind: "platform" })).toBe("");
    expect(scopeLabel({ kind: "platform" })).toBe("platform");
    expect(scopeLabel({ kind: "node", node: "node-b" })).toBe("node node-b");
  });
});

describe("evidenceLocation", () => {
  it("parses the dashboard path a finding points at", () => {
    expect(evidenceLocation("/platform/nodes?node=node-b")).toEqual({
      path: "/platform/nodes",
      query: { node: "node-b" },
    });
    expect(evidenceLocation("/platform/events?kind=Pod&name=shop-abc&namespace=kitchen-shop")).toEqual({
      path: "/platform/events",
      query: { kind: "Pod", name: "shop-abc", namespace: "kitchen-shop" },
    });
    expect(evidenceLocation("/platform")).toEqual({ path: "/platform", query: {} });
  });

  it("refuses anything that would leave the dashboard", () => {
    expect(evidenceLocation("https://example.com/platform")).toBeNull();
    expect(evidenceLocation("//example.com/platform")).toBeNull();
    expect(evidenceLocation("javascript:alert(1)")).toBeNull();
    expect(evidenceLocation("")).toBeNull();
    expect(evidenceLocation(undefined)).toBeNull();
  });
});

describe("evidenceLabel", () => {
  it("names the screen rather than showing the path", () => {
    expect(evidenceLabel("/platform/nodes?node=node-b")).toBe("Nodes");
    expect(evidenceLabel("/platform/workloads?namespace=kitchen-system&name=x")).toBe("Workloads");
    expect(evidenceLabel("/platform/storage?claim=data")).toBe("Storage");
    expect(evidenceLabel("/platform/edge?host=old.example.com")).toBe("Edge");
    expect(evidenceLabel("/platform/events")).toBe("Events");
    expect(evidenceLabel("/platform")).toBe("Platform");
    expect(evidenceLabel("/environments/shop-production?section=workload")).toBe("Environment");
    expect(evidenceLabel("/builds/shop-42")).toBe("Build");
    expect(evidenceLabel("/projects/shop")).toBe("Project");
  });

  it("has no label for a link it would not follow", () => {
    expect(evidenceLabel("https://example.com")).toBe("");
  });
});

describe("problemCount", () => {
  it("counts what fired and nothing else", () => {
    expect(problemCount({ critical: 1, warning: 2, info: 0 })).toBe(3);
    expect(problemsSentence({ critical: 1, warning: 1, info: 0 })).toBe("2 problems");
    expect(problemsSentence({ critical: 1, warning: 0, info: 0 })).toBe("1 problem");
    expect(problemsSentence({ critical: 0, warning: 0, info: 0 })).toBe("no problems");
    expect(problemsSentence(undefined)).toBe("no problems");
  });
});

describe("unreadableSentence", () => {
  it("says nobody looked, rather than letting an empty list say nothing is wrong", () => {
    const sentence = unreadableSentence([{ input: "http_requests_1m", reason: "the query failed" }], 0);
    expect(sentence).toContain("could not be read");
    expect(sentence).toContain("not checked at all");
  });

  it("says the list is incomplete when something did fire", () => {
    const sentence = unreadableSentence([{ input: "otel_logs", reason: "unreachable" }], 2);
    expect(sentence).toContain("incomplete");
  });

  it("says nothing at all when everything was readable", () => {
    expect(unreadableSentence([], 0)).toBe("");
    expect(unreadableSentence(undefined, 3)).toBe("");
  });
});

describe("hasSomethingToSay", () => {
  const answer = (over: Partial<SignalsAnswer>): SignalsAnswer => ({
    items: [],
    counts: { critical: 0, warning: 0, info: 0 },
    evaluatedAt: "2026-08-16T10:00:00Z",
    ...over,
  });

  it("is quiet only when nothing fired and nothing went unread", () => {
    expect(hasSomethingToSay(answer({}))).toBe(false);
    expect(hasSomethingToSay(answer({ items: [finding()] }))).toBe(true);
    // The load-bearing one: no findings, but an input nobody could read is not
    // a clean environment.
    expect(hasSomethingToSay(answer({ unreadable: [{ input: "http_requests_1m", reason: "down" }] }))).toBe(true);
    expect(hasSomethingToSay(null)).toBe(false);
  });
});
