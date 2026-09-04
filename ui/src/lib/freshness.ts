/**
 * How old the screen is, and who is allowed to move it.
 *
 * The dashboard polls: five clocks were running on the overview alone, all of
 * them silent. Nothing said when what you were reading was true, a failed poll
 * looked exactly like a fresh one because `useAsync` keeps the last good data,
 * and a table could reorder itself under the cursor mid-click. This is the one
 * object that answers all three, and it is here rather than in a view so that
 * every screen inherits it from `useAsync`/`usePoll` instead of each screen
 * inventing an answer.
 *
 * Three decisions are worth naming, because they are the ones somebody will
 * want to change:
 *
 * - **The screen is as old as its oldest part.** A screen is usually three or
 *   four polled sources; the age in the header is the oldest of them, not the
 *   newest. The reader is asking "can I trust what is in front of me", and the
 *   weakest part is the honest answer.
 * - **Pause holds what is displayed, not what is fetched.** Polling carries
 *   on while a screen is paused; the results are held, counted, and applied on
 *   resume. That is what makes "3 changes waiting" possible, and it is why the
 *   age does not tick forward while paused — the age belongs to the data on
 *   the screen, not to the last request that succeeded.
 * - **An explicit refresh is not a poll.** A poll is the thing pause exists to
 *   stop. Somebody pressing Refresh is asking to see it now, so a refresh that
 *   did not come from a timer is applied even while paused. `duringPoll` is
 *   how a source tells the two apart, and how it learns the interval it is
 *   polled at without every call site having to say it twice.
 *
 * `docs/UI.md` carries the rest — where the control sits, why pause is per
 * screen, and why it expires.
 */
import { computed, onScopeDispose, ref, shallowRef, type ComputedRef, type Ref } from "vue";
import { formatDurationSeconds } from "./format";

/**
 * How long a pause lasts before it lets go by itself.
 *
 * A pause that outlives the reading it was for is the stale screen this whole
 * control exists to prevent, wearing a label that says it is fine. Five
 * minutes is long enough to read a failing build's log excerpt and short
 * enough that a screen left open over lunch is live again when its owner
 * comes back.
 */
export const PAUSE_EXPIRY_MS = 5 * 60_000;

export type FreshnessState = "loading" | "live" | "paused" | "stale";

/** One polled thing on the screen: in practice, one `useAsync`. */
export interface FreshnessSource {
  /** When the data now on the screen was fetched, ms epoch. `null` until it
   * first answers. It does not move while newer data is held. */
  updatedAt: Ref<number | null>;
  /** Polls that have failed since the last answer. */
  failures: Ref<number>;
  /** The interval this source is polled at, learned from `usePoll`. `0` until
   * a poll has actually run — a source nobody polls is never stale. */
  intervalMs: Ref<number>;
  /** Whether newer data is being held back by a pause. */
  holding: Ref<boolean>;
  /** Show what is being held. */
  apply: () => void;
}

/** The state a screen's freshness control is in, and what it says. */
export interface ScreenFreshness {
  state: ComputedRef<FreshnessState>;
  /** Age of the oldest thing on the screen, in seconds. `null` before the
   * first answer. */
  ageSeconds: ComputedRef<number | null>;
  /** Sources holding newer data behind the pause. */
  queued: ComputedRef<number>;
  paused: Ref<boolean>;
  /** True when a poll has failed for longer than its own interval. */
  stale: ComputedRef<boolean>;
  /** `Live · 4s ago`, `Paused · 3 changes waiting`, `Stale · 22m ago`. */
  label: ComputedRef<string>;
  pause: () => void;
  /** Let go, and show everything that was held. */
  resume: () => void;
  register: (source: FreshnessSource) => () => void;
}

// ---------------------------------------------------------------------------
// The clock
// ---------------------------------------------------------------------------

// One second-hand for the whole dashboard rather than one per age on the
// screen. It runs while anything is watching it and stops when nothing is, so
// a test — or a background tab's last torn-down view — leaves no timer behind.
const now = ref(Date.now());
let ticker: ReturnType<typeof setInterval> | null = null;
let watching = 0;

/** The shared second-hand, held for as long as the calling scope lives. */
export function useClock(): Ref<number> {
  watching += 1;
  if (ticker === null) {
    now.value = Date.now();
    ticker = setInterval(() => {
      now.value = Date.now();
    }, 1000);
  }
  onScopeDispose(() => {
    watching -= 1;
    if (watching <= 0 && ticker !== null) {
      clearInterval(ticker);
      ticker = null;
      watching = 0;
    }
  }, true);
  return now;
}

// ---------------------------------------------------------------------------
// Telling a poll from a refresh
// ---------------------------------------------------------------------------

let polling: { intervalMs: number } | null = null;

/**
 * Run a poll's callback, saying so. A `useAsync` refresh that starts inside
 * this call knows two things it cannot otherwise know: that it is a poll
 * rather than somebody pressing Refresh, and how often it is being polled.
 *
 * The callback is synchronous up to its first `await`, which is where every
 * `refresh()` reads this — so the context is exact rather than ambient.
 */
export function duringPoll(intervalMs: number, run: () => void): void {
  const previous = polling;
  polling = { intervalMs };
  try {
    run();
  } finally {
    polling = previous;
  }
}

/** The poll a refresh is running inside, or `null` for an explicit one. */
export function currentPoll(): { intervalMs: number } | null {
  return polling;
}

