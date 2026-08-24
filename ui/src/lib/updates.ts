/**
 * The platform's own upgrade, as the screen watching it has to read it.
 *
 * One thing here is not like the rest of the dashboard: **the API being down is
 * part of the normal sequence.** `helm upgrade --atomic --wait` replaces the
 * manager Deployment, and the manager is what serves this dashboard's API — so
 * for a minute or two in the middle of a perfectly healthy upgrade every
 * authenticated request fails. A screen that renders that as an error is
 * reporting a failure at the exact moment there is none, which was the update
 * panel's worst behaviour.
 *
 * So the reading of a failed poll depends on what else is true: while an update
 * is in flight, "the API stopped answering" is a phase of the upgrade; an
 * answer that *did* arrive and said no is an error however inconvenient the
 * timing. `unreachable` is where that line is drawn, and it is drawn on the
 * status code rather than on the moment, because the two are genuinely
 * different events.
 */

import type { ComponentStatus, PlatformUpdate } from "./api";

/**
 * The namespace the platform installs into. Compiled into the operator and the
 * chart both (CLAUDE.md, "The platform namespace is kitchen-system"), so the
 * dashboard spells it rather than asking for it — it is what scopes the
 * cluster warnings shown during an upgrade to the platform's own workloads.
 */
export const PLATFORM_NAMESPACE = "kitchen-system";

/** The phases an update stops moving in. */
export const TERMINAL_PHASES = ["Succeeded", "Failed"];

/** Whether this update is still going somewhere. */
export function moving(update: PlatformUpdate | null | undefined): boolean {
  return !!update && !TERMINAL_PHASES.includes(update.phase ?? "");
}

/** The update currently in flight, out of the history the API answers with. */
export function inFlight(items: PlatformUpdate[] | undefined): PlatformUpdate | undefined {
  return (items ?? []).find((update) => update.phase === "Running" || update.phase === "Pending");
}

/**
 * The statuses that mean nothing answered, as opposed to something answering
 * with a refusal.
 *
 * `0` is what the client's own stream helper raises for a connection that never
 * established. 502/503/504 are what sits in front of a Deployment with no
 * ready pods — the ingress, the Gateway, or Cloudflare — and 52x are what
 * cloudflared's edge says when the tunnel has no origin to reach, which is the
 * same event through the tunnel this platform often runs behind.
 */
const UNREACHABLE = new Set([0, 502, 503, 504, 520, 521, 522, 523, 524]);

/**
 * The status an `APIError` carries, or null for a failure that never had a
 * response to read one off.
 *
 * It is read structurally rather than with `instanceof APIError` so that this
 * module needs only the API's *types*: importing the client itself pulls the
 * browser session in with it, and everything here is decided by a unit test.
 */
function statusOf(err: unknown): number | null {
  if (err && typeof err === "object" && "status" in err) {
    const status = (err as { status: unknown }).status;
    if (typeof status === "number") return status;
  }
  return null;
}

/**
 * Whether a failed read means the API never answered.
 *
 * A failure with no status came out of `fetch` itself (or out of the token
 * renewal's own fetch, which propagates a network failure rather than treating
 * it as the session ending) — a DNS failure, a refused connection, a dropped
 * socket. There is no response to have read a status off, so it is the clearest
 * case of all.
 *
 * A status is what the server said. A 4xx with a body is a real answer and
 * stays an error — `403 not an operator` does not stop being true because an
 * upgrade happens to be running — and only the codes above, which are emitted
 * by whatever sits in front of a workload with no pods, read as the platform
 * being briefly absent.
 */
export function unreachable(err: unknown): boolean {
  const status = statusOf(err);
  return status === null || UNREACHABLE.has(status);
}

/**
 * Where an upgrade is, as the operator watching it experiences it rather than
 * as the record describes it. The record is one of its inputs: for the middle
 * of the sequence there is no record to read, because the process that would
 * write it is being replaced.
 */
export type UpdateStage =
  /** Admitted, nothing applied yet — queued behind another update, or held. */
  | "waiting"
  /** helm is applying the chart and the API is still answering. */
  | "applying"
  /** The API stopped answering. Expected: the operator is replacing itself. */
  | "restarting"
  /** A new version is serving /config.json; the API has not caught up yet. */
  | "landed"
  /** Both are back, on the new version, and the record has not settled yet. */
  | "reconnected"
  /** The record says it finished. */
  | "succeeded"
  | "failed";

export interface FlightReading {
  /** `status.phase` on the PlatformUpdate, as last read. */
  phase?: string;
  /** Whether the last read of /updates answered at all. */
  reachable: boolean;
  /** Whether /config.json now reports a version other than the one this page
   *  started on — the new operator serving, which needs no authentication. */
  landed: boolean;
}

/**
 * The stage the three readings add up to.
 *
 * A terminal phase outranks everything: once the API is back and the record
 * says Failed, the upgrade failed, whatever the blackout looked like on the way
 * there. Below that the blackout outranks the phase, because during it the
 * phase is simply the last thing anyone heard.
 */
export function stageOf(reading: FlightReading): UpdateStage {
  if (reading.phase === "Succeeded") return "succeeded";
  if (reading.phase === "Failed") return "failed";
  if (!reading.reachable) return reading.landed ? "landed" : "restarting";
  if (reading.landed) return "reconnected";
  return reading.phase === "Pending" ? "waiting" : "applying";
}

/** Whether the stage is one where nothing more will happen on its own. */
export function settled(stage: UpdateStage): boolean {
  return stage === "succeeded" || stage === "failed";
}

/**
 * Whether what the screen is showing was read before the API went away — the
 * checklist and the warnings freeze during the blackout, and saying "last seen"
 * over frozen numbers is the difference between a stale reading and a false
 * claim.
 */
export function frozen(stage: UpdateStage): boolean {
  return stage === "restarting" || stage === "landed";
}

/**
 * The component survey in the order a checklist wants it: what is not ready
 * yet first, then everything else by name.
 *
 * Mid-upgrade an unhealthy component is the normal case rather than a fault —
 * a Deployment being rolled has pods missing by definition — so this sorts to
 * put what helm is still waiting for at the front, and says nothing about
 * whether waiting for it is bad.
 */
export function checklist(components: ComponentStatus[] | undefined): ComponentStatus[] {
  return [...(components ?? [])].sort((a, b) => {
    if (a.healthy !== b.healthy) return a.healthy ? 1 : -1;
    return a.name.localeCompare(b.name);
  });
}

/** What a component that is not ready is waiting on, in one line. */
export function componentDetail(component: ComponentStatus): string {
  if (component.message) return component.message;
  return `${component.available} of ${component.desired} pod${component.desired === 1 ? "" : "s"} available`;
}

/** The version as the dashboard writes it: `v1.2.3`, or `dev` unadorned. */
export function versionLabel(version: string | undefined): string {
  if (!version) return "—";
  return version === "dev" ? "dev" : `v${version}`;
}
