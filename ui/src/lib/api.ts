import { loadConfig } from "./config";
import { renew, signOut, token } from "./auth";

// Typed client for the operator REST API (docs/API.md). The types mirror the
// API's view shapes — the platform's own vocabulary, nothing Kubernetes.

export interface Condition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime: string;
}

/** One key of a Secret or a ResourceClaim binding backing an env var. */
export interface KeyRef {
  name: string;
  key: string;
}

/** One of a project's environment variables. Secret- and claim-backed
 * variables carry references; the API never resolves them to values. */
export interface EnvVar {
  name: string;
  value?: string;
  previewValue?: string;
  fromSecret?: KeyRef;
  fromClaim?: KeyRef;
}

export interface Project {
  name: string;
  repo: string;
  connection: string;
  registry: string;
  productionBranch: string;
  previews: boolean;
  previewsProtected: boolean;
  buildStrategy?: string;
  dockerfilePath?: string;
  rootDirectory?: string;
  env?: EnvVar[];
  port?: number;
  replicas?: number;
  cpu?: string;
  memory?: string;
  productionEnvironment?: string;
  latestBuild?: string;
  createdAt: string;
  conditions?: Condition[];
}

/** What PATCH /projects/{name} accepts: absent fields keep their value. */
export interface ProjectSettings {
  productionBranch?: string;
  previews?: boolean;
  previewsProtected?: boolean;
  buildStrategy?: string;
  dockerfilePath?: string;
  rootDirectory?: string;
  env?: EnvVar[];
  port?: number;
  replicas?: number;
  cpu?: string;
  memory?: string;
}

export interface NewProject {
  name: string;
  repo: string;
  connection: string;
  registry: string;
  productionBranch?: string;
  previews?: boolean;
}

export interface Revision {
  sha: string;
  branch: string;
  message?: string;
  author?: string;
  pullRequest?: number;
}

export interface Build {
  name: string;
  project: string;
  phase?: string;
  git: Revision;
  detectedFramework?: string;
  image?: string;
  startedAt?: string;
  completedAt?: string;
  createdAt: string;
  conditions?: Condition[];
}

export interface Release {
  name: string;
  project: string;
  build: string;
  image: string;
  environments?: string[];
  createdAt: string;
}

export interface Preview {
  pullRequest: number;
  branch: string;
}

/** One completed stint of a release being current on an environment: when it
 * held the environment, and how and by whom it stopped being current. */
export interface ReleaseHistoryEntry {
  release: string;
  from: string;
  to: string;
  reason: "promoted" | "rolledBack" | "superseded";
  by?: string;
}

export interface Environment {
  name: string;
  project: string;
  type: string;
  release: string;
  observedRelease?: string;
  phase?: string;
  url?: string;
  preview?: Preview;
  history?: ReleaseHistoryEntry[];
  createdAt: string;
  conditions?: Condition[];
}

/** One pod behind an environment (GET /environments/{name}/workload). */
export interface WorkloadPod {
  name: string;
  phase: string;
  ready: boolean;
  restarts: number;
  node?: string;
  startedAt?: string;
  /** Why the pod is not serving — a crash loop, a pull failure. */
  message?: string;
}

/** What an environment is actually running, as opposed to what it was asked
 * to run: the Deployment's replica counts, its pods and their restarts. */
export interface Workload {
  environment: string;
  namespace: string;
  /** Empty when nothing has been materialized yet; `message` says why. */
  deployment?: string;
  image?: string;
  replicas: { desired: number; ready: number; available: number; updated: number };
  restarts: number;
  startedAt?: string;
  resources?: { cpuRequest?: string; cpuLimit?: string; memoryRequest?: string; memoryLimit?: string };
  pods?: WorkloadPod[];
  message?: string;
}

/** One Kubernetes object the operator materialized for an environment
 * (GET /environments/{name}/objects) — operator mode's inspect surface. */
export interface MaterializedObject {
  kind: string;
  apiVersion: string;
  name: string;
  namespace: string;
  present: boolean;
  manifest?: Record<string, unknown>;
  message?: string;
}

export interface EnvironmentObjects {
  environment: string;
  namespace: string;
  objects: MaterializedObject[];
}

/** One platform workload out of the operator's component survey. */
export interface ComponentStatus {
  name: string;
  kind: string;
  healthy: boolean;
  available: number;
  desired: number;
  message?: string;
}

/** The platform as it is running (GET /status) — the status bar's request. */
export interface PlatformStatus {
  cluster: { name?: string; nodes: number; readyNodes: number; message?: string };
  tunnel: { enabled: boolean; connected: boolean; message?: string };
  builds: { running: number; capacity: number; queued: number };
  gateway: { address?: string; programmed: boolean; message?: string };
  components?: ComponentStatus[];
}

