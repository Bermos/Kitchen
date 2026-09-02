import type {
  Build,
  ConfigChange,
  ConfigDiff,
  Environment,
  Release,
  RequestSummary,
  VariableChange,
  Workload,
} from "./api";

/**
 * The reasoning behind the rollback panel (#181).
 *
 * Rollback is the one destructive action the dashboard offers, and it is
 * usually taken under pressure by whoever is on call rather than by whoever
 * wrote the code. A confirm dialog that reveals nothing the release list had
 * not already shown is not a safety mechanism, so the panel is three steps —
 * pick, review the diff, verify — and everything the middle step needs is
 * derived here, where it can be tested without a screen.
 *
 * Nothing in this module fetches. It joins lists the environment view already
 * holds (the project's releases and builds, the environment's history) with
 * the one answer only the server can give: the variable diff, which is
 * computed there precisely so that the values never have to travel.
 */

/** One release as the rail draws it: what it is, what it was built from, and
 * where it stands relative to the environment. */
export interface ReleaseRow {
  release: Release;
  /** The build's commit, when the build still exists. A release outlives its
   * build's retention, so this is genuinely optional rather than a loading
   * state. */
  build?: Build;
  /** Running here right now. */
  live: boolean;
  /** The newest release this environment has held before — "last green" in
   * the sense that it did serve traffic, which is the only claim the platform
   * can make without inventing one. */
  lastServed: boolean;
  /** This environment has been rolled back off this release before. Worth a
   * word: going back to it again is going back to something somebody already
   * decided against. */
  rolledBackBefore: boolean;
  /** How many releases back from the live one, counting along the project's
   * release list. Negative for a release newer than what is live, which is a
   * promotion rather than a rollback. */
  distance: number;
}

/** Releases newest first — creation time, names breaking the tie, which is the
 * order the API already answers in and the order the rail draws. */
export function newestFirst(releases: Release[]): Release[] {
  return [...releases].sort((a, b) => {
    if (a.createdAt !== b.createdAt) return a.createdAt < b.createdAt ? 1 : -1;
    return a.name < b.name ? 1 : -1;
  });
}

/** The build a release was cut from, by name. */
export function buildFor(release: Release | undefined, builds: Build[]): Build | undefined {
  if (!release) return undefined;
  return builds.find((b) => b.name === release.build);
}

/** The release this environment held immediately before the live one. The
 * history records every release that stopped being current, newest first, so
 * the newest entry is what it was running before — which is what `kitchen
 * rollback` with no release named goes back to, and what the rail marks. */
export function previouslyServed(environment: Environment | undefined): string {
  return environment?.history?.[0]?.release ?? "";
}

/** Whether this environment has been rolled back off a release before. */
export function rolledBackBefore(environment: Environment | undefined, release: string): boolean {
  return (environment?.history ?? []).some((e) => e.release === release && e.reason === "rolledBack");
}

/** When the live release became current: the moment the environment moved off
 * the release it held before. Empty when it has only ever held one, where the
 * environment's own creation is the honest answer and the caller supplies it. */
export function servingSince(environment: Environment | undefined): string {
  return environment?.history?.[0]?.to ?? environment?.createdAt ?? "";
}

/** The last stint a release had on this environment: when it stopped being
 * current, and how long it had been. Undefined for a release that has never
 * been live here, which is the ordinary case for a promotion. */
export function lastServedStint(
  environment: Environment | undefined,
  release: string,
): { from: string; to: string } | undefined {
  const entry = (environment?.history ?? []).find((e) => e.release === release);
  return entry ? { from: entry.from, to: entry.to } : undefined;
}

/** The rail: every release of the project, annotated against this
 * environment. */
export function releaseRows(releases: Release[], builds: Build[], environment: Environment | undefined): ReleaseRow[] {
  const ordered = newestFirst(releases);
  const liveIndex = ordered.findIndex((r) => r.name === environment?.release);
  const previous = previouslyServed(environment);
  return ordered.map((release, index) => ({
    release,
    build: buildFor(release, builds),
    live: release.name === environment?.release,
    lastServed: release.name === previous,
    rolledBackBefore: rolledBackBefore(environment, release.name),
    distance: liveIndex < 0 ? 0 : index - liveIndex,
  }));
}

/** The commits that stop being served by a move — the builds behind every
 * release between where the environment is and where it is going, the live one
 * included and the target one not.
 *
 * A forward move serves *more* commits rather than fewer, and the same range
 * read the other way is what it starts serving; `direction` says which
 * sentence the panel writes. A release whose build has aged out of the cluster
 * contributes nothing, which is why this can be shorter than the distance. */
