/**
 * The request surface's own vocabulary: how the edge's numbers are rendered,
 * what the three shapes of `edge` mean on screen, and how a request row finds
 * the log lines that were written around it.
 *
 * It lives here rather than in the components because these are the decisions
 * that have to be the same in every place they appear — the tile, the route
 * table, the request row and the crash report all render a latency and an error
 * rate, and two of them rounding differently is two of them disagreeing.
 */

import type { EdgeStatus, HealthChecks, PlatformEvent, RequestRow } from "./api";
import { renderClause } from "./logquery";
import type { Tone } from "./status";

/**
 * What the request surface is looking at, which is three different answers that
 * all come back as zeroes:
 *
 *   - `off-edge` — nothing publishes this environment on the shared Gateway, so
 *     there is nothing there to observe. A worker is not broken for having no
 *     HTTP traffic, and four empty charts would describe the platform rather
 *     than the application.
 *   - `quiet` — it is on the edge and nothing was asked of it in this window.
 *   - `serving` — it is on the edge and it served something.
 *
 * `caveat` is separate from all three: `routed` true carrying a message means
 * the check could not be run (Gateway API CRDs a version behind, a ClusterRole
 * that may not read routes). That is not evidence about the application, so the
 * numbers stand and the screen says the check did not happen — declaring an
 * application off the edge on the strength of a failed read is the loud way to
 * be wrong.
 */
export type EdgeKind = "off-edge" | "quiet" | "serving";

export interface EdgeState {
  kind: EdgeKind;
  /** The sentence the screen leads with, for `off-edge`. */
  message: string;
  /** Why the platform is not sure, when it is not. Never a claim about traffic. */
  caveat: string;
}

export function edgeState(edge: EdgeStatus | undefined, requests: number | undefined): EdgeState {
  if (edge && !edge.routed) {
    return {
      kind: "off-edge",
      message: edge.message || NO_EDGE_MESSAGE,
      caveat: "",
    };
  }
  return {
    kind: requests && requests > 0 ? "serving" : "quiet",
    message: "",
    caveat: edge?.message ?? "",
  };
}

/**
 * What the requests section says about the platform's own health checks.
 *
 * The API decides which route is one — the path the project declared as its
 * health check — and answers every read with the route and whether that read
 * left it out. Two rules turn that into a line on the screen:
 *
 *   - **Nothing is said where nothing is declared.** A project with no HTTP
 *     health check has no probes to discount, and a note explaining an
 *     exclusion that is not happening is worse than no note.
 *   - **Nothing is said while a route is selected.** The filter chip above
 *     already says exactly what the numbers are of, and the API counts the
 *     health route when it is the route that was asked for — so the offer to
 *     count them would be an offer to change nothing.
 */
export interface HealthCheckNote {
  /** The route the platform's own check asks for, as the rows spell it. */
  route: string;
  /** Whether the numbers beside the note left it out. */
  excluded: boolean;
}

export function healthCheckNote(
  health: HealthChecks | undefined,
  selectedRoute: string | null | undefined,
): HealthCheckNote | null {
  if (!health?.route || selectedRoute) return null;
  return { route: health.route, excluded: health.excluded };
}

/** What the screen says when the platform is sure nothing publishes an
 * environment, if the API did not send its own longer sentence. */
export const NO_EDGE_MESSAGE =
  "No HTTP traffic reaches this environment through the platform's edge";

/**
 * One tile of the golden-signal header: a number, the shape of the window
 * beside it, and a line saying what it is of.
 *
 * The header is a list of these rather than four fixed tiles because an
 * environment the edge does not reach gets a *different four* — log volume,
 * error lines, restarts, saturation — and the honest degrade is a different
 * list, not a header with holes in it.
 */
export interface SignalTile {
  label: string;
  value: string;
  detail?: string;
  points?: number[];
  tone?: string;
}

/** Latency, at the coarseness a number in a table is read at: `0.4 ms`,
 * `9.6 ms`, `240 ms`, `1.24 s`. Anything that never happened is a dash rather
 * than a zero, because "as fast as zero milliseconds" is not a measurement. */
export function formatLatency(ms: number | undefined): string {
  if (ms === undefined || Number.isNaN(ms) || ms <= 0) return "—";
  if (ms < 10) return `${ms.toFixed(1)} ms`;
  if (ms < 1000) return `${Math.round(ms)} ms`;
  return `${(ms / 1000).toFixed(2).replace(/\.?0+$/, "")} s`;
}

