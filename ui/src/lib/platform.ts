/**
 * The operator screens' own vocabulary: how the platform's health is judged,
 * and how the two answers that are neither good nor bad are rendered.
 *
 * Everything here exists because of one rule the observability design keeps
 * repeating: **a number nobody measured is not a zero, and a check that did not
 * run is not a pass.** The API is careful to say so — `telemetryMessage`,
 * `usageMessage`, `eventsMessage`, `trafficMessage`, `unreadable` — and a
 * screen that renders those as health throws the distinction away at the last
 * possible moment. So the three states are modelled here, once, and the screens
 * render what they are given.
 */

import type {
  Certificate,
  ComponentStatus,
  NodeCondition,
  NodeTelemetry,
  PlatformEdge,
  PlatformIngest,
  PlatformStatus,
  PlatformStorage,
  UsagePoint,
} from "./api";
import { formatBytes, formatDurationSeconds } from "./format";
import type { Tone } from "./status";

/** Where the operator's own thresholds are mirrored, so that a bar the screen
 * paints amber and a finding that fires cannot disagree about the number.
 * They are the catalogue's constants (`internal/signals/thresholds.go`). */
export const VOLUME_FULL_FRACTION = 0.85;
export const NODE_SATURATION_FRACTION = 0.9;
export const CERT_EXPIRY_DAYS = 21;

/**
 * The three readings of a node's telemetry.
 *
 * `unknown` is the one that has to exist. A store nobody could reach must not
 * make the whole cluster look silent — that is the same wrong answer this
 * column exists to prevent, arrived at from the other side — and it must not
 * make it look fresh either.
 */
export type FreshnessState = "fresh" | "silent" | "unknown";

export interface Freshness {
  state: FreshnessState;
  /** What the cell says: `12s ago`, `silent`, `unknown`. */
  label: string;
  /** The sentence behind it. */
  detail: string;
  tone: Tone;
}

/**
 * When the store last heard from a node's collector, read the way the API
 * writes it: an absent `lastSeen` with `silent` true is a node that said
 * nothing inside the lookback, and both absent is a freshness read that did not
 * happen at all.
 *
 * A `telemetryMessage` on the body outranks everything: freshness is then
 * unknown for every node, whatever the per-node fields say.
 */
export function freshness(telemetry: NodeTelemetry | undefined, telemetryMessage?: string): Freshness {
  const unknown = (detail: string): Freshness => ({
    state: "unknown",
    label: "unknown",
    detail,
    tone: "neutral",
  });

  if (telemetryMessage) return unknown(telemetryMessage);
  if (!telemetry) return unknown("freshness was not read for this node");

  if (telemetry.silent) {
    return {
      state: "silent",
      label: "silent",
      detail: telemetry.lastSeen
        ? `nothing received for ${formatDurationSeconds(telemetry.ageSeconds)} — everything this node runs is missing from every number the platform reports`
        : "nothing received in the last hour — everything this node runs is missing from every number the platform reports",
      tone: "error",
    };
  }
  if (!telemetry.lastSeen) return unknown("the freshness read did not happen");

  return {
    state: "fresh",
    label: `${formatDurationSeconds(telemetry.ageSeconds)} ago`,
    detail: `last row from this node's collector ${formatDurationSeconds(telemetry.ageSeconds)} ago`,
    tone: "success",
  };
}

/**
 * A node's conditions that are not where they should be.
 *
 * `Ready` is the one that is wrong when it is *not* True; every other condition
 * a node carries — MemoryPressure, DiskPressure, PIDPressure,
 * NetworkUnavailable — is wrong when it *is*. Reading them all the same way is
 * how a screen ends up reporting a healthy node as under memory pressure.
 */
export function nodePressure(conditions: NodeCondition[] | undefined): NodeCondition[] {
  return (conditions ?? []).filter((condition) =>
    condition.type === "Ready" ? condition.status !== "True" : condition.status === "True",
  );
}

/**
 * The newest bucket of a series that was actually observed.
 *
 * A series ends in a partial bucket and may have holes where a scrape did not
 * happen, and those holes are null rather than zero — so the last element is
 * often not the last measurement. Null where nothing in the window was
 * observed, which is not the same as zero and must not render as one.
 */