export function commitsBetween(
  rows: ReleaseRow[],
  target: Release | undefined,
): { direction: "rollback" | "promotion"; builds: Build[] } {
  const targetRow = rows.find((r) => r.release.name === target?.name);
  if (!targetRow || targetRow.distance === 0) return { direction: "rollback", builds: [] };
  const direction = targetRow.distance > 0 ? "rollback" : "promotion";
  // Rolling back: the live release and everything cut after the target stop
  // being served. Promoting: everything up to and including the target starts.
  const span = rows.filter((row) =>
    direction === "rollback"
      ? row.distance >= 0 && row.distance < targetRow.distance
      : row.distance >= targetRow.distance && row.distance < 0,
  );
  return { direction, builds: span.map((row) => row.build).filter((b): b is Build => b !== undefined) };
}

/** How many variables move, by kind. The panel leads with this because the
 * count is the sentence: "three variables change" is what somebody decides
 * on, and the rows are what they check it against. */
export function variableCounts(diff: ConfigDiff | undefined): Record<ConfigChange, number> {
  const counts: Record<ConfigChange, number> = { changed: 0, removed: 0, added: 0, unchanged: 0 };
  for (const variable of diff?.variables ?? []) counts[variable.change] += 1;
  return counts;
}

/** The variables the panel lists one by one: everything that is not identical
 * on both sides. The unchanged ones are summarized in a single row instead,
 * because a list of forty unremarkable names buries the three that matter. */
export function movedVariables(diff: ConfigDiff | undefined): VariableChange[] {
  return (diff?.variables ?? []).filter((v) => v.change !== "unchanged");
}

/** The unchanged ones, by name, for that single row. */
export function unchangedVariables(diff: ConfigDiff | undefined): VariableChange[] {
  return (diff?.variables ?? []).filter((v) => v.change === "unchanged");
}

/** The sign the row is marked with, the diff vocabulary the issue asks for. */
export function changeSign(change: ConfigChange): string {
  switch (change) {
    case "changed":
      return "~";
    case "removed":
      return "−";
    case "added":
      return "+";
    default:
      return "=";
  }
}

/** What a variable's row says instead of a value. There is no value to say —
 * the API never reads one back — so the row says what *kind* of change it is
 * and, where the source moved, what it moved between. That is the part
 * somebody acts on anyway: a variable that went from a literal to a claim
 * binding has changed in a way no diff of values would have explained. */
export function changeDetail(variable: VariableChange): string {
  const source = (kind?: string) => {
    switch (kind) {
      case "secret":
        return "a secret";
      case "claim":
        return "a claim binding";
      case "value":
        return "a value";
      default:
        return "unset";
    }
  };
  switch (variable.change) {
    case "removed":
      return `${source(variable.againstSource)} → unset`;
    case "added":
      return `unset → ${source(variable.source)}`;
    case "changed":
      if (variable.previewOnly) return "only the preview override differs";
      if (variable.againstSource !== variable.source) {
        return `${source(variable.againstSource)} → ${source(variable.source)}`;
      }
      if (variable.ref && variable.againstRef && variable.ref.key !== variable.againstRef.key) {
        return `${variable.againstRef.name}/${variable.againstRef.key} → ${variable.ref.name}/${variable.ref.key}`;
      }
      return "the value differs";
    default:
      return variable.ref ? `unchanged · ${variable.ref.name}/${variable.ref.key}` : "unchanged";
  }
}

/** The runtime fields that actually moved. */
export function movedRuntime(diff: ConfigDiff | undefined) {
  return (diff?.runtime ?? []).filter((f) => f.changed);
}

/** The processes that moved. */
export function movedProcesses(diff: ConfigDiff | undefined) {
  return (diff?.processes ?? []).filter((p) => p.change !== "unchanged");
}

/**
 * The deploy tasks the release being rolled back to declares, which run again
 * before it takes traffic.
 *
 * They are named here whether or not they *changed*, which is the one place
 * this panel reports something unchanged: a rollback re-runs the older
 * release's migration, and "nothing about the migration differs" is not the
 * same sentence as "the migration does not run". A task marked `removed` is
 * one the release being left behind declared and the target does not, so it
 * is exactly the one that will not run.
 */
export function deployTasksThatRunAgain(diff: ConfigDiff | undefined) {
  return (diff?.processes ?? []).filter((p) => p.type === "task" && p.change !== "removed");
}

