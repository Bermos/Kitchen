<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api, type Environment } from "../lib/api";
import { duration, shortSHA, timeAgo } from "../lib/format";
import { useFreshness } from "../lib/freshness";
import { buildLink, environmentLink } from "../lib/links";
import { autoRollbackFor, host } from "../lib/project";
import { useAsync, usePoll } from "../lib/useAsync";
import EnvironmentCard from "../components/EnvironmentCard.vue";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import SourceLink from "../components/SourceLink.vue";

// Where this project is published: production, and one environment per open
// pull request.
//
// It was two tabs of the project page — Environments and Previews — over one
// list, which meant the ceiling that refused a preview was on one of them and
// the environment that never appeared because of it was on the other. One
// screen, and `EnvironmentView` is the detail behind every row (#470).
//
// **The auto-rollback column does not mean what its name sounds like.** There
// is no per-environment automatic rollback; the field is a property of a
// break-glass compliance grant, and what it says is *"an expired exception
// covering this environment will roll it back"*. `lib/project.ts` holds that
// reading and the reason it is worded so carefully: the other sentence would
// be a promise the platform does not make.

const route = useRoute();
const name = computed(() => route.params.name as string);

const { data, error, loading, refresh } = useAsync(async () => {
  const [project, environments, builds, exceptions] = await Promise.all([
    api.project(name.value),
    api.projectEnvironments(name.value),
    api.projectBuilds(name.value),
    api.exceptions({ project: name.value }),
  ]);
  return { project, environments, builds, exceptions };
});
watch(name, () => void refresh());
// How old this screen is, and the reader's hold on it: every fetch above
// reports into it and the header renders it.
const freshness = useFreshness();
usePoll(() => void refresh(), 15000, () => true);

const project = computed(() => data.value?.project);
const environments = computed(() => data.value?.environments ?? []);
const production = computed(() =>
  environments.value.find((environment) => environment.name === project.value?.productionEnvironment),
);
const previews = computed(() => environments.value.filter((environment) => environment.type === "preview"));

// The mini-cards, production first: previews and production at a glance, with
// what each one served, how much of it failed and how slow it was — so nobody
// has to open five environments to find the one that is unwell.
const cards = computed(() => {
  const first = production.value;
  return first ? [first, ...environments.value.filter((e) => e.name !== first.name)] : environments.value;
});

function autoRollback(environment: Environment) {
  return autoRollbackFor(environment, data.value?.exceptions ?? []);
}

/** The pull requests currently refused a preview. Empty is the ordinary case
 * and shows nothing. */
const refusedPreviews = computed(() => project.value?.previewCapacity?.refused ?? []);

/** The ceiling in a sentence: what is live against it, and what is waiting on
 *  it. It is on the screen because a preview that never appeared is otherwise
 *  only visible on the pull request that asked for one. */
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

