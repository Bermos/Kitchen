<script setup lang="ts">
import { computed, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, CRITICALITIES, DATA_CLASSES, type Claim, type Project, type Release } from "../lib/api";
import { buildFailureLine } from "../lib/builds";
import { deletionGatedByName, destroysData, destroysDataRefusal, mayDestroyData } from "../lib/claims";
import { duration, shortImage, shortSHA, timeAgo } from "../lib/format";
import { useFreshness } from "../lib/freshness";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { pipelineShown } from "../lib/promotions";
import { releaseHistoryEntry, releaseHistoryLabel } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import ClaimModal from "../components/ClaimModal.vue";
import ClaimRecoveryModal from "../components/ClaimRecoveryModal.vue";
import CommitBody from "../components/CommitBody.vue";
import CommitBodyToggle from "../components/CommitBodyToggle.vue";
import ConditionsTable from "../components/ConditionsTable.vue";
import EnvVarsPanel from "../components/EnvVarsPanel.vue";
import ProjectFilesPanel from "../components/ProjectFilesPanel.vue";
import ProjectSecretsPanel from "../components/ProjectSecretsPanel.vue";
import ProjectWorkloadsPanel from "../components/ProjectWorkloadsPanel.vue";
import EnvironmentCard from "../components/EnvironmentCard.vue";
import KeysPanel from "../components/KeysPanel.vue";
import MembersPanel from "../components/MembersPanel.vue";
import OperatorOnly from "../components/OperatorOnly.vue";
import PageHeader from "../components/PageHeader.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import PipelinePanel from "../components/PipelinePanel.vue";
import StatusDot from "../components/StatusDot.vue";

const route = useRoute();
const router = useRouter();
const toast = useToast();
const name = computed(() => route.params.name as string);

/** Opening a build from anywhere on its row, without stealing the clicks that
 *  already mean something: a link inside the row, and the modifier clicks a
 *  browser opens in a new tab with. */
function openBuild(build: string, event: MouseEvent) {
  if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0) return;
  if ((event.target as HTMLElement | null)?.closest("a")) return;
  void router.push({ name: "build", params: { name: build } });
}

/** The build whose commit message is open, if any. One at a time, so the list
 *  stays a list of subjects with one of them answered at length. */
const expandedMessage = ref<string | null>(null);
function toggleMessage(build: string) {
  expandedMessage.value = expandedMessage.value === build ? null : build;
}

const { data, error, loading, refresh } = useAsync(async () => {
  const [project, environments, releases, builds, claims, allDomains, promotions] = await Promise.all([
    api.project(name.value),
    api.projectEnvironments(name.value),
    api.projectReleases(name.value),
    api.projectBuilds(name.value),
    api.claims({ project: name.value }),
    api.domains(),
    api.projectPromotions(name.value),
  ]);
  // Domains attach to environments; the project's are the ones pointing at
  // one of its environments.
  const environmentNames = new Set(environments.map((e) => e.name));
  const domains = allDomains.filter((d) => environmentNames.has(d.environment));
  return { project, environments, releases, builds, claims, domains, promotions };
});
watch(name, () => void refresh());
// How old this screen is, and the reader's hold on it: every fetch above
// reports into it and the header renders it.
const freshness = useFreshness();
usePoll(() => void refresh(), 10000, () => true);

const project = computed(() => data.value?.project);

// What this account may do here, keyed to the role that arrived on this
// project's own payload rather than to the mode the dashboard is in. A viewer
// gets the same screens as a developer with the write controls gone — not
// disabled with a tooltip. A disabled button is an invitation to go and find
// somebody who can press it, which is right for a shared enterprise tool and
// wrong here: the screens are the same, and the buttons are simply not part of
// a viewer's dashboard.
//
// The route is named once, at the control, and `may` answers from the same
// table the API enforces (`policy.generated.ts`), so nothing here is a second
// opinion about who may redeploy.
const caller = computed(() => callerFor(project.value?.role, name.value));
const mayBuild = computed(() => may("POST /api/v1/projects/{name}/builds", caller.value));
// Acquiring an image somebody else built is admin's where a rebuild is a
// developer's, and it is a different route, so it gets its own answer from
// the same table rather than sharing the one above.
const mayAcquire = computed(() => may("POST /api/v1/projects/{name}/acquisitions", caller.value));
const mayDeploy = computed(() => may("PATCH /api/v1/environments/{name}", caller.value));
const mayClaim = computed(() => may("POST /api/v1/claims", caller.value));
// Reading what a claim can be recovered to is the viewer's; the control is
// offered on the kinds of claim that hold data somebody would want back, and
// the panel behind it says — in the provider's own words — when this one
// cannot be recovered at all.
const mayReadRecovery = computed(() => may("GET /api/v1/claims/{name}/recoveries", caller.value));
function offersRecovery(claim: Claim): boolean {
  return claim.type === "postgres";
}
const mayUnclaim = computed(() => may("DELETE /api/v1/claims/{name}", caller.value));
const mayConfigure = computed(() => may("PATCH /api/v1/projects/{name}", caller.value));
const mayDelete = computed(() => may("DELETE /api/v1/projects/{name}", caller.value));
// Who is on a project is every member's to read and an admin's to change, so
// the People tab is everybody's and the controls inside it are not. The CI
// keys sit under it on the same terms — they are the same list with its
// non-human half shown.
const mayReadMembers = computed(() => may("GET /api/v1/projects/{name}/members", caller.value));
// Environment variables are the developer's day job and have a route of their
// own, so they are a tab of their own rather than a section of a form only an
// admin has. An admin has both, since admin contains developer.
//
// **The tab is keyed to reading the project, not to writing the variables.**
// `GET /projects/{name}` is a viewer's and already carries the list — names,
// whether each has a value, and the secret and claim references — so hiding
// the tab from a viewer would be the dashboard enforcing something the API
// does not, which is the mirror image of the mistake this whole table exists
// to prevent. What a viewer gets is the same screen with the write
// affordances gone, which is what every other screen here does; EnvVarsPanel
// keys those to the write route itself.
const mayReadProject = computed(() => may("GET /api/v1/projects/{name}", caller.value));

const production = computed(() =>
  data.value?.environments.find((e) => e.name === data.value?.project.productionEnvironment),
);
const previews = computed(() => (data.value?.environments ?? []).filter((e) => e.type === "preview"));
// The mini-cards, production first: previews and production at a glance, with
// what each one served, how much of it failed and how slow it was — so nobody
// has to open five environments to find the one that is unwell.
const environmentCards = computed(() => {
  const environments = data.value?.environments ?? [];
  const first = production.value;
  return first ? [first, ...environments.filter((e) => e.name !== first.name)] : environments;
});
const latestBuild = computed(() => data.value?.builds[0]);
// The pipeline section earns its place when stages are configured or
// promotions exist to explain; every other project keeps today's screen.
const promotions = computed(() => data.value?.promotions ?? []);
const showPipeline = computed(() => pipelineShown(project.value?.promotionStages, promotions.value));
const framework = computed(() => data.value?.builds.find((b) => b.detectedFramework)?.detectedFramework);

