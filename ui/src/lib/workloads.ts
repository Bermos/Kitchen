import type { ImageWrite, Process, ProcessWrite, Security, SecuritySettings, VolumeInit } from "./api";

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
  /** This workload's own security posture, merged over the project's field by
   * field: a field set here wins, a field left blank inherits the project's.
   * Off means it declares none at all and runs under the project's whole.
   *
   * Everything here is zero-means-inherit, which is the reading the posture
   * already had one level up — so a workload adds to the project's posture or
   * redirects it, and takes a constraint off by the project not declaring it
   * on everything. */
  security: boolean;
  runAsNonRoot: boolean;
  runAsUser: string;
  runAsGroup: string;
  fsGroup: string;
  fsGroupChangePolicy: string;
  readOnlyRootFilesystem: boolean;
  allowPrivilegeEscalation: boolean;
  /** One capability per line, the shape every word list here takes. */
  dropCapabilities: string;
  /** What this workload prepares inside the volumes it mounts before it
   * starts. Empty for the great majority of workloads, which mount none. */
  init: VolumeInitDraft[];
}

/** One volume's preparation as the form holds it (#348).
 *
 * A volume claim hands a workload an empty filesystem, and software the
 * platform did not build often will not start on one. What a project declares
 * is two kinds of typed step — a directory that has to exist, and a
 * configuration file copied in once — which the platform runs itself before
 * the workload's own container starts. There is no command here, and there is
 * deliberately no field that could become one.
 *
 * The two step lists are held as text, one step per line, for the reason the
 * command and the arguments are: a list is a list of lines, and a box per step
 * would be a form nobody can paste into. */
export interface VolumeInitDraft {
  /** Stable across renders and never sent: `volume` is editable, so it cannot
   * also be the key the list is rendered by. */
  key: string;
  volume: string;
  /** One per line: a path inside the volume, and optionally an octal mode —
   * `custom_components 0750`. */
  directories: string;
  /** One per line: the name of one of the project's files, where it goes
   * inside the volume, and optionally an octal mode —
   * `configuration configuration.yaml 0640`. */
  seed: string;
}

/** The eight fields of a posture, spelled the way the API, kitchen.json and
 * the operator all spell them — which is also the way `overrides` names them.
 * A panel that marks the overridden fields of an effective posture types its
 * own names against this, so a name that drifts from the API's fails the
 * build rather than quietly marking nothing. */
export const SECURITY_FIELDS = [
  "runAsNonRoot",
  "runAsUser",
  "runAsGroup",
  "fsGroup",
  "fsGroupChangePolicy",
  "readOnlyRootFilesystem",
  "allowPrivilegeEscalation",
  "dropCapabilities",
] as const;

export type SecurityField = (typeof SECURITY_FIELDS)[number];

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
    ...securityDraft(workload.security),
    init: volumeInitDrafts(workload.init),
  }));
}

/** A declaration read back as drafts. */
export function volumeInitDrafts(inits: VolumeInit[] | undefined): VolumeInitDraft[] {
  return (inits ?? []).map((init) => ({
    key: `read-init-${nextKey++}`,
    volume: init.volume,
    directories: (init.directories ?? [])
      .map((dir) => [dir.path, dir.mode].filter(Boolean).join(" "))
      .join("\n"),
    seed: (init.seed ?? []).map((seed) => [seed.file, seed.path, seed.mode].filter(Boolean).join(" ")).join("\n"),
  }));
}

/** A volume the "Prepare a volume" button just made. */
export function newVolumeInitDraft(): VolumeInitDraft {
  return { key: `new-init-${nextKey++}`, volume: "", directories: "", seed: "" };
}

/** The steps as the route takes them. A line's words are its fields, in order,
 * and a line with too few is left out rather than sent half-formed — the form
 * says what is wrong beside the box, and the API is the one validator. */
export function volumeInitWrites(drafts: VolumeInitDraft[]): VolumeInit[] {
  return drafts
    .filter((draft) => draft.volume.trim() !== "")
    .map((draft) => {
      const init: VolumeInit = { volume: draft.volume.trim() };
      const directories = wordsOf(draft.directories)
        .map((line) => line.split(/\s+/))
        .filter((words) => words[0])
        .map((words) => (words[1] ? { path: words[0], mode: words[1] } : { path: words[0] }));
      if (directories.length) init.directories = directories;
      const seed = wordsOf(draft.seed)
        .map((line) => line.split(/\s+/))
        .filter((words) => words.length >= 2)
        .map((words) => (words[2] ? { file: words[0], path: words[1], mode: words[2] } : { file: words[0], path: words[1] }));
      if (seed.length) init.seed = seed;
      return init;
    })
    .filter((init) => (init.directories?.length ?? 0) + (init.seed?.length ?? 0) > 0);
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
    ...securityDraft(undefined),
    init: [],
  };
}

