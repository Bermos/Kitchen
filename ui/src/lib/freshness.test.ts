/**
 * What the freshness control claims, and whether a pause actually holds.
 *
 * The sentences the strip says are pinned here rather than read off a screen,
 * for the same reason the policy table is: they are the whole of what a reader
 * is told about whether the numbers in front of them are true.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { effectScope } from "vue";
import {
  PAUSE_EXPIRY_MS,
  ageLabel,
  clockTime,
  freshnessLabel,
  isStale,
  resetScreenFreshness,
  staleNotice,
  useFreshness,
} from "./freshness";
import { useAsync, usePoll } from "./useAsync";

// `useAsync` reaches for the router on a 401 and for `APIError` to recognise
// one, and both of those modules want a browser — a history, and the session
// in local storage. Nothing here signs in or out, so these stubs are the whole
// of what is needed to exercise the polling.
vi.mock("../router", () => ({
  router: { push: () => undefined, currentRoute: { value: { fullPath: "/" } } },
}));
vi.mock("./api", () => ({
  APIError: class extends Error {
    status = 500;
  },
}));

describe("the words", () => {
  it("says the age of what is on the screen", () => {
    expect(freshnessLabel("live", 4, 0)).toBe("Live · 4s ago");
    expect(freshnessLabel("live", 62, 0)).toBe("Live · 1m ago");
    expect(freshnessLabel("loading", null, 0)).toBe("Loading…");
  });

  it("counts what a pause is holding, and admits when it is holding nothing", () => {
    expect(freshnessLabel("paused", 12, 3)).toBe("Paused · 3 changes waiting");
    expect(freshnessLabel("paused", 12, 1)).toBe("Paused · 1 change waiting");
    expect(freshnessLabel("paused", 12, 0)).toBe("Paused · 12s ago");
  });

  it("names the real age when a source has stopped answering", () => {
    expect(freshnessLabel("stale", 22 * 60, 0)).toBe("Stale · 22m ago");
    expect(ageLabel(null)).toBe("never");
  });

  it("banners a stale source with what it is and when it last spoke", () => {
    const at = new Date(2026, 7, 24, 22, 38).getTime();
    expect(staleNotice("the metrics store", 3, at)).toBe(
      "the metrics store did not answer the last 3 polls — these numbers are from 22:38",
    );
    expect(staleNotice("the metrics store", 1, at)).toContain("the last 1 poll —");
    expect(clockTime(null)).toBe("—");
  });
});

describe("staleness", () => {
  const at = 1_000_000;

  it("is failures that have outlasted the interval, not a single missed poll", () => {
    expect(isStale({ updatedAt: at - 10_000, failures: 1, intervalMs: 15_000 }, at)).toBe(false);
    expect(isStale({ updatedAt: at - 20_000, failures: 1, intervalMs: 15_000 }, at)).toBe(true);
  });

  it("is never claimed of a source that is answering, or has never answered", () => {
    expect(isStale({ updatedAt: at - 60_000, failures: 0, intervalMs: 15_000 }, at)).toBe(false);
    expect(isStale({ updatedAt: null, failures: 4, intervalMs: 15_000 }, at)).toBe(false);
    // Nothing polls it, so nothing is late.
    expect(isStale({ updatedAt: at - 600_000, failures: 2, intervalMs: 0 }, at)).toBe(false);
  });
});

describe("a screen's freshness", () => {
  let scope: ReturnType<typeof effectScope>;

  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(new Date(2026, 7, 24, 22, 0, 0));
    resetScreenFreshness();
    scope = effectScope();
  });

  afterEach(() => {
    scope.stop();
    resetScreenFreshness();
    vi.useRealTimers();
  });

  /** One polled source, the way a view builds one. */
  function source<T>(answers: () => Promise<T>, intervalMs = 15_000) {
    return scope.run(() => {
      const state = useAsync(answers);
      usePoll(() => void state.refresh(), intervalMs, () => true);
      return state;
    })!;
  }

  it("is as old as the oldest thing on it", async () => {
    const freshness = useFreshness();
    const slow = source(() => Promise.resolve("b"), 60_000);
    await vi.advanceTimersByTimeAsync(0);
    expect(freshness.state.value).toBe("live");

    // Thirty seconds later a second source loads. The screen does not get
    // younger for it: the metrics tile beside it is still half a minute old.
    await vi.advanceTimersByTimeAsync(30_000);
    const fast = source(() => Promise.resolve("a"), 5_000);
    await vi.advanceTimersByTimeAsync(0);

    expect(slow.data.value).toBe("b");
    expect(fast.data.value).toBe("a");
    expect(freshness.ageSeconds.value).toBe(30);
  });

  it("holds a poll's answer while paused, counts it, and applies it on resume", async () => {
    const freshness = useFreshness();
    let answer = "first";
    const state = source(() => Promise.resolve(answer));
    await vi.advanceTimersByTimeAsync(0);
    expect(state.data.value).toBe("first");

    freshness.pause();
    answer = "second";
    await vi.advanceTimersByTimeAsync(15_000);

    expect(state.data.value, "a paused screen does not move under the reader").toBe("first");
    expect(state.pending.value).toBe("second");
    expect(freshness.queued.value).toBe(1);
    expect(freshness.label.value).toBe("Paused · 1 change waiting");

    freshness.resume();
    expect(state.data.value).toBe("second");
    expect(freshness.queued.value).toBe(0);
    expect(freshness.state.value).toBe("live");
  });

  it("counts nothing when a poll returns what is already on the screen", async () => {
    const freshness = useFreshness();
    const state = source(() => Promise.resolve({ rows: [1, 2] }));
    await vi.advanceTimersByTimeAsync(0);

    freshness.pause();
    await vi.advanceTimersByTimeAsync(45_000);
    expect(freshness.queued.value).toBe(0);
    expect(state.pending.value).toBeNull();
  });

  it("does not age while paused — the age belongs to what is displayed", async () => {
    const freshness = useFreshness();
    let answer = "first";
    source(() => Promise.resolve(answer), 15_000);
    await vi.advanceTimersByTimeAsync(0);

    freshness.pause();
    answer = "second";
    await vi.advanceTimersByTimeAsync(60_000);
    expect(freshness.ageSeconds.value).toBe(60);

    freshness.resume();
    expect(freshness.ageSeconds.value).toBe(0);
  });

  it("lets go by itself, so a screen left paused does not quietly rot", async () => {
    const freshness = useFreshness();
    let answer = "first";
    const state = source(() => Promise.resolve(answer));
    await vi.advanceTimersByTimeAsync(0);

    freshness.pause();
    answer = "second";
    await vi.advanceTimersByTimeAsync(PAUSE_EXPIRY_MS + 1);

    expect(freshness.paused.value).toBe(false);
    expect(state.data.value).toBe("second");
  });

  it("applies an explicit refresh even while paused — a poll is what pause is about", async () => {
    const freshness = useFreshness();
    let answer = "first";
    const state = source(() => Promise.resolve(answer));
    await vi.advanceTimersByTimeAsync(0);

    freshness.pause();
    answer = "second";
    await state.refresh();

    expect(state.data.value).toBe("second");
    expect(freshness.queued.value).toBe(0);
  });

  it("goes stale once a poll has failed for longer than its interval", async () => {
    const freshness = useFreshness();
    let fail = false;
    const state = source(() => (fail ? Promise.reject(new Error("no answer")) : Promise.resolve("ok")), 15_000);
    await vi.advanceTimersByTimeAsync(0);
    expect(freshness.state.value).toBe("live");

    fail = true;
    await vi.advanceTimersByTimeAsync(15_000);
    expect(state.failures.value).toBe(1);
    expect(state.stale.value, "one failed poll on the interval itself is not yet a lie").toBe(false);

    await vi.advanceTimersByTimeAsync(15_000);
    expect(state.failures.value).toBe(2);
    expect(state.stale.value).toBe(true);
    expect(freshness.state.value).toBe("stale");
    expect(state.data.value, "the last good answer is still there — it is the age that changed").toBe("ok");

    fail = false;
    await vi.advanceTimersByTimeAsync(15_000);
    expect(freshness.state.value).toBe("live");
  });

  it("leaves the shell's own pollers out of the screen", async () => {
    const freshness = useFreshness();
    const shell = scope.run(() => {
      const state = useAsync(() => Promise.resolve("sidebar"), { screen: false });
      usePoll(() => void state.refresh(), 30_000, () => true, { screen: false });
      return state;
    })!;
    await vi.advanceTimersByTimeAsync(0);

    expect(freshness.state.value, "nothing the reader is reading has loaded yet").toBe("loading");
    freshness.pause();
    await vi.advanceTimersByTimeAsync(30_000);
    expect(shell.data.value, "the sidebar is navigation, and keeps moving").toBe("sidebar");
    expect(freshness.queued.value).toBe(0);
  });

  it("lets go of the pause when the reader navigates, and keeps the sources it still has", async () => {
    const freshness = useFreshness();
    let answer = "first";
    const state = source(() => Promise.resolve(answer));
    await vi.advanceTimersByTimeAsync(0);
    freshness.pause();
    answer = "second";
    await vi.advanceTimersByTimeAsync(15_000);
    expect(freshness.queued.value).toBe(1);

    // What the router does on every navigation. The screen that is being left
    // gets what it was holding — nothing is thrown away — and the next screen
    // starts live.
    resetScreenFreshness();
    expect(freshness.paused.value).toBe(false);
    expect(state.data.value).toBe("second");

    // A navigation between two addresses of one screen reuses the component,
    // so its sources are still the screen's afterwards.
    expect(freshness.state.value).toBe("live");
    scope.stop();
    expect(freshness.state.value, "and are gone once it is actually torn down").toBe("loading");
  });
});
