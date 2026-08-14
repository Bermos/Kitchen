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

export interface Environment {
  name: string;
  project: string;
  type: string;
  release: string;
  observedRelease?: string;
  phase?: string;
  url?: string;
  preview?: Preview;
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
  message: string;
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

  connections: list<Connection>("/connections"),
  domains: list<Domain>("/domains"),
  claims: list<Claim>("/claims"),

  settings: () => request<Settings>("GET", "/settings"),
  updateSettings: (changes: Partial<Pick<Settings, "buildStrategy" | "buildConcurrency" | "logRetentionDays">>) =>
    request<Settings>("PATCH", "/settings", changes),
};
