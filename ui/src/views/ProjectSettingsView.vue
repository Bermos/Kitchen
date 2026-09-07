<script setup lang="ts">
import { computed, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  api,
  CRITICALITIES,
  DATA_CLASSES,
  type Claim,
  type Exposure,
  type ForkPolicy,
  type Project,
} from "../lib/api";
import {
  claimBackupBadge,
  claimCautions,
  claimDeletionOutcome,
  claimDeletionWarning,
  claimRecoveryPoint,
  claimRefusal,
  claimRequirements,
  deletionGatedByName,
  destroysData,
  destroysDataRefusal,
  mayDestroyData,
} from "../lib/claims";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { environmentLink } from "../lib/links";
import {
  EXPOSURE_OPTIONS,
  exposureNote,
  SETTINGS_SECTIONS,
  settingsSection,
  type SettingsSection,
} from "../lib/project";
import { useAsync } from "../lib/useAsync";
import { volumeInitDrafts, volumeInitProblems, volumeInitWrites, type VolumeInitDraft } from "../lib/workloads";
import ClaimModal from "../components/ClaimModal.vue";
import ClaimRecoveryModal from "../components/ClaimRecoveryModal.vue";
import EnvVarsPanel from "../components/EnvVarsPanel.vue";
import KeysPanel from "../components/KeysPanel.vue";
import MembersPanel from "../components/MembersPanel.vue";
import NotificationsPanel from "../components/NotificationsPanel.vue";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import OfferingsPanel from "../components/OfferingsPanel.vue";
import ProjectFilesPanel from "../components/ProjectFilesPanel.vue";
import ProjectSecretsPanel from "../components/ProjectSecretsPanel.vue";
import ProjectWorkloadsPanel from "../components/ProjectWorkloadsPanel.vue";
import VolumeInitEditor from "../components/VolumeInitEditor.vue";

// Everything about this project somebody may change after creating it, as
// forms at one width behind a left rail.
//
// **This is the screen #470 is really about.** Eleven cards of form, four
// panels and a danger zone used to be a tab of a page that also held the
// releases, the builds, the previews, the claims and five metrics pollers — so
// changing a member's role loaded the request rollups, and the design guide's
// one form width was decided per panel rather than per page. Twelve panels
// behind a rail is a normal shape; twelve panels on one scroll is the shape
// this issue objects to, and a single-scroll Settings would have re-created the
// problem one level down (#470, decision 2).
//
// So: one pane at a time, each `max-w-3xl`, each with its own address
// (`?section=`), and **nothing on this screen polls**. It is read once and
// re-read after a write, because a form that reloads under the cursor is worse
// than one that is a few seconds stale.
//
// The pane list is `SETTINGS_SECTIONS` in `lib/project.ts` rather than markup,
// because `routes.ts` maps the old page's section names onto the same ids and
// two spellings of one vocabulary is how a redirect quietly stops landing.

const route = useRoute();
const router = useRouter();
const toast = useToast();
const name = computed(() => route.params.name as string);

// Read once. There is no poll on this screen and that is the point: the only
// thing that moves the answer is a save, and a save re-reads.
const { data, error, loading, refresh } = useAsync(async () => {
  const [project, environments, builds, claims, allDomains] = await Promise.all([
    api.project(name.value),
    api.projectEnvironments(name.value),
    api.projectBuilds(name.value),
    // Every claim this account can see, not only this project's: the
    // offerings pane names the projects binding to what this one offers, and
    // those claims belong to them.
    api.claims(),
    api.domains(),
  ]);
  // Domains attach to environments; the project's are the ones pointing at one
  // of its environments.
  const mine = new Set(environments.map((environment) => environment.name));
  return {
    project,
    builds,
    claims: claims.filter((claim) => claim.project === project.name),
    bindings: claims.filter((claim) => claim.type === "service"),
    domains: allDomains.filter((domain) => mine.has(domain.environment)),
  };
});
watch(name, () => void refresh());

const project = computed(() => data.value?.project);
const claims = computed(() => data.value?.claims ?? []);
// Every service claim in view, which is how the offerings pane says who binds
// to each of this project's offerings.
const bindings = computed(() => data.value?.bindings ?? []);

// What this account may do here, keyed to the role that arrived on this
// project's own payload. A viewer gets the same screen with the write controls
// gone — not disabled with a tooltip. A disabled button is an invitation to go
// and find somebody who can press it, which is right for a shared enterprise
// tool and wrong here.
const caller = computed(() => callerFor(project.value?.role, name.value));
const mayConfigure = computed(() => may("PATCH /api/v1/projects/{name}", caller.value));
const mayDelete = computed(() => may("DELETE /api/v1/projects/{name}", caller.value));
const mayReadMembers = computed(() => may("GET /api/v1/projects/{name}/members", caller.value));
// **The variables pane is keyed to reading the project, not to writing the
// variables.** `GET /projects/{name}` is a viewer's and already carries the
// list, so hiding the pane from a viewer would be the dashboard enforcing
// something the API does not.
const mayReadProject = computed(() => may("GET /api/v1/projects/{name}", caller.value));
const mayClaim = computed(() => may("POST /api/v1/claims", caller.value));
const mayUnclaim = computed(() => may("DELETE /api/v1/claims/{name}", caller.value));
const mayUnclaimData = computed(() => mayDestroyData(caller.value));
// Reading what a claim can be recovered to is the viewer's; the control is
// offered on the kinds of claim that hold data somebody would want back.
const mayReadRecovery = computed(() => may("GET /api/v1/claims/{name}/recoveries", caller.value));
function offersRecovery(claim: Claim): boolean {
  return claim.type === "postgres";
}

// ── The rail ────────────────────────────────────────────────────────────────

/** Which panes exist for this account. A pane nobody may open is not offered,
 * which is the same rule every affordance here follows. */
const sections = computed<SettingsSection[]>(() =>
  SETTINGS_SECTIONS.filter((section) => {
    switch (section.id) {
      case "source":
      case "runtime":
      case "security":
      case "continuity":
        return mayConfigure.value;
      case "danger":
        return mayDelete.value;
      case "variables":
        return mayReadProject.value;
      case "members":
      case "keys":
        return mayReadMembers.value;
      default:
        return true;
    }
  }),
);

/** The pane in the address, or the first one this account has. A bookmarked
 * pane a role no longer opens falls back rather than rendering nothing. */
const current = computed<SettingsSection>(() => {
  const asked = settingsSection(route.query.section);
  return sections.value.some((section) => section.id === asked.id) ? asked : (sections.value[0] ?? asked);
});
function open(section: SettingsSection) {
  void router.replace({ query: { ...route.query, section: section.id } });
}

// ── What the repository has taken over ──────────────────────────────────────

// Read off the most recent build that found a kitchen.json. The file wins for
// the settings it names, so a form that did not say so would be a form whose
// Save button silently does nothing for one field in five (#310).
//
// The notice moves with the pane it is about rather than sitting once at the
// top of everything: a pane is now a screen's worth of form, and a warning two
// panes away is a warning nobody reads.
const config = computed(() => data.value?.builds.find((build) => build.config)?.config);
const declares = computed(() => config.value?.declares ?? []);
function declaredInRepo(field: string): boolean {
  return declares.value.includes(field);
}
/** The settings this file declares that belong to one pane, so the notice says
 * what is about to be overwritten here and not everywhere. */
const SECTION_FIELDS: Record<string, (field: string) => boolean> = {
  source: (field) => field.startsWith("build."),
  runtime: (field) => field.startsWith("runtime.") && field !== "runtime.security",
  security: (field) => field === "runtime.security",
  variables: (field) => field.startsWith("env."),
  files: (field) => field.startsWith("files."),
  resources: (field) => field.startsWith("volumes."),
  offerings: (field) => field.startsWith("offers."),
  processes: (field) => field === "processes",
};
const declaredHere = computed(() => {
  const belongs = SECTION_FIELDS[current.value.id];
  return belongs ? declares.value.filter(belongs) : [];
});
// Configuration files are named one by one — "files.configuration" — because
// they merge onto the project's by name rather than replacing the list.
const repoFiles = computed(() =>
  declares.value.filter((field) => field.startsWith("files.")).map((field) => field.slice("files.".length)),
);
// And so are offerings, for the same reason: the file declares the shape of
// one and the project keeps the grant, so the row has to say which of the two
// the next build will set back.
const repoOffers = computed(() =>
  declares.value.filter((field) => field.startsWith("offers.")).map((field) => field.slice("offers.".length)),
);

