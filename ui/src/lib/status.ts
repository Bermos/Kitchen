import type { Condition } from "./api";

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
