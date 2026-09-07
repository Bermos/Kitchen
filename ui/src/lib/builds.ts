import type { Build } from "./api";

/**
 * A failed build in one line, for a list.
 *
 * The Job behind a build reports "Job has reached the specified backoff limit",
 * which is the same sentence for every build that ever failed and is why a list
 * of failures used to read as one failure repeated. What distinguishes them is
 * on `failure`: the container that stopped, and how it exited.
 *
 * A build that failed before it ever had a pod — an unsupported strategy, a
 * commit refused for want of review — has no container to name, and the Ready
 * condition the reconciler left is the answer instead.
 */
export function buildFailureLine(build: Build): string {
  if (build.phase !== "Failed") return "";
  const detail = build.failure;
  if (detail?.message) return detail.message;
  if (detail?.container) {
    return detail.exitCode === undefined
      ? `${detail.container} did not run`
      : `${detail.container} exited ${detail.exitCode}`;
  }
  const condition = build.conditions?.find((c) => c.type === "Ready" && c.status === "False");
  return condition?.message ?? "";
}

/**
 * A build the platform had nothing to build for, in one line.
 *
 * The commit's source under the project's root directory was byte-identical to
 * the one the build it names used, so no image was produced, no release was
 * cut and nothing was deployed. The line is what keeps that readable as a
 * statement about the *commit* — this changed nothing here — rather than as a
 * build that mysteriously did nothing, which is the whole reason the skip is
 * recorded instead of being silent (#500).
 */
export function buildSkipLine(build: Build): string {
  if (build.phase !== "Skipped") return "";
  const where = build.sourceTree?.path || "the repository";
  const matched = build.sourceTree?.matchedBuild;
  return matched
    ? `Nothing to build — ${where} is unchanged since ${matched}.`
    : `Nothing to build — ${where} is unchanged.`;
}

/**
 * A running build that is not moving, in one line.
 *
 * A build whose Job has never created a pod reports Running for as long as
 * anybody leaves it there — the pods were refused before they existed, so
 * `status.failed` stays 0 and the Job writes no condition at all. The
 * reconciler notices, finds the warning the job controller left behind, and
 * puts it on a `Stalled` condition; this is where a reader of the dashboard
 * sees it, which is the whole point of its not being a `kubectl describe job`
 * away.
 */
export function buildStallLine(build: Build): string {
  if (build.phase !== "Running") return "";
  const condition = build.conditions?.find((c) => c.type === "Stalled" && c.status === "True");
  return condition?.message ?? "";
}