// ── The form ────────────────────────────────────────────────────────────────

// The panes edit a copy of the project, loaded once per project. Environment
// variables are not in this copy and must not be: `PATCH /projects/{name}`
// refuses a body that carries `env`, naming the route that takes them instead.
const settings = reactive({
  loadedFor: "",
  productionBranch: "",
  requirePullRequest: false,
  previews: true,
  previewsProtected: true,
  // The project's own ceiling on live previews. -1 is "inherit the platform's",
  // which is what the API takes a negative number to mean, and 0 is a ceiling
  // this project has cleared — the two cannot be the same value.
  previewsMax: -1,
  // What a pull request from a fork gets: none, build or full. `none` is the
  // default and the safe one — a fork's head is a stranger's code, and `full`
  // hands it this project's own secrets.
  previewsForks: "none" as ForkPolicy,
  buildStrategy: "auto",
  dockerfilePath: "",
  dockerfileTarget: "",
  rootDirectory: "",
  // Off by default, and deliberately so: a repository whose services share
  // code outside their root directories wants every push built, and the
  // platform cannot tell that kind from a clean monorepo (#500).
  skipUnchanged: false,
  // 0 is "let the platform decide": the port then comes from the framework each
  // build detects, and the field shows what that would be.
  port: 0,
  replicas: 1,
  cpu: "",
  memory: "",
  // The health check. An empty path is a TCP connect to the container port,
  // which is what every environment gets when nobody has said otherwise; the
  // four numbers are 0 for "take the platform's default".
  healthPath: "",
  healthPort: 0,
  healthPeriod: 0,
  healthTimeout: 0,
  healthFailures: 0,
  healthStartupFailures: 0,
  // The command and the arguments, one word per line. Exec form is a list of
  // words and never a shell line, so the field is a list of lines rather than
  // one box to be split on spaces: an argument with a space in it is ordinary,
  // and splitting would quietly break it.
  command: "",
  args: "",
  // Empty is no override, the same reading an empty preview value gets, so an
  // empty box is how one is taken away and no switch is needed to say so.
  previewArgs: "",
  // The security posture every workload of the project runs under. Every field
  // is off or 0 for "the platform's default", so an untouched form sends the
  // posture the project already has.
  runAsNonRoot: false,
  runAsUser: 0,
  runAsGroup: 0,
  // The gid that owns the volumes the workloads mount, and when the kubelet
  // applies it. Empty policy is the default, and it is sent only alongside a
  // group id because that is the only time it applies.
  fsGroup: 0,
  fsGroupChangePolicy: "",
  readOnlyRootFilesystem: false,
  allowPrivilegeEscalation: false,
  // One capability per line, the same shape the argument fields take.
  dropCapabilities: "",
  // What the web process needs done inside the volumes it mounts before it
  // starts. Empty for every project that mounts none, which is most of them.
  init: [] as VolumeInitDraft[],
  // Two of this workload must never run at once. It is next to the replica
  // count because it is the same decision from the other side.
  singleton: false,
  // Work nobody asked for. Idling is request-driven by construction, so an
  // application with a background loop is the one it silently breaks.
  notRequestDriven: false,
  // Whether this project is on the internet at all. `public` is what every
  // project was before the setting existed, and `internal` publishes none of
  // its environments — the difference between a service other applications
  // call and one anybody can.
  exposure: "public" as Exposure,
  // "" is unclassified — a state shown as such, never a default.
  dataClass: "",
  // "" is undesignated, for the same reason: Kitchen does not decide what is
  // critical, so it must not appear to have an opinion by defaulting one.
  criticality: "",
  rto: "",
  rpo: "",
});

const vendoredImage = computed(() => project.value?.image);
const builtHere = computed(() => Boolean(project.value?.repo));
// The port field's help sentence: an empty port comes from whatever the build
// detected, so the field says which framework that was.
const framework = computed(() => data.value?.builds.find((build) => build.detectedFramework)?.detectedFramework);

function loadSettings(from: Project) {
  settings.loadedFor = from.name;
  settings.productionBranch = from.productionBranch;
  settings.requirePullRequest = from.requirePullRequest;
  settings.previews = from.previews;
  settings.previewsProtected = from.previewsProtected;
  settings.previewsMax = from.previewsMax ?? -1;
  settings.previewsForks = from.previewsForks ?? "none";
  settings.buildStrategy = from.buildStrategy || "auto";
  settings.dockerfilePath = from.dockerfilePath ?? "";
  settings.dockerfileTarget = from.dockerfileTarget ?? "";
  settings.rootDirectory = from.rootDirectory ?? "";
  settings.skipUnchanged = from.skipUnchanged ?? false;
  settings.port = from.port ?? 0;
  settings.replicas = from.replicas ?? 1;
  settings.cpu = from.cpu ?? "";
  settings.memory = from.memory ?? "";
  settings.healthPath = from.health?.path ?? "";
  settings.healthPort = from.health?.port ?? 0;
  settings.healthPeriod = from.health?.periodSeconds ?? 0;
  settings.healthTimeout = from.health?.timeoutSeconds ?? 0;
  settings.healthFailures = from.health?.failureThreshold ?? 0;
  settings.healthStartupFailures = from.health?.startupFailureThreshold ?? 0;
  settings.command = wordLines(from.command);
  settings.args = wordLines(from.args);
  settings.previewArgs = wordLines(from.previewArgs);
  settings.runAsNonRoot = from.security?.runAsNonRoot ?? false;
  settings.runAsUser = from.security?.runAsUser ?? 0;
  settings.runAsGroup = from.security?.runAsGroup ?? 0;
  settings.fsGroup = from.security?.fsGroup ?? 0;
  settings.fsGroupChangePolicy = from.security?.fsGroupChangePolicy ?? "";
  settings.readOnlyRootFilesystem = from.security?.readOnlyRootFilesystem ?? false;
  settings.allowPrivilegeEscalation = from.security?.allowPrivilegeEscalation ?? false;
  settings.dropCapabilities = wordLines(from.security?.dropCapabilities);
  settings.init = volumeInitDrafts(from.init);
  settings.singleton = from.singleton ?? false;
  settings.notRequestDriven = from.notRequestDriven ?? false;
  settings.exposure = from.exposure ?? "public";
  settings.dataClass = from.dataClass ?? "";
  settings.criticality = from.criticality ?? "";
  settings.rto = from.rto ?? "";
  settings.rpo = from.rpo ?? "";
}
watch(project, (value) => {
  if (value && value.name !== settings.loadedFor) loadSettings(value);
});

// A word list as the form holds it and as the API takes it: one word per line,
// blank lines dropped. Nothing is split on spaces, because a single argument
// containing one is ordinary and exec form has no quoting to lean on.
function wordLines(words: string[] | undefined): string {
  return (words ?? []).join("\n");
}
function wordsOf(lines: string): string[] {
  return lines
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
}

// An empty port field is not an unconfigured one: it is the framework's,
// decided per build, so the field shows nothing and says where the number
// comes from instead.
const portField = computed({
  get: () => (settings.port ? String(settings.port) : ""),
  set: (value: string) => {
    settings.port = Number(value) || 0;
  },
});
const portHelp = computed(() =>
  settings.port
    ? "The application listens here, and PORT is set to it."
    : framework.value
      ? `From the detected framework (${framework.value}).`
      : "From the detected framework.",
);

// Turning the singleton switch on takes the replica count with it. The API
// refuses the pair rather than clamping it — a value quietly lowered reads back
// as a setting that did not take — so the form must not be able to send one it
// knows will be refused.
watch(
  () => settings.singleton,
  (on) => {
    if (on) settings.replicas = 1;
  },
);
const replicasHelp = computed(() =>
  settings.singleton ? "Fixed at 1 while this workload must never run twice." : "Previews always run 1.",
);

