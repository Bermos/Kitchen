/**
 * What the six project screens have in common.
 *
 * `/projects/:name` was one file and nine tabs: the health of the thing, its
 * release history, its builds, its previews, its domains, its claims, its
 * variables, its people and eleven cards of settings, all mounted at once so
 * that opening the members panel started the metrics pollers (#470). It is six
 * screens now — Overview, Deploys, Environments, Observability, Alerts,
 * Settings — and this module is the handful of derivations more than one of
 * them needs, so that the split is a split of the *screens* rather than of the
 * reasoning.
 *
 * Nothing here fetches and nothing here renders. Every function is a pure
 * reading of payloads the API already answers with, which is what lets
 * `project.test.ts` pin the parts that are easy to get subtly wrong — which
 * artifact a process deploys, and what the auto-rollback column is actually
 * claiming.
 */
import type { Artifact, Build, Claim, Environment, Exception, Exposure, Process, Project, Promotion } from "./api";

/**
 * What the platform calls a project's own image, and its own web process.
 *
 * It is not one of `spec.processes[]`: the published process is `spec.runtime`,
 * deliberately singular because the URL is. `web` is the name every other
 * surface already uses for it — `WebProcessName` in the operator, the artifact
 * name in a build, the workload a volume claim mounts into — so it is the name
 * used here too rather than a second word for one thing.
 */
export const WEB_PROCESS = "web";

