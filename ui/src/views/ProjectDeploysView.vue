<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, type Build, type Release } from "../lib/api";
import { buildFailureLine, buildSkipLine, buildStallLine } from "../lib/builds";
import { duration, exactTime, shortImage, shortSHA, timeAgo } from "../lib/format";
import { useFreshness } from "../lib/freshness";
import { buildLink, environmentLink } from "../lib/links";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { pipelineShown, promotionTone } from "../lib/promotions";
import { artifactFor, deployEntries, deployedProcesses, WEB_PROCESS } from "../lib/project";
import { releaseHistoryEntry, releaseHistoryLabel } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import CommitBody from "../components/CommitBody.vue";
import CommitBodyToggle from "../components/CommitBodyToggle.vue";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import PipelinePanel from "../components/PipelinePanel.vue";
import RollbackPanel from "../components/RollbackPanel.vue";
import SourceLink from "../components/SourceLink.vue";
import StatusDot from "../components/StatusDot.vue";

// What shipped, in one order.
//
// Builds and promotions were two tabs of the project page, so reading them
// meant interleaving two lists by eye — the one thing a reader cannot do
// reliably and a sort can. A promotion is a decision about a build's release,
// so the two are one timeline here: the build that produced the release, and
// what the policy then allowed or refused to do with it (#470).
//
// **The process filter is over artifacts, not over builds.** A Build is per
// project and produces an artifact per workload; a process that declared no
// build of its own shares the web artifact rather than disappearing from the
// timeline. `lib/project.ts` carries that reading, because "no artifact of its
// own" and "not in this build" are the same shape and opposite answers.
//
// Rollback is the panel, not a confirm dialog: pick, review the diff, verify
// (#181). It hangs off a release row, which is where somebody deciding to roll
// back is already looking.

const route = useRoute();
const router = useRouter();
const name = computed(() => route.params.name as string);

const { data, error, loading, refresh } = useAsync(async () => {
  const [project, environments, releases, builds, promotions] = await Promise.all([
    api.project(name.value),
    api.projectEnvironments(name.value),
    api.projectReleases(name.value),
    api.projectBuilds(name.value),
    api.projectPromotions(name.value),
  ]);
  return { project, environments, releases, builds, promotions };
});
watch(name, () => void refresh());
// How old this screen is, and the reader's hold on it: every fetch above
// reports into it and the header renders it.
const freshness = useFreshness();
usePoll(() => void refresh(), 10000, () => true);

const project = computed(() => data.value?.project);
const production = computed(() =>
  data.value?.environments.find((environment) => environment.name === project.value?.productionEnvironment),
);
const releases = computed(() => data.value?.releases ?? []);
const builds = computed(() => data.value?.builds ?? []);
const promotions = computed(() => data.value?.promotions ?? []);
const showPipeline = computed(() => pipelineShown(project.value?.promotionStages, promotions.value));

const caller = computed(() => callerFor(project.value?.role, name.value));
const mayDeploy = computed(() => may("PATCH /api/v1/environments/{name}", caller.value));

// ── The process filter ──────────────────────────────────────────────────────

/** Empty is every artifact of every build. Otherwise the timeline is narrowed
 * to the artifact that process deploys, which for a process with no build of
 * its own is the project's own image. */
const process = ref<string>((route.query.process as string) ?? "");
watch(process, (value) => {
  const { process: _dropped, ...rest } = route.query;
  void router.replace({ query: value ? { ...rest, process: value } : rest });
});
const processes = computed(() => deployedProcesses(project.value, builds.value));
/** Whether this process builds anything of its own, which is what decides
 * whether filtering on it narrows the timeline at all. */
function buildsItsOwn(candidate: string): boolean {
  if (candidate === WEB_PROCESS) return true;
  return builds.value.some((build) => (build.workloads ?? []).some((workload) => workload.name === candidate));
}
const sharesTheWebImage = computed(() => Boolean(process.value) && !buildsItsOwn(process.value));

const entries = computed(() => {
  const all = deployEntries(builds.value, promotions.value);
  if (!process.value || sharesTheWebImage.value) return all;
  // A process that builds its own image is only in the builds that produced
  // one for it. A promotion is a decision about the whole unit and stays.
  return all.filter(
    (entry) =>
      entry.kind === "promotion" || (entry.build.workloads ?? []).some((workload) => workload.name === process.value),
  );
});

