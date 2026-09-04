import { computed, onScopeDispose, ref, shallowRef, type ComputedRef, type Ref, type ShallowRef } from "vue";
import { APIError } from "./api";
import { currentPoll, duringPoll, isStale, useClock, useFreshness } from "./freshness";
import { router } from "../router";

export interface AsyncState<T> {
  data: ShallowRef<T | null>;
  error: Ref<string | null>;
  loading: Ref<boolean>;
  refresh: () => Promise<void>;
  /** When the data now on the screen was fetched, ms epoch; `null` until the
   * first answer. It does not move while a pause holds newer data — the age
   * belongs to what is displayed, not to the last request that succeeded. */
  updatedAt: Ref<number | null>;
  /** Polls that have failed since the last answer. */
  failures: Ref<number>;
  /** True once those failures have outlasted this source's poll interval: the
   * point at which the numbers on the screen have stopped being true and a
   * view should say `—` rather than repeat them. */
  stale: ComputedRef<boolean>;
  /** Newer data held behind the screen's pause, `null` when there is none.
   * A view that can say something useful about it — "N newer entries queued"
   * — reads it; everything else lets the freshness control count it. */
  pending: ShallowRef<T | null>;
}

/** Whether two answers differ. A poll that returns what is already displayed
 * is not a change waiting, and counting it as one would put a number on the
 * pause button that never goes down. */
function differs<T>(next: T, shown: T): boolean {
  try {
    return JSON.stringify(next) !== JSON.stringify(shown);
  } catch {
    return true;
  }
}

/** Fetch-and-hold for a view: one load on setup, `refresh()` for polling and
 * after writes. A 401 sends the browser back through the login.
 *
 * Every load also reports to the screen's freshness (`lib/freshness.ts`), so
 * the header can say how old the screen is, hold it still while somebody
 * reads, and admit when a source has stopped answering. `screen: false` opts
 * out — the shell's own pollers do, because the sidebar is navigation rather
 * than something being read. */
export function useAsync<T>(
  fetcher: () => Promise<T>,
  { immediate = true, screen = true } = {},
): AsyncState<T> {
  const data = shallowRef<T | null>(null);
  const pending = shallowRef<T | null>(null);
  const error = ref<string | null>(null);
  const loading = ref(false);
  const updatedAt = ref<number | null>(null);
  const pendingAt = ref<number | null>(null);
  const failures = ref(0);
  const intervalMs = ref(0);
  const holding = ref(false);
  const clock = useClock();
  const stale = computed(() =>
    isStale({ updatedAt: updatedAt.value, failures: failures.value, intervalMs: intervalMs.value }, clock.value),
  );

  /** Show what the pause was holding. */
  function apply() {
    if (pending.value !== null) {
      data.value = pending.value;
      updatedAt.value = pendingAt.value;
      pending.value = null;
      pendingAt.value = null;
    }
    holding.value = false;
  }

  // The screen this source belongs to, taken once: a source stays with the
  // screen it was created on even if the reader has since navigated away.
  const freshness = useFreshness();
  let alive = true;
  if (screen) {
    onScopeDispose(freshness.register({ updatedAt, failures, intervalMs, holding, apply }));
  }
  onScopeDispose(() => {
    alive = false;
  });

  const refresh = async () => {
    // Read at entry, synchronously, while still inside the poll's callback:
    // this is what tells a timer's refresh from somebody pressing Refresh,
    // and it is where a source learns how often it is polled.
    const poll = currentPoll();
    if (poll) intervalMs.value = poll.intervalMs;
    const held = screen && poll !== null && freshness.paused.value;

    loading.value = true;
    try {
      const result = await fetcher();
      if (!alive) return;
      error.value = null;
      failures.value = 0;
      if (held && data.value !== null && differs(result, data.value)) {
        pending.value = result;
        pendingAt.value = Date.now();
        holding.value = true;
        return;
      }
      data.value = result;
      updatedAt.value = Date.now();
      pending.value = null;
      pendingAt.value = null;
      holding.value = false;
    } catch (err) {
      if (!alive) return;
      if (err instanceof APIError && err.status === 401) {
        void router.push({ name: "login", query: { returnTo: router.currentRoute.value.fullPath } });
        return;
      }
      failures.value += 1;
      error.value = err instanceof Error ? err.message : String(err);
    } finally {
      if (alive) loading.value = false;
    }
  };

  if (immediate) void refresh();
  return { data, error, loading, refresh, updatedAt, failures, stale, pending };
}

/** Poll while `active()` says so — running builds, deploying environments.
 *
 * The callback runs inside `duringPoll`, which is how the `useAsync` refreshes
 * it triggers know they are polls: they queue behind a pause instead of moving
 * the screen, and they record the interval that decides when this source has
 * gone stale. `screen: false` runs the callback outside all of that, for the
 * shell's pollers. */
export function usePoll(
  callback: () => void,
  intervalMs: number,
  active: () => boolean,
  { screen = true } = {},
): void {
  const timer = setInterval(() => {
    if (!active()) return;
    if (screen) duringPoll(intervalMs, callback);
    else callback();
  }, intervalMs);
  onScopeDispose(() => clearInterval(timer));
}