/**
 * Whether the confirm is gated on typing the environment's name.
 *
 * The issue asks whether it is worth gating "when the target is more than N
 * releases back", and the answer this takes is that the distance alone is the
 * wrong test. What makes a move dangerous is not how far it goes but whether
 * "exact and reversible" is still the whole story:
 *
 * - **A preview is never gated.** It is disposable, and a gate on the cheap
 *   case teaches people to type through the expensive one.
 * - **Two releases or more back is gated**, because at that distance nobody is
 *   undoing the deploy they just watched.
 * - **Any move whose variable snapshot differs is gated, however near**, since
 *   an environment whose configuration changes with the image is no longer
 *   just going back to the image that worked.
 *
 * A move that fails all three — one release back, same configuration, or any
 * preview — stays a single click, which is the on-call undo this screen must
 * not slow down.
 */
export function gatedByName(
  environment: Environment | undefined,
  row: ReleaseRow | undefined,
  diff: ConfigDiff | undefined,
): boolean {
  if (!environment || !row) return false;
  if (environment.type === "preview") return false;
  if (row.distance >= 2) return true;
  const counts = variableCounts(diff);
  return counts.changed + counts.removed + counts.added > 0;
}

/** One check the verification step makes after the swap. `pending` is its own
 * state and not a failure: a p95 that has not settled yet is a thing to keep
 * watching, and calling it red would train people to ignore red. */
export interface VerificationCheck {
  label: string;
  state: "ok" | "pending" | "bad";
  detail: string;
}

/** Whether the environment has finished rolling onto the release it was
 * moved to. `observedRelease` is what the operator has applied; until it
 * catches up the swap has been asked for and not yet made. */
export function swapLanded(environment: Environment | undefined, target: string): boolean {
  return Boolean(environment && environment.release === target && environment.observedRelease === target);
}

/** The condition an environment reports about its route. Named here because
 * the verification step reads it and the operator writes it — the two
 * spellings have to agree. */
export const routeProgrammedCondition = "RouteProgrammed";

/**
 * What the panel stays to prove: did the swap land, and is the environment
 * still healthy since it did.
 *
 * Every check is answered from something the screen already polls, and every
 * one of them is honest about not knowing yet. `pending` is deliberately not
 * `bad`: replicas that have not all rolled and a p95 that has not settled are
 * things to keep watching, and colouring them red would teach somebody to
 * ignore red at the exact moment it matters.
 *
 * `baseline` is the p95 over the same length of window before the swap, so
 * that "still settling" is a comparison rather than a number nobody can place.
 */
export function verificationChecks(input: {
  environment?: Environment;
  target: string;
  workload?: Workload;
  since?: RequestSummary;
  baseline?: RequestSummary;
}): VerificationCheck[] {
  const { environment, target, workload, since, baseline } = input;
  const checks: VerificationCheck[] = [];

  const replicas = workload?.replicas;
  const rolled = swapLanded(environment, target) && Boolean(replicas) && replicas!.updated >= replicas!.desired;
  checks.push({
    label: "Replicas updated",
    state: !replicas ? "pending" : rolled && replicas.ready >= replicas.desired ? "ok" : "pending",
    detail: replicas
      ? `${replicas.updated} of ${replicas.desired} · ${workload?.restarts ?? 0} restarts`
      : "waiting for the workload",
  });

  const route = environment?.conditions?.find((c) => c.type === routeProgrammedCondition);
  checks.push({
    label: "Route programmed",
    state: !route ? "pending" : route.status === "True" ? "ok" : "bad",
    detail: route?.message ?? "waiting for the route",
  });

  // 5xx *since the swap*, which is the only window that answers "did that
  // work". The environment's own errors, not the platform's.
  checks.push({
    label: "5xx since the swap",
    state: !since ? "pending" : since.errors === 0 ? "ok" : "bad",
    detail: since ? `${since.errors} in ${since.requests} requests` : "waiting for the edge",
  });

  // p95 against the same window before the swap. Slower is not a failure —
  // a cold cache after a swap is expected — so it settles rather than fails.
  const settled = since && baseline ? since.p95Ms <= baseline.p95Ms : false;
  checks.push({
    label: settled ? "p95 recovered" : "p95 still settling",
    state: !since ? "pending" : settled ? "ok" : "pending",
    detail: since && baseline ? `${Math.round(baseline.p95Ms)} ms → ${Math.round(since.p95Ms)} ms` : "waiting for the edge",
  });

  return checks;
}