/** The image this build produced for the process being read, which is the
 * whole reason the filter is over artifacts. */
function artifactLine(build: Build): string {
  const found = artifactFor(build, process.value || WEB_PROCESS);
  if (!found?.artifact) return "";
  const reference = found.artifact.repository
    ? `${found.artifact.repository}${found.artifact.digest ? `@${found.artifact.digest}` : ""}`
    : (found.artifact.digest ?? "");
  return reference ? `${found.name}: ${shortImage(reference)}` : "";
}

// ── The releases ────────────────────────────────────────────────────────────

const currentRelease = computed(() => production.value?.release);
function buildOf(release: Release) {
  return builds.value.find((build) => build.name === release.build);
}
function releaseState(release: Release): { label: string; tone: "success" | "neutral" | "warning" } {
  if (release.name === currentRelease.value) {
    const observed = production.value?.observedRelease;
    return observed === release.name ? { label: "Live", tone: "success" } : { label: "Rolling out", tone: "warning" };
  }
  // Past releases read their label off the environment's history: how each one
  // stopped being current, not just that it did.
  return { label: releaseHistoryLabel(release.name, production.value), tone: "neutral" };
}
/** The badge's tooltip: who moved production off this release. */
function releaseMovedBy(release: Release): string {
  const entry = releaseHistoryEntry(release.name, production.value);
  if (!entry?.by) return "";
  return entry.reason === "promoted" ? `by build ${entry.by}` : `by ${entry.by}`;
}

// Pick, review the diff, verify. The panel outlives the write on purpose:
// closing on confirm would leave "did that work" to be answered by going and
// looking somewhere else.
const rollbackOpen = ref(false);
const rollbackFrom = ref("");
function openRollback(release: Release) {
  rollbackFrom.value = release.name;
  rollbackOpen.value = true;
}

// ── The timeline's rows ─────────────────────────────────────────────────────

/** Opening a build from anywhere on its row, without stealing the clicks that
 *  already mean something: a link inside the row, and the modifier clicks a
 *  browser opens in a new tab with. */
function openBuild(build: string, event: MouseEvent) {
  if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0) return;
  if ((event.target as HTMLElement | null)?.closest("a")) return;
  void router.push(buildLink(build, name.value));
}

/** The build whose commit message is open, if any. One at a time, so the list
 *  stays a list of subjects with one of them answered at length. */
