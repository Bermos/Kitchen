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
