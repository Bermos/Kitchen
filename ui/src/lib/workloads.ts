import type { ImageWrite, Process, ProcessWrite } from "./api";

// The workloads editor's side of a project's process list.
//
// A project declares what it runs besides its web process, and until #310 the
// dashboard could only read it. That was defensible while every project had a
// repository — `kitchen.json` is the real surface, and the settings screen says
// so above the form — and it stopped being defensible the moment a project
// could have no repository at all: such a project has no file, so a screen that
// only reads is a unit that cannot be described.
//
// The route replaces the whole list, so the editor holds every workload as a
// draft and sends all of them on every save. What makes that safe is that a
// draft carries what it was read with: a workload nobody touched is written
// back exactly as it came, including the two fields that used not to survive
// the round trip — a parked workload's `replicas: 0` and a declared `previews`.

/** The four things a workload can be. The web process is not among them: it is
 * the project's own runtime, and it is singular because the URL is. */
export const WORKLOAD_TYPES = ["worker", "service", "cron", "task"] as const;

/** What a workload's image comes from, as the form asks it. `project` is the
 * ordinary case — the project's own image, started with another command. */
export type ImageOrigin = "project" | "build" | "image";

/** Whether a workload runs in previews: what it declared, or nothing, in which
 * case its type answers. */
export type PreviewsChoice = "default" | "yes" | "no";

/** One workload as the form holds it. Everything is a string where the form
 * edits it, because an input yields strings and a half-typed number is not a
 * number — the conversion happens once, on the way out. */
export interface WorkloadDraft {
  /** Stable across renders and never sent: `name` is editable, so it cannot
   * also be the key a list is rendered by. */
  key: string;
  name: string;
  type: string;
  /** Exec form, one word per line. Nothing is split on spaces: an argument
   * with a space in it is ordinary, and splitting would quietly break it. */
  command: string;
  args: string;
  port: string;
  replicas: string;
  singleton: boolean;
  cpu: string;
  memory: string;
  schedule: string;
  concurrencyPolicy: string;
  timeout: string;
  previews: PreviewsChoice;
  origin: ImageOrigin;
  imageRepository: string;
  imageTag: string;
  imageDigest: string;
  imageConnection: string;
  buildStrategy: string;
  buildRootDirectory: string;
  buildDockerfilePath: string;
  buildDockerfileTarget: string;
  /** A health check the workload declared. Off means none at all, which for a
   * worker is the default: its liveness is whether its process is running. */
  health: boolean;
  healthPath: string;
  healthPort: string;
  healthPeriod: string;
  healthTimeout: string;
  healthFailures: string;
  healthStartupFailures: string;
}

let nextKey = 0;

function numberField(value: number | undefined): string {
  return value === undefined ? "" : String(value);
}

function wordLines(words: string[] | undefined): string {
  return (words ?? []).join("\n");
}

function wordsOf(lines: string): string[] {
  return lines
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
}

/** Where a workload's image comes from, read off what it declared. */
export function originOf(workload: Process): ImageOrigin {
  if (workload.imageSource) return "image";
  if (workload.build) return "build";
  return "project";
}

/** The project's workloads as drafts. */
export function workloadDrafts(processes: Process[] | undefined): WorkloadDraft[] {
  return (processes ?? []).map((workload) => ({
    key: `read-${nextKey++}`,
    name: workload.name,
    type: workload.type,
    command: wordLines(workload.command),
    args: wordLines(workload.args),
    port: numberField(workload.port),
    replicas: numberField(workload.replicas),
    singleton: workload.singleton ?? false,
    cpu: workload.cpu ?? "",
    memory: workload.memory ?? "",
    schedule: workload.schedule ?? "",
    concurrencyPolicy: workload.concurrencyPolicy ?? "",
    timeout: workload.timeout ?? "",
    previews: workload.previews === undefined ? "default" : workload.previews ? "yes" : "no",
    origin: originOf(workload),
    imageRepository: workload.imageSource?.repository ?? "",
    imageTag: workload.imageSource?.tag ?? "",
    imageDigest: workload.imageSource?.digest ?? "",
    imageConnection: workload.imageSource?.connection ?? "",
    buildStrategy: workload.build?.strategy ?? "auto",
    buildRootDirectory: workload.build?.rootDirectory ?? "",
    buildDockerfilePath: workload.build?.dockerfilePath ?? "",
    buildDockerfileTarget: workload.build?.dockerfileTarget ?? "",
    health: Boolean(workload.health),
    healthPath: workload.health?.path ?? "",
    healthPort: numberField(workload.health?.port),
    healthPeriod: numberField(workload.health?.periodSeconds),
    healthTimeout: numberField(workload.health?.timeoutSeconds),
    healthFailures: numberField(workload.health?.failureThreshold),
    healthStartupFailures: numberField(workload.health?.startupFailureThreshold),
  }));
}

