import { describe, expect, it } from "vitest";
import type { Certificate, PlatformEdge, PlatformIngest, PlatformStatus, PlatformStorage } from "./api";
import {
  buildsTile,
  certificatesTile,
  certificateTrouble,
  CERT_EXPIRY_DAYS,
  componentsTile,
  edgeTile,
  fillTone,
  FLOWS_LOST_FIRING,
  flowsUnderReporting,
  formatFraction,
  freshness,
  healthStrip,
  ingestTile,
  latestObserved,
  nodePressure,
  nodesTile,
  storeTile,
} from "./platform";

const status = (over: Partial<PlatformStatus> = {}): PlatformStatus => ({
  cluster: { name: "chef", nodes: 3, readyNodes: 3 },
  tunnel: { enabled: false, connected: false },
  builds: { running: 0, capacity: 2, queued: 0 },
  gateway: { address: "203.0.113.7", programmed: true },
  components: [
    { name: "collector", kind: "DaemonSet", healthy: true, available: 3, desired: 3 },
    { name: "clickhouse", kind: "StatefulSet", healthy: true, available: 1, desired: 1 },
  ],
  ...over,
});

const ingest = (over: Partial<PlatformIngest> = {}): PlatformIngest => ({
  items: [
    { node: "node-a", collector: "Running", telemetry: { lastSeen: "2026-08-16T10:00:00Z", silent: false, ageSeconds: 12 } },
    { node: "node-b", collector: "Running", telemetry: { lastSeen: "2026-08-16T10:00:00Z", silent: false, ageSeconds: 30 } },
  ],
  silentNodes: 0,
  nodesWithoutCollector: 0,
  collector: { present: true, name: "kitchen-collector", namespace: "kitchen-system", desired: 2, ready: 2, available: 2 },
  flows: { events: 0, notices: 0, reconnects: 0, windowSeconds: 3600, lossless: true },
  ...over,
});

const storage = (over: Partial<PlatformStorage> = {}): PlatformStorage => ({
  items: [],
  volumes: 1,
  unbound: 0,
  filling: 0,
  store: { bytesOnDisk: 5_000_000_000, capacityBytes: 50_000_000_000, usedFraction: 0.1, rowsPerSecond: 42, retentionDays: 30 },
  ...over,
});

const edge = (over: Partial<PlatformEdge> = {}): PlatformEdge => ({
  requests: {
    since: "2026-08-16T09:00:00Z",
    until: "2026-08-16T10:00:00Z",
    requests: 1200,
    requestsPerSecond: 0.3,
    errors: 0,
    errorRate: 0,
    p50Ms: 9,
    p95Ms: 210,
    p99Ms: 900,
    unrouted: 0,
    rollup: "1m",
  },
  topRoutes: [],
  worstRoutes: [],
  topHosts: [],
  worstHosts: [],
  latencyLeaders: [],
  unrouted: [],
  gateways: [{ namespace: "kitchen-system", name: "kitchen", addresses: ["203.0.113.7"], programmed: true, accepted: true }],
  certificates: { items: [{ namespace: "kitchen-system", name: "kitchen-wildcard", ready: true, daysToExpiry: 60 }] },
  ...over,
});

describe("freshness", () => {
  it("is fresh where the store heard from the node recently", () => {
    const answer = freshness({ lastSeen: "2026-08-16T10:00:00Z", silent: false, ageSeconds: 42 });
    expect(answer.state).toBe("fresh");
    expect(answer.tone).toBe("success");
    expect(answer.label).toBe("42s ago");
  });

  it("is loudly silent where the node said nothing inside the lookback", () => {
    const answer = freshness({ silent: true });
    expect(answer.state).toBe("silent");
    expect(answer.tone).toBe("error");
    expect(answer.detail).toContain("missing from every number");
  });

  it("is unknown — not fine, not silent — when the store could not be read", () => {
    const answer = freshness({ silent: false }, "the telemetry store could not be read");
    expect(answer.state).toBe("unknown");
    expect(answer.tone).toBe("neutral");
    expect(answer.detail).toBe("the telemetry store could not be read");
  });

  it("outranks even a per-node answer with the body's message", () => {
    // A store nobody could reach must not make a whole cluster look silent.
    const answer = freshness({ silent: true }, "the telemetry store could not be read");
    expect(answer.state).toBe("unknown");
  });

  it("is unknown where the freshness read simply did not happen", () => {
    expect(freshness({ silent: false }).state).toBe("unknown");
    expect(freshness(undefined).state).toBe("unknown");
  });
});

