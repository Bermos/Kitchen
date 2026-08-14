import { loadConfig } from "./config";
import { signOut, token } from "./auth";

// Typed client for the operator REST API (docs/API.md). The types mirror the
// API's view shapes — the platform's own vocabulary, nothing Kubernetes.

export interface Condition {
  type: string;
  status: string;
  reason?: string;
  message?: string;
  lastTransitionTime: string;
}

export interface Project {
  name: string;
  repo: string;
  connection: string;
  registry: string;
  productionBranch: string;
  previews: boolean;
  productionEnvironment?: string;
  latestBuild?: string;
  createdAt: string;
  conditions?: Condition[];
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

export interface Connection {
  name: string;
  provider: string;
  capabilities?: string[];
  createdAt: string;
  conditions?: Condition[];
}

export interface Domain {
  name: string;
  hostname: string;
  environment: string;
  tls?: string;
  verified: boolean;
  createdAt: string;
  conditions?: Condition[];
}

export interface Claim {
  name: string;
  project: string;
  connection: string;
  type: string;
  phase?: string;
  secret?: string;
  createdAt: string;
  conditions?: Condition[];
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

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const config = await loadConfig();
  const bearer = token();
  if (!bearer) {
    signOut();
    throw new APIError(401, "not signed in");
  }
  const base = config.apiURL === window.location.origin ? "" : config.apiURL;
  const res = await fetch(`${base}/api/v1${path}`, {
    method,
    headers: {
      authorization: `Bearer ${bearer}`,
      ...(body !== undefined ? { "content-type": "application/json" } : {}),
    },
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) {
    // The token expired or the issuer rotated its keys under us: back through
    // the login, which is invisible for a browser that still has a session.
    signOut();
    throw new APIError(401, "the session expired");
  }
  if (!res.ok) {
    let message = `${res.status}`;
    try {
      message = ((await res.json()) as { error: string }).error;
    } catch {
      // keep the status
    }
    throw new APIError(res.status, message);
  }
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
 * Follow a log endpoint as Server-Sent Events. The server sends the current
 * page first and then every line that arrives, until `signal` aborts. Uses
 * fetch rather than EventSource because the API wants a bearer token, which
 * EventSource cannot carry. Throws when the stream cannot be established or
 * drops — the caller's cue to fall back to polling.
 */
async function streamLines(path: string, onLine: (line: LogLine) => void, signal: AbortSignal): Promise<void> {
  const config = await loadConfig();
  const bearer = token();
  if (!bearer) {
    signOut();
    throw new APIError(401, "not signed in");
  }
  const base = config.apiURL === window.location.origin ? "" : config.apiURL;
  const res = await fetch(`${base}/api/v1${path}`, {
    headers: { authorization: `Bearer ${bearer}`, accept: "text/event-stream" },
    signal,
  });
  if (res.status === 401) {
    signOut();
    throw new APIError(401, "the session expired");
  }
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
        onLine(JSON.parse(data) as LogLine);
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
  projectBuilds: (name: string) => list<Build>(`/projects/${name}/builds`)(),
  projectReleases: (name: string) => list<Release>(`/projects/${name}/releases`)(),
  projectEnvironments: (name: string) => list<Environment>(`/projects/${name}/environments`)(),
  rebuild: (project: string, revision?: { sha: string; branch?: string }) =>
    request<Build>("POST", `/projects/${project}/builds`, revision ?? {}),

  builds: list<Build>("/builds"),
  build: (name: string) => request<Build>("GET", `/builds/${name}`),
  buildLogs: (name: string, query: LogQuery = {}) =>
    request<{ items: LogLine[] }>("GET", `/builds/${name}/logs${logQuery(query)}`).then((b) => b.items),

  releases: list<Release>("/releases"),

  environments: list<Environment>("/environments"),
  environment: (name: string) => request<Environment>("GET", `/environments/${name}`),
  moveEnvironment: (name: string, release: string) =>
    request<Environment>("PATCH", `/environments/${name}`, { release }),
  environmentLogs: (name: string, query: LogQuery = {}) =>
    request<{ items: LogLine[] }>("GET", `/environments/${name}/logs${logQuery(query)}`).then((b) => b.items),

  // The observability surface: a ClickHouse expression over the whole logs
  // table, evaluated as written (read-only, capped server-side).
  logs: (where: string, query: LogQuery = {}) => {
    const params = new URLSearchParams({ where });
    if (query.limit) params.set("limit", String(query.limit));
    if (query.since) params.set("since", query.since);
    if (query.until) params.set("until", query.until);
    return request<{ items: LogLine[] }>("GET", `/logs?${params}`).then((b) => b.items);
  },

  // Live tails of the same log endpoints, as Server-Sent Events.
  streamBuildLogs: (name: string, query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
    streamLines(`/builds/${name}/logs${logQuery(query)}`, onLine, signal),
  streamEnvironmentLogs: (name: string, query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
    streamLines(`/environments/${name}/logs${logQuery(query)}`, onLine, signal),
  streamLogs: (where: string, query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) => {
    const params = new URLSearchParams({ where });
    if (query.limit) params.set("limit", String(query.limit));
    if (query.since) params.set("since", query.since);
    return streamLines(`/logs?${params}`, onLine, signal);
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

  // The service map's aggregated edges for a window.
  traffic: (query: { project?: string; since?: string; until?: string } = {}) => {
    const params: Record<string, string> = {};
    if (query.project) params.project = query.project;
    if (query.since) params.since = query.since;
    if (query.until) params.until = query.until;
    return list<TrafficEdge>("/traffic")(params);
  },

  connections: list<Connection>("/connections"),
  domains: list<Domain>("/domains"),
  claims: list<Claim>("/claims"),

  settings: () => request<Settings>("GET", "/settings"),
  updateSettings: (changes: Partial<Pick<Settings, "buildStrategy" | "buildConcurrency" | "logRetentionDays">>) =>
    request<Settings>("PATCH", "/settings", changes),
};