export function latestObserved(points: UsagePoint[] | undefined): number | null {
  for (let i = (points?.length ?? 0) - 1; i >= 0; i -= 1) {
    const value = points?.[i]?.value;
    if (value !== null && value !== undefined) return value;
  }
  return null;
}

/** A fill fraction as a percentage, or a dash where nothing measured it. */
export function formatFraction(fraction: number | undefined | null): string {
  if (fraction === undefined || fraction === null || Number.isNaN(fraction)) return "—";
  const percent = fraction * 100;
  return `${percent < 10 ? percent.toFixed(1) : Math.round(percent)}%`;
}

/** The tone a fill bar takes, against the same threshold `pvc.filling` and
 * `store.disk` fire on. Nothing measured is neutral, never green. */
export function fillTone(fraction: number | undefined | null): Tone {
  if (fraction === undefined || fraction === null || Number.isNaN(fraction)) return "neutral";
  if (fraction >= VOLUME_FULL_FRACTION) return "error";
  if (fraction >= VOLUME_FULL_FRACTION * 0.9) return "warning";
  return "success";
}

/**
 * One panel of the health strip: green, or naming its problem.
 *
 * `state` is three-valued for the same reason everything else here is. A tile
 * that could not read its source says `unknown` and carries the reason —
 * rendering it green would be the dashboard asserting health it never checked,
 * and rendering it red would be crying wolf over an operator that is a version
 * behind.
 */
export type HealthState = "ok" | "problem" | "unknown";

export interface HealthTile {
  key: string;
  label: string;
  /** The headline: `8/8`, `12%`, `healthy`, `—`. */
  value: string;
  state: HealthState;
  tone: Tone;
  /** Green: what the number means. Otherwise: the problem, in words. */
  detail: string;
  /** The screen that shows the numbers behind it. */
  to?: string;
}

const unknownTile = (key: string, label: string, detail: string): HealthTile => ({
  key,
  label,
  value: "—",
  state: "unknown",
  tone: "neutral",
  detail,
});

/** Nodes, as the API server sees them. Silence belongs to the ingest tile —
 * this one is about whether the machines are Ready. */
export function nodesTile(status: PlatformStatus | null): HealthTile {
  if (!status) return unknownTile("nodes", "Nodes", "the platform's status could not be read");
  const cluster = status.cluster;
  if (cluster.message) return unknownTile("nodes", "Nodes", cluster.message);
  // Zero nodes is not a cluster where every node is Ready. It is a read that
  // told us nothing, and a green tile over it would be the one lie this strip
  // must not tell.
  if (cluster.nodes === 0) return unknownTile("nodes", "Nodes", "the cluster reported no nodes at all");

  const missing = cluster.nodes - cluster.readyNodes;
  return {
    key: "nodes",
    label: "Nodes",
    value: `${cluster.readyNodes}/${cluster.nodes}`,
    state: missing > 0 ? "problem" : "ok",
    tone: missing > 0 ? "error" : "success",
    detail:
      missing > 0
        ? `${missing} node${missing === 1 ? " is" : "s are"} not Ready`
        : `every node is Ready${cluster.name ? ` on ${cluster.name}` : ""}`,
    to: "/platform/nodes",
  };
}

/** The component survey: the only place a workload whose pods were refused at
 * admission shows up at all, because it has no pods to look at. */
export function componentsTile(status: PlatformStatus | null): HealthTile {
  const components: ComponentStatus[] | undefined = status?.components;
  if (!status) return unknownTile("components", "Components", "the platform's status could not be read");
  if (!components?.length) {
    return unknownTile("components", "Components", "the component survey answered with nothing to judge");
  }

  const unhealthy = components.filter((component) => !component.healthy);
  return {
    key: "components",
    label: "Components",
    value: `${components.length - unhealthy.length}/${components.length}`,
    state: unhealthy.length ? "problem" : "ok",
    tone: unhealthy.length ? "error" : "success",
    detail: unhealthy.length
      ? `${unhealthy[0].name}: ${unhealthy[0].message || `${unhealthy[0].available} of ${unhealthy[0].desired} available`}`
      : "every platform workload has the pods it wants",
    to: "/platform/workloads",
  };
}