/**
 * Traffic as a rate. Under one a second, per-minute reads better than a string
 * of leading zeros — `6.6/min` is a number someone can hold, `0.11/s` is one
 * they have to convert.
 */
export function formatRate(perSecond: number | undefined): string {
  if (perSecond === undefined || Number.isNaN(perSecond) || perSecond <= 0) return "0/s";
  if (perSecond >= 10) return `${Math.round(perSecond)}/s`;
  if (perSecond >= 1) return `${perSecond.toFixed(1)}/s`;
  const perMinute = perSecond * 60;
  if (perMinute >= 10) return `${Math.round(perMinute)}/min`;
  if (perMinute >= 0.1) return `${perMinute.toFixed(1)}/min`;
  return "<0.1/min";
}

/** A rate the API gives as a fraction, as a percentage. A rate that is small
 * but not zero says so rather than rounding itself away: `0.00%` next to a
 * non-zero error count is the screen calling the user a liar. */
export function formatPercent(rate: number | undefined): string {
  if (rate === undefined || Number.isNaN(rate) || rate <= 0) return "0%";
  const percent = rate * 100;
  if (percent < 0.01) return "<0.01%";
  if (percent < 10) return `${percent.toFixed(2)}%`;
  return `${percent.toFixed(1)}%`;
}

/** The tone of one answer. 5xx is the service's fault and the only one the
 * signals count as an error; a 4xx is the caller's, and worth seeing without
 * being alarming. */
export function statusTone(status: number | undefined): Tone {
  if (!status) return "neutral";
  if (status >= 500) return "error";
  if (status >= 400) return "warning";
  if (status >= 300) return "info";
  if (status >= 200) return "success";
  return "neutral";
}

export function statusClass(status: number | undefined): string {
  const tones: Record<Tone, string> = {
    error: "text-error",
    warning: "text-warning",
    info: "text-info",
    success: "text-success",
    neutral: "text-toned",
  };
  return tones[statusTone(status)];
}

/**
 * How long the raw request rows live, which is not the platform's retention.
 *
 * The store keeps one knob (`retentionDays`, `logRetentionDays` on `/settings`)
 * and derives every table's TTL from it: the rollups the charts and the route
 * table read are kept for the whole of it, and the raw rows the listing reads
 * for `min(7, retentionDays)` days — capped because a row per request is the
 * expensive table, floored by nothing because an installation that keeps three
 * days of logs did not ask to keep seven days of requests.
 *
 * A retention nobody has read yet is the cap: it is the widest the raw rows can
 * ever reach, so a bound derived from it is a bound the store will honour or
 * better — and the sentence beside it must not claim the number was checked.
 */
export const MAX_RAW_RETENTION_DAYS = 7;

export function rawRetentionDays(retentionDays: number | undefined | null): number {
  if (!retentionDays || !Number.isFinite(retentionDays) || retentionDays <= 0) return MAX_RAW_RETENTION_DAYS;
  return Math.min(MAX_RAW_RETENTION_DAYS, retentionDays);
}

/** The oldest instant the raw rows can answer for, as an ISO timestamp. */
export function rawRetentionStart(retentionDays?: number | null, now = Date.now()): string {
  return new Date(now - rawRetentionDays(retentionDays) * 24 * 3600 * 1000).toISOString();
}

/**
 * Whether a row was served over HTTP/2 — the only place the platform can tell
 * anyone it might be looking at gRPC. A failed gRPC call is an HTTP 200 with a
 * `grpc-status` trailer the edge does not read, so the error numbers on this
 * screen are transport-level for such a service, and the screen has to say so
 * rather than be quietly wrong.
 */
export function isHTTP2(protocol: string | undefined): boolean {
  return /^\s*(http\/2(\.0)?|h2c?|grpc)\s*$/i.test(protocol ?? "");
}

/** The footnote the error column carries once this environment has been seen
 * serving HTTP/2 — a statement about what these numbers can count, not about
 * the rows on screen, which is why nothing narrows it. */
export const GRPC_FOOTNOTE =
  "This environment serves HTTP/2. If that is gRPC, a failed call is an HTTP 200 with a grpc-status trailer the edge " +
  "does not read — those failures are not counted here, in any route's row.";