/** A workload the "Add workload" button just made. It starts as a worker with
 * no image of its own — the project's image run with another command, which is
 * the ordinary case and one build rather than two. A project with no repository
 * has no such image, so the form points such a workload at `image` instead. */
export function newWorkloadDraft(origin: ImageOrigin = "project"): WorkloadDraft {
  return {
    key: `new-${nextKey++}`,
    name: "",
    type: "worker",
    command: "",
    args: "",
    port: "",
    replicas: "1",
    singleton: false,
    cpu: "",
    memory: "",
    schedule: "",
    concurrencyPolicy: "",
    timeout: "",
    previews: "default",
    origin,
    imageRepository: "",
    imageTag: "",
    imageDigest: "",
    imageConnection: "",
    buildStrategy: "auto",
    buildRootDirectory: "",
    buildDockerfilePath: "",
    buildDockerfileTarget: "",
    health: false,
    healthPath: "",
    healthPort: "",
    healthPeriod: "",
    healthTimeout: "",
    healthFailures: "",
    healthStartupFailures: "",
  };
}

/** Whether this type of workload is addressed by the rest of the unit, and so
 * has a port and may be probed. */
export function addressed(type: string): boolean {
  return type === "service";
}

/** Whether this type runs once rather than continuously: a scheduled job's
 * firing, or a deploy task's one run. Neither has replicas, and neither is
 * kept alive by a probe — how a run went is its exit status. */
export function runsOnce(type: string): boolean {
  return type === "cron" || type === "task";
}

function positiveNumber(value: string): number | undefined {
  const trimmed = value.trim();
  if (trimmed === "") return undefined;
  const parsed = Number(trimmed);
  return Number.isFinite(parsed) ? parsed : undefined;
}

function imageWrite(draft: WorkloadDraft): ImageWrite | undefined {
  const repository = draft.imageRepository.trim();
  if (repository === "") return undefined;
  const image: ImageWrite = { repository };
  if (draft.imageTag.trim()) image.tag = draft.imageTag.trim();
  if (draft.imageDigest.trim()) image.digest = draft.imageDigest.trim();
  if (draft.imageConnection.trim()) image.connection = draft.imageConnection.trim();
  return image;
}

/** What the PATCH carries: every workload, with the fields its type actually
 * has. The fields a type does not have are left out rather than sent empty —
 * a port on a worker and a schedule on a service are both refused, and a form
 * that sent one because the box was still on the screen would be a form whose
 * Save button fails for a field nobody can see. */