describe("nodePressure", () => {
  it("reads Ready and the pressure conditions in opposite directions", () => {
    const conditions = [
      { type: "Ready", status: "True", since: "" },
      { type: "MemoryPressure", status: "False", since: "" },
      { type: "DiskPressure", status: "True", since: "" },
    ];
    expect(nodePressure(conditions).map((condition) => condition.type)).toEqual(["DiskPressure"]);
    expect(nodePressure([{ type: "Ready", status: "False", since: "" }]).map((c) => c.type)).toEqual(["Ready"]);
    expect(nodePressure(undefined)).toEqual([]);
  });
});

describe("latestObserved", () => {
  it("walks back past the buckets nothing was observed in", () => {
    expect(
      latestObserved([
        { start: "a", value: 0.4 },
        { start: "b", value: 0.6 },
        { start: "c", value: null },
      ]),
    ).toBe(0.6);
  });

  it("is null — never zero — where the whole window went unobserved", () => {
    expect(latestObserved([{ start: "a", value: null }])).toBeNull();
    expect(latestObserved([])).toBeNull();
    expect(latestObserved(undefined)).toBeNull();
  });
});

describe("fillTone", () => {
  it("turns at the threshold the platform's own rules fire on", () => {
    expect(fillTone(0.1)).toBe("success");
    expect(fillTone(0.8)).toBe("warning");
    expect(fillTone(0.86)).toBe("error");
  });

  it("is neutral where nothing measured the fill, never green", () => {
    expect(fillTone(null)).toBe("neutral");
    expect(fillTone(undefined)).toBe("neutral");
    expect(formatFraction(null)).toBe("—");
    expect(formatFraction(0.855)).toBe("86%");
    expect(formatFraction(0.042)).toBe("4.2%");
  });
});

describe("nodesTile", () => {
  it("is green when every node is Ready", () => {
    const tile = nodesTile(status());
    expect(tile.state).toBe("ok");
    expect(tile.value).toBe("3/3");
  });

  it("names the problem when one is not", () => {
    const tile = nodesTile(status({ cluster: { name: "chef", nodes: 3, readyNodes: 2 } }));
    expect(tile.state).toBe("problem");
    expect(tile.tone).toBe("error");
    expect(tile.detail).toContain("1 node is not Ready");
  });

  it("is unknown where the count itself could not be read", () => {
    const tile = nodesTile(status({ cluster: { nodes: 0, readyNodes: 0, message: "nodes are forbidden to this operator" } }));
    expect(tile.state).toBe("unknown");
    expect(tile.value).toBe("—");
    expect(tile.detail).toContain("forbidden");
  });

  it("is unknown rather than green before anything answered", () => {
    expect(nodesTile(null).state).toBe("unknown");
  });

  it("never reads “no nodes” as “every node is Ready”", () => {
    expect(nodesTile(status({ cluster: { nodes: 0, readyNodes: 0 } })).state).toBe("unknown");
  });
});

describe("componentsTile", () => {
  it("counts the survey and names the first thing wrong with it", () => {
    expect(componentsTile(status()).value).toBe("2/2");
    const tile = componentsTile(
      status({
        components: [
          { name: "collector", kind: "DaemonSet", healthy: false, available: 0, desired: 3, message: "0 of 3 pods available" },
        ],
      }),
    );
    expect(tile.state).toBe("problem");
    expect(tile.detail).toContain("collector: 0 of 3 pods available");
  });

  it("has nothing to judge when the survey answered with nothing", () => {
    expect(componentsTile(status({ components: [] })).state).toBe("unknown");
  });
});