export interface Connection {
  name: string;
  provider: string;
  capabilities?: string[];
  createdAt: string;
  conditions?: Condition[];
}

/** A credential as the API accepts one — a token, or a username and password,
 * depending on the provider. Write-only: the API never reads it back. */
export interface ConnectionCredential {
  token?: string;
  username?: string;
  password?: string;
}

export interface NewConnection {
  name: string;
  provider: string;
  config?: Record<string, unknown>;
  credential: ConnectionCredential;
}

/** What PATCH /connections/{name} accepts: a new config, a rotated
 * credential, or both. */
export interface ConnectionChanges {
  config?: Record<string, unknown>;
  credential?: ConnectionCredential;
}

/** What POST /connections/test takes: a credential to try before it is
 * stored, or the name of a connection whose stored credential should be
 * re-checked. Nothing is written either way. */
export interface ConnectionTestRequest {
  name?: string;
  provider?: string;
  config?: Record<string, unknown>;
  credential?: ConnectionCredential;
}

/** The probe's verdict, in the same parts the Connected and CredentialsValid
 * conditions are written from — a provider that is down and a credential that
 * is wrong are different answers. */
export interface ConnectionTestResult {
  reachable: boolean;
  credentialChecked: boolean;
  credentialValid: boolean;
  message: string;
  /** What an accepted credential still cannot do — a token that registers
   * webhooks but could not post a commit status. The connection works;
   * something the platform wants would not. */
  warnings?: string[];
}

/** The DNS change that proves ownership of a custom domain, exactly as the
 * user has to type it into their zone. Either record satisfies the check;
 * the CNAME also routes the hostname at the platform. */
export interface DomainVerification {
  txtRecord: string;
  txtValue: string;
  cnameTarget?: string;
}

export interface Domain {
  name: string;
  hostname: string;
  environment: string;
  /** The spec's own TLS mode; empty inherits the platform's. */
  tls?: string;
  /** The mode actually in effect, as the operator resolved it. */
  effectiveTLS?: string;
  verified: boolean;
  verification?: DomainVerification;
  createdAt: string;
  conditions?: Condition[];
}

/** What POST /domains takes: the name is derived from the hostname when
 * absent, and tls empty inherits the platform's mode. */
export interface NewDomain {
  name?: string;
  hostname: string;
  environment: string;
  tls?: string;
}

export interface Claim {
  name: string;
  project: string;
  connection: string;
  type: string;
  phase?: string;
  secret?: string;
  /** Retain (default) keeps the provisioned database when the claim is
   * deleted; Delete destroys it and its data. */
  deletionPolicy?: string;
  previewBranching: boolean;
  createdAt: string;
  conditions?: Condition[];
}

export interface NewClaim {
  name: string;
  project: string;
  connection: string;
  type: string;
  previewBranching?: boolean;
  deletionPolicy?: string;
}

export interface Settings {
  baseDomain: string;
  apiExternalURL?: string;
  gatewayClassName?: string;
  authEnabled: boolean;
  authHost?: string;
  buildStrategy?: string;
  buildConcurrency?: number;
  logRetentionDays?: number;
  gatewayAddress?: string;
  conditions?: Condition[];
}

/** One attempt to upgrade the platform itself. */
export interface PlatformUpdate {
  name: string;
  version: string;
  phase?: string;
  fromVersion?: string;
  message?: string;
  requestedBy?: string;
  startedAt?: string;
  completedAt?: string;
  conditions?: Condition[];
}

/** The platform's own version, what it can move to, and what it has tried. */
export interface PlatformUpdates {
  /** Whether the chart was installed with `selfUpdate.enabled`. */
  enabled: boolean;
  /** Why not, when it was not — including how to turn it on. */
  reason?: string;
  currentVersion: string;
  latestVersion?: string;
  available: boolean;
  /**
   * What this installation would actually accept, newest first. It is not
   * simply everything newer than `currentVersion`: pre-1.0 a minor crossing
   * carries the breaking changes, so those are left out unless `allowMinor`.
   */
  upgradableTo?: string[];
  allowMinor: boolean;
  /** Why the published versions could not be listed — usually no egress. */
  discoveryError?: string;
  items: PlatformUpdate[];
}

