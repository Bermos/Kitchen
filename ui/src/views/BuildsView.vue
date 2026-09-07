<script setup lang="ts">
import { computed, ref } from "vue";
import { useRouter } from "vue-router";
import { api, type Build } from "../lib/api";
import { buildFailureLine, buildSkipLine, buildStallLine } from "../lib/builds";
import { buildLink } from "../lib/links";
import { duration, exactTime, formatDurationSeconds, shortSHA, timeAgo } from "../lib/format";
import { useFreshness } from "../lib/freshness";
import { useAsync, usePoll } from "../lib/useAsync";
import CommitBody from "../components/CommitBody.vue";
import CommitBodyToggle from "../components/CommitBodyToggle.vue";
import SourceLink from "../components/SourceLink.vue";
import PageHeader from "../components/PageHeader.vue";
import PhaseBadge from "../components/PhaseBadge.vue";

const router = useRouter();

// The Fleet scope's deploy list: every build across every project the caller
// can see, with the project a column and a filter.
//
// A project's own deploys are `ProjectDeploysView.vue`, which is a different
// screen rather than this one with a column removed: it puts builds and
// promotions on one timeline, filters them by process, and hangs the rollback
// off a release row (#470). What the two have in common is a list of builds,
// which is not enough to be one component wearing two shapes.

/** Opening a build from anywhere on its row, without stealing the clicks that
 *  already mean something: a link the row contains, and the modifier clicks a
 *  browser opens in a new tab with. */
function open(name: string, event: MouseEvent) {
  if (event.defaultPrevented || event.metaKey || event.ctrlKey || event.shiftKey || event.button !== 0) return;
  if ((event.target as HTMLElement | null)?.closest("a")) return;
  void router.push(buildLink(name, projectOf(name)));
}

const failureOf = (build: Build) => buildFailureLine(build);
// A running build that is not moving. It reads as a failure on the row on
// purpose — it is one, it just has not been called one yet.
const stallOf = (build: Build) => buildStallLine(build);
// A build the platform had nothing to build for. It reads as neither of the
// two above: no colour of its own, because nothing is wrong and nothing
// shipped.
const skipOf = (build: Build) => buildSkipLine(build);

const { data, error, loading, refresh } = useAsync(() => api.builds());

/** Which project a build is in, for the link out. The list carries it, so this
 * never has to guess. */
function projectOf(build: string): string | undefined {
  return (data.value ?? []).find((candidate) => candidate.name === build)?.project;
}
// How old this screen is, and the reader's hold on it: every fetch above
// reports into it and the header renders it.
const freshness = useFreshness();
usePoll(() => void refresh(), 10000, () => true);

// The queue is the gate's own state, which lives on /status rather than on any
// build: a build cannot say what is in front of it. Its failure is not this
// page's — the list of builds is still worth showing without it.
const { data: status, refresh: refreshStatus } = useAsync(() => api.status());
usePoll(() => void refreshStatus(), 10000, () => true);

const queue = computed(() => status.value?.builds);
/** A queue is only interesting while something is in it. */
const waiting = computed(() => queue.value?.waiting ?? []);

// The rest of a commit message, on the row that asked for it. One at a time:
// two open bodies is a list nobody can scan, and a list is read by subjects.
const expanded = ref<string | null>(null);
function toggleMessage(name: string) {
  expanded.value = expanded.value === name ? null : name;
}

/** The projects that have builds — the filter's own items. */
const filter = ref<string>("");
const projects = computed(() => [...new Set((data.value ?? []).map((b) => b.project))].sort());
const visible = computed(() => {
  const builds = data.value ?? [];
  return filter.value ? builds.filter((b) => b.project === filter.value) : builds;
});
</script>

