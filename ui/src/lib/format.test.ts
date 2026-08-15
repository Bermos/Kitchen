import { describe, expect, it } from "vitest";
import { duration, shortImage, shortSHA, timeAgo, uptime } from "./format";

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

  it("images shorten to their digest", () => {
    expect(shortImage("registry.example.com/kitchen/shop@sha256:ab12f9deadbeef00")).toBe("sha256:ab12f9de…");
    expect(shortImage("registry.example.com/kitchen/shop:latest")).toBe("registry.example.com/kitchen/shop:latest");
    expect(shortImage(undefined)).toBe("—");
  });

  it("shas shorten to seven characters", () => {
    expect(shortSHA("8f3a2c1d0000000")).toBe("8f3a2c1");
    expect(shortSHA(undefined)).toBe("—");
  });
});