export interface LogLine {
  timestamp: string;
  source: string;
  project: string;
  environment: string;
  build: string;
  pod: string;
  container: string;
  stream: string;
  /** Best-effort severity the collector parsed out of the line; "" when unknown. */
  level?: string;
  message: string;
  /** The line's own trace and span, lifted out of its structured fields by the
   * collector. A line that carries one can offer the whole request. */
  traceId?: string;
  spanId?: string;
  /** The line's own structured fields, when it was JSON the collector could flatten. */
  fields?: Record<string, string>;
}

/**
 * What an observability question is asked over. `q` is Kitchen's log query
 * language and the front door; `where` is a raw ClickHouse expression, the
 * escape hatch. Given both, they compose with AND — which is how the view
 * scopes the cluster's own pods out of an operator's hand-written SQL.
 */
export interface LogSelection {
  q?: string;
  where?: string;
  since?: string;
  until?: string;
}

/** One bar of the log histogram. */
export interface LogBucket {
  start: string;
  count: number;
  errors: number;
  warnings: number;
}

/** The shape of a window (GET /logs/histogram), empty buckets included. */
export interface LogHistogram {
  start: string;
  end: string;
  bucketSeconds: number;
  buckets: LogBucket[];
  total: number;
}

/** One value a field takes in the current selection, and how often. */
export interface LogFacetValue {
  value: string;
  count: number;
}

/** One field's distinct values over the window (GET /logs/facets). */
export interface LogFacet {
  field: string;
  values: LogFacetValue[];
  distinct: number;
}

/** One message template the selection's lines collapse into. */
export interface LogPattern {
  pattern: string;
  count: number;
  level?: string;
  sample: string;
  firstSeen: string;
  lastSeen: string;
}

/** A question about the logs that was worth keeping (GET /logs/saved). It is
 * the observability view's own URL state, named — so it can be found by
 * someone who was never sent the link. */
export interface SavedQuery {
  name: string;
  title: string;
  description?: string;
  query?: string;
  where?: string;
  /** The window it is asked over, relative to whenever it is opened. 0 means
   * everything retained. */
  rangeMinutes: number;
  limit?: number;
  view?: "lines" | "patterns";
  includeCluster?: boolean;
  savedBy?: string;
  createdAt: string;
}

/** What POST /logs/saved accepts. */
export interface NewSavedQuery {
  title: string;
  description?: string;
  query?: string;
  where?: string;
  rangeMinutes?: number;
  limit?: number;
  view?: "lines" | "patterns";
  includeCluster?: boolean;
}

/** One bucket of an environment's resource history. CPU and memory are summed
 * across the environment's containers; `replicas` is how many distinct pods
 * reported in the bucket, which is the only way to see one idle to zero and
 * come back. */
export interface ResourcePoint {
  start: string;
  cpuCores: number;
  cpuPeakCores: number;
  memoryBytes: number;
  memoryPeakBytes: number;
  replicas: number;
  restarts: number;
  oomKills: number;
}

/** What an environment has been using (GET /environments/{name}/metrics), as
 * opposed to what it is using — which is the workload endpoint. */
export interface ResourceSeries {
  start: string;
  end: string;
  bucketSeconds: number;
  points: ResourcePoint[];
  cpuLimitCores: number;
  memoryLimitBytes: number;
  restarts: number;
  oomKills: number;
  /** Whether the five-minute rollup answered — why a wide window is coarser
   * than the resolution that was asked for. */
  rollup: boolean;
}

/**
 * Whether the platform's edge publishes this environment, which is what tells
 * "nothing reaches this environment" from "nothing was asked of it in this
 * window" — both of which are zeroes. `routed` is false only where the platform
 * is *sure*; a route it could not read leaves it true with a `message` saying
 * the check did not happen.
 */
export interface EdgeStatus {
  routed: boolean;
  message?: string;
}

/** The golden-signal header (GET /environments/{name}/requests/summary).
 * `since` and `until` are the window that was *answered* — these numbers come
 * off indivisible buckets, so the start is snapped to `rollup`'s resolution. */
export interface RequestSummary {
  since: string;
  until: string;
  requests: number;
  requestsPerSecond: number;
  /** Answers of 500 and above. A 4xx is the caller's fault and is not counted. */
  errors: number;
  errorRate: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
  /** Which rollup answered — `1m` or `1h`. */
  rollup: string;
  environment: string;
  edge: EdgeStatus;
}

/** One bucket of the request charts. Every bucket in the window is present,
 * empty ones included: a gap is an environment that served nothing. */
export interface RequestPoint {
  start: string;
  requests: number;
  requestsPerSecond: number;
  errors: number;
  errorRate: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
}

/** The same signals over time (GET /environments/{name}/requests/series). */
export interface RequestSeries {
  start: string;
  end: string;
  bucketSeconds: number;
  points: RequestPoint[];
  rollup: string;
  environment: string;
  edge: EdgeStatus;
}