const strategyOptions = [
  { label: "auto — detect the framework", value: "auto" },
  { label: "dockerfile", value: "dockerfile" },
  { label: "buildpacks", value: "buildpacks" },
];
const dataClassOptions = [
  { label: "unclassified", value: "" },
  ...DATA_CLASSES.map((value) => ({ label: value, value: value as string })),
];
// When the ownership of a volume is applied. Empty is the default, which is
// what a project that has not thought about it should get.
const fsGroupChangePolicyOptions = [
  { label: "Always (every start)", value: "" },
  { label: "OnRootMismatch", value: "OnRootMismatch" },
];
// The vocabulary and the sentence under it live in lib/project.ts, where
// project.test.ts can hold them: the difference between the two words is the
// difference between a service other applications call and one anybody can.
const exposureLine = computed(() => exposureNote(settings.exposure));

const criticalityOptions = [
  { label: "undesignated", value: "" },
  ...CRITICALITIES.map((value) => ({ label: value, value: value as string })),
];

/** Whether this project sets its own preview ceiling, as a switch over the one
 *  field that carries both answers: a negative number is "take the platform's",
 *  0 is "no ceiling for this project", and the two cannot share a value. */
const previewsMaxOwn = computed({
  get: () => settings.previewsMax >= 0,
  set: (own: boolean) => {
    settings.previewsMax = own ? (project.value?.previewCapacity?.max ?? 5) : -1;
  },
});

/** The ceiling in a sentence: what is live against it, and what is waiting on
 *  it. */
const previewCeilingLine = computed(() => {
  const capacity = project.value?.previewCapacity;
  if (!capacity || capacity.max <= 0) {
    return "No ceiling: every open pull request gets a preview, and each one costs a copy of every backing service this project claims.";
  }
  const waiting = capacity.refused?.length ?? 0;
  const head = `${capacity.live} of ${capacity.max} previews live.`;
  if (!waiting) return `${head} A pull request past the ceiling is told so on the request rather than started.`;
  const numbers = (capacity.refused ?? []).map((r) => `#${r.pullRequest}`).join(", ");
  return `${head} Waiting on a slot: ${numbers}. Nothing is queued — each gets its preview on its next push once one is free.`;
});

/** What a fork's pull request may be given, worst to best for the reader: the
 *  label says what the project *does*, not what the value is called, because
 *  "full" on its own does not tell an admin that they are about to hand a
 *  stranger's branch this project's secrets. */
const forkOptions = [
  { label: "none — a fork gets nothing built and nothing published", value: "none" },
  { label: "build — build a fork's commit, publish no environment", value: "build" },
  { label: "full — treat a fork as this project's own branch", value: "full" },
];
const forkLine = computed(() => {
  switch (settings.previewsForks) {
    case "full":
      return "A pull request from a fork is built and deployed exactly like this project's own branches, with this project's environment variables, secrets and claim bindings. Anybody who can open a pull request against this repository can run code with them.";
    case "build":
      return "A pull request from a fork is built, and nothing is published: no preview environment, so none of this project's variables, secrets or claim bindings reach it.";
    default:
      return "A pull request from a fork is not built at all. The request is told so: a kitchen/<project>/preview check and a comment on it.";
  }
});

/** The claims whose provider holds the workload up — a connect worker's
 * outbound connection, say. Scale to zero is a project-level policy, so one
 * such claim keeps every environment of this project on its pods. */
const pinningClaims = computed(() => claims.value.filter((claim) => claim.keepsPodsRunning));

// What is wrong with the web process's volume preparation, in the words the API
// would use — shown beside the form rather than arriving as a failed save.
const volumePreparationProblems = computed(() => volumeInitProblems(settings.init, "The web process"));