/** The posture half of a draft, read off what the workload declared. Absent is
 * a workload that declared none: it inherits the project's whole, which is the
 * ordinary case and the one the form starts a new workload in. */
function securityDraft(security: Security | undefined): Pick<
  WorkloadDraft,
  | "security"
  | "runAsNonRoot"
  | "runAsUser"
  | "runAsGroup"
  | "fsGroup"
  | "fsGroupChangePolicy"
  | "readOnlyRootFilesystem"
  | "allowPrivilegeEscalation"
  | "dropCapabilities"
> {
  return {
    security: Boolean(security),
    runAsNonRoot: security?.runAsNonRoot ?? false,
    runAsUser: numberField(security?.runAsUser),
    runAsGroup: numberField(security?.runAsGroup),
    fsGroup: numberField(security?.fsGroup),
    fsGroupChangePolicy: security?.fsGroupChangePolicy ?? "",
    readOnlyRootFilesystem: security?.readOnlyRootFilesystem ?? false,
    allowPrivilegeEscalation: security?.allowPrivilegeEscalation ?? false,
    dropCapabilities: (security?.dropCapabilities ?? []).join("\n"),
  };
}

/** What a workload's own posture sends, or nothing at all where it declares
 * none. A block of nothing but defaults is no override — the API stores it as
 * none — so it is left out rather than sent, which keeps a workload that
 * declared nothing reading as a workload that declared nothing. */
function securityWrite(draft: WorkloadDraft): SecuritySettings | undefined {
  if (!draft.security) return undefined;
  const fsGroup = positiveNumber(draft.fsGroup);
  const write: SecuritySettings = {
    runAsNonRoot: draft.runAsNonRoot,
    readOnlyRootFilesystem: draft.readOnlyRootFilesystem,
    allowPrivilegeEscalation: draft.allowPrivilegeEscalation,
    dropCapabilities: wordsOf(draft.dropCapabilities),
  };
  const ids: [(value: number) => void, string][] = [
    [(value) => (write.runAsUser = value), draft.runAsUser],
    [(value) => (write.runAsGroup = value), draft.runAsGroup],
    [(value) => (write.fsGroup = value), draft.fsGroup],
  ];
  for (const [set, value] of ids) {
    const parsed = positiveNumber(value);
    if (parsed !== undefined) set(parsed);
  }
  // The policy applies only where there is a group to apply it to, and the
  // API refuses one without it — so an emptied group takes the policy with it
  // rather than making the whole save fail.
  if (fsGroup && draft.fsGroupChangePolicy) write.fsGroupChangePolicy = draft.fsGroupChangePolicy;
  return write;
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
      const init = volumeInitWrites(draft.init);
      if (init.length) write.init = init;

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
      const security = securityWrite(draft);
      if (security) write.security = security;
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
    problems.push(...volumeInitProblems(draft.init, name));
  }
  return problems;
}

/** What is wrong with a workload's volume preparation, in the words the API
 * would use. Like the rest of this file it checks only what a form can check
 * without the platform: whether a volume was named twice, whether an entry
 * does anything, and whether a seed line says both what and where. */
export function volumeInitProblems(drafts: VolumeInitDraft[], workload: string): string[] {
  const problems: string[] = [];
  const seen = new Set<string>();
  for (const draft of drafts) {
    const volume = draft.volume.trim();
    if (volume === "") {
      problems.push(`${workload} prepares a volume without saying which.`);
      continue;
    }
    if (seen.has(volume)) {
      problems.push(`${workload} prepares ${volume} twice: one volume is one entry, with all of its steps in it.`);
    }
    seen.add(volume);
    for (const line of wordsOf(draft.seed)) {
      if (line.split(/\s+/).length < 2) {
        problems.push(`${workload}, preparing ${volume}: a seeded file says which file and where it goes — "configuration configuration.yaml".`);
      }
    }
    const steps = wordsOf(draft.directories).length + wordsOf(draft.seed).length;
    if (steps === 0) {
      problems.push(`${workload} prepares ${volume} and says nothing to do to it.`);
    }
  }
  return problems;
}
