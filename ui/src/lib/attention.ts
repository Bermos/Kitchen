/**
 * What is wrong, in the order somebody would deal with it.
 *
 * The overview used to render a broken project as one row among four healthy
 * ones — a red dot, and the one string that would have told you what to do
 * truncated at `max-w-sm`. Resolving it took at least two navigations: the
 * phase badge for the log, the environment screen for the rollback. This is
 * the derivation behind the band that hoists those rows out of the table: what
 * broke, what it costs, and what the way out is, from the three lists the
 * screen already holds.
 *
 * Nothing here fetches. Every field comes off `/projects`, `/environments` and
 * `/builds` as the overview already asks for them — the failing container's
 * log tail included, which the API keeps on the build precisely because "a pod
 * is the operator's to read while a build that failed is the developer's to
 * fix" (`internal/api/views.go`).
 *
 * The set of projects with an incident is exactly the set the table marks with
 * an error dot. That is deliberate: the table dims what the band has taken, so
 * the two agreeing is what makes "in band" true rather than approximate.
 */
import { ref } from "vue";
import type { Build, Condition, Environment, Project } from "./api";
import { buildFailureLine } from "./builds";
import { shortSHA } from "./format";
import { statusDetail, unhealthyConditions } from "./status";

/**
 * How many incidents the band shows before it folds.
 *
 * Five is the point at which a band stops being a band. Past it the thing that
 * was supposed to put the worst problem where the eye lands becomes its own
 * scrolling list with the table pushed off the screen behind it — and a
 * platform with six things wrong at once has a different question to answer
 * ("what happened at 03:00"), which the events screen is for. The rest are one
 * click away rather than gone: the band says how many it is holding back.
 */
export const ATTENTION_CAP = 5;

export type IncidentKind = "build" | "environment" | "project";

/** One thing that is wrong, as the band renders it. */
export interface Incident {
  /** What this incident is about, stable across polls. */
  key: string;
  /**
   * What would have to change for this to be a different incident.
   *
   * A dismissal is recorded against this rather than against the key, which is
   * how "dismissed until the condition changes" is expressed: the same failure
   * on the next poll has the same signature and stays down; a new failure, a
   * new release, or a recovery has a different one and comes back.
   */
  signature: string;
  kind: IncidentKind;
  project: string;
  /** The caller's role on that project, so the band can decide whether to
   * offer the way out or only the way in. */
  role: string;
  /** The badge beside the name: what kind of work this is. */
  scope: string;
  /** The small facts under the name — a branch, a commit, a release. */
  facts: string[];
  /** The error line, as it was reported and at its full length. */
  error: string;
  /** What it costs, as its own sentence. */
  blastRadius: string;
  /** When this became true. */
  since?: string;
  /** The object's conditions. Operator content — the band gates them. */
  conditions: Condition[];
  /** The failing step's output, oldest line first. Empty when the build never
   * produced any, or when this is not a build. */
  log: string[];
  /** The build to retry, on a build incident. */
  build?: Build;
  /** The environment to roll back, on an environment incident. */
  environment?: Environment;
}

