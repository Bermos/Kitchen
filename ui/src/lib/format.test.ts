import { describe, expect, it } from "vitest";
import {
  bucketLabel,
  duration,
  formatCores,
  formatDurationSeconds,
  formatMemory,
  parseQuantity,
  shortImage,
  commitSubject,
  shortSHA,
  timeAgo,
  uptime,
} from "./format";

describe("format", () => {
  it("durations read like the mockup", () => {
    expect(duration("2026-01-01T00:00:00Z", "2026-01-01T00:01:43Z")).toBe("1m 43s");
    expect(duration("2026-01-01T00:00:00Z", "2026-01-01T00:00:52Z")).toBe("52s");
    expect(duration(undefined)).toBe("—");
  });

  it("time ago handles the near past and nonsense", () => {
    expect(timeAgo(new Date(Date.now() - 10_000).toISOString())).toBe("just now");
    expect(timeAgo(new Date(Date.now() - 3 * 3600_000).toISOString())).toBe("3 hours ago");
    expect(timeAgo("not a date")).toBe("—");
    expect(timeAgo(undefined)).toBe("—");
  });

  it("uptime coarsens with age", () => {
    expect(uptime(new Date(Date.now() - 43_000).toISOString())).toBe("43s");
    expect(uptime(new Date(Date.now() - 12 * 60_000).toISOString())).toBe("12m");
    expect(uptime(new Date(Date.now() - (2 * 3600 + 14 * 60) * 1000).toISOString())).toBe("2h 14m");
    expect(uptime(new Date(Date.now() - (6 * 24 + 3) * 3600_000).toISOString())).toBe("6d 3h");
    expect(uptime("not a date")).toBe("—");
    expect(uptime(undefined)).toBe("—");
  });

  it("a span of seconds coarsens the same way", () => {
    expect(formatDurationSeconds(42)).toBe("42s");
    expect(formatDurationSeconds(2040)).toBe("34m");
    expect(formatDurationSeconds(3600 + 5 * 60)).toBe("1h 5m");
    expect(formatDurationSeconds(3 * 86400 + 4 * 3600)).toBe("3d 4h");
    expect(formatDurationSeconds(undefined)).toBe("—");
    expect(formatDurationSeconds(-1)).toBe("—");
  });

  it("images shorten to their digest", () => {
    expect(shortImage("registry.example.com/kitchen/shop@sha256:ab12f9deadbeef00")).toBe("sha256:ab12f9de…");
    expect(shortImage("registry.example.com/kitchen/shop:latest")).toBe("registry.example.com/kitchen/shop:latest");
    expect(shortImage(undefined)).toBe("—");
  });

  it("shas shorten to seven characters", () => {
    expect(shortSHA("8f3a2c1d0000000")).toBe("8f3a2c1");
    expect(shortSHA(undefined)).toBe("—");
  });

  it("shows a commit as its subject line alone", () => {
    expect(commitSubject("feat(api): add the route")).toBe("feat(api): add the route");
    expect(commitSubject("feat(api): add the route\n\nA body nothing shows.\n\nCo-Authored-By: somebody")).toBe(
      "feat(api): add the route",
    );
    expect(commitSubject("  fix: trim what surrounds it  \nbody")).toBe("fix: trim what surrounds it");
    expect(commitSubject("")).toBe("");
    expect(commitSubject(undefined)).toBe("");
  });

  it("says how coarse a chart's buckets are", () => {
    expect(bucketLabel(30)).toBe("30s buckets");
    expect(bucketLabel(300)).toBe("5m buckets");
    expect(bucketLabel(3600)).toBe("1h buckets");
    expect(bucketLabel(0)).toBe("");
    expect(bucketLabel(undefined)).toBe("");
  });
});

describe("quantities", () => {
  it("reads the suffixes a node writes", () => {
    expect(parseQuantity("15950m")).toBeCloseTo(15.95);
    expect(parseQuantity("4")).toBe(4);
    expect(parseQuantity("64720076Ki")).toBe(64720076 * 1024);
    expect(parseQuantity("2Gi")).toBe(2 * 1024 ** 3);
    expect(parseQuantity("1M")).toBe(1e6);
  });

  it("answers nothing for what is not a quantity", () => {
    expect(parseQuantity(undefined)).toBeUndefined();
    expect(parseQuantity("")).toBeUndefined();
    expect(parseQuantity("lots")).toBeUndefined();
    expect(parseQuantity("12Xi")).toBeUndefined();
  });

  it("says a CPU quantity as cores", () => {
    expect(formatCores("15950m")).toBe("15.95 cores");
    expect(formatCores("500m")).toBe("0.5 cores");
    expect(formatCores("4")).toBe("4 cores");
    expect(formatCores("1000m")).toBe("1 core");
    expect(formatCores(undefined)).toBe("?");
  });

  it("climbs memory to the largest binary unit above one", () => {
    expect(formatMemory("64720076Ki")).toBe("61.72 GiB");
    expect(formatMemory("2Gi")).toBe("2 GiB");
    expect(formatMemory("1536Mi")).toBe("1.5 GiB");
    expect(formatMemory("512")).toBe("512 B");
    expect(formatMemory("1234567Ki")).toBe("1.18 GiB");
    expect(formatMemory(undefined)).toBe("?");
  });
});