// ---------------------------------------------------------------------------
// Staleness, and the words for it
// ---------------------------------------------------------------------------

/**
 * A source is stale once its failures have outlasted its own poll interval:
 * one missed poll on a 60 s metrics fetch is a blip, a minute of them is a
 * number that has stopped being true. A source that has never answered is not
 * stale — there is nothing on the screen to be wrong about, and the view's own
 * error alert says so.
 */
export function isStale(
  source: { updatedAt: number | null; failures: number; intervalMs: number },
  at: number,
): boolean {
  if (source.updatedAt === null || source.failures === 0 || source.intervalMs === 0) return false;
  return at - source.updatedAt > source.intervalMs;
}

/** `4s ago`, `22m ago`, `2h 14m ago` — the coarseness a status line reads at. */
export function ageLabel(seconds: number | null): string {
  if (seconds === null) return "never";
  return `${formatDurationSeconds(Math.max(0, seconds))} ago`;
}

/** What the strip says. */
export function freshnessLabel(state: FreshnessState, ageSeconds: number | null, queued: number): string {
  switch (state) {
    case "loading":
      return "Loading…";
    case "paused":
      return queued > 0
        ? `Paused · ${queued} change${queued === 1 ? "" : "s"} waiting`
        : `Paused · ${ageLabel(ageSeconds)}`;
    case "stale":
      return `Stale · ${ageLabel(ageSeconds)}`;
    default:
      return `Live · ${ageLabel(ageSeconds)}`;
  }
}

/** A wall clock reading, for a banner that has to name a real time. */
export function clockTime(at: number | null): string {
  if (at === null) return "—";
  const when = new Date(at);
  return `${String(when.getHours()).padStart(2, "0")}:${String(when.getMinutes()).padStart(2, "0")}`;
}

/**
 * The banner over data that has stopped being true: what did not answer, how
 * many times, and when the numbers under it are actually from.
 *
 *     the metrics store did not answer the last 3 polls — these numbers are from 22:38
 */
export function staleNotice(what: string, failures: number, updatedAt: number | null): string {
  const polls = `the last ${failures} poll${failures === 1 ? "" : "s"}`;
  return `${what} did not answer ${polls} — these numbers are from ${clockTime(updatedAt)}`;
}

// ---------------------------------------------------------------------------
// The screen's freshness
// ---------------------------------------------------------------------------

function createFreshness(): ScreenFreshness {
  const sources = shallowRef<FreshnessSource[]>([]);
  const paused = ref(false);
  let expiry: ReturnType<typeof setTimeout> | null = null;

  const answered = computed(() => sources.value.filter((s) => s.updatedAt.value !== null));
  const oldest = computed<number | null>(() => {
    const times = answered.value.map((s) => s.updatedAt.value as number);
    return times.length ? Math.min(...times) : null;
  });
  const ageSeconds = computed<number | null>(() =>
    oldest.value === null ? null : Math.max(0, Math.round((now.value - oldest.value) / 1000)),
  );
  const queued = computed(() => sources.value.filter((s) => s.holding.value).length);
  const stale = computed(() =>
    sources.value.some((s) =>
      isStale({ updatedAt: s.updatedAt.value, failures: s.failures.value, intervalMs: s.intervalMs.value }, now.value),
    ),
  );
  const state = computed<FreshnessState>(() => {
    if (paused.value) return "paused";
    if (stale.value) return "stale";
    if (oldest.value === null) return "loading";
    return "live";
  });
  const label = computed(() => freshnessLabel(state.value, ageSeconds.value, queued.value));

  function clearExpiry() {
    if (expiry !== null) {
      clearTimeout(expiry);
      expiry = null;
    }
  }

  function resume() {
    clearExpiry();
    paused.value = false;
    for (const source of sources.value) source.apply();
  }

  function pause() {
    clearExpiry();
    paused.value = true;
    expiry = setTimeout(resume, PAUSE_EXPIRY_MS);
  }

  function register(source: FreshnessSource): () => void {
    sources.value = [...sources.value, source];
    return () => {
      sources.value = sources.value.filter((s) => s !== source);
    };
  }

  return { state, ageSeconds, queued, paused, stale, label, pause, resume, register };
}

const screen = createFreshness();

/**
 * The freshness of the screen being looked at.
 *
 * It is one object per screen rather than one per view *component*, because a
 * screen is a view plus the panels inside it and they all poll: the age in the
 * header covers the environment card's traffic fetch and the log viewer's tail
 * as well as the view's own list. One screen is mounted at a time, so a shared
 * object is the screen's — its sources join as they mount and leave as they
 * are torn down, which is the whole of what "this screen" means here.
 */
export function useFreshness(): ScreenFreshness {
  return screen;
}

/**
 * Let go of a pause, because the reader has moved on.
 *
 * Pause is per screen — deliberately. It exists so that what somebody is
 * reading does not move under the cursor, and navigating is them saying they
 * have finished reading it. A pause carried from screen to screen would be a
 * global "stop updating" nobody asked for, on screens whose data they have not
 * looked at yet. The router calls this on every navigation; `AppShell`'s own
 * pollers are outside all of it, because the sidebar is navigation rather than
 * something being read.
 *
 * The object itself is not replaced: a navigation between two addresses of the
 * same screen — one project to another — reuses the component, and its sources
 * with it. They leave when their scope does.
 */
export function resetScreenFreshness(): void {
  screen.resume();
}