function host(url?: string): string {
  if (!url) return "";
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/** The newest transition among the conditions that are not True — when the
 * thing being complained about started being true. */
function unhealthySince(conditions: Condition[] | undefined): string | undefined {
  const times = unhealthyConditions(conditions)
    .map((c) => c.lastTransitionTime)
    .filter((t): t is string => Boolean(t))
    .sort();
  return times[times.length - 1];
}

/** What production is doing, in the words the blast radius needs. */
function productionServing(production: Environment | undefined): string {
  if (!production) return "nothing is published for this project yet";
  if (production.phase === "Live") return `production still serving ${production.release}`;
  return `production is ${(production.phase ?? "unknown").toLowerCase()}`;
}

/**
 * A failed build's blast radius.
 *
 * The question it answers is the one that decides whether this is worth
 * interrupting somebody for: did this break what is serving traffic, or only
 * the branch it was on. A build off the production branch cannot have touched
 * production — the release that is live was built from a different commit and
 * is still live — and saying so is what turns a red dot into a decision.
 */
function buildBlastRadius(project: Project, production: Environment | undefined, build: Build): string {
  const serving = productionServing(production);
  if (build.git.branch && build.git.branch !== project.productionBranch) return `preview only — ${serving}`;
  return production?.phase === "Live" ? `${serving} — this commit has not shipped` : serving;
}

/** A degraded environment's, which is the case where something *is* broken for
 * whoever is using it. */
function environmentBlastRadius(environment: Environment): string {
  const where = host(environment.url) || environment.name;
  const asked = environment.release;
  const running = environment.observedRelease;
  if (running && running !== asked) {
    return `${where} degraded — asked for ${asked}, still running ${running}`;
  }
  return `${where} degraded — still serving ${asked}`;
}

/**
 * Everything wrong, newest first.
 *
 * Newest first rather than worst first, because "worst" is not a judgement
 * this can make honestly — a degraded preview of somebody's demo and a
 * degraded production look identical from here — while "what just happened" is
 * the question somebody watching a deploy is actually asking. The blast radius
 * on each row is what ranks them, and it is a sentence rather than a sort key.
 */
export function incidentsFrom(projects: Project[], environments: Environment[], builds: Build[]): Incident[] {
  const incidents: Incident[] = [];
  for (const project of projects) {
    const production = environments.find((e) => e.name === project.productionEnvironment);
    const latestBuild = builds.find((b) => b.project === project.name);

    if (latestBuild?.phase === "Failed") {
      const error = buildFailureLine(latestBuild) || "the build failed and reported no reason";
      incidents.push({
        key: `build/${latestBuild.name}`,
        signature: `build/${latestBuild.name}/${error}`,
        kind: "build",
        project: project.name,
        role: project.role,
        scope: "build",
        facts: [latestBuild.git.branch, shortSHA(latestBuild.git.sha), latestBuild.name].filter(Boolean),
        error,
        blastRadius: buildBlastRadius(project, production, latestBuild),
        since: latestBuild.completedAt ?? latestBuild.createdAt,
        conditions: latestBuild.conditions ?? [],
        log: latestBuild.failure?.log ?? [],
        build: latestBuild,
      });
    }

    if (production?.phase === "Degraded") {
      const error = statusDetail(production.conditions) || "the environment is degraded and reported no reason";
      incidents.push({
        key: `environment/${production.name}`,
        signature: `environment/${production.name}/${production.release}/${error}`,
        kind: "environment",
        project: project.name,
        role: project.role,
        scope: production.type,
        facts: [production.type, production.release].filter(Boolean),
        error,
        blastRadius: environmentBlastRadius(production),
        since: unhealthySince(production.conditions) ?? production.createdAt,
        conditions: production.conditions ?? [],
        log: [],
        environment: production,
      });
    }

    // The residual case: the project itself is unhappy about something that is
    // neither its latest build nor its production environment — a connection
    // it cannot read, a registry that refuses it. It is emitted only when
    // nothing more specific was, because the specific one already says it.
    const projectTrouble = unhealthyConditions(project.conditions);
    const alreadySaid = incidents.some((i) => i.project === project.name);
    if (projectTrouble.length > 0 && !alreadySaid) {
      const error = statusDetail(project.conditions);
      incidents.push({
        key: `project/${project.name}`,
        signature: `project/${project.name}/${error}`,
        kind: "project",
        project: project.name,
        role: project.role,
        scope: "project",
        facts: [project.repo].filter(Boolean),
        error,
        blastRadius: productionServing(production),
        since: unhealthySince(project.conditions),
        conditions: project.conditions ?? [],
        log: [],
      });
    }
  }

  return incidents.sort((a, b) => {
    const left = a.since ?? "";
    const right = b.since ?? "";
    if (left === right) return 0;
    return left < right ? 1 : -1;
  });
}

// ---------------------------------------------------------------------------
// Dismissal
// ---------------------------------------------------------------------------

/**
 * What has been waved off, by signature.
 *
 * **A dismissal lasts until the condition changes, and no longer.** The
 * alternative — back on the next poll — makes the control useless during the
 * ten minutes somebody is already fixing the thing, and a dismissal that
 * outlived the failure would hide the *next* one, which is the failure mode
 * that makes people stop trusting a band like this at all. Signatures carry
 * the error, the release and the build, so a second failure of the same build
 * is a new incident and comes back.
 *
 * It is deliberately not persisted. A dismissal is a statement about this
 * sitting — "I have seen it, I am on it" — not a permanent silence, and a
 * reload is somebody asking the platform what is wrong again.
 */
const dismissed = ref(new Set<string>());

export function dismiss(incident: Incident): void {
  dismissed.value = new Set(dismissed.value).add(incident.signature);
}

export function isDismissed(incident: Incident): boolean {
  return dismissed.value.has(incident.signature);
}

/** Everything still worth showing: what the band holds, and what the table
 * dims as "in band". */
export function undismissed(incidents: Incident[]): Incident[] {
  return incidents.filter((incident) => !isDismissed(incident));
}

/** Forget every dismissal. The overview does this when nothing is wrong any
 * more, so that a set of signatures does not grow for the life of the tab. */
export function clearDismissals(): void {
  if (dismissed.value.size) dismissed.value = new Set();
}