<template>
  <div class="space-y-6">
    <PageHeader
      :freshness="freshness"
      title="Deploys"
    >
      <template #description>
        Every build across the projects you can see, newest first — what is running now, what is waiting for a slot, and
        what the last commit did.
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

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <!-- Running against the limit, and what is behind it. The count says the
         platform is busy; the wait is what says whether the queue is moving. -->
    <!-- The gate's own state, which is the platform's rather than a
         project's: how many slots there are and what is behind them is the
         same answer whichever project asked. It stays on the fleet screen. -->
    <div v-if="queue" class="flex items-center gap-x-4 gap-y-1 flex-wrap text-sm">
      <span class="text-muted">
        <span class="text-highlighted font-medium">{{ queue.running }}</span>
        of {{ queue.capacity }} building
      </span>
      <span v-if="queue.queued" class="text-muted">
        <span class="text-highlighted font-medium">{{ queue.queued }}</span>
        waiting for a slot<template v-if="queue.oldestWaitSeconds">
          , longest {{ formatDurationSeconds(queue.oldestWaitSeconds) }}</template>
      </span>
      <span v-else class="text-dimmed">nothing waiting</span>
    </div>

    <div v-if="waiting.length" class="rounded border border-default divide-y divide-default text-sm">
      <div
        v-for="build in waiting"
        :key="build.name"
        class="flex items-center justify-between px-3 py-2"
      >
        <RouterLink :to="buildLink(build.name, build.project)" class="text-highlighted hover:underline">
          {{ build.project }} · {{ build.name }}
        </RouterLink>
        <span class="text-muted">waiting {{ formatDurationSeconds(build.waitSeconds) }}</span>
      </div>
    </div>

    <div class="flex items-center gap-2 flex-wrap">
      <UButton
        size="xs"
        :color="filter === '' ? 'primary' : 'neutral'"
        :variant="filter === '' ? 'soft' : 'subtle'"
        @click="filter = ''"
        >All</UButton
      >
      <UButton
        v-for="p in projects"
        :key="p"
        size="xs"
        :color="filter === p ? 'primary' : 'neutral'"
        :variant="filter === p ? 'soft' : 'subtle'"
        @click="filter = p"
        >{{ p }}</UButton
      >
    </div>

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[42rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-3 py-2 font-medium">Commit</th>
            <th class="px-3 py-2 font-medium">Project</th>
            <th class="px-3 py-2 font-medium">Status</th>
            <th class="px-3 py-2 font-medium">Duration</th>
            <th class="px-3 py-2 font-medium text-right">Created</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!visible.length">
            <td colspan="5" class="px-3 py-8 text-center text-muted">
              {{ loading ? "Loading…" : "No builds yet." }}
            </td>
          </tr>
          <!-- The whole row opens the build. A row that is nine tenths dead
               space with one word in it that navigates reads as a list that
               does nothing when you click it, which is what this was. The
               anchor stays for the keyboard and the middle click; the row
               handler is for the other nine tenths. -->
          <template v-for="build in visible" :key="build.name">
            <tr
              class="border-b border-muted last:border-0 hover:bg-elevated/40 cursor-pointer"
              @click="open(build.name, $event)"
            >
              <td class="px-3 py-2">
                <span class="flex items-center gap-1 max-w-2xl">
                  <RouterLink :to="buildLink(build.name, build.project)" class="group min-w-0">
                    <span
                      class="block truncate text-highlighted group-hover:underline"
                      :title="build.git.message || build.name"
                      >{{ build.git.message || build.name }}</span
                    >
                  </RouterLink>
                  <!-- What the commit says under its subject, for the ones
                       that say anything. Its own control rather than the
                       row's click, which opens the build. -->
                  <CommitBodyToggle
                    v-if="build.git.body"
                    :open="expanded === build.name"
                    @toggle="toggleMessage(build.name)"
                  />
                </span>
                <!-- The commit, its branch and its request, each a link back
                     to the code — and how old the commit is, which is the one
                     thing a truncated SHA cannot say about itself. -->
                <span class="block text-xs text-muted font-mono mt-0.5">
                  <SourceLink :href="build.git.commitUrl">{{ shortSHA(build.git.sha) }}</SourceLink> ·
                  <SourceLink :href="build.git.branchUrl">{{ build.git.branch }}</SourceLink
                  ><span v-if="build.git.pullRequest">
                    ·
                    <SourceLink :href="build.git.pullRequestUrl">#{{ build.git.pullRequest }}</SourceLink></span
                  ><span v-if="build.git.committedAt" :title="exactTime(build.git.committedAt)">
                    · committed {{ timeAgo(build.git.committedAt) }}</span
                  >
                </span>
                <!-- Why it failed, on the row, so that a list of failures is
                     readable as a list of *different* failures. -->
                <span v-if="failureOf(build)" class="block text-xs text-error mt-1 break-words">
                  {{ failureOf(build) }}
                </span>
                <!-- And why one that says Running is not moving, which without
                     this is only on a warning event on the Job. -->
                <span v-else-if="stallOf(build)" class="block text-xs text-warning mt-1 break-words">
                  {{ stallOf(build) }}
                </span>
                <!-- And why one that shipped nothing is not a failure: the
                     commit changed nothing under this project's build root. -->
                <span v-else-if="skipOf(build)" class="block text-xs text-muted mt-1 break-words">
                  {{ skipOf(build) }}
                </span>
              </td>
              <td class="px-3 py-2">
                <RouterLink
                  :to="{ name: 'project', params: { name: build.project } }"
                  class="text-toned hover:underline"
                >
                  {{ build.project }}
                </RouterLink>
              </td>
              <td class="px-3 py-2"><PhaseBadge :phase="build.phase" /></td>
              <td class="px-3 py-2 font-mono text-xs text-muted">{{ duration(build.startedAt, build.completedAt) }}</td>
              <td class="px-3 py-2 text-right text-xs text-muted whitespace-nowrap">{{ timeAgo(build.createdAt) }}</td>
            </tr>
            <tr v-if="expanded === build.name" class="border-b border-muted last:border-0">
              <td colspan="5" class="px-3 py-2 bg-elevated/30">
                <CommitBody :body="build.git.body" />
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>
  </div>
</template>
