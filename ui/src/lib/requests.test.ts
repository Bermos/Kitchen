import { describe, expect, it } from "vitest";
import type { PlatformEvent } from "./api";
import {
  anyHTTP2,
  bucketFor,
  correlatedLogsQuery,
  correlationWindow,
  deployMarks,
  edgeState,
  formatLatency,
  formatPercent,
  formatRate,
  formatSaturation,
  isHTTP2,
  MAX_RAW_RETENTION_DAYS,
  rawRetentionDays,
  rawRetentionStart,
  saturation,
  statusTone,
} from "./requests";

describe("edgeState", () => {
  it("tells an environment off the edge from a quiet one", () => {
    const off = edgeState({ routed: false, message: "no HTTP traffic reaches this environment…" }, 0);
    expect(off.kind).toBe("off-edge");
    expect(off.message).toBe("no HTTP traffic reaches this environment…");

    const quiet = edgeState({ routed: true }, 0);
    expect(quiet.kind).toBe("quiet");
    expect(quiet.message).toBe("");
  });

  it("says so itself when the API sent no sentence", () => {
    expect(edgeState({ routed: false }, 0).message).toContain("through the platform's edge");
  });

  it("never reads a failed check as being off the edge", () => {
    const unchecked = edgeState({ routed: true, message: "could not be read: forbidden" }, 0);
    expect(unchecked.kind).toBe("quiet");
    expect(unchecked.caveat).toBe("could not be read: forbidden");
  });

  it("is serving once anything was asked of it", () => {
    const serving = edgeState({ routed: true }, 3600);
    expect(serving.kind).toBe("serving");
    expect(serving.caveat).toBe("");
  });

  it("treats a missing edge as one that answered nothing about itself", () => {
    expect(edgeState(undefined, 12).kind).toBe("serving");
    expect(edgeState(undefined, 0).kind).toBe("quiet");
  });
});

describe("formatLatency", () => {
  it("coarsens as it grows", () => {
    expect(formatLatency(0.4)).toBe("0.4 ms");
    expect(formatLatency(9.63)).toBe("9.6 ms");
    expect(formatLatency(240)).toBe("240 ms");
    expect(formatLatency(1240)).toBe("1.24 s");
    expect(formatLatency(30_000)).toBe("30 s");
  });

  it("has nothing to say about a latency nothing measured", () => {
    expect(formatLatency(0)).toBe("—");
    expect(formatLatency(undefined)).toBe("—");
    expect(formatLatency(Number.NaN)).toBe("—");
  });
});

describe("formatRate", () => {
  it("reads per second above one and per minute below it", () => {
    expect(formatRate(42)).toBe("42/s");
    expect(formatRate(1.24)).toBe("1.2/s");
    expect(formatRate(0.11)).toBe("6.6/min");
    expect(formatRate(0.002)).toBe("0.1/min");
  });

  it("does not round a trickle away to nothing", () => {
    expect(formatRate(0.0001)).toBe("<0.1/min");
    expect(formatRate(0)).toBe("0/s");
  });
});

describe("formatPercent", () => {
  it("keeps enough precision to be believed", () => {
    expect(formatPercent(0.01)).toBe("1.00%");
    expect(formatPercent(0.125)).toBe("12.5%");
    expect(formatPercent(0)).toBe("0%");
  });

  it("never rounds a real error rate to zero", () => {
    expect(formatPercent(0.00001)).toBe("<0.01%");
  });
});

describe("statusTone", () => {
  it("counts only 5xx as the service's own failure", () => {
    expect(statusTone(503)).toBe("error");
    expect(statusTone(404)).toBe("warning");
    expect(statusTone(304)).toBe("info");
    expect(statusTone(200)).toBe("success");
    expect(statusTone(undefined)).toBe("neutral");
  });
});

describe("isHTTP2", () => {
  it("recognises the protocols gRPC hides inside", () => {
    expect(isHTTP2("HTTP/2")).toBe(true);
    expect(isHTTP2("h2")).toBe(true);
    expect(isHTTP2("h2c")).toBe(true);
    expect(isHTTP2("gRPC")).toBe(true);
  });

  it("leaves HTTP/1.1 alone", () => {
    expect(isHTTP2("HTTP/1.1")).toBe(false);
    expect(isHTTP2("")).toBe(false);
    expect(isHTTP2(undefined)).toBe(false);
  });

  it("flags a listing as soon as one row was HTTP/2", () => {
    const row = { timestamp: "", method: "GET", path: "/", status: 200, durationMs: 1 };
    expect(anyHTTP2([{ ...row, protocol: "HTTP/1.1" }, { ...row, protocol: "HTTP/2" }])).toBe(true);
    expect(anyHTTP2([{ ...row, protocol: "HTTP/1.1" }])).toBe(false);
    expect(anyHTTP2(null)).toBe(false);
  });
});