/**
 * Whether the platform is still hearing from its own collection layer.
 *
 * Three readings, because each catches a failure the others cannot: per-node
 * freshness catches a collector that stopped shipping, the DaemonSet's counts
 * catch the one that never started, and the flow ledger is the only evidence
 * that a perfectly plausible request count is wrong.
 */
export function ingestTile(ingest: PlatformIngest | null): HealthTile {
  if (!ingest) return unknownTile("ingest", "Ingest", "the collection layer could not be read");
  if (ingest.telemetryMessage) return unknownTile("ingest", "Ingest", ingest.telemetryMessage);

  const collector = ingest.collector;
  const nodes = ingest.items?.length ?? 0;
  if (nodes === 0) return unknownTile("ingest", "Ingest", "no node answered, so there is no freshness to judge");
  const reporting = nodes - ingest.silentNodes;
  const tile: HealthTile = {
    key: "ingest",
    label: "Ingest",
    value: `${reporting}/${nodes}`,
    state: "ok",
    tone: "success",
    detail: "every node's collector is still shipping",
    to: "/platform/nodes",
  };

  if (!collector.present) {
    return { ...tile, value: "none", state: "problem", tone: "error", detail: collector.message || "no node collector is installed" };
  }
  if (collector.message) {
    return { ...tile, value: `${collector.available}/${collector.desired}`, state: "problem", tone: "error", detail: collector.message };
  }
  if (ingest.silentNodes > 0) {
    return {
      ...tile,
      state: "problem",
      tone: "error",
      detail: `${ingest.silentNodes} node${ingest.silentNodes === 1 ? " has" : "s have"} shipped nothing for at least ten minutes`,
    };
  }
  if (ingest.nodesWithoutCollector > 0) {
    return {
      ...tile,
      state: "problem",
      tone: "warning",
      detail: `${ingest.nodesWithoutCollector} node${ingest.nodesWithoutCollector === 1 ? " has" : "s have"} no collector pod at all`,
    };
  }
  if (ingest.flows && !ingest.flows.lossless) {
    return {
      ...tile,
      state: "problem",
      tone: "warning",
      detail: `Hubble reported dropping ${ingest.flows.events} flow events — request counts under-report`,
    };
  }
  return tile;
}

/** The telemetry store's own disk, read from the same query `store.disk` fires
 * on, so the screen and the finding cannot disagree about the number. */
export function storeTile(storage: PlatformStorage | null): HealthTile {
  if (!storage) return unknownTile("store", "Store", "the telemetry store's health could not be read");
  const store = storage.store;
  if (store.message) return unknownTile("store", "Store", store.message);

  // An external store is a disk the platform does not own and has no business
  // judging: it gets its size and no verdict.
  if (!store.capacityBytes) {
    return {
      key: "store",
      label: "Store",
      value: formatBytes(store.bytesOnDisk),
      state: "ok",
      tone: "success",
      detail: `on a volume the platform does not own${store.retentionDays ? `, ${store.retentionDays} days retained` : ""}`,
      to: "/platform/storage",
    };
  }

  const fraction = store.usedFraction ?? 0;
  const full = fraction >= VOLUME_FULL_FRACTION;
  return {
    key: "store",
    label: "Store",
    value: formatFraction(fraction),
    state: full ? "problem" : "ok",
    tone: fillTone(fraction),
    detail: full
      ? `${formatBytes(store.bytesOnDisk)} of ${formatBytes(store.capacityBytes)} used — past the point the platform reports a filling volume`
      : `${formatBytes(store.bytesOnDisk)} of ${formatBytes(store.capacityBytes)}${store.retentionDays ? `, ${store.retentionDays} days retained` : ""}`,
    to: "/platform/storage",
  };
}

/** The front door itself: the Gateway that publishes everything, and the tunnel
 * in front of it where there is one. */
