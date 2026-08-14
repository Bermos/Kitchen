<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute } from "vue-router";
import { api, type LogQuery, type Release } from "../lib/api";
import { shortImage, timeAgo } from "../lib/format";
import { operatorMode } from "../lib/mode";
import { useAsync, usePoll } from "../lib/useAsync";
import ConditionsTable from "../components/ConditionsTable.vue";
import LogViewer from "../components/LogViewer.vue";
import PhaseBadge from "../components/PhaseBadge.vue";

const route = useRoute();
const toast = useToast();
const name = computed(() => route.params.name as string);

const { data, error, loading, refresh } = useAsync(async () => {
  const environment = await api.environment(name.value);
  const releases = await api.projectReleases(environment.project);
  return { environment, releases };
});
watch(name, () => void refresh());

const environment = computed(() => data.value?.environment);
const moving = computed(() => environment.value?.phase === "Deploying" || environment.value?.phase === "Pending");
usePoll(() => void refresh(), 5000, () => moving.value);

const currentRelease = computed(() =>
  data.value?.releases.find((r) => r.name === environment.value?.release),
);
const otherReleases = computed(() =>
  (data.value?.releases ?? []).filter((r) => r.name !== environment.value?.release),
);

const target = ref<Release | null>(null);
const movingRelease = ref(false);
async function move() {
  if (!target.value || !environment.value) return;
  movingRelease.value = true;
  try {
    await api.moveEnvironment(environment.value.name, target.value.name);
    toast.add({ title: `${environment.value.name} moved to ${target.value.name}`, color: "success", icon: "i-lucide-undo-2" });
    target.value = null;
    await refresh();
  } catch (err) {
    toast.add({ title: "Move failed", description: err instanceof Error ? err.message : String(err), color: "error" });
  } finally {
    movingRelease.value = false;
  }
}

const logFetcher = (query: LogQuery) => api.environmentLogs(name.value, query);
</script>

<template>
  <div class="space-y-6">
    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <template v-else-if="environment">
      <div class="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <div class="flex items-center gap-2 text-xs text-muted mb-1">
            <RouterLink to="/" class="hover:text-highlighted">Overview</RouterLink>
            <span>/</span>
            <RouterLink :to="{ name: 'project', params: { name: environment.project } }" class="hover:text-highlighted">
              {{ environment.project }}
            </RouterLink>
            <span>/</span>
            <span class="text-toned font-mono">{{ environment.name }}</span>
          </div>
          <div class="flex items-center gap-3 flex-wrap">
            <h1 class="text-xl font-semibold text-highlighted">{{ environment.name }}</h1>
            <UBadge color="neutral" variant="subtle" size="sm">{{ environment.type }}</UBadge>
            <PhaseBadge :phase="environment.phase" />
          </div>
          <div class="flex items-center gap-3 mt-1 text-xs text-muted flex-wrap">
            <span v-if="environment.preview" class="font-mono">
              #{{ environment.preview.pullRequest }} · {{ environment.preview.branch }}
            </span>
            <span>created {{ timeAgo(environment.createdAt) }}</span>
          </div>
        </div>
        <UButton
          v-if="environment.url"
          :href="environment.url"
          target="_blank"
          size="sm"
          icon="i-lucide-arrow-up-right"
          trailing
        >
          Open
        </UButton>
      </div>

      <div class="rounded-md border border-default bg-muted px-5 py-4 grid gap-6 sm:grid-cols-3">
        <div>
          <p class="text-xs text-muted mb-1">Release</p>
          <p class="font-mono text-sm text-highlighted">{{ environment.release }}</p>
          <p class="text-xs text-dimmed mt-0.5">
            observed {{ environment.observedRelease || "—"
            }}<template v-if="environment.observedRelease && environment.observedRelease !== environment.release">
              — still rolling</template
            >
          </p>
        </div>
        <div>
          <p class="text-xs text-muted mb-1">Image</p>
          <p class="font-mono text-sm text-toned truncate" :title="currentRelease?.image">
            {{ shortImage(currentRelease?.image) }}
          </p>
        </div>
        <div>
          <p class="text-xs text-muted mb-1">URL</p>
          <a
            v-if="environment.url"
            :href="environment.url"
            target="_blank"
            rel="noopener"
            class="font-mono text-sm text-primary hover:underline break-all"
            >{{ environment.url }}</a
          >
          <p v-else class="text-sm text-dimmed">no route — see conditions</p>
        </div>
      </div>

      <ConditionsTable v-if="operatorMode" :conditions="environment.conditions" />

      <div v-if="otherReleases.length">
        <h2 class="text-sm font-medium text-highlighted mb-2">Move to another release</h2>
        <p class="text-xs text-muted mb-3">
          Rollback and promotion are the same one-field change: point the environment at an immutable release and the
          operator puts back exactly what it snapshotted.
        </p>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full text-sm">
            <tbody>
              <tr v-for="release in otherReleases" :key="release.name" class="border-b border-muted last:border-0">
                <td class="px-4 py-2.5 font-mono text-highlighted w-44">{{ release.name }}</td>
                <td class="px-4 py-2.5 font-mono text-xs text-toned truncate max-w-xs" :title="release.image">
                  {{ shortImage(release.image) }}
                </td>
                <td class="px-4 py-2.5 text-xs text-muted whitespace-nowrap">{{ timeAgo(release.createdAt) }}</td>
                <td class="px-4 py-2.5 text-right">
                  <UButton color="neutral" variant="subtle" size="xs" @click="target = release">Move here</UButton>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Runtime logs</h2>
        <LogViewer :fetcher="logFetcher" :live="moving" :query-clause="`environment = '${environment.name}'`" />
      </div>
    </template>
    <div v-else-if="loading" class="py-24 text-center text-muted text-sm">Loading…</div>

    <UModal
      :open="target !== null"
      :title="`Move ${environment?.name}?`"
      :description="`The environment moves to ${target?.name}. Releases are immutable snapshots of image and config, so this is exact — and just as easy to undo.`"
      @update:open="(open: boolean) => { if (!open) target = null; }"
    >
      <template #footer>
        <div class="flex justify-end gap-2 w-full">
          <UButton color="neutral" variant="subtle" @click="target = null">Cancel</UButton>
          <UButton color="primary" :loading="movingRelease" icon="i-lucide-undo-2" @click="move">
            Move to {{ target?.name }}
          </UButton>
        </div>
      </template>
    </UModal>
  </div>
</template>
