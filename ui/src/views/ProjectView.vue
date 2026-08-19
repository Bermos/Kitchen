<script setup lang="ts">
import { computed, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, type Claim, type Project, type Release } from "../lib/api";
import { type EnvVarDraft, envVarDrafts, envVarWrites, newEnvVarDraft, renamed } from "../lib/envvars";
import { duration, shortImage, shortSHA, timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { operatorMode } from "../lib/mode";
import { may } from "../lib/policy";
import { releaseHistoryEntry, releaseHistoryLabel } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import ClaimModal from "../components/ClaimModal.vue";
import ConditionsTable from "../components/ConditionsTable.vue";
import EnvironmentCard from "../components/EnvironmentCard.vue";
import MembersPanel from "../components/MembersPanel.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import StatusDot from "../components/StatusDot.vue";

const route = useRoute();
const router = useRouter();
const toast = useToast();
const name = computed(() => route.params.name as string);

const { data, error, loading, refresh } = useAsync(async () => {
  const [project, environments, releases, builds, claims, allDomains] = await Promise.all([
    api.project(name.value),
    api.projectEnvironments(name.value),
    api.projectReleases(name.value),
    api.projectBuilds(name.value),
    api.claims({ project: name.value }),
    api.domains(),
  ]);
  // Domains attach to environments; the project's are the ones pointing at
  // one of its environments.
  const environmentNames = new Set(environments.map((e) => e.name));
  const domains = allDomains.filter((d) => environmentNames.has(d.environment));
  return { project, environments, releases, builds, claims, domains };
});
watch(name, () => void refresh());
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
const mayDeploy = computed(() => may("PATCH /api/v1/environments/{name}", caller.value));
const mayClaim = computed(() => may("POST /api/v1/claims", caller.value));
const mayUnclaim = computed(() => may("DELETE /api/v1/claims/{name}", caller.value));
const mayConfigure = computed(() => may("PATCH /api/v1/projects/{name}", caller.value));
const mayDelete = computed(() => may("DELETE /api/v1/projects/{name}", caller.value));
// Membership is an admin's read as well as an admin's write: the API refuses
// the listing to anybody else, so the tab is theirs rather than everyone's
// with the buttons taken out.
const mayReadMembers = computed(() => may("GET /api/v1/projects/{name}/members", caller.value));

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
const framework = computed(() => data.value?.builds.find((b) => b.detectedFramework)?.detectedFramework);

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
    await api.moveEnvironment(environment.name, target.name);
    toast.add({ title: `${environment.name} moved to ${target.name}`, color: "success", icon: "i-lucide-undo-2" });
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
// Settings and People are the project admin's whole tab, not a tab with the
// controls removed: everything on either is a write, or a read the API
// refuses. Everything else is every role's.
const tabs = computed(() =>
  [
    { label: `Deployments`, value: "deployments", shown: true },
    { label: `Previews (${previews.value.length})`, value: "previews", shown: true },
    { label: `Builds (${data.value?.builds.length ?? 0})`, value: "builds", shown: true },
    { label: `Environments (${data.value?.environments.length ?? 0})`, value: "environments", shown: true },
    { label: `Domains (${data.value?.domains.length ?? 0})`, value: "domains", shown: true },
    { label: `Resources (${data.value?.claims.length ?? 0})`, value: "resources", shown: true },
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
// the 10s poll never types over the user. Env vars ride along by name — the
// PATCH replaces the whole list, so every variable has to be in the copy —
// but never by value: the API reports only that a variable has one, and the
// PATCH keeps the stored value of every variable it is not given one for.
const settings = reactive({
  loadedFor: "",
  productionBranch: "",
  previews: true,
  previewsProtected: true,
  buildStrategy: "auto",
  dockerfilePath: "",
  rootDirectory: "",
  // 0 is "let the platform decide": the port then comes from the framework
  // each build detects, and the field shows what that would be.
  port: 0,
  replicas: 1,
  cpu: "",
  memory: "",
  env: [] as EnvVarDraft[],
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

const strategyOptions = [
  { label: "auto — detect the framework", value: "auto" },
  { label: "dockerfile", value: "dockerfile" },
  { label: "buildpacks", value: "buildpacks" },
];
function loadSettings(from: Project) {
  settings.loadedFor = from.name;
  settings.productionBranch = from.productionBranch;
  settings.previews = from.previews;
  settings.previewsProtected = from.previewsProtected;
  settings.buildStrategy = from.buildStrategy || "auto";
  settings.dockerfilePath = from.dockerfilePath ?? "";
  settings.rootDirectory = from.rootDirectory ?? "";
  settings.port = from.port ?? 0;
  settings.replicas = from.replicas ?? 1;
  settings.cpu = from.cpu ?? "";
  settings.memory = from.memory ?? "";
  settings.env = envVarDrafts(from.env);
}
watch(project, (value) => {
  if (value && value.name !== settings.loadedFor) loadSettings(value);
});

function addEnvVar() {
  settings.env.push(newEnvVarDraft());
}
function removeEnvVar(index: number) {
  settings.env.splice(index, 1);
}
// Replacing a value is a deliberate act: the field opens empty, because there
// is nothing to prefill it with. "Keep" closes it again and the stored value
// stays — the same undo the connection modal's blank credential field is.
function replaceValue(envVar: EnvVarDraft) {
  envVar.value = "";
}
function keepValue(envVar: EnvVarDraft) {
  envVar.value = undefined;
}
function replacePreviewValue(envVar: EnvVarDraft) {
  envVar.previewValue = "";
}
function keepPreviewValue(envVar: EnvVarDraft) {
  envVar.previewValue = undefined;
}

const savingSettings = ref(false);
async function saveSettings() {
  savingSettings.value = true;
  try {
    const saved = await api.updateProject(name.value, {
      productionBranch: settings.productionBranch,
      previews: settings.previews,
      previewsProtected: settings.previewsProtected,
      buildStrategy: settings.buildStrategy,
      dockerfilePath: settings.dockerfilePath,
      rootDirectory: settings.rootDirectory,
      port: settings.port,
      replicas: settings.replicas,
      cpu: settings.cpu,
      memory: settings.memory,
      env: envVarWrites(settings.env),
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
      description: "Environments, builds, releases and the project namespace are being torn down.",
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

// Deleting a claim is honest about its blast radius: what happens to the
// data is the deletionPolicy's call, and the confirmation says which it is
// before asking for the click.
const claimToDelete = ref<Claim | null>(null);
const deletingClaim = ref(false);
async function deleteClaim() {
  const claim = claimToDelete.value;
  if (!claim || deletingClaim.value) return;
  deletingClaim.value = true;
  try {
    await api.deleteClaim(claim.name);
    toast.add({
      title: `Claim ${claim.name} is being deleted`,
      description:
        claim.deletionPolicy === "Delete"
          ? "The database and its data are being deprovisioned."
          : "The database is kept at the provider; only the platform's binding is removed.",
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
      <div class="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <div class="flex items-center gap-2 text-xs text-muted mb-1">
            <RouterLink to="/" class="hover:text-highlighted">Overview</RouterLink>
            <span>/</span>
            <span class="text-toned">{{ project.name }}</span>
          </div>
          <h1 class="text-xl font-semibold text-highlighted">{{ project.name }}</h1>
          <div class="flex items-center gap-3 mt-1 text-xs text-muted flex-wrap">
            <span>{{ project.repo }}</span>
            <a
              v-if="production?.url"
              :href="production.url"
              target="_blank"
              rel="noopener"
              class="font-mono text-primary hover:underline"
              >{{ host(production.url) }}</a
            >
            <span v-if="framework" class="inline-flex items-center gap-1">
              <UIcon name="i-lucide-sparkles" class="size-3" />{{ framework }}, detected
            </span>
            <span class="font-mono">{{ project.productionBranch }}</span>
          </div>
        </div>
        <div class="flex items-center gap-2">
          <UButton
            v-if="mayBuild"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-rotate-cw"
            :loading="redeploying"
            @click="redeploy"
          >
            Redeploy
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
        </div>
      </div>

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

      <ConditionsTable v-if="operatorMode" :conditions="project.conditions" />

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
              <td class="px-4 py-8 text-center text-muted">No releases yet — a successful build creates one.</td>
            </tr>
            <tr v-for="release in data?.releases" :key="release.name" class="border-b border-muted last:border-0">
              <td class="px-4 py-3 w-44">
                <span class="flex items-center gap-2.5">
                  <StatusDot :tone="releaseState(release).tone" />
                  <span class="font-mono text-highlighted">{{ release.name }}</span>
                </span>
              </td>
              <td class="px-4 py-3">
                <p class="text-highlighted truncate max-w-md">{{ buildOf(release)?.git.message || release.build }}</p>
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
              <td class="px-4 py-3 whitespace-nowrap">
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
              <td class="px-4 py-3 text-xs text-muted whitespace-nowrap">{{ timeAgo(release.createdAt) }}</td>
              <td class="px-4 py-3 text-right whitespace-nowrap">
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
                  <td class="pl-10 pr-4 py-2 w-32 font-mono text-xs text-toned">{{ shortSHA(build.git.sha) }}</td>
                  <td class="px-4 py-2">
                    <RouterLink
                      :to="{ name: 'build', params: { name: build.name } }"
                      class="text-toned hover:text-highlighted hover:underline"
                      >{{ build.git.message || build.name }}</RouterLink
                    >
                  </td>
                  <td class="px-4 py-2"><PhaseBadge :phase="build.phase" /></td>
                  <td class="px-4 py-2 font-mono text-xs text-muted whitespace-nowrap">
                    {{ duration(build.startedAt, build.completedAt) }}
                  </td>
                  <td class="px-4 py-2 text-right text-xs text-muted whitespace-nowrap">
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
          <tbody>
            <tr v-if="!data?.builds.length">
              <td class="px-4 py-8 text-center text-muted">No builds yet — push to the repository, or hit Redeploy.</td>
            </tr>
            <tr
              v-for="build in data?.builds"
              :key="build.name"
              class="border-b border-muted last:border-0 hover:bg-elevated/40"
            >
              <td class="px-4 py-3 w-24 font-mono text-xs text-toned">{{ shortSHA(build.git.sha) }}</td>
              <td class="px-4 py-3">
                <RouterLink :to="{ name: 'build', params: { name: build.name } }" class="text-highlighted hover:underline">
                  {{ build.git.message || build.name }}
                </RouterLink>
                <p class="text-xs text-muted font-mono mt-0.5">
                  {{ build.git.branch }}<span v-if="build.git.pullRequest"> · #{{ build.git.pullRequest }}</span>
                </p>
              </td>
              <td class="px-4 py-3"><PhaseBadge :phase="build.phase" /></td>
              <td class="px-4 py-3 font-mono text-xs text-muted whitespace-nowrap">
                {{ duration(build.startedAt, build.completedAt) }}
              </td>
              <td class="px-4 py-3 text-right text-xs text-muted whitespace-nowrap">{{ timeAgo(build.createdAt) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Domains: custom hostnames attached to this project's environments. -->
      <div v-else-if="tab === 'domains'" class="rounded-md border border-default overflow-x-auto">
        <table class="w-full min-w-[42rem] text-sm">
          <tbody>
            <tr v-if="!data?.domains.length">
              <td class="px-4 py-8 text-center text-muted">
                No custom domains — they are attached with kubectl until the create flow lands here.
              </td>
            </tr>
            <tr v-for="domain in data?.domains" :key="domain.name" class="border-b border-muted last:border-0">
              <td class="px-4 py-3">
                <a
                  :href="`https://${domain.hostname}`"
                  target="_blank"
                  rel="noopener"
                  class="font-mono text-highlighted hover:underline"
                  >{{ domain.hostname }}</a
                >
              </td>
              <td class="px-4 py-3">
                <RouterLink
                  :to="{ name: 'environment', params: { name: domain.environment } }"
                  class="text-toned hover:underline"
                  >{{ domain.environment }}</RouterLink
                >
              </td>
              <td class="px-4 py-3">
                <UBadge v-if="domain.tls" color="neutral" variant="subtle" size="sm" class="font-mono">
                  tls: {{ domain.tls }}
                </UBadge>
              </td>
              <td class="px-4 py-3">
                <UBadge :color="domain.verified ? 'success' : 'warning'" variant="soft" size="sm">
                  {{ domain.verified ? "Verified" : "Awaiting DNS" }}
                </UBadge>
              </td>
              <td class="px-4 py-3 text-right text-xs text-muted whitespace-nowrap">{{ timeAgo(domain.createdAt) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Resources: provisioned claims (databases, OIDC clients, …) bound to this project. -->
      <div v-else-if="tab === 'resources'" class="space-y-3">
        <div v-if="mayClaim" class="flex justify-end">
          <ClaimModal :project="project.name" @saved="refresh">
            <UButton icon="i-lucide-plus" size="sm">New claim</UButton>
          </ClaimModal>
        </div>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[42rem] text-sm">
            <tbody>
              <tr v-if="!data?.claims.length">
                <td class="px-4 py-8 text-center text-muted">
                  No resource claims — a claim asks a connection to provision something (a database) and binds it into
                  the project's environment through a secret its env vars reference.
                </td>
              </tr>
              <tr v-for="claim in data?.claims" :key="claim.name" class="border-b border-muted last:border-0">
                <td class="px-4 py-3 text-highlighted font-medium">{{ claim.name }}</td>
                <td class="px-4 py-3">
                  <UBadge color="neutral" variant="subtle" size="sm" class="font-mono">{{ claim.type }}</UBadge>
                  <UBadge v-if="claim.previewBranching" color="neutral" variant="subtle" size="sm" class="ml-1">
                    branch per preview
                  </UBadge>
                </td>
                <td class="px-4 py-3 font-mono text-xs text-toned">via {{ claim.connection }}</td>
                <td class="px-4 py-3"><PhaseBadge :phase="claim.phase" /></td>
                <td class="px-4 py-3 font-mono text-xs text-muted truncate max-w-48" :title="claim.secret">
                  {{ claim.secret || "not bound yet" }}
                </td>
                <td class="px-4 py-3 text-xs text-muted whitespace-nowrap">
                  on delete: {{ claim.deletionPolicy === "Delete" ? "delete data" : "retain data" }}
                </td>
                <td class="px-4 py-3 text-right text-xs text-muted whitespace-nowrap">{{ timeAgo(claim.createdAt) }}</td>
                <td class="px-4 py-3 text-right whitespace-nowrap">
                  <UButton
                    v-if="mayUnclaim"
                    color="neutral"
                    variant="subtle"
                    size="xs"
                    icon="i-lucide-trash-2"
                    @click="claimToDelete = claim"
                  >
                    Delete
                  </UButton>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- People: who holds which role on this project. -->
      <MembersPanel v-else-if="tab === 'people'" :project="project.name" :role="project.role" />

      <!-- Settings: everything on the project a user may change after
           creating it, and the danger zone that removes it entirely. -->
      <div v-else-if="tab === 'settings'" class="space-y-6 max-w-2xl">
        <form class="space-y-6" @submit.prevent="saveSettings">
          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <h2 class="text-sm font-semibold text-highlighted">Git</h2>
            <UFormField label="Production branch" help="Builds of this branch promote to production.">
              <UInput v-model="settings.productionBranch" class="w-full max-w-44 font-mono" />
            </UFormField>
            <USwitch v-model="settings.previews" label="Preview environments" description="Every pull request gets its own environment." />
            <USwitch
              v-model="settings.previewsProtected"
              :disabled="!settings.previews"
              label="Protect previews"
              description="Previews sit behind platform login instead of being public."
            />
          </div>

          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <h2 class="text-sm font-semibold text-highlighted">Build</h2>
            <div class="grid gap-4 sm:grid-cols-3">
              <UFormField label="Strategy">
                <USelect v-model="settings.buildStrategy" :items="strategyOptions" class="w-full" />
              </UFormField>
              <UFormField label="Dockerfile" help="Relative to the root directory.">
                <UInput v-model="settings.dockerfilePath" placeholder="Dockerfile" class="w-full font-mono" />
              </UFormField>
              <UFormField label="Root directory" help="For monorepos.">
                <UInput v-model="settings.rootDirectory" placeholder="." class="w-full font-mono" />
              </UFormField>
            </div>
          </div>

          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <h2 class="text-sm font-semibold text-highlighted">Runtime</h2>
            <div class="grid gap-4 sm:grid-cols-4">
              <UFormField label="Port" :help="portHelp">
                <UInput v-model="portField" type="number" min="0" placeholder="auto" class="w-full font-mono" />
              </UFormField>
              <UFormField label="Replicas" help="Previews always run 1.">
                <UInput v-model.number="settings.replicas" type="number" min="1" class="w-full font-mono" />
              </UFormField>
              <UFormField label="CPU" help="Per replica, e.g. 250m.">
                <UInput v-model="settings.cpu" placeholder="unset" class="w-full font-mono" />
              </UFormField>
              <UFormField label="Memory" help="Per replica, e.g. 512Mi.">
                <UInput v-model="settings.memory" placeholder="unset" class="w-full font-mono" />
              </UFormField>
            </div>
          </div>

          <div class="rounded-md border border-default bg-muted p-5 space-y-4">
            <div class="flex items-center justify-between">
              <h2 class="text-sm font-semibold text-highlighted">Environment variables</h2>
              <UButton color="neutral" variant="subtle" size="xs" icon="i-lucide-plus" @click="addEnvVar">
                Add variable
              </UButton>
            </div>
            <p class="text-xs text-muted">
              Values are never read back — a variable that has one shows <span class="font-mono">•••• set</span>, and
              replacing it means typing the new one. They land in new releases: what is running keeps its release's
              snapshot until the next deploy.
            </p>
            <p v-if="!settings.env.length" class="text-xs text-muted">None yet.</p>
            <div v-for="(envVar, index) in settings.env" :key="index" class="flex items-start gap-2 flex-wrap sm:flex-nowrap">
              <UInput v-model="envVar.name" placeholder="NAME" class="w-full sm:w-44 font-mono" />
              <div v-if="!envVar.fromSecret && !envVar.fromClaim" class="flex-1 min-w-40 grid gap-2 sm:grid-cols-2">
                <!-- The value: shown as presence, replaced by typing. -->
                <div class="flex items-center gap-2 min-h-8">
                  <UInput
                    v-if="envVar.value !== undefined"
                    v-model="envVar.value"
                    :placeholder="envVar.set ? 'new value' : 'value'"
                    autocomplete="off"
                    class="flex-1 min-w-0 font-mono"
                  />
                  <UBadge
                    v-else
                    :color="envVar.set && renamed(envVar) ? 'warning' : 'neutral'"
                    variant="subtle"
                    size="sm"
                    class="font-mono"
                  >
                    {{ envVar.set ? (renamed(envVar) ? "renamed — set again" : "•••• set") : "no value" }}
                  </UBadge>
                  <UButton
                    v-if="envVar.value === undefined"
                    color="neutral"
                    variant="link"
                    size="xs"
                    class="px-0"
                    @click="replaceValue(envVar)"
                  >
                    {{ envVar.set && !renamed(envVar) ? "Replace" : "Set" }}
                  </UButton>
                  <UButton
                    v-else-if="envVar.set"
                    color="neutral"
                    variant="link"
                    size="xs"
                    class="px-0"
                    @click="keepValue(envVar)"
                  >
                    Keep
                  </UButton>
                </div>
                <!-- The preview value, on the same terms. -->
                <div class="flex items-center gap-2 min-h-8">
                  <UInput
                    v-if="envVar.previewValue !== undefined"
                    v-model="envVar.previewValue"
                    :placeholder="envVar.previewSet ? 'new preview value' : 'preview value (optional)'"
                    autocomplete="off"
                    class="flex-1 min-w-0 font-mono"
                  />
                  <UBadge
                    v-else
                    :color="envVar.previewSet && renamed(envVar) ? 'warning' : 'neutral'"
                    variant="subtle"
                    size="sm"
                    class="font-mono"
                  >
                    {{
                      envVar.previewSet
                        ? renamed(envVar)
                          ? "renamed — set again"
                          : "•••• preview set"
                        : "no preview value"
                    }}
                  </UBadge>
                  <UButton
                    v-if="envVar.previewValue === undefined"
                    color="neutral"
                    variant="link"
                    size="xs"
                    class="px-0"
                    @click="replacePreviewValue(envVar)"
                  >
                    {{ envVar.previewSet && !renamed(envVar) ? "Replace" : "Set" }}
                  </UButton>
                  <UButton
                    v-else-if="envVar.previewSet"
                    color="neutral"
                    variant="link"
                    size="xs"
                    class="px-0"
                    @click="keepPreviewValue(envVar)"
                  >
                    Keep
                  </UButton>
                </div>
              </div>
              <UBadge v-else color="neutral" variant="subtle" size="sm" class="font-mono mt-1.5 flex-1">
                {{ envVar.fromSecret ? `secret ${envVar.fromSecret.name}/${envVar.fromSecret.key}` : `claim ${envVar.fromClaim!.name}/${envVar.fromClaim!.key}` }}
              </UBadge>
              <UButton
                color="neutral"
                variant="ghost"
                size="xs"
                icon="i-lucide-x"
                aria-label="Remove variable"
                class="mt-1"
                @click="removeEnvVar(index)"
              />
            </div>
          </div>

          <div class="flex justify-end">
            <UButton type="submit" :loading="savingSettings" icon="i-lucide-check">Save settings</UButton>
          </div>
        </form>

        <!-- Deleting is the admin's alone, and it is on the admin's own tab —
             but it is named separately from the settings above it, because
             `PATCH` and `DELETE` are two rows of the table and only one of
             them takes the project away. -->
        <div v-if="mayDelete" class="rounded-md border border-error/40 p-5 space-y-3">
          <h2 class="text-sm font-semibold text-error">Danger zone</h2>
          <p class="text-xs text-muted">
            Deleting the project tears down its environments — production included — and removes its builds, releases,
            domains and namespace. There is no undo.
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
              <td class="px-4 py-8 text-center text-muted">
                No environments yet — the first production build creates one.
              </td>
            </tr>
            <tr
              v-for="environment in data?.environments"
              :key="environment.name"
              class="border-b border-muted last:border-0 hover:bg-elevated/40"
            >
              <td class="px-4 py-3">
                <RouterLink
                  :to="{ name: 'environment', params: { name: environment.name } }"
                  class="text-highlighted font-medium hover:underline"
                  >{{ environment.name }}</RouterLink
                >
              </td>
              <td class="px-4 py-3"><UBadge color="neutral" variant="subtle" size="sm">{{ environment.type }}</UBadge></td>
              <td class="px-4 py-3"><PhaseBadge :phase="environment.phase" /></td>
              <td class="px-4 py-3 font-mono text-xs text-toned">{{ environment.release }}</td>
              <td class="px-4 py-3 text-right">
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
         does to the data, which is the whole difference between the two. -->
    <UModal
      :open="claimToDelete !== null"
      :title="`Delete claim ${claimToDelete?.name}?`"
      :description="
        claimToDelete?.deletionPolicy === 'Delete'
          ? `This claim's policy is Delete: the ${claimToDelete?.type} database and ALL ITS DATA are destroyed at ${claimToDelete?.connection}. Preview branches and the binding secrets go too. There is no undo.`
          : `This claim's policy is Retain: the ${claimToDelete?.type} database and its data are kept at ${claimToDelete?.connection}, but the platform forgets it — preview branches and the binding secrets are removed, and environments referencing it will fail to deploy until the variable is removed.`
      "
      @update:open="(open: boolean) => { if (!open) claimToDelete = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="claimToDelete = null">Cancel</UButton>
          <UButton color="error" :loading="deletingClaim" icon="i-lucide-trash-2" @click="deleteClaim">
            {{ claimToDelete?.deletionPolicy === "Delete" ? "Delete claim and data" : "Delete claim, keep data" }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
