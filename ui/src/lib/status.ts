import type { Condition, Environment, ReleaseHistoryEntry } from "./api";

// Phases are the coarse summary the CRDs expose; the platform's vocabulary
// comes verbatim from docs/CRDS.md. Conditions carry the detail and the UI
// prefers them when something is off.

export type Tone = "success" | "warning" | "error" | "info" | "neutral";

const phaseTones: Record<string, Tone> = {
  // Build: Queued | Running | Succeeded | Failed | Cancelled
  Queued: "neutral",
  Running: "warning",
  Succeeded: "success",
  Failed: "error",
  Cancelled: "neutral",
  // Environment: Pending | Deploying | Live | Degraded | Terminating
  Pending: "neutral",
  Deploying: "warning",
  Live: "success",
  Degraded: "error",
  Terminating: "neutral",
  // ResourceClaim
  Bound: "success",
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

/** A condition that is not where it should be — the thing the UI surfaces
 * even when the phase still reads fine. */
export function unhealthyConditions(conditions: Condition[] | undefined): Condition[] {
  return (conditions ?? []).filter((c) => c.status !== "True");
}

/** One line of status detail: the message of the worst condition, if any. */
export function statusDetail(conditions: Condition[] | undefined): string {
  const bad = unhealthyConditions(conditions);
  if (bad.length === 0) return "";
  return bad[0].message || bad[0].reason || bad[0].type;
}