describe("ingestTile", () => {
  it("is green when every node's collector is still shipping", () => {
    const tile = ingestTile(ingest());
    expect(tile.state).toBe("ok");
    expect(tile.value).toBe("2/2");
  });

  it("names silence first", () => {
    const tile = ingestTile(ingest({ silentNodes: 1 }));
    expect(tile.state).toBe("problem");
    expect(tile.tone).toBe("error");
    expect(tile.detail).toContain("1 node has shipped nothing");
  });

  it("catches the collector that never started, which has no pods to list", () => {
    const tile = ingestTile(
      ingest({
        collector: { present: true, desired: 3, ready: 0, available: 0, message: "the collector wants 3 pods and has none at all" },
      }),
    );
    expect(tile.state).toBe("problem");
    expect(tile.detail).toContain("has none at all");
  });

  it("says request counts under-report when the flow stream lost events", () => {
    const tile = ingestTile(ingest({ flows: { events: 4096, notices: 3, reconnects: 1, windowSeconds: 3600, lossless: false } }));
    expect(tile.state).toBe("problem");
    expect(tile.detail).toContain("under-report");
  });

  // The mirror itself: `ingest.flows-lost` fires at a hundred events or at any
  // reconnect, and a tile that turned amber on the first lost event was a
  // warning beside a problems list that named nothing.
  it("turns at the same hundred `ingest.flows-lost` turns at, not at the first lost event", () => {
    const under = ingestTile(ingest({ flows: { events: FLOWS_LOST_FIRING - 1, notices: 1, reconnects: 0, windowSeconds: 3600, lossless: false } }));
    expect(under.state).toBe("ok");
    expect(under.tone).toBe("success");
    // Still said out loud: it is the only number that hints the charts are low.
    expect(under.detail).toContain("99 flow events lost");
    expect(under.detail).toContain(`under the ${FLOWS_LOST_FIRING}`);

    const at = ingestTile(ingest({ flows: { events: FLOWS_LOST_FIRING, notices: 1, reconnects: 0, windowSeconds: 3600, lossless: false } }));
    expect(at.state).toBe("problem");
    expect(at.tone).toBe("warning");
    expect(at.detail).toContain("under-report");
  });

  it("reports a single reconnect, whose gap is of unknown size", () => {
    const tile = ingestTile(ingest({ flows: { events: 0, notices: 0, reconnects: 1, windowSeconds: 3600, lossless: false } }));
    expect(tile.state).toBe("problem");
    expect(tile.detail).toContain("reconnected 1 time");
    expect(tile.detail).toContain("under-report");
  });

  it("is unknown — not silent — when freshness itself could not be read", () => {
    const tile = ingestTile(ingest({ telemetryMessage: "the telemetry store could not be read" }));
    expect(tile.state).toBe("unknown");
    expect(tile.tone).toBe("neutral");
  });

  it("does not call an empty node list a healthy collection layer", () => {
    expect(ingestTile(ingest({ items: [] })).state).toBe("unknown");
  });
});

describe("storeTile", () => {
  it("is green under the threshold and names it over", () => {
    expect(storeTile(storage()).state).toBe("ok");
    const full = storeTile(
      storage({ store: { bytesOnDisk: 46_000_000_000, capacityBytes: 50_000_000_000, usedFraction: 0.92, rowsPerSecond: 4 } }),
    );
    expect(full.state).toBe("problem");
    expect(full.tone).toBe("error");
    expect(full.detail).toContain("past the point");
  });

  it("passes no judgement on a disk the platform does not own", () => {
    const tile = storeTile(storage({ store: { bytesOnDisk: 1_000_000, rowsPerSecond: 1 } }));
    expect(tile.state).toBe("ok");
    expect(tile.detail).toContain("does not own");
  });

  it("is unknown where the store could not answer", () => {
    const tile = storeTile(storage({ store: { bytesOnDisk: 0, rowsPerSecond: 0, message: "no telemetry store" } }));
    expect(tile.state).toBe("unknown");
  });
});

