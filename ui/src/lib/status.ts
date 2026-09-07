import type { Condition, ConditionSeverity, Environment, ReleaseHistoryEntry } from "./api";

// Phases are the coarse summary the CRDs expose; the platform's vocabulary
// comes verbatim from docs/CRDS.md. Conditions carry the detail and the UI
// prefers them when something is off.

export type Tone = "success" | "warning" | "error" | "info" | "neutral";

const phaseTones: Record<string, Tone> = {
  // Build: Queued | Running | Succeeded | Failed | Cancelled | Skipped
  Queued: "neutral",
  Running: "warning",
  Succeeded: "success",
  Failed: "error",
  Cancelled: "neutral",
  // A build the platform had nothing to build for: the commit's source under
  // the project's root directory was byte-identical to the last build's. It
  // is neither a failure nor a deployment, and `info` is the tone that says
  // so — the whole point of recording the skip is that a monorepo's ordinary
  // day should not read as seven broken builds or seven releases (#500).
  Skipped: "info",
  // Environment: Pending | Deploying | Live | Degraded | Terminating
  Pending: "neutral",
  Deploying: "warning",
  Live: "success",
  Degraded: "error",
  Terminating: "neutral",
  // ResourceClaim
  Bound: "success",
  // A binding waiting for the project that makes the offering to answer it
  // (#495). `info` rather than `warning`: nothing is wrong, and the thing it
  // waits for is a person on the other side of a grant rather than anything
  // this project's reader can fix.
  PendingApproval: "info",
};

export function phaseTone(phase: string | undefined): Tone {
  return (phase && phaseTones[phase]) || "neutral";
}

/** The newest history entry for a release — how its latest stint on the
 * environment ended. History arrives newest first from the API. */
export function releaseHistoryEntry(
  release: string,
  environment: Environment | undefined,
): ReleaseHistoryEntry | undefined {
  return environment?.history?.find((entry) => entry.release === release);
}

/** The deployment timeline's label for a release that is not current. A
 * release rolled back off reads "Rolled back" wherever it sits; otherwise the
 * one the environment left most recently is "Previous" and older ones are
 * "Superseded". A release the history never saw current gets no label. */
export function releaseHistoryLabel(release: string, environment: Environment | undefined): string {
  const entry = releaseHistoryEntry(release, environment);
  if (!entry) return "";
  if (entry.reason === "rolledBack") return "Rolled back";
  return entry === environment?.history?.[0] ? "Previous" : "Superseded";
}

/**
 * How much attention one condition deserves.
 *
 * The API says, and this is the whole of what the dashboard knows about it:
 * a condition's `status` says whether the statement in its `type` holds, not
 * whether anything is wrong — `Previews=False` with reason `Disabled` is
 * previews turned off, which is what somebody asked for. Reading `status` here
 * is what drew two healthy projects as failures and spent both slots of the
 * attention band on them (#436). The decision belongs to whoever wrote the
 * condition, and `internal/api/conditions.go` is where it is made.
 *
 * The fallback is the old reading, for a payload from an operator older than
 * the field: not True is not well.
 */
export function conditionSeverity(condition: Condition): ConditionSeverity {
  if (condition.severity) return condition.severity;
  if (condition.status === "True") return "none";
  return condition.status === "False" ? "error" : "warning";
}

/** A condition that is not where it should be — the thing the UI surfaces
 * even when the phase still reads fine. A condition the API classified as a
 * setting or a fact is not one of them, however its status reads. */
export function unhealthyConditions(conditions: Condition[] | undefined): Condition[] {
  return (conditions ?? []).filter((c) => {
    const severity = conditionSeverity(c);
    return severity === "error" || severity === "warning";
  });
}

/** The dot for an object summarized by its conditions alone. A fault outranks
 * something unassessed — a credential the operator could not check is a
 * different message from one a provider rejected — while no conditions at all
 * means the operator has not looked yet. */
export function conditionsTone(conditions: Condition[] | undefined): Tone {
  if (!conditions?.length) return "neutral";
  if (conditions.some((c) => conditionSeverity(c) === "error")) return "error";
  if (conditions.some((c) => conditionSeverity(c) === "warning")) return "warning";
  return "success";
}

/** One line of status detail: the message of the worst condition, if any. */
export function statusDetail(conditions: Condition[] | undefined): string {
  const bad = unhealthyConditions(conditions);
  if (bad.length === 0) return "";
  return bad[0].message || bad[0].reason || bad[0].type;
}