/** The host of a published URL, or "" for an environment that has none. */
export function host(url?: string): string {
  if (!url) return "";
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

// ── The Processes table ─────────────────────────────────────────────────────

/**
 * One row of the Overview's Processes table.
 *
 * **The word is processes, never services.** `service` is one of the four
 * `ProcessType` values and means something narrow — runs continuously, is
 * addressed from inside the platform and from nowhere else — so a table headed
 * Services listing the web process, the workers and the scheduled jobs would
 * contradict the type of three of its own rows (#470).
 */
export interface ProcessRow {
  name: string;
  /** `web` for the project's own, otherwise the declared `ProcessType`. */
  type: string;
  /** The release this row is running. The unit deploys as one, so every row of
   * one environment carries the same release — which is the fact worth showing
   * rather than hiding: a row that disagreed would be a deploy half landed. */
  release: string;
  /** `ready/wanted` for anything that runs continuously; empty for a scheduled
   * job and a deploy task, which run once and have no replicas. */
  replicas: string;
  /** Who can reach it, in one phrase. Never "nothing": a worker that nothing
   * addresses is a fact about the worker, and saying it is the point of the
   * column. */
  addressedFrom: string;
  /** The published address, for the one row that has one. */
  url?: string;
}

/**
 * The project's processes as the Overview lists them: the published one first,
 * then everything the project declares.
 *
 * `live` is what the production environment is actually running
 * (`GET /environments/{name}/processes`), which is the only place the replica
 * counts and a service's in-platform address exist. It is allowed to be empty
 * — a project with nothing deployed still has a declaration to show, and the
 * counts are simply absent rather than zero.
 */
export function processRows(project: Project, production?: Environment, live: Process[] = []): ProcessRow[] {
  const release = production?.release ?? "";
  const rows: ProcessRow[] = [
    {
      name: WEB_PROCESS,
      type: WEB_PROCESS,
      release,
      replicas: production ? `${project.replicas ?? 1}` : "",
      addressedFrom: production?.url ? "the internet" : "not published yet",
      url: production?.url,
    },
  ];
  for (const declared of project.processes ?? []) {
    const running = live.find((candidate) => candidate.name === declared.name);
    rows.push({
      name: declared.name,
      type: declared.type,
      release,
      replicas: replicaLine(declared, running),
      addressedFrom: addressedFrom(declared.type, running?.address),
    });
  }
  return rows;
}

function replicaLine(declared: Process, running?: Process): string {
  if (declared.type === "cron" || declared.type === "task") return "";
  const wanted = running?.replicas ?? declared.replicas ?? 1;
  if (!running) return `${wanted}`;
  return `${running.readyReplicas ?? 0}/${wanted}`;
}

/**
 * Who addresses a process of this type, in the words the platform means them
 * in — and the whole reason the column exists is the two rows that answer
 * "nobody", because that is what makes a worker's empty request chart correct
 * rather than broken.
 */
export function addressedFrom(type: string, address?: string): string {
  switch (type) {
    case WEB_PROCESS:
      return "the internet";
    case "service":
      return address ? `this project, at ${address}` : "this project";
    case "worker":
      return "nothing — it is never addressed";
    case "cron":
      return "nothing — it runs on its schedule";
    case "task":
      return "nothing — it runs once per deploy";
    default:
      return "nothing";
  }
}

/** Whether a process of this type gets a route, and so has requests, latency
 * and error rates worth charting. Only the web process does: a `service` is
 * addressed from inside the platform and published nowhere. */
export function hasRoute(type: string): boolean {
  return type === WEB_PROCESS;
}

/**
 * Why a process has no traffic to show, in the words the empty answer uses.
 *
 * `docs/OBSERVABILITY.md` §3.4 is the rule this implements: an unaddressed
 * workload is shown its own signals rather than four charts of zeroes, and is
 * told which it is. A chart of zeroes and "nothing measures this" are different
 * claims, and only one of them is true.
 */
export function noRouteReason(type: string): string {
  switch (type) {
    case "service":
      return "no route: addressed only from web";
    case "worker":
      return "no route: nothing addresses a worker";
    case "cron":
      return "no route: a scheduled job is not addressed";
    case "task":
      return "no route: a deploy task is not addressed";
    default:
      return "no route";
  }
}

// ── The Deploys process filter ──────────────────────────────────────────────

/**
 * Which artifact of a build one process deploys.
 *
 * **The filter is over artifacts, not over builds.** A Build is per project,
 * and it produces a `BuildArtifact` per workload — named by workload, with
 * `web` reserved for the project's own image. A process declares a build of its
 * own with `spec.processes[].build`, or runs an image the platform did not
 * build with `spec.processes[].image`; a process that declared neither shares
 * the web artifact, so it *shares* the project's timeline rather than
 * disappearing from it (#470, decision 3).
 *
 * That fallback is the whole of why this is a function and not a filter written
 * inline on the screen: "no artifact of its own" and "not in this build" are
 * the same shape and opposite answers.
 */
export function artifactFor(build: Build, process: string): { name: string; artifact?: Artifact } | undefined {
  if (!process || process === WEB_PROCESS) return { name: WEB_PROCESS, artifact: build.artifact };
  const own = (build.workloads ?? []).find((workload) => workload.name === process);
  if (own) return { name: own.name, artifact: own.artifact };
  // The process built nothing of its own in this build, so what it runs is the
  // project's image — the web artifact, which every build has.
  return { name: WEB_PROCESS, artifact: build.artifact };
}

/** Every artifact name a build produced, web first. */
export function artifactNames(build: Build): string[] {
  return [WEB_PROCESS, ...(build.workloads ?? []).map((workload) => workload.name)];
}

/**
 * The processes the Deploys filter offers: the ones the project declares,
 * plus any a build produced an artifact for that the project no longer
 * declares — a workload removed last week still has a history.
 */
export function deployedProcesses(project: Project | undefined, builds: Build[]): string[] {
  const names = new Set<string>([WEB_PROCESS]);
  for (const declared of project?.processes ?? []) names.add(declared.name);
  for (const build of builds) for (const name of artifactNames(build)) names.add(name);
  return [...names];
}

// ── The deploy timeline ─────────────────────────────────────────────────────

/** One thing that happened to this project's software, whichever kind it was. */
export type DeployEntry =
  | { kind: "build"; at: string; key: string; build: Build }
  | { kind: "promotion"; at: string; key: string; promotion: Promotion };

/**
 * Builds and promotions in one order.
 *
 * They were two lists on two tabs, and reading them meant interleaving them by
 * eye — which is the one thing a reader cannot do reliably and a sort can. A
 * promotion is a decision about a build's release, so the two belong on one
 * timeline: the build that produced the release, and what the policy then
 * allowed or refused to do with it.
 */
export function deployEntries(builds: Build[], promotions: Promotion[]): DeployEntry[] {
  const entries: DeployEntry[] = [
    ...builds.map((build) => ({ kind: "build" as const, at: build.createdAt, key: `build/${build.name}`, build })),
    ...promotions.map((promotion) => ({
      kind: "promotion" as const,
      at: promotion.createdAt,
      key: `promotion/${promotion.name}`,
      promotion,
    })),
  ];
  return entries.sort((a, b) => (a.at === b.at ? a.key.localeCompare(b.key) : a.at < b.at ? 1 : -1));
}

// ── The auto-rollback column ────────────────────────────────────────────────

/**
 * What the auto-rollback column claims, which is not what its name sounds
 * like.
 *
 * **There is no per-environment automatic rollback.** The field is
 * `Exception.spec.autoRollback`, a property of a break-glass compliance grant,
 * off by default. `internal/controller/rescan.go` acts on it narrowly: only
 * when the grant has expired unresolved, the verdict is `Blocked`, a rule that
 * grant waived is now firing unwaived, and there is a previous release to go
 * back to.
 *
 * So the column says *"an expired exception covering this environment will roll
 * it back"* and never *"this environment rolls back on a bad deploy"* — the
 * second would be a promise the platform does not make (#470).
 */
export interface AutoRollback {
  /** Whether any grant covering this environment asks for it at all. */
  armed: boolean;
  /** The column's own word. */
  label: string;
  /** The sentence behind it, in full. */
  detail: string;
  tone: "neutral" | "warning";
  /** The grants this is about, newest expiry first, for the link out. */
  exceptions: Exception[];
}

export function autoRollbackFor(environment: Environment, exceptions: Exception[]): AutoRollback {
  const covering = exceptions
    .filter((exception) => exception.autoRollback && exception.environment === environment.name)
    // A grant naming a release covers only that release; one naming none
    // covers whatever the environment is running.
    .filter((exception) => !exception.release || exception.release === environment.release)
    .filter((exception) => exception.phase !== "Resolved")
    .sort((a, b) => (a.expiresAt < b.expiresAt ? 1 : -1));

  if (!covering.length) {
    return {
      armed: false,
      label: "none",
      detail:
        "No break-glass grant covering this environment asks for a rollback. This is not a promise that a bad deploy is rolled back — the platform makes none.",
      tone: "neutral",
      exceptions: [],
    };
  }
  const expired = covering.filter((exception) => exception.phase === "Expired");
  if (expired.length) {
    return {
      armed: true,
      label: "expired grant",
      detail: `${names(expired)} expired unresolved and asked for a rollback. If a rule ${
        expired.length === 1 ? "it waived" : "they waived"
      } is still firing against what is deployed here, this environment goes back to its previous release.`,
      tone: "warning",
      exceptions: covering,
    };
  }
  return {
    armed: true,
    label: "on expiry",
    detail: `${names(covering)} waives a rule for this environment and asks for a rollback on expiry. Nothing happens while the grant is active, or if it is resolved before it expires.`,
    tone: "neutral",
    exceptions: covering,
  };
}

function names(exceptions: Exception[]): string {
  return exceptions.map((exception) => exception.name).join(", ");
}

// ── The Attached resources table ────────────────────────────────────────────

/**
 * Which processes a claim reaches.
 *
 * A volume is mounted into exactly one process and says so; everything else
 * binds a secret the environment's variables reference, which every process of
 * the unit reads. "Used by" is therefore two different questions with one
 * column, and answering the second with a list of every process would be true
 * and useless.
 */
export function claimUsedBy(claim: Claim): string {
  if (claim.volume?.process) return claim.volume.process;
  if (claim.type === "oidcClient") return "whatever signs people in";
  return claim.secret ? "every process, through the variables" : "not bound yet";
}

/** A claim's plan, in the sense the Attached resources table means it: what was
 * asked for, in one phrase, or the connection it came through. */
export function claimPlan(claim: Claim): string {
  if (claim.volume) return claim.volume.size || claim.volume.bound?.capacity || "bound storage";
  if (claim.postgres?.version) return `pg ${claim.postgres.version}`;
  if (claim.redis) return claim.redis.usage ?? "cache";
  if (claim.inngest) return claim.inngest.selfHosted ? "self-hosted" : "Inngest Cloud";
  if (claim.objectStore?.size) return claim.objectStore.size;
  return claim.connection || "—";
}

// ── The Settings sub-navigation ─────────────────────────────────────────────

/**
 * Settings is a left rail and one pane at a time, the way `/platform` is a
 * scope and one screen at a time.
 *
 * Twelve panels behind a rail is a normal shape; twelve panels on one scroll is
 * the shape #470 objects to — and since the settings-bound panels are more
 * lines than the whole of the host page was, a single-scroll Settings would
 * re-create the problem one level down. Each pane is its own `max-w-3xl`
 * column, which is how "one page, one form width" is honoured rather than
 * asserted (docs/UI.md).
 *
 * The id is in the address as `?section=`, so a pane is a link: "the fork
 * policy is here" is a thing somebody can send.
 */
export interface SettingsSection {
  id: string;
  label: string;
  /** What this pane answers, under its heading. */
  description: string;
  /** A pane that only exists for a project built from a repository — a project
   * running a vendored image has no branch, no previews and no build. */
  builtHereOnly?: boolean;
}

export const SETTINGS_SECTIONS: SettingsSection[] = [
  {
    id: "source",
    label: "Source",
    description: "Where this project's software comes from, and what is done to it before it runs.",
  },
  {
    id: "processes",
    label: "Processes",
    description: "What this project runs besides its web process: workers, services, scheduled jobs and deploy tasks.",
  },
  {
    id: "resources",
    label: "Attached resources",
    description: "What this project depends on, and what it costs to take one away.",
  },
  { id: "variables", label: "Variables", description: "The environment this project's processes start with." },
  { id: "files", label: "Files", description: "The configuration files the platform places into the workloads." },
  { id: "secrets", label: "Secrets", description: "The credentials a variable points at. Nothing here is read back." },
  { id: "domains", label: "Domains", description: "The hostnames attached to this project's environments." },
  { id: "members", label: "Members", description: "Who is on this project, and what each of them may do." },
  { id: "keys", label: "Keys", description: "The non-human members: one key, one project, issued and revoked here." },
  {
    id: "notifications",
    label: "Notifications",
    description: "Where this project's own activity is sent.",
  },
  {
    id: "runtime",
    label: "Runtime",
    description: "How much of this project runs, what it is started with, and what the platform asks it before sending anyone to it.",
  },
  {
    id: "security",
    label: "Security",
    description: "What the containers are allowed to be, for every workload this project ships.",
  },
  {
    id: "continuity",
    label: "Continuity",
    description: "What class of data this handles, and the tolerances the institution set for it.",
  },
  { id: "danger", label: "Danger zone", description: "Removing the project and everything it is running." },
];

/** The section a `?section=` names, or the first one for anything else. A
 * bookmark to a pane that has been renamed opens Settings rather than nothing. */
export function settingsSection(id: unknown): SettingsSection {
  return SETTINGS_SECTIONS.find((section) => section.id === id) ?? SETTINGS_SECTIONS[0];
}

/** Whether the project is on the internet, as the settings form offers it. The
 * label says what the platform *does* with the project rather than repeating
 * the value, because "internal" on its own does not tell an admin that every
 * environment is about to lose its address. */
export const EXPOSURE_OPTIONS: { label: string; value: Exposure }[] = [
  { label: "public — every environment gets a hostname on the internet", value: "public" },
  { label: "internal — nothing is published; reachable inside the cluster only", value: "internal" },
];

/** The sentence under that field: what the setting as it stands does, and what
 * changing it would do. An internal project loses three things at once and
 * each of them is somewhere somebody would otherwise go looking for a fault —
 * the missing address, the refused domain, and the environment that stopped
 * idling — so all three are named here rather than met one at a time. */
export function exposureNote(exposure: Exposure): string {
  if (exposure === "internal") {
    return (
      "No environment of this project is published: no hostname, no certificate and no preview gate, previews " +
      "included. Each one still runs and is still reachable from the other applications on this platform, at the " +
      "address their bindings carry — and none of them can idle, because nothing routes to them that could wake a " +
      "parked one. A custom domain and an OIDC client claim are refused while this is set."
    );
  }
  return (
    "Every environment is published at a generated hostname, and previews are gated according to the preview " +
    "settings. Turning this to internal takes those addresses away on the next reconcile."
  );
}
