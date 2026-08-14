<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api, type Release } from "../lib/api";
import { duration, shortImage, shortSHA, timeAgo } from "../lib/format";
import { operatorMode } from "../lib/mode";
import { useAsync, usePoll } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import StatusDot from "../components/StatusDot.vue";

const route = useRoute();
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
const production = computed(() =>
  data.value?.environments.find((e) => e.name === data.value?.project.productionEnvironment),
);
const previews = computed(() => (data.value?.environments ?? []).filter((e) => e.type === "preview"));
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
  return { label: "", tone: "neutral" };
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
const tabs = computed(() => [
  { label: `Deployments`, value: "deployments" },
  { label: `Previews (${previews.value.length})`, value: "previews" },
  { label: `Builds (${data.value?.builds.length ?? 0})`, value: "builds" },
  { label: `Environments (${data.value?.environments.length ?? 0})`, value: "environments" },
  { label: `Domains (${data.value?.domains.length ?? 0})`, value: "domains" },
  { label: `Resources (${data.value?.claims.length ?? 0})`, value: "resources" },
]);

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
          <UButton color="neutral" variant="subtle" size="sm" icon="i-lucide-rotate-cw" :loading="redeploying" @click="redeploy">
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
        <div>
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

      <ConditionsTable v-if="operatorMode" :conditions="project.conditions" />

      <UTabs v-model="tab" :items="tabs" color="neutral" variant="link" size="sm" :content="false" />

      <!-- Deployments: the release history, newest first, with one-click rollback. -->
      <div v-if="tab === 'deployments'" class="rounded-md border border-default overflow-x-auto">
        <table class="w-full text-sm">
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
              </td>
              <td class="px-4 py-3 whitespace-nowrap">
                <UBadge v-if="releaseState(release).label" :color="releaseState(release).tone" variant="soft" size="sm">
                  {{ releaseState(release).label }}
                </UBadge>
              </td>
              <td class="px-4 py-3 text-xs text-muted whitespace-nowrap">{{ timeAgo(release.createdAt) }}</td>
              <td class="px-4 py-3 text-right whitespace-nowrap">
                <UButton
                  v-if="production && release.name !== currentRelease"
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
          <table
            v-if="previewLayout === 'pr' && previewBuilds(preview.preview?.pullRequest).length"
            class="w-full text-sm border-t border-muted"
          >
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

      <!-- Builds: the project's build history. -->
      <div v-else-if="tab === 'builds'" class="rounded-md border border-default overflow-x-auto">
        <table class="w-full text-sm">
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
        <table class="w-full text-sm">
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
      <div v-else-if="tab === 'resources'" class="rounded-md border border-default overflow-x-auto">
        <table class="w-full text-sm">
          <tbody>
            <tr v-if="!data?.claims.length">
              <td class="px-4 py-8 text-center text-muted">
                No resource claims — a claim asks a connection to provision something (a database, an OIDC client) and
                binds it into the project's environment. Claims are created with kubectl for now.
              </td>
            </tr>
            <tr v-for="claim in data?.claims" :key="claim.name" class="border-b border-muted last:border-0">
              <td class="px-4 py-3 text-highlighted font-medium">{{ claim.name }}</td>
              <td class="px-4 py-3">
                <UBadge color="neutral" variant="subtle" size="sm" class="font-mono">{{ claim.type }}</UBadge>
              </td>
              <td class="px-4 py-3 font-mono text-xs text-toned">via {{ claim.connection }}</td>
              <td class="px-4 py-3"><PhaseBadge :phase="claim.phase" /></td>
              <td class="px-4 py-3 font-mono text-xs text-muted truncate max-w-48" :title="claim.secret">
                {{ claim.secret || "not bound yet" }}
              </td>
              <td class="px-4 py-3 text-right text-xs text-muted whitespace-nowrap">{{ timeAgo(claim.createdAt) }}</td>
            </tr>
          </tbody>
        </table>
      </div>

      <!-- Environments: production and previews alike, with their URLs. -->
      <div v-else class="rounded-md border border-default overflow-x-auto">
        <table class="w-full text-sm">
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
  </div>
</template>