/** Whether any row of a listing was served over HTTP/2. Only ever adds to what
 * is known about an environment: a page without one proves nothing, since the
 * page is a sample and the rollups carry no protocol to check against. */
export function anyHTTP2(rows: RequestRow[] | null | undefined): boolean {
  return (rows ?? []).some((row) => isHTTP2(row.protocol));
}

/**
 * The window a request is correlated with its logs over: the instant, plus or
 * minus thirty seconds. The same width the crash report uses for its own
 * requests, so the two views of one moment cover the same moment.
 */
export function correlationWindow(timestamp: string, seconds = 30): { since: string; until: string } | null {
  const at = new Date(timestamp).getTime();
  if (Number.isNaN(at)) return null;
  return {
    since: new Date(at - seconds * 1000).toISOString(),
    until: new Date(at + seconds * 1000).toISOString(),
  };
}

/**
 * The Observability view's own URL state, pre-filled: this environment's lines
 * in the window around one request. It goes through the query language rather
 * than a raw ClickHouse expression because the query bar is the front door, and
 * a query someone lands on should be one they can edit by clicking.
 */
export function correlatedLogsQuery(
  environment: string,
  timestamp: string,
  seconds = 30,
): Record<string, string> | null {
  const window = correlationWindow(timestamp, seconds);
  if (!window) return null;
  return {
    q: renderClause({ field: "environment", value: environment, negated: false }),
    since: window.since,
    until: window.until,
  };
}

/** One mark on a chart's baseline, in the shape ResourceChart draws. */
export interface ChartMark {
  start: string;
  count: number;
  tone: string;
  label: string;
}

/**
 * The bucket a moment falls in, or null when it falls outside the series.
 *
 * Marks are drawn against a point's `start`, so an event's own timestamp has to
 * be snapped to the bucket that contains it — a deploy at 10:32:17 belongs on
 * the 10:30 bar, and an exact match would put it nowhere at all.
 */
export function bucketFor(
  timestamp: string,
  points: { start: string }[],
  bucketSeconds: number,
): string | null {
  const at = new Date(timestamp).getTime();
  if (Number.isNaN(at) || !points.length || bucketSeconds <= 0) return null;
  const width = bucketSeconds * 1000;
  let found: string | null = null;
  for (const point of points) {
    const start = new Date(point.start).getTime();
    if (Number.isNaN(start) || start > at) break;
    if (at < start + width) found = point.start;
  }
  return found;
}

/**
 * The deploys inside a series' window, as marks.
 *
 * A request row cannot say which release served it — the edge routes to a
 * Service, and during a rollout both revisions answer under one route — so the
 * activity feed's deploy entries are the only way to see "the latency stepped
 * up when this went out". Correlating them by time is exactly what the API's
 * own note says to do, and the mark is honest about being a correlation: it
 * sits on the time axis, not on the rows.
 */
export function deployMarks(
  events: PlatformEvent[] | null | undefined,
  points: { start: string }[],
  bucketSeconds: number,
): ChartMark[] {
  const byBucket = new Map<string, ChartMark>();
  for (const event of events ?? []) {
    if (!event.type.startsWith("release.")) continue;
    const bucket = bucketFor(event.timestamp, points, bucketSeconds);
    if (!bucket) continue;
    const rolledBack = event.type === "release.rolledBack";
    const existing = byBucket.get(bucket);
    if (existing) {
      existing.count += 1;
      // A rollback in the bucket colours the whole mark: it is the more
      // interesting of the two things that happened.
      if (rolledBack) {
        existing.tone = "stroke-warning/70";
        existing.label = "rollback";
      }
      continue;
    }
    byBucket.set(bucket, {
      start: bucket,
      count: 1,
      tone: rolledBack ? "stroke-warning/70" : "stroke-info/70",
      label: rolledBack ? "rollback" : "deploy",
    });
  }
  return [...byBucket.values()];
}

/** Saturation as the header reads it: usage against the limit, as a percentage
 * of it. No limit is not zero percent — it is a workload nobody capped, and the
 * tile says so instead of implying headroom nobody promised. */
export function saturation(used: number | undefined, limit: number | undefined): number | null {
  if (!limit || limit <= 0 || used === undefined || Number.isNaN(used)) return null;
  return (used / limit) * 100;
}

export function formatSaturation(percent: number | null): string {
  if (percent === null) return "—";
  return `${percent < 10 ? percent.toFixed(1) : Math.round(percent)}%`;
}
