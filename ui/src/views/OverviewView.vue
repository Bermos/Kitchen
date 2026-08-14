<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type Build, type Environment } from "../lib/api";
import { timeAgo } from "../lib/format";
import { statusDetail, unhealthyConditions, type Tone } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import PhaseBadge from "../components/PhaseBadge.vue";
import StatusDot from "../components/StatusDot.vue";

// The overview joins three collections client-side: projects are the rows,
// environments carry the production URL and preview count, builds carry the
// latest build's phase. The API answers newest-first, so "latest" is "first".

const { data, error, loading, refresh } = useAsync(() =>
  Promise.all([api.projects(), api.environments(), api.builds()]),
);
usePoll(() => void refresh(), 15000, () => true);

const filter = ref<"all" | "failing">("all");

interface Row {
  name: string;
  repo: string;
  url?: string;
  environment?: Environment;
  latestBuild?: Build;
  previews: number;
  tone: Tone;
  detail: string;
  lastDeploy?: string;
}

const rows = computed<Row[]>(() => {
  if (!data.value) return [];
  const [projects, environments, builds] = data.value;
  return projects.map((project) => {
    const production = environments.find((e) => e.name === project.productionEnvironment);
    const previews = environments.filter((e) => e.project === project.name && e.type === "preview").length;
    const latestBuild = builds.find((b) => b.project === project.name);
    const failing =
      latestBuild?.phase === "Failed" ||
      production?.phase === "Degraded" ||
      unhealthyConditions(project.conditions).length > 0;
    const busy = latestBuild?.phase === "Running" || production?.phase === "Deploying";
    return {
      name: project.name,
      repo: project.repo,
      url: production?.url,
      environment: production,
      latestBuild,
      previews,
      tone: failing ? "error" : busy ? "warning" : production?.phase === "Live" ? "success" : "neutral",
      detail: statusDetail(project.conditions),
      lastDeploy: production?.createdAt && latestBuild?.completedAt ? latestBuild.completedAt : latestBuild?.createdAt,
    };
  });
});

const failingCount = computed(() => rows.value.filter((r) => r.tone === "error").length);
const visible = computed(() => (filter.value === "failing" ? rows.value.filter((r) => r.tone === "error") : rows.value));

function host(url?: string): string {
  if (!url) return "—";
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}
</script>

<template>
  <div class="space-y-6">
    <div class="flex items-center justify-between">
      <h1 class="text-xl font-semibold text-highlighted">Overview</h1>
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

    <div class="flex items-center gap-2">
      <UButton
        size="xs"
        :color="filter === 'all' ? 'primary' : 'neutral'"
        :variant="filter === 'all' ? 'soft' : 'subtle'"
        @click="filter = 'all'"
      >
        All <span class="font-mono text-dimmed ml-1">{{ rows.length }}</span>
      </UButton>
      <UButton
        size="xs"
        :color="filter === 'failing' ? 'primary' : 'neutral'"
        :variant="filter === 'failing' ? 'soft' : 'subtle'"
        @click="filter = 'failing'"
      >
        Failing <span class="font-mono text-dimmed ml-1">{{ failingCount }}</span>
      </UButton>
    </div>

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-4 py-2.5 font-medium">Project</th>
            <th class="px-4 py-2.5 font-medium">Production</th>
            <th class="px-4 py-2.5 font-medium">Last build</th>
            <th class="px-4 py-2.5 font-medium text-right">Previews</th>
            <th class="px-4 py-2.5 font-medium text-right">Environment</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!visible.length">
            <td colspan="5" class="px-4 py-8 text-center text-muted">
              {{ loading ? "Loading…" : filter === "failing" ? "Nothing is failing." : "No projects yet — projects are created with kubectl until the create flow lands here." }}
            </td>
          </tr>
          <tr
            v-for="row in visible"
            :key="row.name"
            class="border-b border-muted last:border-0 hover:bg-elevated/40"
          >
            <td class="px-4 py-3">
              <RouterLink :to="{ name: 'project', params: { name: row.name } }" class="flex items-center gap-2.5 group">
                <StatusDot :tone="row.tone" :pulse="row.tone === 'warning'" />
                <span>
                  <span class="block text-highlighted font-medium group-hover:underline">{{ row.name }}</span>
                  <span class="block text-xs text-muted">{{ row.repo }}</span>
                </span>
              </RouterLink>
              <p v-if="row.detail" class="text-xs text-error mt-1 pl-6 truncate max-w-sm" :title="row.detail">
                {{ row.detail }}
              </p>
            </td>
            <td class="px-4 py-3">
              <a
                v-if="row.url"
                :href="row.url"
                target="_blank"
                rel="noopener"
                class="font-mono text-xs text-primary hover:underline"
                >{{ host(row.url) }}</a
              >
              <span v-else class="text-dimmed">—</span>
            </td>
            <td class="px-4 py-3">
              <RouterLink
                v-if="row.latestBuild"
                :to="{ name: 'build', params: { name: row.latestBuild.name } }"
                class="inline-flex items-center gap-2"
              >
                <PhaseBadge :phase="row.latestBuild.phase" />
                <span class="text-xs text-muted">{{ timeAgo(row.latestBuild.createdAt) }}</span>
              </RouterLink>
              <span v-else class="text-dimmed">—</span>
            </td>
            <td class="px-4 py-3 text-right font-mono text-toned">{{ row.previews || "—" }}</td>
            <td class="px-4 py-3 text-right"><PhaseBadge :phase="row.environment?.phase" /></td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
