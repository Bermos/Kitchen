<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api, type LogLine, type LogQuery } from "../lib/api";
import { duration, shortSHA, timeAgo } from "../lib/format";
import { operatorMode } from "../lib/mode";
import { useAsync, usePoll } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import LogViewer from "../components/LogViewer.vue";
import PhaseBadge from "../components/PhaseBadge.vue";

const route = useRoute();
const toast = useToast();
const name = computed(() => route.params.name as string);

const { data: build, error, loading, refresh } = useAsync(() => api.build(name.value));
watch(name, () => void refresh());

// A queued or running build is still moving; keep the header fresh while the
// log viewer below follows the output.
const moving = computed(() => build.value?.phase === "Queued" || build.value?.phase === "Running");
usePoll(() => void refresh(), 5000, () => moving.value);

// Cancelling keeps the Build — phase Cancelled — and stops its BuildKit job.
const cancelling = ref(false);
async function cancel() {
  cancelling.value = true;
  try {
    await api.cancelBuild(name.value);
    toast.add({ title: `Build ${name.value} cancelled`, color: "success", icon: "i-lucide-ban" });
    await refresh();
  } catch (err) {
    toast.add({
      title: "Cancelling the build failed",
      description: err instanceof Error ? err.message : String(err),
      color: "error",
    });
  } finally {
    cancelling.value = false;
  }
}

const logFetcher = (query: LogQuery) => api.buildLogs(name.value, query);
const logStreamer = (query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) =>
  api.streamBuildLogs(name.value, query, onLine, signal);
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="build">
      <div>
        <div class="flex items-center gap-2 text-xs text-muted mb-1">
          <RouterLink to="/" class="hover:text-highlighted">Overview</RouterLink>
          <span>/</span>
          <RouterLink :to="{ name: 'project', params: { name: build.project } }" class="hover:text-highlighted">
            {{ build.project }}
          </RouterLink>
          <span>/</span>
          <span class="text-toned font-mono">{{ build.name }}</span>
        </div>
        <div class="flex items-center gap-3 flex-wrap">
          <h1 class="text-xl font-semibold text-highlighted">{{ build.git.message || build.name }}</h1>
          <PhaseBadge :phase="build.phase" />
          <span class="flex-1" />
          <UButton
            v-if="moving"
            color="neutral"
            variant="subtle"
            size="sm"
            icon="i-lucide-ban"
            :loading="cancelling"
            @click="cancel"
          >
            Cancel build
          </UButton>
        </div>
        <div class="flex items-center gap-3 mt-1 text-xs text-muted font-mono flex-wrap">
          <span>{{ shortSHA(build.git.sha) }}</span>
          <span>{{ build.git.branch }}</span>
          <span v-if="build.git.pullRequest">#{{ build.git.pullRequest }}</span>
          <span v-if="build.git.author">{{ build.git.author }}</span>
          <span v-if="build.detectedFramework">{{ build.detectedFramework }}, detected</span>
        </div>
      </div>

      <div class="rounded-md border border-default bg-muted px-5 py-4 grid gap-6 sm:grid-cols-4">
        <div>
          <p class="text-xs text-muted mb-1">Created</p>
          <p class="text-sm text-toned">{{ timeAgo(build.createdAt) }}</p>
        </div>
        <div>
          <p class="text-xs text-muted mb-1">Duration</p>
          <p class="text-sm text-toned font-mono">{{ duration(build.startedAt, build.completedAt) }}</p>
        </div>
        <div class="sm:col-span-2">
          <p class="text-xs text-muted mb-1">Image</p>
          <p class="text-sm text-toned font-mono truncate" :title="build.image">{{ build.image || "not pushed yet" }}</p>
        </div>
      </div>

      <ConditionsTable v-if="operatorMode" :conditions="build.conditions" />

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Build output</h2>
        <LogViewer :fetcher="logFetcher" :streamer="logStreamer" :live="moving" :query-clause="`build = '${build.name}'`" />
      </div>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>
  </div>
</template>