// Where this project's software comes from. A project has a repository or it
// has a vendored image, never both (#307), and the whole of the Git half of
// this screen is about the first — a production branch, a review requirement,
// previews, a build strategy and a root directory are all questions about a
// repository, and a project with none is shown what it actually has instead
// of a column of empty fields.
const vendoredImage = computed(() => project.value?.image);
const builtHere = computed(() => !!project.value?.repo);

// What the repository is deciding for itself, read off the most recent build
// that found a kitchen.json.
//
// It is on the settings tab because that is where somebody is about to change
// a field the next build will change back. The file wins for the settings it
// names, so a form that did not say so would be a form whose Save button
// silently does nothing for one field in five.
const repoConfig = computed(() => data.value?.builds.find((b) => b.config)?.config);
const repoDeclares = computed(() => repoConfig.value?.declares ?? []);
function declaredInRepo(field: string): boolean {
  return repoDeclares.value.includes(field);
}
// Configuration files are named one by one — "files.configuration" — because
// they merge onto the project's by name rather than replacing the list.
const repoFiles = computed(() =>
  repoDeclares.value.filter((field) => field.startsWith("files.")).map((field) => field.slice("files.".length)),
);

const currentRelease = computed(() => production.value?.release);
function buildOf(release: Release) {
  return data.value?.builds.find((b) => b.name === release.build);
}
function releaseState(release: Release): { label: string; tone: "success" | "neutral" | "warning" } {
  if (release.name === currentRelease.value) {
    const observed = production.value?.observedRelease;
    return observed === release.name ? { label: "Live", tone: "success" } : { label: "Rolling out", tone: "warning" };
  }
  // Past releases read their label off the environment's history: how each
  // one stopped being current, not just that it did.
  return { label: releaseHistoryLabel(release.name, production.value), tone: "neutral" };
}

// The badge's tooltip: who moved production off this release.
function releaseMovedBy(release: Release): string {
  const entry = releaseHistoryEntry(release.name, production.value);
  if (!entry?.by) return "";
  return entry.reason === "promoted" ? `by build ${entry.by}` : `by ${entry.by}`;
}