// Previews read best the way the mockup draws them: the pull request as the
// unit, its builds underneath. Flat keeps the one-row-per-environment view.
const previewLayout = ref<"pr" | "flat">("pr");
function previewBuilds(pullRequest: number | undefined) {
  if (!pullRequest) return [];
  return (data.value?.builds ?? []).filter((build) => build.git.pullRequest === pullRequest).slice(0, 5);
}
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="project">
      <PageHeader
        :freshness="freshness"
        title="Environments"
        :breadcrumb="[
          { label: 'Projects', to: '/projects' },
          { label: project.name, mono: true },
          { label: 'Environments' },
        ]"
      >
        <template #description>
          Where this project is published: production, and one environment per open pull request.
        </template>
        <template #actions>
          <UButton
            icon="i-lucide-refresh-cw"
            color="neutral"
            variant="ghost"
            size="sm"
            :loading="loading"
            aria-label="Refresh"
            @click="refresh"
          />
        </template>
      </PageHeader>

      <!-- Every environment with the last day's traffic on it. Health first: a
           Live environment answering 5xx is not green. -->
      <div v-if="cards.length" class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <EnvironmentCard v-for="environment in cards" :key="environment.name" :environment="environment" />
      </div>

      <PageSection
        title="All environments"
        description="What each one is running, and whether a break-glass grant covering it would take it back."
      >
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[48rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-3 py-2 font-medium">Environment</th>
                <th class="px-3 py-2 font-medium">Type</th>
                <th class="px-3 py-2 font-medium">Phase</th>
                <th class="px-3 py-2 font-medium">Release</th>
                <th class="px-3 py-2 font-medium">Auto-rollback</th>
                <th class="px-3 py-2 font-medium text-right">Address</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!environments.length">
                <td colspan="6" class="px-3 py-8 text-center text-muted">
                  Nothing published yet — the first production build creates an environment.
                </td>
              </tr>
              <tr
                v-for="environment in environments"
                :key="environment.name"
                class="border-b border-muted last:border-0 hover:bg-elevated/40"
              >
                <td class="px-3 py-2">
                  <RouterLink
                    :to="environmentLink(environment.name, name)"
                    class="text-highlighted font-medium hover:underline"
                    >{{ environment.name }}</RouterLink
                  >
                </td>
                <td class="px-3 py-2">
                  <UBadge color="neutral" variant="subtle" size="sm">{{ environment.type }}</UBadge>
                </td>
                <td class="px-3 py-2"><PhaseBadge :phase="environment.phase" /></td>
                <td class="px-3 py-2 font-mono text-xs text-toned">{{ environment.release || "—" }}</td>
                <!-- Not "this environment rolls back on a bad deploy": the
                     platform makes no such promise. What the column says is
                     whether an expired grant covering this environment would
                     take it back, which is the only automatic rollback there
                     is. The sentence behind it is on hover. -->
                <td class="px-3 py-2 text-xs">
                  <span
                    :class="autoRollback(environment).tone === 'warning' ? 'text-warning' : 'text-muted'"
                    :title="autoRollback(environment).detail"
                  >
                    {{ autoRollback(environment).label }}
                  </span>
                </td>
                <td class="px-3 py-2 text-right">
                  <a
                    v-if="environment.url"
                    :href="environment.url"
                    target="_blank"
                    rel="noopener"
                    class="font-mono text-xs text-primary hover:underline"
                    >{{ host(environment.url) }}</a
                  >
                  <!-- "internal" and "not published" are two different states:
                       one is the project's own setting, the other an
                       environment still waiting on a route. -->
                  <span
                    v-else-if="environment.exposure === 'internal'"
                    class="text-muted text-xs"
                    title="This project is internal: no hostname and no certificate. The environment is reachable from the other applications on this platform."
                    >internal</span
                  >
                  <span v-else class="text-dimmed text-xs">not published</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </PageSection>

      <PageSection title="Previews" description="One environment per open pull request, with what has been built on it.">
        <template #actions>
          <UFieldGroup v-if="previews.length" size="xs">
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
        </template>

        <!-- A pull request that was refused a preview has no environment to
             appear in the list below, so the ceiling says so here. Without it
             the only account of a missing preview is on the request. -->
        <UAlert
          v-if="refusedPreviews.length"
          class="mb-3"
          color="warning"
          variant="subtle"
          icon="i-lucide-gauge"
          title="At the preview ceiling"
          :description="previewCeilingLine"
        />

        <p v-if="!previews.length" class="text-sm text-muted py-6 text-center">
          No preview environments — they appear when a pull request opens{{
            project.previews ? "" : " (previews are disabled for this project)"
          }}.
        </p>

        <div class="space-y-3">
          <div v-for="preview in previews" :key="preview.name" class="rounded-md border border-default bg-muted">
            <div class="px-4 py-3 flex items-center gap-4 flex-wrap">
              <!-- The preview's own pull request, which is the link this screen
                   exists to have: the platform is the only thing that knows the
                   number, and reaching the discussion meant copying it out. -->
              <SourceLink class="font-mono text-xs" :href="preview.preview?.pullRequestUrl"
                >#{{ preview.preview?.pullRequest ?? "—" }}</SourceLink
              >
              <RouterLink
                :to="environmentLink(preview.name, name)"
                class="text-sm text-highlighted font-medium hover:underline"
                >{{ preview.name }}</RouterLink
              >
              <SourceLink class="font-mono text-xs text-muted" :href="preview.preview?.branchUrl">{{
                preview.preview?.branch
              }}</SourceLink>
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
              <span v-else-if="preview.exposure === 'internal'" class="text-muted text-xs">internal</span>
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
                    <td class="px-3 py-2 w-32 font-mono text-xs text-toned">
                      <SourceLink :href="build.git.commitUrl">{{ shortSHA(build.git.sha) }}</SourceLink>
                    </td>
                    <td class="px-3 py-2">
                      <RouterLink
                        :to="buildLink(build.name, name)"
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
      </PageSection>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>
  </div>
</template>
