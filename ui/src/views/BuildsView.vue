<script setup lang="ts">
import { computed, ref } from "vue";
import { api } from "../lib/api";
import { duration, formatDurationSeconds, shortSHA, timeAgo } from "../lib/format";
import { useAsync, usePoll } from "../lib/useAsync";
import PhaseBadge from "../components/PhaseBadge.vue";

const { data, error, loading, refresh } = useAsync(() => api.builds());
usePoll(() => void refresh(), 10000, () => true);

// The queue is the gate's own state, which lives on /status rather than on any
// build: a build cannot say what is in front of it. Its failure is not this
// page's — the list of builds is still worth showing without it.
const { data: status, refresh: refreshStatus } = useAsync(() => api.status());
usePoll(() => void refreshStatus(), 10000, () => true);

const queue = computed(() => status.value?.builds);
/** A queue is only interesting while something is in it. */
const waiting = computed(() => queue.value?.waiting ?? []);

const project = ref<string>("");
const projects = computed(() => [...new Set((data.value ?? []).map((b) => b.project))].sort());
const visible = computed(() => (project.value ? (data.value ?? []).filter((b) => b.project === project.value) : (data.value ?? [])));
</script>

<template>
  <div class="space-y-6">
    <div class="flex items-center justify-between gap-3 flex-wrap">
      <h1 class="text-xl font-semibold text-highlighted">Builds</h1>
      <UButton
        icon="i-lucide-refresh-cw"
        color="neutral"
        variant="ghost"
        size="sm"
        :loading="loading"
        aria-label="Refresh"
        @click="refresh"
      />
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <!-- Running against the limit, and what is behind it. The count says the
         platform is busy; the wait is what says whether the queue is moving. -->
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
        <RouterLink :to="`/builds/${build.name}`" class="text-highlighted hover:underline">
          {{ build.project }} · {{ build.name }}
        </RouterLink>
        <span class="text-muted">waiting {{ formatDurationSeconds(build.waitSeconds) }}</span>
      </div>
    </div>

    <div class="flex items-center gap-2 flex-wrap">
      <UButton
        size="xs"
        :color="project === '' ? 'primary' : 'neutral'"
        :variant="project === '' ? 'soft' : 'subtle'"
        @click="project = ''"
        >All</UButton
      >
      <UButton
        v-for="p in projects"
        :key="p"
        size="xs"
        :color="project === p ? 'primary' : 'neutral'"
        :variant="project === p ? 'soft' : 'subtle'"
        @click="project = p"
        >{{ p }}</UButton
      >
    </div>

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[42rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-4 py-2.5 font-medium">Commit</th>
            <th class="px-4 py-2.5 font-medium">Project</th>
            <th class="px-4 py-2.5 font-medium">Status</th>
            <th class="px-4 py-2.5 font-medium">Duration</th>
            <th class="px-4 py-2.5 font-medium text-right">Created</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!visible.length">
            <td colspan="5" class="px-4 py-8 text-center text-muted">{{ loading ? "Loading…" : "No builds yet." }}</td>
          </tr>
          <tr v-for="build in visible" :key="build.name" class="border-b border-muted last:border-0 hover:bg-elevated/40">
            <td class="px-4 py-3">
              <RouterLink :to="{ name: 'build', params: { name: build.name } }" class="group">
                <span class="text-highlighted group-hover:underline">{{ build.git.message || build.name }}</span>
                <span class="block text-xs text-muted font-mono mt-0.5">
                  {{ shortSHA(build.git.sha) }} · {{ build.git.branch
                  }}<span v-if="build.git.pullRequest"> · #{{ build.git.pullRequest }}</span>
                </span>
              </RouterLink>
            </td>
            <td class="px-4 py-3">
              <RouterLink :to="{ name: 'project', params: { name: build.project } }" class="text-toned hover:underline">
                {{ build.project }}
              </RouterLink>
            </td>
            <td class="px-4 py-3"><PhaseBadge :phase="build.phase" /></td>
            <td class="px-4 py-3 font-mono text-xs text-muted">{{ duration(build.startedAt, build.completedAt) }}</td>
            <td class="px-4 py-3 text-right text-xs text-muted whitespace-nowrap">{{ timeAgo(build.createdAt) }}</td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