// Rebuild: POST /projects/{name}/builds with an empty body repeats the last
// commit — a rerun after a flaky build or a changed secret.
const redeploying = ref(false);
async function redeploy() {
  redeploying.value = true;
  try {
    const build = await api.rebuild(name.value);
    toast.add({ title: `Build ${build.name} queued`, color: "success", icon: "i-lucide-hammer" });
    await refresh();
  } catch (err) {
    toast.add({
      title: "Rebuild failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    redeploying.value = false;
  }
}

// Acquire: the vendored equivalent of a redeploy. A project with no repository
// has no commit to rebuild, and what moves it is a new digest under the tag it
// follows — which the platform asks about on an interval and this asks about
// now, for somebody who has just published upstream and would rather not wait.
const acquiring = ref(false);
async function acquire() {
  acquiring.value = true;
  try {
    const build = await api.acquire(name.value);
    toast.add({
      title: `Acquisition ${build.name} queued`,
      description: "The registry is being asked what this tag names now.",
      color: "success",
      icon: "i-lucide-package-search",
    });
    await refresh();
  } catch (err) {
    toast.add({
      title: "Acquisition failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    acquiring.value = false;
  }
}

// Rollback: pointing the production environment at an older release puts back
// exactly what was running. The modal confirms which one.
const rollbackTarget = ref<Release | null>(null);
const rollingBack = ref(false);
async function rollBack() {
  const target = rollbackTarget.value;
  const environment = production.value;
  if (!target || !environment) return;
  rollingBack.value = true;
  try {
    const outcome = await api.moveEnvironment(environment.name, target.name);
    // An environment with requirements answers with the promotion the move
    // became: nothing has moved yet, and saying "moved" would be a lie the
    // pipeline section immediately contradicts.
    if ("trigger" in outcome) {
      toast.add({
        title: `Promotion ${outcome.name} requested`,
        description: `${environment.name} declares requirements; the policy decides whether ${target.name} lands.`,
        color: "info",
        icon: "i-lucide-scale",
      });
    } else {
      toast.add({ title: `${environment.name} moved to ${target.name}`, color: "success", icon: "i-lucide-undo-2" });
    }
    rollbackTarget.value = null;
    await refresh();
  } catch (err) {
    toast.add({
      title: "Rollback failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    rollingBack.value = false;
  }
}

const tab = ref("deployments");
// Which tabs exist is a matter of what the API would answer, never of the
// mode the dashboard is in. Settings is the project admin's whole tab, not a
// tab with the controls removed: everything on it is an admin's write.
// Variables is everybody's to read and the developer's to change — which is
// why it is a tab beside People rather than a section inside a form a
// developer does not have. People is everybody's now that reading the list is
// a viewer's, with the add, change and remove controls absent for anyone but
// an admin.
const tabs = computed(() =>
  [
    { label: `Deployments`, value: "deployments", shown: true },
    { label: `Previews (${previews.value.length})`, value: "previews", shown: true },
    { label: `Builds (${data.value?.builds.length ?? 0})`, value: "builds", shown: true },
    { label: `Environments (${data.value?.environments.length ?? 0})`, value: "environments", shown: true },
    { label: `Domains (${data.value?.domains.length ?? 0})`, value: "domains", shown: true },
    { label: `Resources (${data.value?.claims.length ?? 0})`, value: "resources", shown: true },
    { label: `Variables (${data.value?.project.env?.length ?? 0})`, value: "variables", shown: mayReadProject.value },
    { label: `People`, value: "people", shown: mayReadMembers.value },
    { label: `Settings`, value: "settings", shown: mayConfigure.value },
  ].filter((item) => item.shown),
);
// A tab this account does not have — a bookmarked settings tab, or a role
// that changed under an open page — falls back to the one every role has.
watch(tabs, (items) => {
  if (!items.some((item) => item.value === tab.value)) tab.value = "deployments";
});

// The settings tab edits a copy of the project, loaded once per project so
// the 10s poll never types over the user. Environment variables are not in
// this copy and must not be: `PATCH /projects/{name}` now refuses a body that
// carries `env`, naming the route that takes them instead — they are the
// Variables tab's, and EnvVarsPanel holds its own drafts.
const settings = reactive({
  loadedFor: "",
  productionBranch: "",
  requirePullRequest: false,
  previews: true,
  previewsProtected: true,
  buildStrategy: "auto",
  dockerfilePath: "",
  dockerfileTarget: "",
  rootDirectory: "",
  // 0 is "let the platform decide": the port then comes from the framework
  // each build detects, and the field shows what that would be.
  port: 0,
  replicas: 1,
  cpu: "",
  memory: "",
  // The health check. An empty path is a TCP connect to the container port,
  // which is what every environment gets when nobody has said otherwise; the
  // four numbers are 0 for "take the platform's default", so the fields show
  // the resolved values and send back whatever is typed over them.
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
  // Empty is no override, the same reading an empty preview value gets, so
  // an empty box is how one is taken away and no switch is needed to say so.
  previewArgs: "",
  // The security posture every workload of the project runs under. Every
  // field is off or 0 for "the platform's default", so an untouched form
  // sends the posture the project already has — which is what makes clearing
  // the switches the way one is taken back off.
  runAsNonRoot: false,
  runAsUser: 0,
  runAsGroup: 0,
  // The gid that owns the volumes the workloads mount, and when the kubelet
  // applies it. Empty policy is Kubernetes' own default, and it is sent only
  // alongside a group id because that is the only time it applies.
  fsGroup: 0,
  fsGroupChangePolicy: "",
  readOnlyRootFilesystem: false,
  allowPrivilegeEscalation: false,
  // One capability per line, the same shape the argument fields take: a list
  // of words is a list of lines, never a string split on spaces.
  dropCapabilities: "",
  // Two of this workload must never run at once. It is next to the replica
  // count because it is the same decision from the other side, and the form
  // keeps the two consistent rather than letting the API refuse the pair.
  singleton: false,
  // Work nobody asked for. Idling is request-driven by construction, so an
  // application with a background loop is the one it silently breaks.
  notRequestDriven: false,
  // "" is unclassified — a state shown as such, never a default.
  dataClass: "",
  // "" is undesignated, for the same reason: Kitchen does not decide what is
  // critical, so it must not appear to have an opinion by defaulting one.
  criticality: "",
  rto: "",
  rpo: "",
});
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

// A word list as the form holds it and as the API takes it: one word per
// line, blank lines dropped. Nothing is split on spaces, because a single
// argument containing one is ordinary and exec form has no quoting to lean on.
function wordLines(words: string[] | undefined): string {
  return (words ?? []).join("\n");
}
function wordsOf(lines: string): string[] {
  return lines
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
}

// Turning the singleton switch on takes the replica count with it. The API
// refuses the pair rather than clamping it — a value quietly lowered reads
// back as a setting that did not take — so the form must not be able to send
// one it knows will be refused.
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

// When the kubelet applies the volume ownership. Empty is Kubernetes' own
// default, which is what a project that has not thought about it should get.
const fsGroupChangePolicyOptions = [
  { label: "Always (every start)", value: "" },
  { label: "OnRootMismatch", value: "OnRootMismatch" },
];

const criticalityOptions = [
  { label: "undesignated", value: "" },
  ...CRITICALITIES.map((value) => ({ label: value, value: value as string })),
];
function loadSettings(from: Project) {
  settings.loadedFor = from.name;
  settings.productionBranch = from.productionBranch;
  settings.requirePullRequest = from.requirePullRequest;
  settings.previews = from.previews;
  settings.previewsProtected = from.previewsProtected;
  settings.buildStrategy = from.buildStrategy || "auto";
  settings.dockerfilePath = from.dockerfilePath ?? "";
  settings.dockerfileTarget = from.dockerfileTarget ?? "";
  settings.rootDirectory = from.rootDirectory ?? "";
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
  settings.singleton = from.singleton ?? false;
  settings.notRequestDriven = from.notRequestDriven ?? false;
  settings.dataClass = from.dataClass ?? "";
  settings.criticality = from.criticality ?? "";
  settings.rto = from.rto ?? "";
  settings.rpo = from.rpo ?? "";
}
watch(project, (value) => {
  if (value && value.name !== settings.loadedFor) loadSettings(value);
});

const savingSettings = ref(false);
async function saveSettings() {
  savingSettings.value = true;
  try {
    const saved = await api.updateProject(name.value, {
      // A repository's settings, sent only by a project that has one. The
      // API refuses a production branch or a pull request requirement on a
      // project whose source is an image, and rightly — but a save of the
      // data classification should not fail with a sentence about branches,
      // and the cards these belong to are not on the screen either (#307).
      ...(builtHere.value
        ? {
            productionBranch: settings.productionBranch,
            requirePullRequest: settings.requirePullRequest,
            previews: settings.previews,
            previewsProtected: settings.previewsProtected,
            buildStrategy: settings.buildStrategy,
            dockerfilePath: settings.dockerfilePath,
            dockerfileTarget: settings.dockerfileTarget,
            rootDirectory: settings.rootDirectory,
          }
        : {}),
      port: settings.port,
      replicas: settings.replicas,
      cpu: settings.cpu,
      memory: settings.memory,
      health: {
        path: settings.healthPath,
        // 0 is not "no port": it is the check being made against whatever
        // port the application is published on, which is what an application
        // with one listener wants and what keeps the probe with the port when
        // the port moves.
        port: settings.healthPort,
        periodSeconds: settings.healthPeriod,
        timeoutSeconds: settings.healthTimeout,
        failureThreshold: settings.healthFailures,
        startupFailureThreshold: settings.healthStartupFailures,
      },
      command: wordsOf(settings.command),
      args: wordsOf(settings.args),
      // An emptied box sends an empty list rather than nothing: absent would
      // keep the override already there, and clearing the box has to be able
      // to take it away.
      previewArgs: wordsOf(settings.previewArgs),
      // The whole posture every time, so a switch turned off is a constraint
      // taken away: the route replaces it, and a posture of nothing but
      // defaults is stored as no posture at all.
      security: {
        runAsNonRoot: settings.runAsNonRoot,
        runAsUser: settings.runAsUser,
        runAsGroup: settings.runAsGroup,
        fsGroup: settings.fsGroup,
        // The policy applies only where there is a group to apply, and the
        // API refuses one without it — so an emptied group id takes the
        // policy with it rather than making the whole save fail.
        fsGroupChangePolicy: settings.fsGroup > 0 ? settings.fsGroupChangePolicy : "",
        readOnlyRootFilesystem: settings.readOnlyRootFilesystem,
        allowPrivilegeEscalation: settings.allowPrivilegeEscalation,
        dropCapabilities: wordsOf(settings.dropCapabilities),
      },
      singleton: settings.singleton,
      notRequestDriven: settings.notRequestDriven,
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

// What a claim asked its resource to be, as short badges — a database's
// major version, extensions and volume; a bucket's versioning, public reads
// and size. Most claims ask for none of it and get nothing here.
function claimRequirements(claim: Claim): string[] {
  // A volume's is the mount itself: which process, where, how big, and — once
  // the platform has looked — whether it attaches to one copy or many.
  if (claim.volume) {
    const volume = claim.volume;
    return [
      `${volume.process}:${volume.mountPath}`,
      volume.size,
      ...(volume.storageClass ? [volume.storageClass] : []),
      ...(volume.accessMode ? [volume.accessMode] : []),
    ];
  }
  if (claim.redis) {
    // What it is for leads, because it is the fact that decides whether the
    // instance may drop what is in it.
    return [
      claim.redis.usage ?? "cache",
      ...(claim.redis.maxMemory ? [claim.redis.maxMemory] : []),
      ...(claim.redis.version ? [`valkey ${claim.redis.version}`] : []),
    ];
  }
  if (claim.inngest) {
    // What the worker connects as and where: the app ID is the thing the
    // application has to match, so it is the badge.
    return [`app ${claim.inngest.app}`, claim.inngest.environment];
  }
  const postgres = claim.postgres;
  if (postgres) {
    return [
      ...(postgres.version ? [`pg ${postgres.version}`] : []),
      ...(postgres.extensions ?? []),
      ...(postgres.storageSize ? [postgres.storageSize] : []),
      ...(postgres.storageClass ? [postgres.storageClass] : []),
    ];
  }
  const bucket = claim.objectStore;
  if (bucket) {
    return [
      ...(bucket.versioning ? ["versioned"] : []),
      ...(bucket.publicRead ? ["public read"] : []),
      ...(bucket.size ? [bucket.size] : []),
    ];
  }
  return [];
}

/** The claims whose provider holds the workload up — a connect worker's
 * outbound connection, say. Scale to zero is a project-level policy, so one
 * such claim keeps every environment of this project on its pods, and the
 * settings say so next to the switch that would otherwise decide it. */
const pinningClaims = computed(() => (data.value?.claims ?? []).filter((claim) => claim.keepsPodsRunning));

// The refusal a failed claim carries, which is the whole point of failing as a
// claim rather than as an application: the Ready condition's message names
// what could not be supplied and what is available instead.
function claimRefusal(claim: Claim): string {
  const ready = claim.conditions?.find((condition) => condition.type === "Ready");
  return ready?.message || "the platform could not provision this claim";
}

const refusedClaims = computed(() => (data.value?.claims ?? []).filter((claim) => claim.phase === "Failed"));

// Deleting a claim is honest about its blast radius: what happens to the
// data is the deletionPolicy's call, and the confirmation says which it is
// before asking for the click. An OAuth client is not data and has no policy
// — it always goes, which is the whole point of deleting the claim.
function claimDeletionOutcome(claim: Claim): string {
  if (claim.type === "oidcClient") {
    return "The OAuth client is deregistered: nothing can be signed in with it again.";
  }
  if (claim.type === "objectStore") {
    return claim.deletionPolicy === "Delete"
      ? "The bucket, its objects and its credential are being deleted at the store."
      : "The bucket and its objects are kept at the store; only the platform's binding is removed.";
  }
  if (claim.type === "redis") {
    return claim.deletionPolicy === "Delete"
      ? "The instance and everything in it are being destroyed."
      : "The instance is kept, with whatever is in it.";
  }
  if (claim.type === "volume") {
    return claim.deletionPolicy === "Delete"
      ? "The volume and the data on it are being deleted."
      : "The volume is kept; a claim of the same name binds to it again.";
  }
  return claim.deletionPolicy === "Delete"
    ? "The database and its data are being deprovisioned."
    : "The database is kept at the provider; only the platform's binding is removed.";
}

// The confirmation's own sentence, which says what the policy does to the data
// before asking for the click — for the kind of resource this claim is.
function claimDeletionWarning(claim: Claim): string {
  if (claim.type === "volume") {
    return claim.deletionPolicy === "Delete"
      ? "This claim's policy is Delete: the volume and ALL THE DATA ON IT are deleted. Preview volumes go too. There is no undo."
      : "This claim's policy is Retain: the volume and its data are kept, even if the project is later deleted, and a claim of the same name binds to it again. Preview volumes are removed, and the process that mounted it deploys without it until then.";
  }
  return claim.deletionPolicy === "Delete"
    ? `This claim's policy is Delete: the ${claim.type} database and ALL ITS DATA are destroyed at ${claim.connection}. Preview branches and the binding secrets go too. There is no undo.`
    : `This claim's policy is Retain: the ${claim.type} database and its data are kept at ${claim.connection}, but the platform forgets it — preview branches and the binding secrets are removed, and environments referencing it will fail to deploy until the variable is removed.`;
}

// Destroying the data is the admin's, not the developer's (#320): the API
// refuses a Delete-policy claim's deletion below admin, so the row does not
// offer a button the refusal is waiting behind — it says why instead, in the
// same words.
function claimDeleteRefusal(claim: Claim): string | undefined {
  if (!destroysData(claim)) return undefined;
  return destroysDataRefusal(caller.value, "deleting a claim that destroys what it provisioned");
}
const mayUnclaimData = computed(() => mayDestroyData(caller.value));

const claimToDelete = ref<Claim | null>(null);
// A claim whose policy is Delete takes its data with it, so the confirmation
// is the one project deletion uses: typing the name. A click can be a slip,
// the name cannot.
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

// Previews read best the way the mockup draws them: the pull request as the
// unit, its builds underneath. Flat keeps the one-row-per-environment view.
const previewLayout = ref<"pr" | "flat">("pr");
function previewBuilds(pullRequest: number | undefined) {
  if (!pullRequest) return [];
  return (data.value?.builds ?? []).filter((b) => b.git.pullRequest === pullRequest).slice(0, 5);
}

function host(url?: string): string {
  if (!url) return "";
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="project">
      <PageHeader
        :freshness="freshness"
        :title="project.name"
        :breadcrumb="[{ label: 'Overview', to: '/' }, { label: project.name }]"
      >
        <template #meta>
          <span v-if="project.repo">{{ project.repo }}</span>
          <span v-else-if="vendoredImage" class="font-mono">{{ vendoredImage.reference }}</span>
          <a
            v-if="production?.url"
            :href="production.url"
            target="_blank"
            rel="noopener"
            class="font-mono text-primary hover:underline"
            >{{ host(production.url) }}</a
          >
          <span v-if="framework && builtHere" class="inline-flex items-center gap-1">
            <UIcon name="i-lucide-sparkles" class="size-3" />{{ framework }}, detected
          </span>
          <span v-if="builtHere" class="font-mono">{{ project.productionBranch }}</span>
        </template>
        <template #actions>
          <UButton
            v-if="builtHere && mayBuild"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-rotate-cw"
            :loading="redeploying"
            @click="redeploy"
          >
            Redeploy
          </UButton>
          <!-- A project with no repository has no commit to rebuild, so the
               button that moves it is the other one: ask the registry what
               the tag it follows names now, and take it. -->
          <UButton
            v-if="vendoredImage && mayAcquire"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-package-search"
            :loading="acquiring"
            @click="acquire"
          >
            Check for a new digest
          </UButton>
          <UButton
            v-if="production?.url"
            :href="production.url"
            target="_blank"
            size="sm"
            icon="i-lucide-arrow-up-right"
            trailing
          >
            Visit site
          </UButton>
        </template>
      </PageHeader>

      <div v-if="production" class="rounded-md border border-default bg-muted px-5 py-4 grid gap-6 sm:grid-cols-4">
        <div>
          <p class="text-xs text-muted mb-1">Release</p>
          <RouterLink
            :to="{ name: 'environment', params: { name: production.name } }"
            class="font-mono text-sm text-highlighted hover:underline"
            >{{ production.release }}</RouterLink
          >
          <p class="text-xs text-dimmed mt-0.5">observed {{ production.observedRelease || "—" }}</p>
        </div>
        <div class="min-w-0">
          <p class="text-xs text-muted mb-1">Image</p>
          <p class="font-mono text-sm text-toned truncate" :title="latestBuild?.image">
            {{ shortImage(data?.releases.find((r) => r.name === production!.release)?.image) }}
          </p>
        </div>
        <div>
          <p class="text-xs text-muted mb-1">Phase</p>
          <PhaseBadge :phase="production.phase" />
        </div>
        <div>
          <p class="text-xs text-muted mb-1">Running since</p>
          <p class="text-sm text-toned">{{ timeAgo(production.createdAt) }}</p>
        </div>
      </div>

      <!-- Every environment the project runs, with the last day's traffic on
           it. Health first: a Live environment answering 5xx is not green. -->
      <div v-if="environmentCards.length" class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <EnvironmentCard v-for="environment in environmentCards" :key="environment.name" :environment="environment" />
      </div>

      <PipelinePanel
        v-if="showPipeline"
        :stages="project.promotionStages ?? []"
        :environments="data?.environments ?? []"
        :promotions="promotions"
      />

      <OperatorOnly>
        <ConditionsTable :conditions="project.conditions" />
      </OperatorOnly>

      <!-- Seven tabs do not fit across a phone, and a tab abbreviated to
           "Dep…" names nothing: the strip scrolls instead. -->
      <UTabs
        v-model="tab"
        :items="tabs"
        color="neutral"
        variant="link"
        size="sm"
        :content="false"
        :ui="{ list: 'overflow-x-auto', trigger: 'shrink-0' }"
      />

      <!-- Deployments: the release history, newest first, with one-click rollback. -->
      <div v-if="tab === 'deployments'" class="rounded-md border border-default overflow-x-auto">
        <table class="w-full min-w-[42rem] text-sm">
          <tbody>
            <tr v-if="!data?.releases.length">
              <td class="px-3 py-8 text-center text-muted">No releases yet — a successful build creates one.</td>
            </tr>
            <tr v-for="release in data?.releases" :key="release.name" class="border-b border-muted last:border-0">
              <td class="px-3 py-2 w-44">
                <span class="flex items-center gap-2.5">
                  <StatusDot :tone="releaseState(release).tone" />
                  <span class="font-mono text-highlighted">{{ release.name }}</span>
                </span>
              </td>
              <td class="px-3 py-2">
                <p
                  class="text-highlighted truncate max-w-2xl"
                  :title="buildOf(release)?.git.message || release.build"
                >
                  {{ buildOf(release)?.git.message || release.build }}
                </p>
                <p class="text-xs text-muted font-mono mt-0.5">
                  {{ shortSHA(buildOf(release)?.git.sha) }} · {{ buildOf(release)?.git.branch || "—" }} ·
                  {{ buildOf(release)?.git.author || "—" }}
                </p>
                <!-- Where it is actually serving, from the release's own
                     status. The badge beside it only ever speaks for
                     production; a preview parked on an older release shows up
                     nowhere else. -->
                <p v-if="release.environments?.length" class="text-xs text-muted mt-1 flex flex-wrap gap-x-1.5">
                  <span>Serving</span>
                  <RouterLink
                    v-for="name in release.environments"
                    :key="name"
                    :to="{ name: 'environment', params: { name } }"
                    class="font-mono text-toned hover:text-highlighted hover:underline"
                    >{{ name }}</RouterLink
                  >
                </p>
              </td>
              <td class="px-3 py-2 whitespace-nowrap">
                <UBadge
                  v-if="releaseState(release).label"
                  :color="releaseState(release).tone"
                  variant="soft"
                  size="sm"
                  :title="releaseMovedBy(release)"
                >
                  {{ releaseState(release).label }}
                </UBadge>
              </td>
              <td class="px-3 py-2 text-xs text-muted whitespace-nowrap">{{ timeAgo(release.createdAt) }}</td>
              <td class="px-3 py-2 text-right whitespace-nowrap">
                <UButton
                  v-if="mayDeploy && production && release.name !== currentRelease"
                  color="neutral"
                  variant="subtle"
                  size="xs"
                  @click="rollbackTarget = release"
                >
                  Roll back
                </UButton>
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Previews: the pull request as the unit, its builds underneath. -->
      <div v-else-if="tab === 'previews'" class="space-y-3">
        <div v-if="previews.length" class="flex justify-end">
          <UFieldGroup size="xs">
            <UButton
              :color="previewLayout === 'pr' ? 'primary' : 'neutral'"
              :variant="previewLayout === 'pr' ? 'soft' : 'subtle'"
              label="By PR"
              @click="previewLayout = 'pr'"
            />
            <UButton
              :color="previewLayout === 'flat' ? 'primary' : 'neutral'"
              :variant="previewLayout === 'flat' ? 'soft' : 'subtle'"
              label="Flat"
              @click="previewLayout = 'flat'"
            />
          </UFieldGroup>
        </div>
        <p v-if="!previews.length" class="text-sm text-muted py-6 text-center">
          No preview environments — they appear when a pull request opens{{ project.previews ? "" : " (previews are disabled for this project)" }}.
        </p>
        <div v-for="preview in previews" :key="preview.name" class="rounded-md border border-default bg-muted">
          <div class="px-4 py-3 flex items-center gap-4 flex-wrap">
            <span class="font-mono text-xs text-primary">#{{ preview.preview?.pullRequest ?? "—" }}</span>
            <RouterLink
              :to="{ name: 'environment', params: { name: preview.name } }"
              class="text-sm text-highlighted font-medium hover:underline"
              >{{ preview.name }}</RouterLink
            >
            <span class="font-mono text-xs text-muted">{{ preview.preview?.branch }}</span>
            <span class="flex-1" />
            <PhaseBadge :phase="preview.phase" />
            <a
              v-if="preview.url"
              :href="preview.url"
              target="_blank"
              rel="noopener"
              class="font-mono text-xs text-primary hover:underline"
              >{{ host(preview.url) }}</a
            >
          </div>
          <div
            v-if="previewLayout === 'pr' && previewBuilds(preview.preview?.pullRequest).length"
            class="overflow-x-auto border-t border-muted"
          >
            <table class="w-full min-w-[42rem] text-sm">
              <tbody>
                <tr
                  v-for="build in previewBuilds(preview.preview?.pullRequest)"
                  :key="build.name"
                  class="border-b border-muted last:border-0 hover:bg-elevated/40"
                >
                  <td class="pl-10 pr-3 py-2 w-32 font-mono text-xs text-toned">{{ shortSHA(build.git.sha) }}</td>
                  <td class="px-3 py-2">
                    <RouterLink
                      :to="{ name: 'build', params: { name: build.name } }"
                      class="block max-w-2xl truncate text-toned hover:text-highlighted hover:underline"
                      :title="build.git.message || build.name"
                      >{{ build.git.message || build.name }}</RouterLink
                    >
                  </td>
                  <td class="px-3 py-2"><PhaseBadge :phase="build.phase" /></td>
                  <td class="px-3 py-2 font-mono text-xs text-muted whitespace-nowrap">
                    {{ duration(build.startedAt, build.completedAt) }}
                  </td>
                  <td class="px-3 py-2 text-right text-xs text-muted whitespace-nowrap">
                    {{ timeAgo(build.createdAt) }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>
      </div>

      <!-- Builds: the project's build history. -->
      <div v-else-if="tab === 'builds'" class="rounded-md border border-default overflow-x-auto">
        <table class="w-full min-w-[42rem] text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-default bg-muted">
              <th class="px-3 py-2 font-medium w-24">Commit</th>
              <th class="px-3 py-2 font-medium">Message</th>
              <th class="px-3 py-2 font-medium">Status</th>
              <th class="px-3 py-2 font-medium">Duration</th>
              <th class="px-3 py-2 font-medium text-right">Created</th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!data?.builds.length">
              <td colspan="5" class="px-3 py-8 text-center text-muted">
                No builds yet — push to the repository, or hit Redeploy.
              </td>
            </tr>
            <!-- The whole row opens the build. Only the subject used to, which
                 in a row this wide is a link most clicks miss. -->
            <template v-for="build in data?.builds" :key="build.name">
              <tr
                class="border-b border-muted last:border-0 hover:bg-elevated/40 cursor-pointer"
                @click="openBuild(build.name, $event)"
              >
                <td class="px-3 py-2 w-24 font-mono text-xs text-toned align-top">{{ shortSHA(build.git.sha) }}</td>
                <td class="px-3 py-2">
                  <span class="flex items-center gap-1 max-w-2xl">
                    <RouterLink
                      :to="{ name: 'build', params: { name: build.name } }"
                      class="block min-w-0 truncate text-highlighted hover:underline"
                      :title="build.git.message || build.name"
                    >
                      {{ build.git.message || build.name }}
                    </RouterLink>
                    <!-- The rest of what the commit said, for a commit that
                         said more than its subject. -->
                    <CommitBodyToggle
                      v-if="build.git.body"
                      :open="expandedMessage === build.name"
                      @toggle="toggleMessage(build.name)"
                    />
                  </span>
                  <p class="text-xs text-muted font-mono mt-0.5">
                    {{ build.git.branch }}<span v-if="build.git.pullRequest"> · #{{ build.git.pullRequest }}</span>
                  </p>
                  <!-- Why it failed, so that two failed builds read as two
                       failures rather than as the same one twice. -->
                  <p v-if="buildFailureLine(build)" class="text-xs text-error mt-1 break-words">
                    {{ buildFailureLine(build) }}
                  </p>
                </td>
                <td class="px-3 py-2 align-top"><PhaseBadge :phase="build.phase" /></td>
                <td class="px-3 py-2 font-mono text-xs text-muted whitespace-nowrap align-top">
                  {{ duration(build.startedAt, build.completedAt) }}
                </td>
                <td class="px-3 py-2 text-right text-xs text-muted whitespace-nowrap align-top">
                  {{ timeAgo(build.createdAt) }}
                </td>
              </tr>
              <tr v-if="expandedMessage === build.name" class="border-b border-muted last:border-0">
                <td colspan="5" class="px-3 py-2 bg-elevated/30">
                  <CommitBody :body="build.git.body" />
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>

      <!-- Domains: custom hostnames attached to this project's environments. -->
      <div v-else-if="tab === 'domains'" class="rounded-md border border-default overflow-x-auto">
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
                <RouterLink
                  :to="{ name: 'environment', params: { name: domain.environment } }"
                  class="text-toned hover:underline"
                  >{{ domain.environment }}</RouterLink
                >
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
              <td class="px-3 py-2 text-right text-xs text-muted whitespace-nowrap">{{ timeAgo(domain.createdAt) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Resources: provisioned claims (databases, OIDC clients, …) bound to this project. -->
      <div v-else-if="tab === 'resources'" class="space-y-3">
        <div v-if="mayClaim" class="flex justify-end">
          <ClaimModal
            :project="project.name"
            :role="project.role"
            :processes="project.processes?.map((p) => p.name)"
            @saved="refresh"
          >
            <UButton icon="i-lucide-plus" size="sm">New claim</UButton>
          </ClaimModal>
        </div>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[42rem] text-sm">
            <tbody>
              <tr v-if="!data?.claims.length">
                <td class="px-3 py-8 text-center text-muted">
                  No resource claims — a claim asks for something the project needs (a database or a bucket from a
                  connection, single sign-on from the platform's own identity provider, or a persistent volume for
                  one process) and binds it into the project's environments — through a secret its env vars
                  reference, or as a mount.
                  connection, or single sign-on from the platform's own identity provider) and binds it into the
                  project's environment through a secret its env vars reference.
                  No resource claims — a claim asks for something the project needs (a database from a connection,
                  single sign-on from the platform's own identity provider, or durable background work from
                  Inngest) and binds it into the project's environment through a secret its env vars reference.
                </td>
              </tr>
              <tr v-for="claim in data?.claims" :key="claim.name" class="border-b border-muted last:border-0">
                <td class="px-3 py-2 text-highlighted font-medium">{{ claim.name }}</td>
                <td class="px-3 py-2">
                  <UBadge color="neutral" variant="subtle" size="sm" class="font-mono">{{ claim.type }}</UBadge>
                  <!-- What a preview gets, in the provider's own words on
                       hover: the fact a preview writing to production is
                       marked on, rather than implied by the absence of a
                       badge. -->
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
                  <UBadge v-if="claim.forcesRecreate" color="warning" variant="subtle" size="sm" class="ml-1">
                    downtime on deploy
                  </UBadge>
                  <!-- What the claim asked the database itself to be. Absent
                       on most claims, and shown where it is not, because an
                       extension is the thing a failed claim was refused for. -->
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
                  <!-- The two data facts, absences said out loud: the class the
                       claim was filed under, and what the provider says the
                       data derives from. -->
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
                </td>
                <td class="px-3 py-2 font-mono text-xs text-toned">
                  <template v-if="claim.connection">via {{ claim.connection }}</template>
                  <template v-else-if="claim.volume">
                    <span :title="claim.volume.accessModeReason">
                      {{ claim.volume.mountPath }} on {{ claim.volume.process }}
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
                <!-- What the binding is: the secret the env vars read, or
                     for a volume the claim its process mounts. -->
                <td
                  class="px-3 py-2 font-mono text-xs text-muted truncate max-w-48"
                  :title="claim.secret || claim.volume?.claimName"
                >
                  {{ claim.secret || claim.volume?.claimName || "not bound yet" }}
                </td>
                <td class="px-3 py-2 text-xs text-muted whitespace-nowrap">
                  <template v-if="claim.type === 'oidcClient'">on delete: deregister the client</template>
                  <template v-else>
                    on delete: {{ claim.deletionPolicy === "Delete" ? "delete data" : "retain data" }}
                  </template>
                </td>
                <td class="px-3 py-2 text-right text-xs text-muted whitespace-nowrap">{{ timeAgo(claim.createdAt) }}</td>
                <td class="px-3 py-2 text-right whitespace-nowrap">
                  <!-- Recovering the data to a moment in the past, where the
                       provider can do it. It is offered next to the claim
                       because that is where somebody is when they need it,
                       and what it opens says whether this claim's provider
                       can reach back at all. -->
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
              <!-- Why a claim was refused, in the provider's own words. A
                   claim that cannot be satisfied — an extension no image
                   ships, a version nothing publishes — fails here rather than
                   in the application three minutes later, and that is only
                   worth anything if the reason is where the claim is. -->
              <tr v-for="claim in refusedClaims" :key="`${claim.name}-why`" class="border-b border-muted last:border-0">
                <td colspan="9" class="px-3 py-2 text-xs text-error">
                  <span class="font-mono">{{ claim.name }}</span> — {{ claimRefusal(claim) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- Variables: read by anybody who can read the project, changed by a
           developer. It is a tab rather than a section of the settings form
           because the settings form is an admin's and this is not — a
           developer who is not an admin has this and nothing else here.

           The project's own secrets sit under them because they are the
           choice the variables' values used to be: a credential goes in a
           secret, and a variable points at it. -->
      <div v-else-if="tab === 'variables'" class="space-y-10">
        <EnvVarsPanel :project="project.name" :role="project.role" :env="project.env" @saved="refresh" />
        <ProjectSecretsPanel :project="project.name" :role="project.role" :env="project.env" @saved="refresh" />
      </div>

      <!-- People, and the CI keys underneath them: they are one list. A key
           is a non-human member of exactly one project, so its grant is in
           the members list above and its credential is issued and revoked
           below. -->
      <div v-else-if="tab === 'people'" class="space-y-10">
        <MembersPanel :project="project.name" :role="project.role" />
        <KeysPanel :project="project.name" :role="project.role" />
      </div>

      <!-- Settings: everything on the project a user may change after
           creating it, and the danger zone that removes it entirely. -->
      <div v-else-if="tab === 'settings'" class="space-y-6 max-w-3xl">
        <!-- Which of these fields the repository has taken over. Above the
             form rather than beside each field: the answer is one list, and
             the alternative was a badge on nine controls saying the same
             sentence nine times. -->
        <div v-if="repoConfig" class="rounded-md border border-info/40 bg-info/5 px-5 py-4">
          <div class="flex items-start gap-2">
            <UIcon name="i-lucide-file-code" class="size-4 text-info mt-0.5 shrink-0" />
            <div class="min-w-0 space-y-1">
              <p class="text-sm font-medium text-highlighted">
                <span class="font-mono">{{ repoConfig.path }}</span> decides some of this
              </p>
              <p class="text-xs text-toned">
                The repository declares
                {{ repoDeclares.length }} {{ repoDeclares.length === 1 ? "setting" : "settings" }} of its own, read
                at every commit. Changing one here holds until the next build, which sets it back to what the file
                says — change the file instead.
              </p>
              <p class="text-xs text-muted font-mono break-words">{{ repoDeclares.join(", ") }}</p>
            </div>
          </div>
        </div>

        <form class="space-y-6" @submit.prevent="saveSettings">
          <!-- What this project runs, when nothing here built it. It is read
               rather than edited: changing which image a project runs is a
               different question from changing a setting of it, and #308 is
               what makes a new version of it arrive at all. -->
          <div v-if="vendoredImage" class="rounded-md border border-default bg-muted p-5 space-y-4">
            <h2 class="text-sm font-medium text-highlighted">Image</h2>
            <p class="text-xs text-toned max-w-3xl">
              This project's web process runs an image this platform did not build. There is no repository, so
              there is nothing to build, nowhere to push and no pull request to preview — the workloads below
              may still declare images of their own, and they deploy and roll back with this one as one unit.
            </p>
            <p class="text-xs text-toned max-w-3xl">
              <template v-if="vendoredImage.digest">
                This image is pinned to a digest, so nothing moves it: the platform never asks the registry
                about it, and a new version arrives when somebody changes the digest here.
              </template>
              <template v-else>
                The platform asks the registry on an interval whether this tag still names the digest it
                acquired. A new one is a build with no builder — it resolves the digest, releases it and
                deploys it — and rolling it back is the ordinary rollback.
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
                {{ vendoredImage.connection ? `the ${vendoredImage.connection} connection` : "nothing: a public image" }}
              </dd>
            </dl>
          </div>

          <div v-if="builtHere" class="rounded-md border border-default bg-muted p-5 space-y-4">
            <h2 class="text-sm font-medium text-highlighted">Git</h2>
            <UFormField label="Production branch" help="Builds of this branch promote to production.">
              <UInput v-model="settings.productionBranch" class="w-full max-w-44 font-mono" />
            </UFormField>
            <USwitch
              v-model="settings.requirePullRequest"
              label="Require a reviewed pull request"
              description="Refuse to build a production-branch commit the git provider cannot say arrived through a pull request somebody other than its author approved. Preview builds are unaffected — they are what produces the thing being reviewed."
            />
            <USwitch v-model="settings.previews" label="Preview environments" description="Every pull request gets its own environment." />
            <USwitch
              v-model="settings.previewsProtected"
              :disabled="!settings.previews"
              label="Protect previews"
              description="Previews sit behind platform login instead of being public."
            />
          </div>

          <div v-if="builtHere" class="rounded-md border border-default bg-muted p-5 space-y-4">
            <h2 class="text-sm font-medium text-highlighted">Build</h2>
            <div class="grid gap-4 sm:grid-cols-3">
              <UFormField
                label="Strategy"
                :help="declaredInRepo('build.strategy') ? 'Set by the repository; this is overwritten at every build.' : undefined"
              >
                <USelect v-model="settings.buildStrategy" :items="strategyOptions" class="w-full" />
              </UFormField>
              <UFormField
                :label="'Dockerfile'"
                :help="declaredInRepo('build.dockerfilePath') ? 'Set by the repository; this is overwritten at every build.' : 'Relative to the root directory.'"
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
          </div>

          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <h2 class="text-sm font-medium text-highlighted">Data</h2>
            <UFormField
              label="Data classification"
              help="What class of data this project handles. Claims narrow it and never exceed it; a promotion into an environment rated below it is refused by policy. Changes are audit-logged with the previous value."
            >
              <USelect v-model="settings.dataClass" :items="dataClassOptions" class="w-full max-w-60" />
            </UFormField>
          </div>

          <!-- Continuity. The copy leads with what Kitchen does not do,
               because a form that asked "how critical is this?" without
               saying whose question it is would read as the platform having
               an opinion about the institution's functions. It does not. -->
          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <div>
              <h2 class="text-sm font-medium text-highlighted">Continuity</h2>
              <p class="text-xs text-muted mt-1 max-w-3xl">
                <span class="text-toned font-medium">Kitchen does not decide what is critical, and does not
                set these tolerances.</span>
                They are the institution's — a board's judgement about its own functions — and this is where
                that judgement is recorded. What the platform does with it: map the function onto everything
                serving it, and alert against the tolerance. Nothing here refuses a deployment, and an absent
                designation is never defaulted to anything.
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

          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <h2 class="text-sm font-medium text-highlighted">Runtime</h2>
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
                 about how much of it there should be, and they sit under the
                 replica count because both are that number's other half: one
                 says it must stay at one, the other that it must never reach
                 zero. Turning the first on takes the count to 1 here rather
                 than leaving the API to refuse the pair. -->
            <USwitch
              v-model="settings.singleton"
              label="Never run two at once"
              description="For an application that polls, schedules or ingests in the same process as the web server: a
                rolling deploy would overlap the two for a few seconds, and that loop would run twice against one
                store. Deploys stop the old copy before starting the new one, so there is a gap in serving, and
                the replica count is fixed at 1."
            />
            <USwitch
              v-model="settings.notRequestDriven"
              label="Does work nobody asked for"
              description="An idle environment stops doing everything, not only serving — so an application with a
                background loop, a poller or an ingest job goes quiet with a gap in its data that looks exactly like
                the upstream having been down. Turning this on keeps every environment of this project awake,
                previews included."
            />
            <!-- A claim's provider can say the same thing about its binding,
                 and then the switch above is not what decides: scale to zero
                 is a project-level policy, so the one claim costs idling for
                 every environment of the project. Said here, where somebody
                 would otherwise look for the setting that turns it back on. -->
            <UAlert
              v-if="pinningClaims.length"
              color="warning"
              variant="subtle"
              icon="i-lucide-triangle-alert"
              title="This project is not offered scale to zero"
              :description="`${pinningClaims.map((claim) => claim.name).join(', ')} ${pinningClaims.length === 1 ? 'holds' : 'hold'} a worker whose outbound connection never crosses the interceptor, so nothing can tell when an environment is idle. Every environment of this project keeps its pods, previews included — release the claim to idle them.`"
            />
          </div>

          <!-- The posture the containers run under. It applies to every
               workload of the project — the web process, its workers and its
               scheduled runs — because they are one image, and a posture
               describes the image rather than the command it is started
               with. -->
          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <div>
              <h2 class="text-sm font-medium text-highlighted">Security</h2>
              <p class="text-xs text-muted mt-1 max-w-3xl">
                What the containers are allowed to be, for every workload this project ships. Everything here off is
                the platform's default, which is deliberately not the tightest one available: the container runtime's
                own seccomp profile, and no privilege escalation. The rest is a tightening an application asks for,
                because an image that writes into its own filesystem or runs as root is ordinary and would break
                under a default that assumed otherwise. A workload that cannot start under what it asked for says so
                on its environment, naming the constraint. The volume group is the one that goes the other way: a
                workload running as anybody but root needs it to be able to write the volume it was given.
              </p>
            </div>
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

          <!-- How it starts. Exec form is a list of words, so each field is
               a list of lines: an argument with a space in it is ordinary,
               and one box split on spaces would quietly break it. -->
          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <div>
              <h2 class="text-sm font-medium text-highlighted">How it starts</h2>
              <p class="text-xs text-muted mt-1 max-w-3xl">
                What the image is started with, when its own entrypoint is not what this project wants.
                <span class="text-toned font-medium">One word per line</span> — this is never a shell line, so
                nothing is split, quoted or expanded. Preview arguments are the sibling of a variable's preview
                value: the same commit and the same artifact, pointed somewhere else. A release carries all
                three, so a rollback restores the arguments it ran with.
              </p>
            </div>
            <div class="grid gap-4 sm:grid-cols-3">
              <UFormField label="Command" help="Replaces the entrypoint. Empty keeps the image's own.">
                <UTextarea v-model="settings.command" :rows="3" placeholder="./server" class="w-full font-mono" />
              </UFormField>
              <UFormField label="Arguments" help="Passed to it. Empty passes none.">
                <UTextarea v-model="settings.args" :rows="3" placeholder="--config=prod.toml" class="w-full font-mono" />
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
               the one whose consequence is least obvious: the platform still
               checks, it just cannot ask the application anything. It is a
               developer screen, so it says "replica" and never the word the
               cluster uses for one. -->
          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <div>
              <h2 class="text-sm font-medium text-highlighted">Health</h2>
              <p class="text-xs text-muted mt-1 max-w-3xl">
                What the platform asks a new replica before it sends anyone to it — on every deploy and every
                rollback.
                <span class="text-toned font-medium">With no path it opens a TCP connection to the port and no
                more</span>, which is a weaker claim than an answer and a much better one than assuming. A path
                is where the application says what working means for it, and it is also what buys a restart when
                a running container wedges. Previews inherit this, and a release carries it, so a rollback
                restores the check it ran with.
              </p>
            </div>
            <div class="grid gap-4 sm:grid-cols-3">
              <UFormField
                label="Path"
                help="An HTTP GET answering 2xx or 3xx. Empty is a TCP connect — never GET /."
              >
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
              <UFormField label="Failures" help="Checks in a row before a running pod is taken out.">
                <UInput v-model.number="settings.healthFailures" type="number" min="0" class="w-full font-mono" />
              </UFormField>
              <UFormField label="Startup failures" help="The generous one: how long a pod has to come up at all.">
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

        <!-- What the project runs besides its web process. It is its own
             section with its own Save because it is a list of records rather
             than a field of the form above — and it is the whole of how a
             project with no repository describes its unit, which is why it is
             here rather than read-only somewhere (#310). -->
        <ProjectWorkloadsPanel
          :project="project.name"
          :role="project.role"
          :processes="project.processes"
          :built-here="builtHere"
          :declared-in="declaredInRepo('processes') ? repoConfig?.path : undefined"
          @saved="refresh"
        />

        <!-- The configuration files this project places. Its own section with
             its own save, because it is its own concern and its own write: a
             file is not a setting on the form above, and a secret file's
             content goes to a route of its own. -->
        <ProjectFilesPanel
          :project="project.name"
          :role="project.role"
          :files="project.files"
          :processes="project.processes"
          :declared-in="repoConfig?.path"
          :declared-names="repoFiles"
          @saved="refresh"
        />

        <!-- Deleting is the admin's alone, and it is on the admin's own tab —
             but it is named separately from the settings above it, because
             `PATCH` and `DELETE` are two rows of the table and only one of
             them takes the project away. -->
        <div v-if="mayDelete" class="rounded-md border border-error/40 p-5 space-y-3">
          <h2 class="text-sm font-medium text-error">Danger zone</h2>
          <p class="text-xs text-muted">
            Deleting the project tears down its environments — production included — and removes its builds, releases
            and domains. There is no undo.
          </p>
          <div class="flex items-center gap-2">
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
      </div>

      <!-- Environments: production and previews alike, with their URLs. -->
      <div v-else class="rounded-md border border-default overflow-x-auto">
        <table class="w-full min-w-[42rem] text-sm">
          <tbody>
            <tr v-if="!data?.environments.length">
              <td class="px-3 py-8 text-center text-muted">
                No environments yet — the first production build creates one.
              </td>
            </tr>
            <tr
              v-for="environment in data?.environments"
              :key="environment.name"
              class="border-b border-muted last:border-0 hover:bg-elevated/40"
            >
              <td class="px-3 py-2">
                <RouterLink
                  :to="{ name: 'environment', params: { name: environment.name } }"
                  class="text-highlighted font-medium hover:underline"
                  >{{ environment.name }}</RouterLink
                >
              </td>
              <td class="px-3 py-2"><UBadge color="neutral" variant="subtle" size="sm">{{ environment.type }}</UBadge></td>
              <td class="px-3 py-2"><PhaseBadge :phase="environment.phase" /></td>
              <td class="px-3 py-2 font-mono text-xs text-toned">{{ environment.release }}</td>
              <td class="px-3 py-2 text-right">
                <a
                  v-if="environment.url"
                  :href="environment.url"
                  target="_blank"
                  rel="noopener"
                  class="font-mono text-xs text-primary hover:underline"
                  >{{ host(environment.url) }}</a
                >
                <span v-else class="text-dimmed text-xs">no URL</span>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>

    <!-- Rollback confirmation -->
    <UModal
      :open="rollbackTarget !== null"
      :title="`Roll ${production?.name} back?`"
      :description="`Production moves to ${rollbackTarget?.name}. Releases are immutable snapshots, so this puts back exactly what was running — config included.`"
      @update:open="(open: boolean) => { if (!open) rollbackTarget = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="rollbackTarget = null">Cancel</UButton>
          <UButton color="error" :loading="rollingBack" icon="i-lucide-undo-2" @click="rollBack">
            Roll back to {{ rollbackTarget?.name }}
          </UButton>
        </div>
      </template>
    </UModal>

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