const savingSettings = ref(false);
async function saveSettings() {
  savingSettings.value = true;
  try {
    const saved = await api.updateProject(name.value, {
      // A repository's settings, sent only by a project that has one. The API
      // refuses a production branch or a pull request requirement on a project
      // whose source is an image, and rightly — but a save of the data
      // classification should not fail with a sentence about branches (#307).
      ...(builtHere.value
        ? {
            productionBranch: settings.productionBranch,
            requirePullRequest: settings.requirePullRequest,
            previews: settings.previews,
            previewsProtected: settings.previewsProtected,
            previewsMax: settings.previewsMax,
            previewsForks: settings.previewsForks,
            buildStrategy: settings.buildStrategy,
            dockerfilePath: settings.dockerfilePath,
            dockerfileTarget: settings.dockerfileTarget,
            rootDirectory: settings.rootDirectory,
            skipUnchanged: settings.skipUnchanged,
          }
        : {}),
      port: settings.port,
      replicas: settings.replicas,
      cpu: settings.cpu,
      memory: settings.memory,
      health: {
        path: settings.healthPath,
        // 0 is not "no port": it is the check being made against whatever port
        // the application is published on, which keeps the probe with the port
        // when the port moves.
        port: settings.healthPort,
        periodSeconds: settings.healthPeriod,
        timeoutSeconds: settings.healthTimeout,
        failureThreshold: settings.healthFailures,
        startupFailureThreshold: settings.healthStartupFailures,
      },
      command: wordsOf(settings.command),
      args: wordsOf(settings.args),
      // An emptied box sends an empty list rather than nothing: absent would
      // keep the override already there, and clearing the box has to be able to
      // take it away.
      previewArgs: wordsOf(settings.previewArgs),
      // The whole posture every time, so a switch turned off is a constraint
      // taken away: the route replaces it, and a posture of nothing but
      // defaults is stored as no posture at all.
      security: {
        runAsNonRoot: settings.runAsNonRoot,
        runAsUser: settings.runAsUser,
        runAsGroup: settings.runAsGroup,
        fsGroup: settings.fsGroup,
        // The policy applies only where there is a group to apply, and the API
        // refuses one without it — so an emptied group id takes the policy with
        // it rather than making the whole save fail.
        fsGroupChangePolicy: settings.fsGroup > 0 ? settings.fsGroupChangePolicy : "",
        readOnlyRootFilesystem: settings.readOnlyRootFilesystem,
        allowPrivilegeEscalation: settings.allowPrivilegeEscalation,
        dropCapabilities: wordsOf(settings.dropCapabilities),
      },
      // The whole declaration every time, so a volume taken off the list is one
      // the platform stops preparing.
      init: volumeInitWrites(settings.init),
      singleton: settings.singleton,
      notRequestDriven: settings.notRequestDriven,
      exposure: settings.exposure,
      dataClass: settings.dataClass,
      criticality: settings.criticality,
      rto: settings.rto,
      rpo: settings.rpo,
    });
    loadSettings(saved);
    toast.add({
      title: "Settings saved",
      description: "New builds and deployments pick them up; what is already running keeps its release's snapshot.",
      color: "success",
      icon: "i-lucide-check",
    });
    await refresh();
  } catch (err) {
    toast.add({
      title: "Saving the settings failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    savingSettings.value = false;
  }
}

// ── Claims ──────────────────────────────────────────────────────────────────

const refusedClaims = computed(() => claims.value.filter((claim) => claim.phase === "Failed"));
// The cautions a bound claim carries — lib/claims.ts says what counts as one
// and why. They are drawn next to the rows they are about, on the same footing
// as a refusal, and a claim that failed is left to the refusal above rather
// than said twice.
const cautions = computed(() => claimCautions(claims.value.filter((claim) => claim.phase !== "Failed")));

// Destroying the data is the admin's, not the developer's (#320): the API
// refuses a Delete-policy claim's deletion below admin, so the row does not
// offer a button the refusal is waiting behind — it says why instead, in the
// same words.
function claimDeleteRefusal(claim: Claim): string | undefined {
  if (!destroysData(claim)) return undefined;
  return destroysDataRefusal(caller.value, "deleting a claim that destroys what it provisioned");
}

const claimToDelete = ref<Claim | null>(null);
// A claim whose policy is Delete takes its data with it, so the confirmation is
// the one project deletion uses: typing the name. A click can be a slip, the
// name cannot.
const claimConfirmation = ref("");
const claimDeleteGated = computed(() => deletionGatedByName(claimToDelete.value));
const claimDeleteReady = computed(
  () => !claimDeleteGated.value || claimConfirmation.value === claimToDelete.value?.name,
);
watch(claimToDelete, () => {
  claimConfirmation.value = "";
});
const deletingClaim = ref(false);
async function deleteClaim() {
  const claim = claimToDelete.value;
  if (!claim || deletingClaim.value || !claimDeleteReady.value) return;
  deletingClaim.value = true;
  try {
    await api.deleteClaim(claim.name);
    toast.add({
      title: `Claim ${claim.name} is being deleted`,
      description: claimDeletionOutcome(claim),
      color: "success",
      icon: "i-lucide-trash-2",
    });
    claimToDelete.value = null;
    await refresh();
  } catch (err) {
    toast.add({
      title: "Deleting the claim failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    deletingClaim.value = false;
  }
}

// ── The danger zone ─────────────────────────────────────────────────────────

// Deleting a project takes everything with it, so the confirmation is typing
// the name — a click can be a slip, the name cannot.
const deleteConfirmation = ref("");
const deleting = ref(false);
async function deleteProject() {
  if (deleteConfirmation.value !== name.value || deleting.value) return;
  deleting.value = true;
  try {
    await api.deleteProject(name.value);
    toast.add({
      title: `Project ${name.value} is being deleted`,
      description: "Environments, builds, releases and everything they were running are being torn down.",
      color: "success",
      icon: "i-lucide-trash-2",
    });
    void router.push({ name: "overview" });
  } catch (err) {
    toast.add({
      title: "Deleting the project failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
    deleting.value = false;
  }
}
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="project">
      <PageHeader
        title="Settings"
        :breadcrumb="[{ label: 'Projects', to: '/projects' }, { label: project.name, mono: true }, { label: 'Settings' }]"
      >
        <template #description>
          Everything about this project somebody may change after creating it. Nothing here is measured, so nothing here
          moves while you are reading it.
        </template>
      </PageHeader>

      <div class="flex flex-col lg:flex-row gap-6 items-start">
        <!-- The rail. Each pane is an address, so "the fork policy is here" is
             a link somebody can send. -->
        <nav class="w-full lg:w-52 shrink-0 flex lg:flex-col gap-0.5 overflow-x-auto" aria-label="Settings">
          <button
            v-for="section in sections"
            :key="section.id"
            class="text-left shrink-0 px-2.5 py-1.5 rounded-md text-sm hover:bg-elevated hover:text-highlighted"
            :class="current.id === section.id ? 'bg-elevated text-highlighted' : 'text-toned'"
            :aria-current="current.id === section.id ? 'page' : undefined"
            @click="open(section)"
          >
            {{ section.label }}
          </button>
        </nav>

        <!-- One pane, one form width. This is the `max-w-3xl` docs/UI.md means
             by "this page is a form", declared once here rather than per
             panel. -->
        <div class="min-w-0 flex-1 max-w-3xl space-y-6">
          <!-- Which of this pane's fields the repository has taken over.
               Beside the pane rather than once at the top of everything: the
               file wins for the settings it names, so a Save that silently does
               nothing has to be said where the Save is (#310). -->
          <div v-if="declaredHere.length && config" class="rounded-md border border-info/40 bg-info/5 px-5 py-4">
            <div class="flex items-start gap-2">
              <UIcon name="i-lucide-file-code" class="size-4 text-info mt-0.5 shrink-0" />
              <div class="min-w-0 space-y-1">
                <p class="text-sm font-medium text-highlighted">
                  <span class="font-mono">{{ config.path }}</span> decides some of this
                </p>
                <p class="text-xs text-toned">
                  The repository declares
                  {{ declaredHere.length }} {{ declaredHere.length === 1 ? "setting" : "settings" }} on this pane, read
                  at every commit. Changing one here holds until the next build, which sets it back to what the file
                  says — change the file instead.
                </p>
                <p class="text-xs text-muted font-mono break-words">{{ declaredHere.join(", ") }}</p>
              </div>
            </div>
          </div>

          <!-- ── Source ─────────────────────────────────────────────────── -->
          <PageSection v-if="current.id === 'source'" :title="current.label" :description="current.description">
            <form class="space-y-6" @submit.prevent="saveSettings">
              <!-- What this project runs, when nothing here built it. It is
                   read rather than edited: changing which image a project runs
                   is a different question from changing a setting of it. -->
              <div v-if="vendoredImage" class="rounded-md border border-default bg-muted p-5 space-y-4">
                <h3 class="text-xs font-medium text-highlighted">Image</h3>
                <p class="text-xs text-toned">
                  This project's web process runs an image this platform did not build. There is no repository, so there
                  is nothing to build, nowhere to push and no pull request to preview — the processes below may still
                  declare images of their own, and they deploy and roll back with this one as one unit.
                </p>
                <p class="text-xs text-toned">
                  <template v-if="vendoredImage.digest">
                    This image is pinned to a digest, so nothing moves it: the platform never asks the registry about
                    it, and a new version arrives when somebody changes the digest here.
                  </template>
                  <template v-else>
                    The platform asks the registry on an interval whether this tag still names the digest it acquired. A
                    new one is a build with no builder — it resolves the digest, releases it and deploys it — and
                    rolling it back is the ordinary rollback.
                  </template>
                </p>
                <dl class="grid grid-cols-[10rem_1fr] gap-x-4 gap-y-1 text-xs">
                  <dt class="text-dimmed">Repository</dt>
                  <dd class="font-mono text-toned break-all">{{ vendoredImage.repository }}</dd>
                  <template v-if="vendoredImage.tag">
                    <dt class="text-dimmed">Tag</dt>
                    <dd class="font-mono text-toned break-all">{{ vendoredImage.tag }}</dd>
                  </template>
                  <template v-if="vendoredImage.digest">
                    <dt class="text-dimmed">Digest</dt>
                    <dd class="font-mono text-toned break-all">{{ vendoredImage.digest }}</dd>
                  </template>
                  <dt class="text-dimmed">Pulled with</dt>
                  <dd class="text-toned break-all">
                    {{
                      vendoredImage.connection ? `the ${vendoredImage.connection} connection` : "nothing: a public image"
                    }}
                  </dd>
                </dl>
              </div>

              <div v-if="builtHere" class="rounded-md border border-default bg-muted p-5 space-y-4">
                <h3 class="text-xs font-medium text-highlighted">Git</h3>
                <UFormField label="Production branch" help="Builds of this branch promote to production.">
                  <UInput v-model="settings.productionBranch" class="w-full max-w-44 font-mono" />
                </UFormField>
                <USwitch
                  v-model="settings.requirePullRequest"
                  label="Require a reviewed pull request"
                  description="Refuse to build a production-branch commit the git provider cannot say arrived through a pull request somebody other than its author approved. Preview builds are unaffected — they are what produces the thing being reviewed."
                />
                <USwitch
                  v-model="settings.previews"
                  label="Preview environments"
                  description="Every pull request gets its own environment."
                />
                <USwitch
                  v-model="settings.previewsProtected"
                  :disabled="!settings.previews"
                  label="Protect previews"
                  description="Previews sit behind platform login instead of being public."
                />
                <!-- The ceiling on how many of them may be live at once. It is
                     the platform's number unless this project says otherwise,
                     which is why the switch comes before the field. -->
                <USwitch
                  v-model="previewsMaxOwn"
                  :disabled="!settings.previews"
                  label="Own preview ceiling"
                  description="Set how many previews this project may have live at once, instead of taking the platform's."
                />
                <UFormField
                  v-if="previewsMaxOwn"
                  label="Live previews at once"
                  help="A pull request past this gets a commit status and a comment instead of an environment, and its preview on the next push after a slot frees. 0 means no ceiling for this project."
                >
                  <UInputNumber v-model="settings.previewsMax" :min="0" :max="100" class="w-40" />
                </UFormField>
                <p v-if="settings.previews" class="text-xs text-muted">{{ previewCeilingLine }}</p>
                <!-- What a fork's pull request gets. It sits with the preview
                     settings because it is one of them, and after the ceiling
                     because the ceiling is about how many and this is about
                     whose. It is not gated on `previews`: `build` is a setting
                     for a project that publishes no previews at all. -->
                <UFormField
                  label="Pull requests from forks"
                  help="A fork is any repository that is not this project's own. The platform may cap this for every project."
                >
                  <USelect v-model="settings.previewsForks" :items="forkOptions" class="w-full" />
                </UFormField>
                <p class="text-xs text-muted">{{ forkLine }}</p>
              </div>

              <div v-if="builtHere" class="rounded-md border border-default bg-muted p-5 space-y-4">
                <h3 class="text-xs font-medium text-highlighted">Build</h3>
                <div class="grid gap-4 sm:grid-cols-3">
                  <UFormField
                    label="Strategy"
                    :help="
                      declaredInRepo('build.strategy')
                        ? 'Set by the repository; this is overwritten at every build.'
                        : undefined
                    "
                  >
                    <USelect v-model="settings.buildStrategy" :items="strategyOptions" class="w-full" />
                  </UFormField>
                  <UFormField
                    label="Dockerfile"
                    :help="
                      declaredInRepo('build.dockerfilePath')
                        ? 'Set by the repository; this is overwritten at every build.'
                        : 'Relative to the root directory.'
                    "
                  >
                    <UInput v-model="settings.dockerfilePath" placeholder="Dockerfile" class="w-full font-mono" />
                  </UFormField>
                  <UFormField
                    label="Dockerfile stage"
                    :help="
                      declaredInRepo('build.dockerfileTarget')
                        ? 'Set by the repository; this is overwritten at every build.'
                        : 'Which stage of a multi-stage Dockerfile to ship. Empty is its last stage.'
                    "
                  >
                    <UInput v-model="settings.dockerfileTarget" placeholder="the last stage" class="w-full font-mono" />
                  </UFormField>
                  <UFormField label="Root directory" help="For monorepos.">
                    <UInput v-model="settings.rootDirectory" placeholder="." class="w-full font-mono" />
                  </UFormField>
                </div>
                <!-- The other half of the monorepo answer, next to the
                     directory it is about. The caveat is in the description
                     rather than in a document nobody opens: this is the one
                     setting whose wrong answer is a deploy that silently did
                     not happen. -->
                <USwitch
                  v-model="settings.skipUnchanged"
                  label="Skip a push that changed nothing here"
                  description="Compare the git tree object at the root directory with the last build's. When they match, record a skipped build instead of rebuilding and redeploying the same source. Leave it off if this project uses code from outside its root directory — a change there would be skipped."
                />
              </div>

              <div class="flex justify-end">
                <UButton type="submit" :loading="savingSettings" icon="i-lucide-check">Save settings</UButton>
              </div>
            </form>
          </PageSection>

          <!-- ── Processes ──────────────────────────────────────────────── -->
          <!-- The kitchen.json precedence notice travels with this panel: it
               is the difference between an edit that holds and one the next
               build silently reverts (#310, and #470 decision 4). -->
          <ProjectWorkloadsPanel
            v-else-if="current.id === 'processes'"
            :project="project.name"
            :role="project.role"
            :processes="project.processes"
            :built-here="builtHere"
            :declared-in="declaredInRepo('processes') ? config?.path : undefined"
            @saved="refresh"
          />

          <!-- ── Attached resources ─────────────────────────────────────── -->
          <PageSection v-else-if="current.id === 'resources'" :title="current.label" :description="current.description">
            <template #actions>
              <ClaimModal
                v-if="mayClaim"
                :project="project.name"
                :role="project.role"
                :processes="project.processes?.map((p) => p.name)"
                @saved="refresh"
              >
                <UButton icon="i-lucide-plus" size="xs">New claim</UButton>
              </ClaimModal>
            </template>
            <div class="rounded-md border border-default overflow-x-auto">
              <table class="w-full min-w-[42rem] text-sm">
                <tbody>
                  <tr v-if="!claims.length">
                    <td colspan="8" class="px-3 py-8 text-center text-muted">
                      Nothing attached — a claim asks for something the project needs (a database or a bucket from a
                      connection, single sign-on from the platform's own identity provider, durable background work, or
                      storage for one process) and binds it into the project's environments, through a secret its
                      variables reference or as a mount.
                    </td>
                  </tr>
                  <tr v-for="claim in claims" :key="claim.name" class="border-b border-muted last:border-0">
                    <td class="px-3 py-2 text-highlighted font-medium">{{ claim.name }}</td>
                    <td class="px-3 py-2">
                      <UBadge color="neutral" variant="subtle" size="sm" class="font-mono">{{ claim.type }}</UBadge>
                      <!-- What a preview gets, in the provider's own words on
                           hover: a preview writing to production is marked on
                           rather than implied by the absence of a badge. -->
                      <UBadge
                        v-if="claim.previewMode"
                        :color="claim.previewMode === 'shared' ? 'warning' : 'neutral'"
                        variant="subtle"
                        size="sm"
                        class="ml-1"
                        :title="claim.previewReason"
                      >
                        previews: {{ claim.previewMode }}
                      </UBadge>
                      <UBadge v-if="claim.keepsPodsRunning" color="warning" variant="subtle" size="sm" class="ml-1">
                        no scale to zero
                      </UBadge>
                      <UBadge
                        v-if="claim.idleReason && !claim.keepsPodsRunning"
                        :color="claim.canIdle ? 'neutral' : 'warning'"
                        variant="subtle"
                        size="sm"
                        class="ml-1"
                        :title="claim.idleReason"
                      >
                        idle preview: {{ claim.canIdle ? "parks with it" : "keeps running" }}
                      </UBadge>
                      <UBadge v-if="claim.forcesRecreate" color="warning" variant="subtle" size="sm" class="ml-1">
                        downtime on deploy
                      </UBadge>
                      <UBadge
                        v-if="claimBackupBadge(claim)"
                        :color="claimBackupBadge(claim)!.color"
                        variant="subtle"
                        size="sm"
                        class="ml-1"
                        :title="claimBackupBadge(claim)!.title"
                      >
                        {{ claimBackupBadge(claim)!.label }}
                      </UBadge>
                      <UBadge
                        v-for="requirement in claimRequirements(claim)"
                        :key="requirement"
                        color="neutral"
                        variant="subtle"
                        size="sm"
                        class="ml-1 font-mono"
                      >
                        {{ requirement }}
                      </UBadge>
                    </td>
                    <td class="px-3 py-2 text-xs whitespace-nowrap">
                      <!-- The two data facts, absences said out loud: the class
                           the claim was filed under, and what the provider says
                           the data derives from. -->
                      <span :class="claim.dataClass ? 'text-toned' : 'text-dimmed'">
                        {{ claim.dataClass || "unclassified" }}
                      </span>
                      <span class="text-dimmed"> · </span>
                      <span
                        :class="
                          claim.dataProvenance === 'production'
                            ? 'text-warning'
                            : claim.dataProvenance
                              ? 'text-toned'
                              : 'text-dimmed'
                        "
                      >
                        {{ claim.dataProvenance || "undeclared" }}
                      </span>
                      <span v-if="claimRecoveryPoint(claim)" class="block text-dimmed">
                        {{ claimRecoveryPoint(claim) }}
                      </span>
                    </td>
                    <td class="px-3 py-2 font-mono text-xs text-toned">
                      <template v-if="claim.connection">via {{ claim.connection }}</template>
                      <template v-else-if="claim.volume">
                        <span :title="claim.volume.accessModeReason">
                          {{ claim.volume.mountPath }} on {{ claim.volume.process }}
                        </span>
                        <!-- Two projects holding one filesystem is a fact that
                             must not have to be discovered, so the claim says
                             it where somebody is already looking at it. -->
                        <span
                          v-if="claim.volume.bound?.sharedWith?.length"
                          class="block text-dimmed"
                          :title="claim.volume.bound.sharedWith.join('\n')"
                        >
                          shared with {{ claim.volume.bound.sharedWith.join(", ") }}
                        </span>
                      </template>
                      <template v-else-if="claim.redirectURIs?.length">
                        <span :title="claim.redirectURIs.join('\n')">
                          {{ claim.redirectURIs.length }} redirect URI{{ claim.redirectURIs.length === 1 ? "" : "s" }}
                        </span>
                      </template>
                      <template v-else>—</template>
                    </td>
                    <td class="px-3 py-2"><PhaseBadge :phase="claim.phase" /></td>
                    <!-- What the binding is: the secret the variables read, or
                         for a volume the claim its process mounts. -->
                    <td
                      class="px-3 py-2 font-mono text-xs text-muted truncate max-w-48"
                      :title="claim.secret || claim.volume?.claimName"
                    >
                      {{ claim.secret || claim.volume?.claimName || "not bound yet" }}
                    </td>
                    <td class="px-3 py-2 text-xs text-muted whitespace-nowrap">
                      <template v-if="claim.type === 'oidcClient'">on delete: deregister the client</template>
                      <!-- A binding provisions nothing, so there is no policy
                           and nothing for one to keep: the row says what
                           deleting it actually does. -->
                      <template v-else-if="claim.type === 'service'">on delete: the offering stays offered</template>
                      <!-- An Inngest Cloud claim carries a policy the API
                           refuses to set: the app and the keys are the
                           account's, so the row says what actually happens. -->
                      <template v-else-if="claim.type === 'inngest' && !claim.inngest?.selfHosted">
                        on delete: the app stays at Inngest
                      </template>
                      <template v-else>
                        on delete: {{ claim.deletionPolicy === "Delete" ? "delete data" : "retain data" }}
                      </template>
                    </td>
                    <td class="px-3 py-2 text-right whitespace-nowrap">
                      <ClaimRecoveryModal
                        v-if="mayReadRecovery && offersRecovery(claim)"
                        class="mr-1"
                        :claim="claim.name"
                        :project="project.name"
                        :role="project.role"
                        @changed="refresh"
                      />
                      <UButton
                        v-if="mayUnclaim && (!destroysData(claim) || mayUnclaimData)"
                        color="neutral"
                        variant="subtle"
                        size="xs"
                        icon="i-lucide-trash-2"
                        @click="claimToDelete = claim"
                      >
                        Delete
                      </UButton>
                      <span v-else-if="mayUnclaim" class="text-xs text-muted" :title="claimDeleteRefusal(claim)">
                        admin only
                      </span>
                    </td>
                  </tr>
                  <!-- Why a claim was refused, in the provider's own words. -->
                  <tr
                    v-for="claim in refusedClaims"
                    :key="`${claim.name}-why`"
                    class="border-b border-muted last:border-0"
                  >
                    <td colspan="8" class="px-3 py-2 text-xs text-error">
                      <span class="font-mono">{{ claim.name }}</span> — {{ claimRefusal(claim) }}
                    </td>
                  </tr>
                  <!-- And what a bound claim is quietly not doing. An inngest
                       claim in serve mode registers the web process and nothing
                       else, which every other surface reads as a healthy
                       deploy; the claim counts what it synced against what the
                       unit runs and the sentence is here, where the mode was
                       chosen. -->
                  <tr v-for="caution in cautions" :key="caution.key" class="border-b border-muted last:border-0">
                    <td colspan="8" class="px-3 py-2 text-xs text-warning">
                      <span class="font-mono">{{ caution.claim }}</span> — {{ caution.message }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </PageSection>

          <!-- ── Variables, files, secrets ──────────────────────────────── -->
          <EnvVarsPanel
            v-else-if="current.id === 'variables'"
            :project="project.name"
            :role="project.role"
            :env="project.env"
            @saved="refresh"
          />
          <ProjectFilesPanel
            v-else-if="current.id === 'files'"
            :project="project.name"
            :role="project.role"
            :files="project.files"
            :processes="project.processes"
            :declared-in="config?.path"
            :declared-names="repoFiles"
            @saved="refresh"
          />
          <!-- ── Offerings ──────────────────────────────────────────────── -->
          <OfferingsPanel
            v-else-if="current.id === 'offerings'"
            :project="project.name"
            :role="project.role"
            :offers="project.offers"
            :processes="project.processes"
            :claims="bindings"
            :declared-in="config?.path"
            :declared-names="repoOffers"
            @saved="refresh"
          />
          <ProjectSecretsPanel
            v-else-if="current.id === 'secrets'"
            :project="project.name"
            :role="project.role"
            :env="project.env"
            @saved="refresh"
          />

          <!-- ── Domains ────────────────────────────────────────────────── -->
          <PageSection v-else-if="current.id === 'domains'" :title="current.label" :description="current.description">
            <!-- A hostname on an internal project is refused by the API, so the
                 pane says why here rather than letting somebody meet the
                 refusal on the environment screen. -->
            <UAlert
              v-if="project?.exposure === 'internal'"
              class="mb-4"
              color="info"
              variant="subtle"
              icon="i-lucide-shield"
              title="This project is internal"
              description="No environment of it is published, so there is no route for a custom hostname to ride and a new domain is refused. Set the exposure to public on the Runtime pane first."
            />
            <div class="rounded-md border border-default overflow-x-auto">
              <table class="w-full min-w-[42rem] text-sm">
                <tbody>
                  <tr v-if="!data?.domains.length">
                    <td class="px-3 py-8 text-center text-muted">
                      No custom domains — a hostname is attached to an environment, on that environment's own screen.
                    </td>
                  </tr>
                  <tr v-for="domain in data?.domains" :key="domain.name" class="border-b border-muted last:border-0">
                    <td class="px-3 py-2">
                      <a
                        :href="`https://${domain.hostname}`"
                        target="_blank"
                        rel="noopener"
                        class="font-mono text-highlighted hover:underline"
                        >{{ domain.hostname }}</a
                      >
                    </td>
                    <td class="px-3 py-2">
                      <RouterLink :to="environmentLink(domain.environment, name)" class="text-toned hover:underline">
                        {{ domain.environment }}
                      </RouterLink>
                    </td>
                    <td class="px-3 py-2">
                      <UBadge v-if="domain.tls" color="neutral" variant="subtle" size="sm" class="font-mono">
                        tls: {{ domain.tls }}
                      </UBadge>
                    </td>
                    <td class="px-3 py-2">
                      <UBadge :color="domain.verified ? 'success' : 'warning'" variant="soft" size="sm">
                        {{ domain.verified ? "Verified" : "Awaiting DNS" }}
                      </UBadge>
                    </td>
                    <td class="px-3 py-2 text-right text-xs text-muted whitespace-nowrap">
                      {{ timeAgo(domain.createdAt) }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
          </PageSection>

          <!-- ── People, keys, notifications ────────────────────────────── -->
          <MembersPanel v-else-if="current.id === 'members'" :project="project.name" :role="project.role" />
          <KeysPanel v-else-if="current.id === 'keys'" :project="project.name" :role="project.role" />
          <NotificationsPanel
            v-else-if="current.id === 'notifications'"
            :project="project.name"
            :role="project.role"
          />

          <!-- ── Runtime ────────────────────────────────────────────────── -->
          <PageSection v-else-if="current.id === 'runtime'" :title="current.label" :description="current.description">
            <form class="space-y-6" @submit.prevent="saveSettings">
              <!-- Whether anyone outside the cluster is sent to it at all. It
                   comes before how much of it runs because it decides what the
                   rest of this pane is for: an internal project has no address,
                   no gate and no idling. -->
              <div class="rounded-md border border-default bg-muted p-5 space-y-4">
                <h3 class="text-xs font-medium text-highlighted">Who can reach it</h3>
                <UFormField
                  label="Exposure"
                  help="A project that exists to be called by other applications does not need a hostname on the internet."
                >
                  <USelect v-model="settings.exposure" :items="EXPOSURE_OPTIONS" class="w-full" />
                </UFormField>
                <p class="text-xs text-muted">{{ exposureLine }}</p>
              </div>

              <div class="rounded-md border border-default bg-muted p-5 space-y-4">
                <h3 class="text-xs font-medium text-highlighted">How much of it runs</h3>
                <div class="grid gap-4 sm:grid-cols-4">
                  <UFormField label="Port" :help="portHelp">
                    <UInput v-model="portField" type="number" min="0" placeholder="auto" class="w-full font-mono" />
                  </UFormField>
                  <UFormField label="Replicas" :help="replicasHelp">
                    <UInput
                      v-model.number="settings.replicas"
                      type="number"
                      min="1"
                      :max="settings.singleton ? 1 : undefined"
                      :disabled="settings.singleton"
                      class="w-full font-mono"
                    />
                  </UFormField>
                  <UFormField label="CPU" help="Per replica, e.g. 250m.">
                    <UInput v-model="settings.cpu" placeholder="unset" class="w-full font-mono" />
                  </UFormField>
                  <UFormField label="Memory" help="Per replica, e.g. 512Mi.">
                    <UInput v-model="settings.memory" placeholder="unset" class="w-full font-mono" />
                  </UFormField>
                </div>
                <!-- Two declarations about what the workload is rather than
                     about how much of it there should be, and they sit under
                     the replica count because both are that number's other
                     half: one says it must stay at one, the other that it must
                     never reach zero. -->
                <USwitch
                  v-model="settings.singleton"
                  label="Never run two at once"
                  description="For an application that polls, schedules or ingests in the same process as the web server: a
                    rolling deploy would overlap the two for a few seconds, and that loop would run twice against one
                    store. Deploys stop the old copy before starting the new one, so there is a gap in serving, and the
                    replica count is fixed at 1."
                />
                <USwitch
                  v-model="settings.notRequestDriven"
                  label="Does work nobody asked for"
                  description="An idle environment stops doing everything, not only serving — so an application with a
                    background loop, a poller or an ingest job goes quiet with a gap in its data that looks exactly like
                    the upstream having been down. Turning this on keeps every environment of this project awake,
                    previews included."
                />
                <!-- A claim's provider can say the same thing about its
                     binding, and then the switch above is not what decides:
                     scale to zero is a project-level policy, so the one claim
                     costs idling for every environment of the project. -->
                <UAlert
                  v-if="pinningClaims.length"
                  color="warning"
                  variant="subtle"
                  icon="i-lucide-triangle-alert"
                  title="This project is not offered scale to zero"
                  :description="`${pinningClaims.map((claim) => claim.name).join(', ')} ${pinningClaims.length === 1 ? 'holds' : 'hold'} a worker whose outbound connection never crosses the interceptor, so nothing can tell when an environment is idle. Every environment of this project keeps running, previews included — release the claim to idle them.`"
                />
              </div>

              <!-- What the web process needs done inside its volumes before it
                   starts. It sits after the numbers because it depends on the
                   posture: the steps run as the workload runs. -->
              <div class="rounded-md border border-default bg-muted p-5 space-y-4">
                <VolumeInitEditor v-model="settings.init" :may-edit="true" heading="h3" />
                <p v-if="volumePreparationProblems.length" class="text-xs text-warning">
                  {{ volumePreparationProblems.join(" ") }}
                </p>
              </div>

              <!-- How it starts. Exec form is a list of words, so each field is
                   a list of lines: an argument with a space in it is ordinary,
                   and one box split on spaces would quietly break it. -->
              <div class="rounded-md border border-default bg-muted p-5 space-y-4">
                <div>
                  <h3 class="text-xs font-medium text-highlighted">How it starts</h3>
                  <p class="text-xs text-muted mt-1">
                    What the image is started with, when its own entrypoint is not what this project wants.
                    <span class="text-toned font-medium">One word per line</span> — this is never a shell line, so
                    nothing is split, quoted or expanded. Preview arguments are the sibling of a variable's preview
                    value: the same commit and the same artifact, pointed somewhere else. A release carries all three,
                    so a rollback restores the arguments it ran with.
                  </p>
                </div>
                <div class="grid gap-4 sm:grid-cols-3">
                  <UFormField label="Command" help="Replaces the entrypoint. Empty keeps the image's own.">
                    <UTextarea v-model="settings.command" :rows="3" placeholder="./server" class="w-full font-mono" />
                  </UFormField>
                  <UFormField label="Arguments" help="Passed to it. Empty passes none.">
                    <UTextarea
                      v-model="settings.args"
                      :rows="3"
                      placeholder="--config=prod.toml"
                      class="w-full font-mono"
                    />
                  </UFormField>
                  <UFormField label="Preview arguments" help="Used instead, in previews. Empty runs the ones beside it.">
                    <UTextarea
                      v-model="settings.previewArgs"
                      :rows="3"
                      placeholder="--config=fake.toml"
                      class="w-full font-mono"
                    />
                  </UFormField>
                </div>
              </div>

              <!-- Health. The copy leads with what happens when the path is
                   empty, because that is the state every project starts in and
                   the one whose consequence is least obvious. -->
              <div class="rounded-md border border-default bg-muted p-5 space-y-4">
                <div>
                  <h3 class="text-xs font-medium text-highlighted">Health</h3>
                  <p class="text-xs text-muted mt-1">
                    What the platform asks a new replica before it sends anyone to it — on every deploy and every
                    rollback.
                    <span class="text-toned font-medium"
                      >With no path it opens a TCP connection to the port and no more</span
                    >, which is a weaker claim than an answer and a much better one than assuming. A path is where the
                    application says what working means for it, and it is also what buys a restart when a running
                    container wedges. Previews inherit this, and a release carries it, so a rollback restores the check
                    it ran with.
                  </p>
                </div>
                <div class="grid gap-4 sm:grid-cols-3">
                  <UFormField label="Path" help="An HTTP GET answering 2xx or 3xx. Empty is a TCP connect — never GET /.">
                    <UInput v-model="settings.healthPath" placeholder="/healthz" class="w-full font-mono" />
                  </UFormField>
                  <UFormField label="Port" help="Only when the check is not on the port above.">
                    <UInput
                      v-model.number="settings.healthPort"
                      type="number"
                      min="0"
                      placeholder="the application's"
                      class="w-full font-mono"
                    />
                  </UFormField>
                  <UFormField label="Period" help="Seconds between checks. 0 takes the default.">
                    <UInput v-model.number="settings.healthPeriod" type="number" min="0" class="w-full font-mono" />
                  </UFormField>
                  <UFormField label="Timeout" help="Seconds one check may take. 0 takes the default.">
                    <UInput v-model.number="settings.healthTimeout" type="number" min="0" class="w-full font-mono" />
                  </UFormField>
                  <UFormField label="Failures" help="Checks in a row before a running replica is taken out.">
                    <UInput v-model.number="settings.healthFailures" type="number" min="0" class="w-full font-mono" />
                  </UFormField>
                  <UFormField label="Startup failures" help="The generous one: how long it has to come up at all.">
                    <UInput
                      v-model.number="settings.healthStartupFailures"
                      type="number"
                      min="0"
                      class="w-full font-mono"
                    />
                  </UFormField>
                </div>
              </div>

              <div class="flex justify-end">
                <UButton type="submit" :loading="savingSettings" icon="i-lucide-check">Save settings</UButton>
              </div>
            </form>
          </PageSection>

          <!-- ── Security ───────────────────────────────────────────────── -->
          <PageSection v-else-if="current.id === 'security'" :title="current.label" :description="current.description">
            <form class="space-y-6" @submit.prevent="saveSettings">
              <div class="rounded-md border border-default bg-muted p-5 space-y-4">
                <p class="text-xs text-muted">
                  Everything here off is the platform's default, which is deliberately not the tightest one available:
                  the container runtime's own seccomp profile, and no privilege escalation. The rest is a tightening an
                  application asks for, because an image that writes into its own filesystem or runs as root is ordinary
                  and would break under a default that assumed otherwise. A workload that cannot start under what it
                  asked for says so on its environment, naming the constraint. The volume group is the one that goes the
                  other way: a workload running as anybody but root needs it to be able to write the volume it was
                  given.
                </p>
                <USwitch
                  v-model="settings.readOnlyRootFilesystem"
                  label="Read-only root filesystem"
                  description="The container cannot write to its own filesystem. An application that writes a cache, a
                    socket or a temporary file into it needs a volume for that path first."
                />
                <USwitch
                  v-model="settings.runAsNonRoot"
                  label="Never run as root"
                  description="A container whose image would run as uid 0 is refused before it starts, rather than
                    running with the privilege and being noticed later."
                />
                <div class="grid gap-4 sm:grid-cols-2">
                  <UFormField
                    label="User ID"
                    help="The uid the containers run as, overriding the image's. 0 is the image's own user left alone — which is not the same as asking to run as root."
                  >
                    <UInput
                      v-model.number="settings.runAsUser"
                      type="number"
                      min="0"
                      placeholder="the image's"
                      class="w-full font-mono"
                    />
                  </UFormField>
                  <UFormField label="Group ID" help="The gid, on the same reading.">
                    <UInput
                      v-model.number="settings.runAsGroup"
                      type="number"
                      min="0"
                      placeholder="the image's"
                      class="w-full font-mono"
                    />
                  </UFormField>
                </div>
                <div class="grid gap-4 sm:grid-cols-2">
                  <UFormField
                    label="Volume group ID"
                    help="The gid that owns the volumes this project mounts. A new volume comes up owned by root, so a workload running as anybody else cannot write it — it starts, looks healthy, and fails on its first write. 0 leaves the volume's own ownership alone."
                  >
                    <UInput
                      v-model.number="settings.fsGroup"
                      type="number"
                      min="0"
                      placeholder="the volume's"
                      class="w-full font-mono"
                    />
                  </UFormField>
                  <UFormField
                    label="Apply ownership"
                    help="When that ownership is applied. Always walks the whole volume on every start, which on a large one is slow; OnRootMismatch skips the walk when the volume's own root already matches, at the price of a subtree left by a previous user staying unwritable. It needs a volume group ID."
                  >
                    <USelect
                      v-model="settings.fsGroupChangePolicy"
                      :items="fsGroupChangePolicyOptions"
                      :disabled="!settings.fsGroup"
                      class="w-full"
                    />
                  </UFormField>
                </div>
                <UFormField
                  label="Drop capabilities"
                  help="One per line, the way the kernel spells them and without the CAP_ prefix — NET_RAW, SYS_ADMIN — or the single entry ALL. There is no list to add one back: the platform drops none by default."
                >
                  <UTextarea v-model="settings.dropCapabilities" :rows="2" placeholder="ALL" class="w-full font-mono" />
                </UFormField>
                <USwitch
                  v-model="settings.allowPrivilegeEscalation"
                  label="Allow privilege escalation"
                  description="The one default the platform tightens: left alone, nothing in the container can gain more
                    privileges than the process that started it. Turn it on only for an image that genuinely needs a
                    setuid binary."
                />
              </div>

              <div class="flex justify-end">
                <UButton type="submit" :loading="savingSettings" icon="i-lucide-check">Save settings</UButton>
              </div>
            </form>
          </PageSection>

          <!-- ── Continuity ─────────────────────────────────────────────── -->
          <PageSection v-else-if="current.id === 'continuity'" :title="current.label" :description="current.description">
            <form class="space-y-6" @submit.prevent="saveSettings">
              <div class="rounded-md border border-default bg-muted p-5 space-y-4">
                <h3 class="text-xs font-medium text-highlighted">Data</h3>
                <UFormField
                  label="Data classification"
                  help="What class of data this project handles. Claims narrow it and never exceed it; a promotion into an environment rated below it is refused by policy. Changes are audit-logged with the previous value."
                >
                  <USelect v-model="settings.dataClass" :items="dataClassOptions" class="w-full max-w-60" />
                </UFormField>
              </div>

              <!-- The copy leads with what Kitchen does not do, because a form
                   that asked "how critical is this?" without saying whose
                   question it is would read as the platform having an opinion
                   about the institution's functions. It does not. -->
              <div class="rounded-md border border-default bg-muted p-5 space-y-4">
                <div>
                  <h3 class="text-xs font-medium text-highlighted">Tolerances</h3>
                  <p class="text-xs text-muted mt-1">
                    <span class="text-toned font-medium"
                      >Kitchen does not decide what is critical, and does not set these tolerances.</span
                    >
                    They are the institution's — a board's judgement about its own functions — and this is where that
                    judgement is recorded. What the platform does with it: map the function onto everything serving it,
                    and alert against the tolerance. Nothing here refuses a deployment, and an absent designation is
                    never defaulted to anything.
                  </p>
                </div>
                <div class="grid gap-4 sm:grid-cols-3">
                  <UFormField
                    label="Criticality"
                    help="How much it matters that this function keeps working. Production environments read this where they declare nothing of their own; previews never do."
                  >
                    <USelect v-model="settings.criticality" :items="criticalityOptions" class="w-full" />
                  </UFormField>
                  <UFormField
                    label="RTO"
                    help="How long it may be unavailable. Whole hours and minutes: 4h, 30m, 1h30m. It is the threshold the outage alert fires against."
                  >
                    <UInput v-model="settings.rto" placeholder="unset" class="w-full font-mono" />
                  </UFormField>
                  <UFormField
                    label="RPO"
                    help="How much data it may lose. Same spelling. Carried and mapped; nothing alerts on it yet, because the platform observes no recovery points."
                  >
                    <UInput v-model="settings.rpo" placeholder="unset" class="w-full font-mono" />
                  </UFormField>
                </div>
              </div>

              <div class="flex justify-end">
                <UButton type="submit" :loading="savingSettings" icon="i-lucide-check">Save settings</UButton>
              </div>
            </form>
          </PageSection>

          <!-- ── Danger zone ────────────────────────────────────────────── -->
          <PageSection v-else-if="current.id === 'danger'" :title="current.label" :description="current.description">
            <div class="rounded-md border border-error/40 p-5 space-y-3">
              <p class="text-xs text-muted">
                Deleting the project tears down its environments — production included — and removes its builds,
                releases and domains. There is no undo.
              </p>
              <div class="flex items-center gap-2 flex-wrap">
                <UInput
                  v-model="deleteConfirmation"
                  :placeholder="`Type ${project.name} to confirm`"
                  class="w-64 font-mono"
                />
                <UButton
                  color="error"
                  :disabled="deleteConfirmation !== project.name"
                  :loading="deleting"
                  icon="i-lucide-trash-2"
                  @click="deleteProject"
                >
                  Delete project
                </UButton>
              </div>
            </div>
          </PageSection>
        </div>
      </div>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>

    <!-- Claim deletion confirmation: explicit about what the deletionPolicy
         does to the data, which is the whole difference between the two. A
         Delete policy destroys it, so that half is confirmed by typing the
         claim's name — the same gate deleting the project has. -->
    <UModal
      :open="claimToDelete !== null"
      :title="`Delete claim ${claimToDelete?.name}?`"
      :description="claimToDelete ? claimDeletionWarning(claimToDelete) : ''"
      @update:open="(open: boolean) => { if (!open) claimToDelete = null; }"
    >
      <template v-if="claimDeleteGated" #body>
        <UInput
          v-model="claimConfirmation"
          :placeholder="`Type ${claimToDelete?.name} to confirm`"
          class="w-full font-mono"
        />
      </template>
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="claimToDelete = null">Cancel</UButton>
          <UButton
            color="error"
            :disabled="!claimDeleteReady"
            :loading="deletingClaim"
            icon="i-lucide-trash-2"
            @click="deleteClaim"
          >
            {{ claimToDelete?.deletionPolicy === "Delete" ? "Delete claim and data" : "Delete claim, keep data" }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