const expanded = ref<string | null>(null);
function toggleMessage(build: string) {
  expanded.value = expanded.value === build ? null : build;
}
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="project">
      <PageHeader
        :freshness="freshness"
        title="Deploys"
        :breadcrumb="[{ label: 'Projects', to: '/projects' }, { label: project.name, mono: true }, { label: 'Deploys' }]"
      >
        <template #description>
          Every build of this project and every promotion of what it produced, newest first — and the way back to a
          release that worked.
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

      <PipelinePanel
        v-if="showPipeline"
        :stages="project.promotionStages ?? []"
        :environments="data?.environments ?? []"
        :promotions="promotions"
      />

      <!-- The release history, newest first. Rolling back opens the panel on
           the row that was clicked: pick, review the diff, verify. -->
      <PageSection
        title="Releases"
        description="What each release froze, and what production did with it. Rolling back puts back exactly what was running — configuration included."
      >
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[42rem] text-sm">
            <tbody>
              <tr v-if="!releases.length">
                <td class="px-3 py-8 text-center text-muted">No releases yet — a successful build creates one.</td>
              </tr>
              <tr v-for="release in releases" :key="release.name" class="border-b border-muted last:border-0">
                <td class="px-3 py-2 w-44">
                  <span class="flex items-center gap-2.5">
                    <StatusDot :tone="releaseState(release).tone" />
                    <span class="font-mono text-highlighted">{{ release.name }}</span>
                  </span>
                </td>
                <td class="px-3 py-2">
                  <p class="text-highlighted truncate max-w-2xl" :title="buildOf(release)?.git.message || release.build">
                    {{ buildOf(release)?.git.message || release.build }}
                  </p>
                  <p class="text-xs text-muted font-mono mt-0.5">
                    <SourceLink :href="buildOf(release)?.git.commitUrl">{{
                      shortSHA(buildOf(release)?.git.sha)
                    }}</SourceLink>
                    ·
                    <SourceLink :href="buildOf(release)?.git.branchUrl">{{
                      buildOf(release)?.git.branch || "—"
                    }}</SourceLink>
                    · {{ buildOf(release)?.git.author || "—" }}
                    <span v-if="buildOf(release)?.git.committedAt" :title="exactTime(buildOf(release)?.git.committedAt)">
                      · committed {{ timeAgo(buildOf(release)?.git.committedAt) }}</span
                    >
                  </p>
                  <!-- Where it is actually serving, from the release's own
                       status. The badge beside it only ever speaks for
                       production; a preview parked on an older release shows up
                       nowhere else. -->
                  <p v-if="release.environments?.length" class="text-xs text-muted mt-1 flex flex-wrap gap-x-1.5">
                    <span>Serving</span>
                    <RouterLink
                      v-for="served in release.environments"
                      :key="served"
                      :to="environmentLink(served, name)"
                      class="font-mono text-toned hover:text-highlighted hover:underline"
                      >{{ served }}</RouterLink
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
                    @click="openRollback(release)"
                  >
                    Roll back
                  </UButton>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </PageSection>

      <PageSection title="Timeline" description="Builds and promotions in the order they happened.">
        <template #actions>
          <div class="flex items-center gap-1.5 flex-wrap">
            <UButton
              size="xs"
              :color="process === '' ? 'primary' : 'neutral'"
              :variant="process === '' ? 'soft' : 'subtle'"
              @click="process = ''"
              >All</UButton
            >
            <UButton
              v-for="candidate in processes"
              :key="candidate"
              size="xs"
              class="font-mono"
              :color="process === candidate ? 'primary' : 'neutral'"
              :variant="process === candidate ? 'soft' : 'subtle'"
              @click="process = candidate"
              >{{ candidate }}</UButton
            >
          </div>
        </template>

        <!-- A process that declares no build of its own is not absent from the
             timeline: it runs the project's own image, so its history is the
             project's. Said out loud, because a filter that silently changed
             nothing would read as a filter that did not work. -->
        <p v-if="sharesTheWebImage" class="text-xs text-muted mb-3">
          <span class="font-mono">{{ process }}</span> declares no build of its own, so it runs the project's own image
          — <span class="font-mono">{{ WEB_PROCESS }}</span
          >. Its history is the whole timeline below.
        </p>

        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[42rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-3 py-2 font-medium w-24">Commit</th>
                <th class="px-3 py-2 font-medium">What happened</th>
                <th class="px-3 py-2 font-medium">Status</th>
                <th class="px-3 py-2 font-medium">Duration</th>
                <th class="px-3 py-2 font-medium text-right">Created</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!entries.length">
                <td colspan="5" class="px-3 py-8 text-center text-muted">
                  {{ loading ? "Loading…" : "Nothing has shipped yet — push to the repository, or hit Redeploy." }}
                </td>
              </tr>
              <template v-for="entry in entries" :key="entry.key">
                <tr
                  v-if="entry.kind === 'build'"
                  class="border-b border-muted last:border-0 hover:bg-elevated/40 cursor-pointer"
                  @click="openBuild(entry.build.name, $event)"
                >
                  <td class="px-3 py-2 w-24 font-mono text-xs text-toned align-top">
                    <SourceLink :href="entry.build.git.commitUrl">{{ shortSHA(entry.build.git.sha) }}</SourceLink>
                  </td>
                  <td class="px-3 py-2">
                    <span class="flex items-center gap-1 max-w-2xl">
                      <RouterLink
                        :to="buildLink(entry.build.name, name)"
                        class="block min-w-0 truncate text-highlighted hover:underline"
                        :title="entry.build.git.message || entry.build.name"
                      >
                        {{ entry.build.git.message || entry.build.name }}
                      </RouterLink>
                      <CommitBodyToggle
                        v-if="entry.build.git.body"
                        :open="expanded === entry.build.name"
                        @toggle="toggleMessage(entry.build.name)"
                      />
                    </span>
                    <p class="text-xs text-muted font-mono mt-0.5">
                      <SourceLink :href="entry.build.git.branchUrl">{{ entry.build.git.branch }}</SourceLink
                      ><span v-if="entry.build.git.pullRequest">
                        ·
                        <SourceLink :href="entry.build.git.pullRequestUrl"
                          >#{{ entry.build.git.pullRequest }}</SourceLink
                        ></span
                      ><span v-if="artifactLine(entry.build)"> · {{ artifactLine(entry.build) }}</span>
                    </p>
                    <!-- Why it failed, so that two failed builds read as two
                         failures rather than as the same one twice. -->
                    <p v-if="buildFailureLine(entry.build)" class="text-xs text-error mt-1 break-words">
                      {{ buildFailureLine(entry.build) }}
                    </p>
                    <p v-else-if="buildStallLine(entry.build)" class="text-xs text-warning mt-1 break-words">
                      {{ buildStallLine(entry.build) }}
                    </p>
                    <!-- A commit that changed nothing under this project's
                         build root: no image, no release, and nothing wrong.
                         The timeline says so rather than showing a row that
                         shipped nothing and does not say why. -->
                    <p v-else-if="buildSkipLine(entry.build)" class="text-xs text-muted mt-1 break-words">
                      {{ buildSkipLine(entry.build) }}
                    </p>
                  </td>
                  <td class="px-3 py-2 align-top"><PhaseBadge :phase="entry.build.phase" /></td>
                  <td class="px-3 py-2 font-mono text-xs text-muted whitespace-nowrap align-top">
                    {{ duration(entry.build.startedAt, entry.build.completedAt) }}
                  </td>
                  <td class="px-3 py-2 text-right text-xs text-muted whitespace-nowrap align-top">
                    {{ timeAgo(entry.build.createdAt) }}
                  </td>
                </tr>
                <!-- A promotion is a decision about a release rather than a
                     thing that was built, so it reads as one: what was asked
                     for, where, and what the policy said. -->
                <tr v-else class="border-b border-muted last:border-0">
                  <td class="px-3 py-2 w-24 align-top">
                    <UIcon name="i-lucide-scale" class="size-3.5 text-muted" :title="entry.promotion.trigger" />
                  </td>
                  <td class="px-3 py-2">
                    <p class="text-toned">
                      <span class="font-mono text-highlighted">{{ entry.promotion.release }}</span> into
                      <RouterLink
                        :to="environmentLink(entry.promotion.environment, name)"
                        class="font-mono text-toned hover:text-highlighted hover:underline"
                        >{{ entry.promotion.environment }}</RouterLink
                      >
                    </p>
                    <p v-if="entry.promotion.message" class="text-xs text-muted mt-0.5 break-words">
                      {{ entry.promotion.message }}
                    </p>
                    <p v-if="entry.promotion.unmetRules?.length" class="text-xs text-error mt-1 font-mono break-words">
                      {{ entry.promotion.unmetRules.join(", ") }}
                    </p>
                  </td>
                  <td class="px-3 py-2 align-top">
                    <UBadge :color="promotionTone(entry.promotion.phase)" variant="soft" size="sm">
                      {{ entry.promotion.phase }}
                    </UBadge>
                  </td>
                  <td class="px-3 py-2 font-mono text-xs text-muted whitespace-nowrap align-top">
                    {{ entry.promotion.requestedBy || "—" }}
                  </td>
                  <td class="px-3 py-2 text-right text-xs text-muted whitespace-nowrap align-top">
                    {{ timeAgo(entry.promotion.createdAt) }}
                  </td>
                </tr>
                <tr v-if="entry.kind === 'build' && expanded === entry.build.name" class="border-b border-muted last:border-0">
                  <td colspan="5" class="px-3 py-2 bg-elevated/30">
                    <CommitBody :body="entry.build.git.body" />
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </PageSection>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>

    <RollbackPanel
      v-if="production && data"
      :open="rollbackOpen"
      :environment="production"
      :releases="releases"
      :builds="builds"
      :initial-release="rollbackFrom"
      @update:open="(open: boolean) => { rollbackOpen = open; }"
      @moved="refresh"
    />
  </div>
</template>