export function edgeTile(edge: PlatformEdge | null): HealthTile {
  if (!edge) return unknownTile("edge", "Edge", "the edge could not be read");

  const gateways = edge.gateways ?? [];
  if (!gateways.length) {
    return {
      key: "edge",
      label: "Edge",
      value: "none",
      state: "problem",
      tone: "error",
      detail: "no Gateway was found — nothing on this platform is published",
      to: "/platform/edge",
    };
  }

  const unprogrammed = gateways.filter((gateway) => !gateway.programmed || !gateway.accepted);
  if (unprogrammed.length) {
    return {
      key: "edge",
      label: "Edge",
      value: `${gateways.length - unprogrammed.length}/${gateways.length}`,
      state: "problem",
      tone: "error",
      detail: unprogrammed[0].message || `${unprogrammed[0].name} is not programmed — it has no address to serve on`,
      to: "/platform/edge",
    };
  }
  if (edge.tunnel && !edge.tunnel.healthy) {
    return {
      key: "edge",
      label: "Edge",
      value: "tunnel",
      state: "problem",
      tone: "error",
      detail: edge.tunnel.message || `cloudflared has ${edge.tunnel.available} of ${edge.tunnel.desired} pods available`,
      to: "/platform/edge",
    };
  }
  return {
    key: "edge",
    label: "Edge",
    value: gateways[0].addresses?.[0] || "programmed",
    state: "ok",
    tone: "success",
    detail: edge.tunnel ? "the Gateway is programmed and the tunnel is up" : "the Gateway is programmed",
    to: "/platform/edge",
  };
}

/** The certificate table's verdict. A stuck renewal reports itself only in
 * `issuing`: the Ready condition stays true on the still-valid old certificate. */
export function certificatesTile(edge: PlatformEdge | null): HealthTile {
  if (!edge) return unknownTile("certificates", "Certificates", "the certificate table could not be read");
  const table = edge.certificates;
  const items: Certificate[] = table?.items ?? [];
  if (!items.length) {
    // cert-manager not being installed is a supported configuration — TLS mode
    // `none`, or a certificate supplied by hand — so there is nothing to judge
    // rather than something wrong.
    return unknownTile("certificates", "Certificates", table?.message || "no certificates are managed on this platform");
  }

  const broken = items.filter((certificate) => !certificate.ready);
  const expiring = items.filter(
    (certificate) => certificate.daysToExpiry !== undefined && certificate.daysToExpiry <= CERT_EXPIRY_DAYS,
  );
  const stuck = items.filter((certificate) => certificate.issuing);
  const soonest = items.reduce<number | undefined>((least, certificate) => {
    if (certificate.daysToExpiry === undefined) return least;
    return least === undefined || certificate.daysToExpiry < least ? certificate.daysToExpiry : least;
  }, undefined);

  const worst = broken[0] ?? expiring[0] ?? stuck[0];
  if (worst) {
    return {
      key: "certificates",
      label: "Certificates",
      value: `${items.length - broken.length}/${items.length}`,
      state: "problem",
      tone: broken.length ? "error" : "warning",
      detail:
        worst.message ||
        (worst.issuing ? `${worst.name} is renewing: ${worst.issuing}` : `${worst.name} expires in ${Math.round(worst.daysToExpiry ?? 0)} days`),
      to: "/platform/edge",
    };
  }
  return {
    key: "certificates",
    label: "Certificates",
    value: `${items.length}/${items.length}`,
    state: "ok",
    tone: "success",
    detail: soonest !== undefined ? `soonest renewal in ${Math.round(soonest)} days` : "every certificate is ready",
    to: "/platform/edge",
  };
}

/** The build queue as the concurrency gate weighs it. */
export function buildsTile(status: PlatformStatus | null): HealthTile {
  if (!status) return unknownTile("builds", "Builds", "the platform's status could not be read");
  const builds = status.builds;
  return {
    key: "builds",
    label: "Builds",
    value: `${builds.running}/${builds.capacity}`,
    state: builds.queued > 0 ? "problem" : "ok",
    tone: builds.queued > 0 ? "warning" : "success",
    detail:
      builds.queued > 0
        ? `${builds.queued} waiting for a slot against a concurrency of ${builds.capacity}`
        : "nothing is waiting for a slot",
    to: "/builds",
  };
}

/** The whole strip, in the order docs/OBSERVABILITY.md §6.2 lists it. */
export function healthStrip(sources: {
  status: PlatformStatus | null;
  ingest: PlatformIngest | null;
  storage: PlatformStorage | null;
  edge: PlatformEdge | null;
}): HealthTile[] {
  return [
    nodesTile(sources.status),
    componentsTile(sources.status),
    ingestTile(sources.ingest),
    storeTile(sources.storage),
    edgeTile(sources.edge),
    certificatesTile(sources.edge),
    buildsTile(sources.status),
  ];
}