describe("rawRetentionDays", () => {
  // The store keeps one knob and derives the rest: raw rows at
  // `min(7, retentionDays)`, the rollups at the whole of it. A screen that
  // hardcoded seven told an installation retaining three days that its listing
  // should have reached back a week.
  it("is the shorter of a week and the platform's retention", () => {
    expect(rawRetentionDays(30)).toBe(MAX_RAW_RETENTION_DAYS);
    expect(rawRetentionDays(7)).toBe(7);
    expect(rawRetentionDays(3)).toBe(3);
    expect(rawRetentionDays(1)).toBe(1);
  });

  it("assumes the cap where the retention has not been read, which is the widest it can be", () => {
    expect(rawRetentionDays(undefined)).toBe(MAX_RAW_RETENTION_DAYS);
    expect(rawRetentionDays(null)).toBe(MAX_RAW_RETENTION_DAYS);
    expect(rawRetentionDays(0)).toBe(MAX_RAW_RETENTION_DAYS);
  });

  it("starts the protocol probe as far back as raw rows go", () => {
    const now = Date.parse("2026-08-16T10:00:00.000Z");
    expect(rawRetentionStart(30, now)).toBe("2026-08-09T10:00:00.000Z");
    expect(rawRetentionStart(2, now)).toBe("2026-08-14T10:00:00.000Z");
  });
});

describe("correlationWindow", () => {
  it("is the instant plus and minus thirty seconds", () => {
    expect(correlationWindow("2026-08-16T09:59:58.000Z")).toEqual({
      since: "2026-08-16T09:59:28.000Z",
      until: "2026-08-16T10:00:28.000Z",
    });
  });

  it("has no window around a timestamp it cannot read", () => {
    expect(correlationWindow("not a date")).toBeNull();
  });
});

describe("correlatedLogsQuery", () => {
  it("pre-fills the log query the Observability view reads", () => {
    expect(correlatedLogsQuery("shop-production", "2026-08-16T09:59:58.000Z")).toEqual({
      q: 'environment:"shop-production"',
      since: "2026-08-16T09:59:28.000Z",
      until: "2026-08-16T10:00:28.000Z",
    });
  });

  it("offers no link where there is no moment to centre on", () => {
    expect(correlatedLogsQuery("shop-production", "")).toBeNull();
  });
});

describe("bucketFor", () => {
  const points = [
    { start: "2026-08-16T10:00:00.000Z" },
    { start: "2026-08-16T10:05:00.000Z" },
    { start: "2026-08-16T10:10:00.000Z" },
  ];

  it("snaps a moment to the bucket that contains it", () => {
    expect(bucketFor("2026-08-16T10:07:13.000Z", points, 300)).toBe("2026-08-16T10:05:00.000Z");
    expect(bucketFor("2026-08-16T10:05:00.000Z", points, 300)).toBe("2026-08-16T10:05:00.000Z");
  });

  it("has no bucket for a moment outside the window", () => {
    expect(bucketFor("2026-08-16T09:58:00.000Z", points, 300)).toBeNull();
    expect(bucketFor("2026-08-16T10:20:00.000Z", points, 300)).toBeNull();
    expect(bucketFor("2026-08-16T10:05:00.000Z", [], 300)).toBeNull();
  });
});

describe("deployMarks", () => {
  const points = [
    { start: "2026-08-16T10:00:00.000Z" },
    { start: "2026-08-16T10:05:00.000Z" },
    { start: "2026-08-16T10:10:00.000Z" },
  ];
  const event = (type: string, timestamp: string): PlatformEvent => ({ type, timestamp, message: type });

  it("puts a deploy on the bucket it happened in", () => {
    expect(deployMarks([event("release.promoted", "2026-08-16T10:06:41.000Z")], points, 300)).toEqual([
      { start: "2026-08-16T10:05:00.000Z", count: 1, tone: "stroke-info/70", label: "deploy" },
    ]);
  });

  it("ignores everything the feed carries that is not a deploy", () => {
    expect(deployMarks([event("build.succeeded", "2026-08-16T10:06:41.000Z")], points, 300)).toEqual([]);
  });

  it("collapses a bucket's deploys into one mark, and a rollback colours it", () => {
    const marks = deployMarks(
      [
        event("release.promoted", "2026-08-16T10:06:00.000Z"),
        event("release.rolledBack", "2026-08-16T10:08:00.000Z"),
      ],
      points,
      300,
    );
    expect(marks).toEqual([
      { start: "2026-08-16T10:05:00.000Z", count: 2, tone: "stroke-warning/70", label: "rollback" },
    ]);
  });

  it("drops a deploy from outside the window rather than pinning it to an end", () => {
    expect(deployMarks([event("release.promoted", "2026-08-16T08:00:00.000Z")], points, 300)).toEqual([]);
  });
});

describe("saturation", () => {
  it("is usage against the limit, in percent", () => {
    expect(saturation(512, 1024)).toBe(50);
    expect(formatSaturation(saturation(512, 1024))).toBe("50%");
    expect(formatSaturation(saturation(51, 1024))).toBe("5.0%");
  });

  it("has no answer where nothing capped the workload", () => {
    expect(saturation(512, 0)).toBeNull();
    expect(saturation(512, undefined)).toBeNull();
    expect(formatSaturation(null)).toBe("—");
  });
});