describe("edgeTile", () => {
  it("is green with a programmed Gateway", () => {
    expect(edgeTile(edge()).state).toBe("ok");
  });

  it("names an unprogrammed Gateway in its own words", () => {
    const tile = edgeTile(
      edge({
        gateways: [
          {
            namespace: "kitchen-system",
            name: "kitchen",
            programmed: false,
            accepted: true,
            message: "AddressNotAssigned: no LoadBalancer address",
          },
        ],
      }),
    );
    expect(tile.state).toBe("problem");
    expect(tile.detail).toContain("AddressNotAssigned");
  });

  it("reports an unhealthy tunnel in front of a healthy Gateway", () => {
    const tile = edgeTile(
      edge({
        tunnel: { name: "kitchen-cloudflared", namespace: "kitchen-system", desired: 2, ready: 0, available: 0, restarts: 7, healthy: false },
      }),
    );
    expect(tile.state).toBe("problem");
    expect(tile.detail).toContain("0 of 2");
  });

  it("says so when there is no Gateway at all", () => {
    const tile = edgeTile(edge({ gateways: [] }));
    expect(tile.state).toBe("problem");
    expect(tile.detail).toContain("nothing on this platform is published");
  });

  // The same empty list, two different answers. "No Gateway" is the strongest
  // claim this strip makes, and a List that was refused proves none of it.
  it("does not make that claim over a Gateway list it could not read", () => {
    const tile = edgeTile(edge({ gateways: [], gatewayMessage: "the platform's Gateways could not be read: forbidden" }));
    expect(tile.state).toBe("unknown");
    expect(tile.tone).toBe("neutral");
    expect(tile.detail).toContain("could not be read");
  });
});

describe("certificatesTile", () => {
  it("is green while every certificate is ready and far from expiry", () => {
    const tile = certificatesTile(edge());
    expect(tile.state).toBe("ok");
    expect(tile.detail).toContain("60 days");
  });

  it("quotes the CA's own error for a stuck order", () => {
    const tile = certificatesTile(
      edge({
        certificates: {
          items: [
            {
              namespace: "kitchen-system",
              name: "kitchen-wildcard",
              ready: false,
              daysToExpiry: 9.6,
              message: "Failed to wait for order resource: DNS problem: NXDOMAIN",
            },
          ],
        },
      }),
    );
    expect(tile.state).toBe("problem");
    expect(tile.tone).toBe("error");
    expect(tile.detail).toContain("NXDOMAIN");
  });

  it("warns on a renewal in progress inside the window, which is where a failing one hides", () => {
    const tile = certificatesTile(
      edge({
        certificates: {
          items: [
            { namespace: "kitchen-system", name: "kitchen-wildcard", ready: true, daysToExpiry: 12, issuing: "Issuing: order pending" },
          ],
        },
      }),
    );
    expect(tile.state).toBe("problem");
    expect(tile.tone).toBe("warning");
    expect(tile.detail).toContain("order pending");
  });

  // The mirror: `cert.expiring` is the *combination* — inside the window and
  // the renewal not progressing — so a platform on short-lived certificates,
  // permanently inside the window, does not carry a red tile over a problems
  // list that names nothing.
  it("leaves a ready certificate inside its window alone, as `cert.expiring` does", () => {
    const tile = certificatesTile(
      edge({
        certificates: {
          items: [{ namespace: "kitchen-system", name: "kitchen-wildcard", ready: true, daysToExpiry: 3 }],
        },
      }),
    );
    expect(tile.state).toBe("ok");
    expect(tile.tone).toBe("success");
    expect(tile.detail).toContain("3 days");
  });

  it("calls a certificate that has never been issued trouble at once", () => {
    const tile = certificatesTile(
      edge({
        certificates: {
          items: [{ namespace: "kitchen-system", name: "kitchen-wildcard", ready: false }],
        },
      }),
    );
    expect(tile.state).toBe("problem");
    expect(tile.tone).toBe("error");
    expect(tile.detail).toContain("never been issued");
  });

  it("has nothing to judge where cert-manager is not installed", () => {
    const tile = certificatesTile(edge({ certificates: { items: [], message: "cert-manager is not installed" } }));
    expect(tile.state).toBe("unknown");
    expect(tile.detail).toBe("cert-manager is not installed");
  });
});