/** One row of the route table: a route template's share of the window. The
 * set of templates is bounded at ingest, which is what makes this finite. */
export interface RequestRoute {
  route: string;
  requests: number;
  requestsPerSecond: number;
  errors: number;
  errorRate: number;
  p50Ms: number;
  p95Ms: number;
  p99Ms: number;
}

/** How the route table is ordered. The sort is a query rather than a
 * presentation detail: it decides which rows survive the limit. */
export type RouteSort = "requests" | "errors" | "errorRate" | "p95";

/**
 * One request the edge served. It carries project and environment and never a
 * build or a release — the edge routes to a Service, not to a pod — and `path`
 * has already lost its query string, which is never stored.
 */
export interface RequestRow {
  timestamp: string;
  project?: string;
  environment?: string;
  host?: string;
  method: string;
  path: string;
  /** What the path was templated to; the raw path is kept beside it so a
   * mis-templated route stays diagnosable. */
  route?: string;
  status: number;
  durationMs: number;
  protocol?: string;
  /** Which vantage point observed it — `gateway` today. */
  source?: string;
  /** Reserved: filled only when the edge can carry trace context. */
  traceId?: string;
}

/** The window and route filter every request read shares. */
export interface RequestWindow {
  since?: string;
  until?: string;
  /** One route template, spelled as the route table spells it — what clicking
   * a row filters the rest by. */
  route?: string;
}

/** What the raw listing is filtered by on top of the window. */
export interface RequestListQuery extends RequestWindow {
  method?: string;
  /** A *class* of answer — `5xx`. One exact code is not offered. */
  status?: string;
  /** Keep only what the signals count as an error (500 and above). */
  errors?: boolean;
  limit?: number;
}

/** One Warning event the cluster raised (inside the crash report). */
export interface K8sEvent {
  timestamp: string;
  project?: string;
  environment?: string;
  namespace?: string;
  kind?: string;
  name?: string;
  reason: string;
  message: string;
  count: number;
  node?: string;
}

/** The termination itself, in the words the kubelet reported it in. */
export interface CrashDetail {
  pod: string;
  container: string;
  /** The kubelet's reason — OOMKilled, Error, ContainerStatusUnknown. */
  reason?: string;
  /** Its own field beside `reason`: the kernel killing a container for using
   * too much memory and a container crashing are different problems with
   * different fixes and the same exit code. */
  oomKilled: boolean;
  exitCode: number;
  signal?: number;
  message?: string;
  startedAt?: string;
  finishedAt: string;
  restarts: number;
  /** Why the container is not running now — the CrashLoopBackOff and its
   * message. Empty for one that restarted and is serving again. */
  waiting?: string;
  /** The termination ended the run *before* the current one, which is the
   * ordinary shape of a crash loop. */
  previous: boolean;
}

/** Everything the platform knows about the crash, assembled: the sections do
 * not all use the whole span, and each is what it is for. */
export interface CrashReport {
  crash: CrashDetail;
  since: string;
  until: string;
  /** The dead container's own last lines, oldest first, up to the instant. */
  logs: LogLine[];
  /** Usage leading up to the termination, against the limit the release set. */
  resources: ResourceSeries;
  /** The cluster's Warnings around the crash — they run past it, because a
   * crash loop keeps announcing itself. */
  events: K8sEvent[];
  /** What the edge served in the ±30 seconds around the instant. */
  requests: RequestRow[];
}

/** GET /environments/{name}/diagnostics. Nothing having crashed is an answer,
 * not an empty report: `crashed` false comes with the sentence that says so. */
export interface Diagnostics {
  environment: string;
  namespace: string;
  crashed: boolean;
  message?: string;
  /** Every restart the environment's pods carry right now, off the API server. */
  restarts: number;
  report?: CrashReport;
}

/** One trace as a list entry (GET /traces): what it was, how long it took end
 * to end, and whether anything in it failed. */
export interface Trace {
  traceId: string;
  timestamp: string;
  name: string;
  service: string;
  project?: string;
  environment?: string;
  durationMs: number;
  spans: number;
  errors: number;
  /** How many distinct services the trace touched — one process, or a
   * conversation. */
  services: number;
  httpStatus?: number;
}

/** One operation inside a trace (GET /traces/{traceId}). */
export interface Span {
  timestamp: string;
  traceId: string;
  spanId: string;
  parentSpanId?: string;
  name: string;
  kind?: string;
  service: string;
  project?: string;
  environment?: string;
  durationMs: number;
  statusCode?: string;
  statusMessage?: string;
  httpStatus?: number;
  attributes?: Record<string, string>;
  resource?: Record<string, string>;
}