export function processWrites(drafts: WorkloadDraft[]): ProcessWrite[] {
  return drafts
    .filter((draft) => draft.name.trim() !== "")
    .map((draft) => {
      const type = draft.type;
      const write: ProcessWrite = { name: draft.name.trim(), type };

      const command = wordsOf(draft.command);
      if (command.length) write.command = command;
      const args = wordsOf(draft.args);
      if (args.length) write.args = args;

      if (addressed(type)) {
        const port = positiveNumber(draft.port);
        if (port !== undefined) write.port = port;
      }
      if (!runsOnce(type)) {
        const replicas = positiveNumber(draft.replicas);
        if (replicas !== undefined) write.replicas = replicas;
        if (draft.singleton) write.singleton = true;
      }
      if (draft.cpu.trim()) write.cpu = draft.cpu.trim();
      if (draft.memory.trim()) write.memory = draft.memory.trim();
      if (type === "cron") {
        if (draft.schedule.trim()) write.schedule = draft.schedule.trim();
        if (draft.concurrencyPolicy.trim()) write.concurrencyPolicy = draft.concurrencyPolicy.trim();
      }
      if (runsOnce(type) && draft.timeout.trim()) write.timeout = draft.timeout.trim();
      if (draft.previews !== "default") write.previews = draft.previews === "yes";

      if (draft.origin === "image") {
        const image = imageWrite(draft);
        if (image) write.image = image;
      } else if (draft.origin === "build" && type !== "cron") {
        write.build = { strategy: draft.buildStrategy || "auto" };
        if (draft.buildRootDirectory.trim()) write.build.rootDirectory = draft.buildRootDirectory.trim();
        if (draft.buildDockerfilePath.trim()) write.build.dockerfilePath = draft.buildDockerfilePath.trim();
        if (draft.buildDockerfileTarget.trim()) write.build.dockerfileTarget = draft.buildDockerfileTarget.trim();
      }

      // A probe belongs to a workload that runs continuously. A scheduled job
      // and a deploy task are refused one, and sending theirs would fail the
      // save for a check the form does not show them.
      if (draft.health && !runsOnce(type)) {
        write.health = {};
        if (draft.healthPath.trim()) write.health.path = draft.healthPath.trim();
        const health = write.health;
        const fields: [(value: number) => void, string][] = [
          [(value) => (health.port = value), draft.healthPort],
          [(value) => (health.periodSeconds = value), draft.healthPeriod],
          [(value) => (health.timeoutSeconds = value), draft.healthTimeout],
          [(value) => (health.failureThreshold = value), draft.healthFailures],
          [(value) => (health.startupFailureThreshold = value), draft.healthStartupFailures],
        ];
        for (const [set, value] of fields) {
          const parsed = positiveNumber(value);
          if (parsed !== undefined) set(parsed);
        }
      }
      return write;
    });
}

/** The sentence a workload's row shows about where its image comes from —
 * short enough for a summary line, and the same three answers `origin` has. */
export function originLabel(draft: WorkloadDraft): string {
  if (draft.origin === "image") {
    const repository = draft.imageRepository.trim();
    if (!repository) return "an image somebody else built";
    if (draft.imageDigest.trim()) return `${repository}@${draft.imageDigest.trim()}`;
    return draft.imageTag.trim() ? `${repository}:${draft.imageTag.trim()}` : repository;
  }
  if (draft.origin === "build") {
    return draft.buildRootDirectory.trim() || "built from this repository";
  }
  return "this project's own image";
}

/** What is wrong with the list, in the words the API would use, so that the
 * Save button can be off rather than the save failing.
 *
 * It is deliberately the four rules a form can check without the platform —
 * a name that is missing or repeated, and the two a type decides. Everything
 * else is the API's to refuse: there is one validator for what a workload is,
 * and a second copy here would be a second thing to be wrong. */
export function workloadProblems(drafts: WorkloadDraft[]): string[] {
  const problems: string[] = [];
  const seen = new Set<string>();
  for (const draft of drafts) {
    const name = draft.name.trim();
    if (name === "") {
      problems.push("A workload needs a name.");
      continue;
    }
    if (seen.has(name)) problems.push(`Two workloads are called ${name}.`);
    seen.add(name);
    if (name === "web") {
      problems.push("The web process is the project's own runtime, not a workload in this list.");
    }
    if (addressed(draft.type) && positiveNumber(draft.port) === undefined) {
      problems.push(`${name} is addressed by the rest of the unit, so it has to say which port it listens on.`);
    }
    if (draft.type === "cron" && draft.schedule.trim() === "") {
      problems.push(`${name} runs on a schedule, so it needs one.`);
    }
    if (draft.origin === "image" && draft.imageRepository.trim() === "") {
      problems.push(`${name} runs an image somebody else built, so it has to say which.`);
    }
    if (draft.origin === "image" && !draft.imageTag.trim() && !draft.imageDigest.trim()) {
      problems.push(`${name} needs a tag or a digest: a vendored image is pinned to a version.`);
    }
  }
  return problems;
}