// The two predicates the screens share with the catalogue. They are tested on
// their own as well as through the tiles, because what they are for is agreeing
// with a rule in Go — the numbers and the shape of the condition both.
describe("flowsUnderReporting", () => {
  const loss = (events: number, reconnects: number) => ({
    events,
    notices: events ? 1 : 0,
    reconnects,
    windowSeconds: 3600,
    lossless: events === 0 && reconnects === 0,
  });

  it("is `FlowsLost >= FlowsLostFiring || Reconnects > 0`, and that alone", () => {
    expect(FLOWS_LOST_FIRING).toBe(100);
    expect(flowsUnderReporting(loss(0, 0))).toBe(false);
    expect(flowsUnderReporting(loss(1, 0))).toBe(false);
    expect(flowsUnderReporting(loss(FLOWS_LOST_FIRING - 1, 0))).toBe(false);
    expect(flowsUnderReporting(loss(FLOWS_LOST_FIRING, 0))).toBe(true);
    expect(flowsUnderReporting(loss(0, 1))).toBe(true);
  });

  it("has no ledger to judge where no follower answered", () => {
    expect(flowsUnderReporting(undefined)).toBe(false);
  });
});

describe("certificateTrouble", () => {
  const certificate = (over: Partial<Certificate> = {}): Certificate => ({
    namespace: "kitchen-system",
    name: "kitchen-wildcard",
    ready: true,
    daysToExpiry: 60,
    ...over,
  });

  it("is trouble at once where nothing was ever issued", () => {
    expect(certificateTrouble(certificate({ daysToExpiry: undefined, ready: false }))).toBe(true);
    expect(certificateTrouble(certificate({ daysToExpiry: undefined, ready: true }))).toBe(false);
  });

  it("says nothing outside the window, whatever the certificate is doing", () => {
    expect(certificateTrouble(certificate({ daysToExpiry: CERT_EXPIRY_DAYS + 1 }))).toBe(false);
    expect(certificateTrouble(certificate({ daysToExpiry: 40, issuing: "Issuing: order pending" }))).toBe(false);
    expect(certificateTrouble(certificate({ daysToExpiry: 40, ready: false }))).toBe(false);
  });

  it("inside the window, is the renewal not progressing rather than the date", () => {
    expect(certificateTrouble(certificate({ daysToExpiry: CERT_EXPIRY_DAYS }))).toBe(false);
    expect(certificateTrouble(certificate({ daysToExpiry: 1 }))).toBe(false);
    expect(certificateTrouble(certificate({ daysToExpiry: 1, ready: false }))).toBe(true);
    expect(certificateTrouble(certificate({ daysToExpiry: 1, issuing: "Issuing: order errored" }))).toBe(true);
  });
});

describe("buildsTile", () => {
  it("is green with nothing waiting and amber with a queue", () => {
    expect(buildsTile(status()).state).toBe("ok");
    const queued = buildsTile(status({ builds: { running: 2, capacity: 2, queued: 3 } }));
    expect(queued.state).toBe("problem");
    expect(queued.tone).toBe("warning");
    expect(queued.detail).toContain("3 waiting");
  });
});

describe("healthStrip", () => {
  it("is the seven panels §6.2 names, in that order", () => {
    const tiles = healthStrip({ status: status(), ingest: ingest(), storage: storage(), edge: edge() });
    expect(tiles.map((tile) => tile.key)).toEqual([
      "nodes",
      "components",
      "ingest",
      "store",
      "edge",
      "certificates",
      "builds",
    ]);
  });

  it("darkens only the panels whose source could not be read", () => {
    const tiles = healthStrip({ status: status(), ingest: null, storage: null, edge: edge() });
    const states = Object.fromEntries(tiles.map((tile) => [tile.key, tile.state]));
    expect(states.nodes).toBe("ok");
    expect(states.edge).toBe("ok");
    expect(states.ingest).toBe("unknown");
    expect(states.store).toBe("unknown");
  });
});