/** A whole trace: its spans in start order, which is how a waterfall is drawn. */
export interface TraceDetail {
  traceId: string;
  spans: Span[];
}

/** One entry of the platform's activity feed (GET /events). */
export interface PlatformEvent {
  timestamp: string;
  type: string;
  project?: string;
  environment?: string;
  build?: string;
  release?: string;
  claim?: string;
  message: string;
  actor?: string;
  value?: number;
}

/** Per-project 24h traffic inside the metrics overview. */
export interface ProjectTraffic {
  project: string;
  requests24h: number;
  errors5xx24h: number;
  p95Ms: number;
  requestsPerHour: number[];
}

/** The dashboard's numbers, pre-aggregated (GET /metrics/overview). */
export interface MetricsOverview {
  deploys7d: number;
  deploysPerDay: number[];
  medianBuildSeconds: number;
  requests24h: number;
  errorRate24h: number;
  p95Ms24h: number;
  requestsPerHour: number[];
  errorsPerHour: number[];
  p95MsPerHour: number[];
  logLines24h: number;
  logLinesPerHour: number[];
  storeBytes: number;
  storeRowsPerSecond: number;
  projects?: ProjectTraffic[];
}

/** One aggregated edge of the service map (GET /traffic). */
export interface TrafficEdge {
  source: string;
  sourceNamespace?: string;
  destination: string;
  destinationNamespace?: string;
  protocol: string;
  flows: number;
  rps: number;
  errors: number;
  drops: number;
  p95Ms: number;
}

export class APIError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

/**
 * Send a request with the session's bearer token, and give a 401 exactly one
 * more chance: the token may have been refused a moment before the scheduled
 * renewal, or the issuer may have rotated its keys under us. Renewing and
 * retrying keeps the page — and whatever was half-typed on it — where it is,
 * rather than sending the browser back through the identity provider.
 *
 * A 401 that survives the retry is a session that is over, and the caller
 * routes back to the login.
 */
async function authorized(send: (bearer: string) => Promise<Response>): Promise<Response> {
  const bearer = await token();
  if (!bearer) {
    void signOut();
    throw new APIError(401, "not signed in");
  }
  const res = await send(bearer);
  if (res.status !== 401) return res;

  const renewed = await renew();
  // The same token back means the renewal had nothing new to offer, so the
  // retry would be the request that just failed.
  if (!renewed || renewed === bearer) {
    void signOut();
    throw new APIError(401, "the session expired");
  }
  const retry = await send(renewed);
  if (retry.status === 401) {
    void signOut();
    throw new APIError(401, "the session expired");
  }
  return retry;
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const config = await loadConfig();
  const base = config.apiURL === window.location.origin ? "" : config.apiURL;
  const res = await authorized((bearer) =>
    fetch(`${base}/api/v1${path}`, {
      method,
      headers: {
        authorization: `Bearer ${bearer}`,
        ...(body !== undefined ? { "content-type": "application/json" } : {}),
      },
      body: body !== undefined ? JSON.stringify(body) : undefined,
    }),
  );
  if (!res.ok) {
    let message = `${res.status}`;
    try {
      message = ((await res.json()) as { error: string }).error;
    } catch {
      // keep the status
    }
    throw new APIError(res.status, message);
  }
  // A delete that answers 204 has nothing to parse.
  if (res.status === 204) return undefined as T;
  return res.json();
}

const list =
  <T>(path: string) =>
  async (query?: Record<string, string>): Promise<T[]> => {
    const qs = query && Object.keys(query).length ? `?${new URLSearchParams(query)}` : "";
    const body = await request<{ items: T[] }>("GET", `${path}${qs}`);
    return body.items;
  };

/**
 * Follow an endpoint as Server-Sent Events. The server sends the current page
 * first and then every row that arrives, until `signal` aborts. Uses fetch
 * rather than EventSource because the API wants a bearer token, which
 * EventSource cannot carry. Throws when the stream cannot be established or
 * drops — the caller's cue to fall back to polling.
 *
 * It is generic over the row because the API's two live tails — log lines and
 * the edge's requests — are the same loop over different rows, on the server
 * as much as here.
 */
async function streamRows<T>(path: string, onRow: (row: T) => void, signal: AbortSignal): Promise<void> {
  const config = await loadConfig();
  const base = config.apiURL === window.location.origin ? "" : config.apiURL;
  const res = await authorized((bearer) =>
    fetch(`${base}/api/v1${path}`, {
      headers: { authorization: `Bearer ${bearer}`, accept: "text/event-stream" },
      signal,
    }),
  );
  if (!res.ok || !res.body || !(res.headers.get("content-type") ?? "").includes("text/event-stream")) {
    throw new APIError(res.status, `streaming unavailable (${res.status})`);
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) throw new APIError(0, "the stream ended");
    buffer += decoder.decode(value, { stream: true });
    let boundary;
    while ((boundary = buffer.indexOf("\n\n")) >= 0) {
      const chunk = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      let event = "message";
      let data = "";
      for (const line of chunk.split("\n")) {
        if (line.startsWith("event:")) event = line.slice(6).trim();
        if (line.startsWith("data:")) data += line.slice(5).trim();
      }
      if (!data) continue;
      if (event === "error") {
        let message = data;
        try {
          message = (JSON.parse(data) as { error: string }).error;
        } catch {
          // keep the raw payload
        }
        throw new APIError(0, message);
      }
      try {
        onRow(JSON.parse(data) as T);
      } catch {
        // an unreadable event is dropped, the stream lives on
      }
    }
  }
}

export interface LogQuery {
  limit?: number;
  since?: string;
  until?: string;
  search?: string;
  container?: string;
}

/** The selection as query parameters, leaving out what was not asked. */
function selectionParams(selection: LogSelection): URLSearchParams {
  const params = new URLSearchParams();
  if (selection.q) params.set("q", selection.q);
  if (selection.where) params.set("where", selection.where);
  if (selection.since) params.set("since", selection.since);
  if (selection.until) params.set("until", selection.until);
  return params;
}

/** The window and route filter the four request reads share. */
function requestParams(window: RequestWindow): URLSearchParams {
  const params = new URLSearchParams();
  if (window.since) params.set("since", window.since);
  if (window.until) params.set("until", window.until);
  if (window.route) params.set("route", window.route);
  return params;
}

/** The listing's own filters on top of that window. */
function requestListParams(query: RequestListQuery): URLSearchParams {
  const params = requestParams(query);
  if (query.method) params.set("method", query.method);
  if (query.status) params.set("status", query.status);
  if (query.errors) params.set("errors", "1");
  if (query.limit) params.set("limit", String(query.limit));
  return params;
}

function logQuery(query: LogQuery): string {
  const params = new URLSearchParams();
  if (query.limit) params.set("limit", String(query.limit));
  if (query.since) params.set("since", query.since);
  if (query.until) params.set("until", query.until);
  if (query.search) params.set("search", query.search);
  if (query.container) params.set("container", query.container);
  const qs = params.toString();
  return qs ? `?${qs}` : "";
}

export const api = {
  projects: list<Project>("/projects"),
  createProject: (project: NewProject) => request<Project>("POST", "/projects", project),
  project: (name: string) => request<Project>("GET", `/projects/${name}`),
  updateProject: (name: string, changes: ProjectSettings) =>
    request<Project>("PATCH", `/projects/${name}`, changes),
  deleteProject: (name: string) => request<Project>("DELETE", `/projects/${name}`),
  projectBuilds: (name: string) => list<Build>(`/projects/${name}/builds`)(),
  projectReleases: (name: string) => list<Release>(`/projects/${name}/releases`)(),
  projectEnvironments: (name: string) => list<Environment>(`/projects/${name}/environments`)(),
  rebuild: (project: string, revision?: { sha: string; branch?: string }) =>
    request<Build>("POST", `/projects/${project}/builds`, revision ?? {}),

  // The platform upgrading itself. Creating an update takes a version and
  // nothing else; every other decision is the operator's.
  updates: () => request<PlatformUpdates>("GET", "/updates"),
  startUpdate: (version: string) => request<PlatformUpdate>("POST", "/updates", { version }),

  builds: list<Build>("/builds"),
  build: (name: string) => request<Build>("GET", `/builds/${name}`),
  cancelBuild: (name: string) => request<Build>("POST", `/builds/${name}/cancel`),
  buildLogs: (name: string, query: LogQuery = {}) =>
    request<{ items: LogLine[] }>("GET", `/builds/${name}/logs${logQuery(query)}`).then((b) => b.items),

  releases: list<Release>("/releases"),

  environments: list<Environment>("/environments"),
  environment: (name: string) => request<Environment>("GET", `/environments/${name}`),
  moveEnvironment: (name: string, release: string) =>
    request<Environment>("PATCH", `/environments/${name}`, { release }),
  deleteEnvironment: (name: string) => request<Environment>("DELETE", `/environments/${name}`),
  environmentWorkload: (name: string) => request<Workload>("GET", `/environments/${name}/workload`),
  // What the workload endpoint cannot be: the same environment over time.
  environmentMetrics: (name: string, query: { since?: string; until?: string; points?: number } = {}) => {
    const params = new URLSearchParams();
    if (query.since) params.set("since", query.since);
    if (query.until) params.set("until", query.until);
    if (query.points) params.set("points", String(query.points));
    return request<ResourceSeries>("GET", `/environments/${name}/metrics?${params}`);
  },
  environmentObjects: (name: string) => request<EnvironmentObjects>("GET", `/environments/${name}/objects`),
  environmentLogs: (name: string, query: LogQuery = {}) =>
    request<{ items: LogLine[] }>("GET", `/environments/${name}/logs${logQuery(query)}`).then((b) => b.items),

  // The observability surface. An empty selection is a legitimate question —
  // everything in the window — so nothing has to be typed to ask it.
  logs: (selection: LogSelection, limit?: number) => {
    const params = selectionParams(selection);
    if (limit) params.set("limit", String(limit));
    return request<{ items: LogLine[] }>("GET", `/logs?${params}`).then((b) => b.items);
  },

  // The same selection, asked three other ways: when, what else is in it, and
  // what it is actually saying.
  logHistogram: (selection: LogSelection, buckets?: number) => {
    const params = selectionParams(selection);
    if (buckets) params.set("buckets", String(buckets));
    return request<LogHistogram>("GET", `/logs/histogram?${params}`);
  },
  logFacets: (selection: LogSelection, fields?: string[]) => {
    const params = selectionParams(selection);
    if (fields?.length) params.set("fields", fields.join(","));
    // A facet no line in the window holds has no values. An operator running
    // an older API against this dashboard gets `null` there rather than an
    // empty list, so it is normalised here and the type stays honest.
    return request<{ items: LogFacet[] }>("GET", `/logs/facets?${params}`).then((b) =>
      b.items.map((facet) => ({ ...facet, values: facet.values ?? [] })),
    );
  },
  logPatterns: (selection: LogSelection, limit?: number) => {
    const params = selectionParams(selection);
    if (limit) params.set("limit", String(limit));
    return request<{ items: LogPattern[] }>("GET", `/logs/patterns?${params}`).then((b) => b.items);
  },

  // A question worth keeping. The URL already makes any selection a link;
  // this is what makes one findable by whoever did not get the link.
  savedQueries: list<SavedQuery>("/logs/saved"),
  saveQuery: (query: NewSavedQuery) => request<SavedQuery>("POST", "/logs/saved", query),
  deleteSavedQuery: (name: string) => request<SavedQuery>("DELETE", `/logs/saved/${encodeURIComponent(name)}`),

  // Live tails of the same log endpoints, as Server-Sent Events.
  streamBuildLogs: (name: string, query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
    streamRows<LogLine>(`/builds/${name}/logs${logQuery(query)}`, onLine, signal),
  streamEnvironmentLogs: (name: string, query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
    streamRows<LogLine>(`/environments/${name}/logs${logQuery(query)}`, onLine, signal),
  streamLogs: (selection: LogSelection, limit: number, onLine: (line: LogLine) => void, signal: AbortSignal) => {
    const params = selectionParams(selection);
    if (limit) params.set("limit", String(limit));
    return streamRows<LogLine>(`/logs?${params}`, onLine, signal);
  },

  // What the internet asked of an environment. The three aggregate reads come
  // off the rollups — a year-wide summary costs what an hour's does — and the
  // listing off the raw rows, which are kept for seven days.
  requestSummary: (name: string, window: RequestWindow = {}) =>
    request<RequestSummary>("GET", `/environments/${name}/requests/summary?${requestParams(window)}`),
  requestSeries: (name: string, window: RequestWindow & { buckets?: number } = {}) => {
    const params = requestParams(window);
    if (window.buckets) params.set("buckets", String(window.buckets));
    return request<RequestSeries>("GET", `/environments/${name}/requests/series?${params}`);
  },
  // The sort travels to the server because it decides which rows survive the
  // limit: the ten busiest routes and the ten slowest are not the same ten.
  requestRoutes: (name: string, window: RequestWindow & { sort?: RouteSort; limit?: number } = {}) => {
    const params = requestParams(window);
    if (window.sort) params.set("sort", window.sort);
    if (window.limit) params.set("limit", String(window.limit));
    return request<{ items: RequestRoute[]; environment: string; edge: EdgeStatus }>(
      "GET",
      `/environments/${name}/requests/routes?${params}`,
    );
  },
  // The rows themselves, newest first. The body is an object rather than a
  // bare collection because the edge's answer belongs beside them: an empty
  // list means one thing for an environment on the edge and another for one
  // that is not.
  requests: (name: string, query: RequestListQuery = {}) =>
    request<{ items: RequestRow[]; environment: string; edge: EdgeStatus }>(
      "GET",
      `/environments/${name}/requests?${requestListParams(query)}`,
    ),
  // The same listing followed live, over the same loop the log tails use. The
  // server sends its page oldest first and then every request as it lands.
  streamRequests: (name: string, query: RequestListQuery, onRow: (row: RequestRow) => void, signal: AbortSignal) =>
    streamRows<RequestRow>(`/environments/${name}/requests?${requestListParams(query)}`, onRow, signal),

  // The crash report: exit code and reason, the last lines, the memory series
  // leading up to it, the cluster's warnings and the edge's requests, joined.
  environmentDiagnostics: (name: string, sizes: { logs?: number; requests?: number } = {}) => {
    const params = new URLSearchParams();
    if (sizes.logs) params.set("logs", String(sizes.logs));
    if (sizes.requests) params.set("requests", String(sizes.requests));
    return request<Diagnostics>("GET", `/environments/${name}/diagnostics?${params}`);
  },

  // The platform's recent activity, newest first.
  events: (query: { project?: string; limit?: number } = {}) => {
    const params: Record<string, string> = {};
    if (query.project) params.project = query.project;
    if (query.limit) params.limit = String(query.limit);
    return list<PlatformEvent>("/events")(params);
  },

  // The dashboard's numbers, pre-aggregated server-side.
  metricsOverview: (project?: string) =>
    request<MetricsOverview>("GET", `/metrics/overview${project ? `?project=${encodeURIComponent(project)}` : ""}`),

  // Traces, from applications that instrument themselves. The two filters are
  // the two reasons anyone opens a trace list: something failed, or it was
  // slow.
  traces: (
    query: {
      project?: string;
      environment?: string;
      service?: string;
      since?: string;
      until?: string;
      errors?: boolean;
      minDuration?: number;
      limit?: number;
    } = {},
  ) => {
    const params: Record<string, string> = {};
    if (query.project) params.project = query.project;
    if (query.environment) params.environment = query.environment;
    if (query.service) params.service = query.service;
    if (query.since) params.since = query.since;
    if (query.until) params.until = query.until;
    if (query.errors) params.errors = "1";
    if (query.minDuration) params.minDuration = String(query.minDuration);
    if (query.limit) params.limit = String(query.limit);
    return list<Trace>("/traces")(params);
  },
  // No window: a trace id arrives from a log line or from the list, and
  // needing to know when it happened would break that link.
  trace: (traceId: string) => request<TraceDetail>("GET", `/traces/${encodeURIComponent(traceId)}`),

  // The service map's aggregated edges for a window.
  traffic: (query: { project?: string; since?: string; until?: string } = {}) => {
    const params: Record<string, string> = {};
    if (query.project) params.project = query.project;
    if (query.since) params.since = query.since;
    if (query.until) params.until = query.until;
    return list<TrafficEdge>("/traffic")(params);
  },

  connections: list<Connection>("/connections"),
  createConnection: (connection: NewConnection) =>
    request<Connection>("POST", "/connections", connection),
  updateConnection: (name: string, changes: ConnectionChanges) =>
    request<Connection>("PATCH", `/connections/${name}`, changes),
  testConnection: (test: ConnectionTestRequest) =>
    request<ConnectionTestResult>("POST", "/connections/test", test),
  deleteConnection: (name: string) => request<void>("DELETE", `/connections/${name}`),
  domains: list<Domain>("/domains"),
  domain: (name: string) => request<Domain>("GET", `/domains/${name}`),
  createDomain: (domain: NewDomain) => request<Domain>("POST", "/domains", domain),
  deleteDomain: (name: string) => request<Domain>("DELETE", `/domains/${name}`),
  claims: list<Claim>("/claims"),
  createClaim: (claim: NewClaim) => request<Claim>("POST", "/claims", claim),
  // Answers 202: the operator's finalizer finishes the teardown — branches,
  // binding secrets and, under deletionPolicy Delete, the database itself.
  deleteClaim: (name: string) => request<Claim>("DELETE", `/claims/${name}`),

  // The platform as it is running: cluster, tunnel, build queue, components.
  status: () => request<PlatformStatus>("GET", "/status"),

  settings: () => request<Settings>("GET", "/settings"),
  updateSettings: (changes: Partial<Pick<Settings, "buildStrategy" | "buildConcurrency" | "logRetentionDays">>) =>
    request<Settings>("PATCH", "/settings", changes),
};
