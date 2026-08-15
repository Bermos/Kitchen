<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type Build, type Environment, type PlatformEvent, type ProjectTraffic } from "../lib/api";
import { compactCount, formatSeconds, timeAgo } from "../lib/format";
import { statusDetail, unhealthyConditions, type Tone } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import NewProjectModal from "../components/NewProjectModal.vue";
import PhaseBadge from "../components/PhaseBadge.vue";
import Sparkline from "../components/Sparkline.vue";
import StatusDot from "../components/StatusDot.vue";

// The overview joins three collections client-side: projects are the rows,
// environments carry the production URL and preview count, builds carry the
// latest build's phase. The API answers newest-first, so "latest" is "first".
// The numbers and the feed ride separately on /metrics/overview and /events,
// so an installation without a telemetry store still gets the table.

const { data, error, loading, refresh } = useAsync(() =>
  Promise.all([api.projects(), api.environments(), api.builds()]),
);
const metrics = useAsync(() => api.metricsOverview());
const activity = useAsync(() => api.events({ limit: 20 }));
usePoll(() => void refresh(), 15000, () => true);
usePoll(() => void metrics.refresh(), 60000, () => true);
usePoll(() => void activity.refresh(), 15000, () => true);

// The KPI strip: each tile is a headline number with the shape of its window
// next to it. Traffic tiles read "—" until the flow pipeline is configured —
// zero requests measured and no measurement at all are different answers.
const kpis = computed(() => {
  const m = metrics.data.value;
  if (!m) return null;
  const measuresTraffic = m.requests24h > 0 || m.requestsPerHour.some((v) => v > 0);
  return [
    {
      label: "Deploys · 7 days",
      value: compactCount(m.deploys7d),
      points: m.deploysPerDay,
      detail: m.medianBuildSeconds > 0 ? `median build ${formatSeconds(m.medianBuildSeconds)}` : "no builds finished",
    },
    {
      label: "Requests · 24 h",
      value: measuresTraffic ? compactCount(m.requests24h) : "—",
      points: m.requestsPerHour,
      detail: measuresTraffic ? `${(m.errorRate24h * 100).toFixed(2)}% errors` : "no flow data",
    },
    {
      label: "p95 latency · 24 h",
      value: measuresTraffic && m.p95Ms24h > 0 ? `${Math.round(m.p95Ms24h)} ms` : "—",
      points: m.p95MsPerHour,
      detail: measuresTraffic ? "from Hubble flows" : "no flow data",
    },
    {
      label: "Log lines · 24 h",
      value: compactCount(m.logLines24h),
      points: m.logLinesPerHour,
      detail: `${m.storeRowsPerSecond.toFixed(m.storeRowsPerSecond < 10 ? 1 : 0)} rows/s into the store`,
    },
  ];
});

const trafficByProject = computed(() => {
  const rows = new Map<string, ProjectTraffic>();
  for (const entry of metrics.data.value?.projects ?? []) rows.set(entry.project, entry);
  return rows;
});
const anyProjectTraffic = computed(() =>
  [...trafficByProject.value.values()].some((entry) => entry.requests24h > 0),
);

// What a feed entry links to: the most specific object it names.
function eventTarget(event: PlatformEvent): { name: string; params: Record<string, string> } | null {
  if (event.build) return { name: "build", params: { name: event.build } };
  if (event.environment) return { name: "environment", params: { name: event.environment } };
  if (event.project) return { name: "project", params: { name: event.project } };
  return null;
}

function eventIcon(event: PlatformEvent): string {
  if (event.type === "build.failed") return "i-lucide-x-circle";
  if (event.type === "build.succeeded") return "i-lucide-hammer";
  if (event.type.startsWith("release.")) return "i-lucide-rocket";
  if (event.type.startsWith("preview.")) return "i-lucide-git-pull-request";
  if (event.type.startsWith("claim.")) return "i-lucide-database";
  if (event.type.startsWith("platform.")) return "i-lucide-arrow-up-circle";
  return "i-lucide-sparkles";
}

function eventTone(event: PlatformEvent): string {
  if (event.type === "build.failed" || event.type === "claim.failed") return "text-error";
  if (event.type === "platform.updateFailed") return "text-error";
  if (event.type === "release.rolledBack") return "text-warning";
  return "text-muted";
}

const filter = ref<"all" | "production" | "previews" | "failing">("all");

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

const filters = computed(() => {
  const rowsOf = {
    all: rows.value,
    production: rows.value.filter((r) => r.environment?.phase === "Live"),
    previews: rows.value.filter((r) => r.previews > 0),
    failing: rows.value.filter((r) => r.tone === "error"),
  };
  return [
    { value: "all" as const, label: "All", rows: rowsOf.all },
    { value: "production" as const, label: "Production", rows: rowsOf.production },
    { value: "previews" as const, label: "Previews", rows: rowsOf.previews },
    { value: "failing" as const, label: "Failing", rows: rowsOf.failing },
  ];
});
const visible = computed(() => filters.value.find((f) => f.value === filter.value)?.rows ?? rows.value);

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
      <div class="flex items-center gap-2">
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="loading"
          aria-label="Refresh"
          @click="refresh"
        />
        <NewProjectModal @created="refresh" />
      </div>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <!-- The KPI strip only renders once the metrics endpoint answered; an
         installation without a telemetry store simply has no numbers row. -->
    <div v-if="kpis" class="grid grid-cols-2 lg:grid-cols-4 gap-3">
      <div v-for="kpi in kpis" :key="kpi.label" class="rounded-md border border-default px-4 py-3">
        <p class="text-xs text-muted">{{ kpi.label }}</p>
        <div class="flex items-end justify-between gap-3 mt-1">
          <span class="text-xl font-semibold text-highlighted tabular-nums">{{ kpi.value }}</span>
          <Sparkline :points="kpi.points" />
        </div>
        <p class="text-[11px] text-dimmed mt-1">{{ kpi.detail }}</p>
      </div>
    </div>

    <div class="flex items-center gap-2 flex-wrap">
      <UButton
        v-for="chip in filters"
        :key="chip.value"
        size="xs"
        :color="filter === chip.value ? 'primary' : 'neutral'"
        :variant="filter === chip.value ? 'soft' : 'subtle'"
        @click="filter = chip.value"
      >
        {{ chip.label }} <span class="font-mono text-dimmed ml-1">{{ chip.rows.length }}</span>
      </UButton>
    </div>

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-4 py-2.5 font-medium">Project</th>
            <th class="px-4 py-2.5 font-medium">Production</th>
            <th class="px-4 py-2.5 font-medium">Last build</th>
            <th v-if="anyProjectTraffic" class="px-4 py-2.5 font-medium">Traffic · 24 h</th>
            <th v-if="anyProjectTraffic" class="px-4 py-2.5 font-medium text-right">p95 · 5xx</th>
            <th class="px-4 py-2.5 font-medium text-right">Previews</th>
            <th class="px-4 py-2.5 font-medium text-right">Environment</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!visible.length">
            <td :colspan="anyProjectTraffic ? 7 : 5" class="px-4 py-8 text-center text-muted">
              {{ loading ? "Loading…" : filter === "failing" ? "Nothing is failing." : "No projects yet — “New project” connects your first repository." }}
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
            <td v-if="anyProjectTraffic" class="px-4 py-3">
              <div class="flex items-center gap-2">
                <Sparkline :points="trafficByProject.get(row.name)?.requestsPerHour ?? []" :width="72" :height="20" />
                <span class="text-xs font-mono text-toned">{{
                  compactCount(trafficByProject.get(row.name)?.requests24h ?? 0)
                }}</span>
              </div>
            </td>
            <td v-if="anyProjectTraffic" class="px-4 py-3 text-right font-mono text-xs">
              <span class="text-toned">{{
                (trafficByProject.get(row.name)?.p95Ms ?? 0) > 0
                  ? `${Math.round(trafficByProject.get(row.name)!.p95Ms)} ms`
                  : "—"
              }}</span>
              <span class="mx-1 text-dimmed">·</span>
              <span :class="(trafficByProject.get(row.name)?.errors5xx24h ?? 0) > 0 ? 'text-error' : 'text-dimmed'">{{
                trafficByProject.get(row.name)?.errors5xx24h || 0
              }}</span>
            </td>
            <td class="px-4 py-3 text-right font-mono text-toned">{{ row.previews || "—" }}</td>
            <td class="px-4 py-3 text-right"><PhaseBadge :phase="row.environment?.phase" /></td>
          </tr>
        </tbody>
      </table>
    </div>

    <!-- Recent activity: the events the reconcilers and the API recorded,
         newest first, each linking to the thing it is about. -->
    <div v-if="activity.data.value?.length" class="rounded-md border border-default">
      <div class="px-4 py-2.5 border-b border-default bg-muted flex items-center justify-between">
        <h2 class="text-xs font-medium text-muted">Recent activity</h2>
        <span class="text-[11px] text-dimmed">from the platform's event feed</span>
      </div>
      <ul class="divide-y divide-muted">
        <li v-for="(event, i) in activity.data.value" :key="i" class="px-4 py-2 flex items-center gap-3 text-sm">
          <UIcon :name="eventIcon(event)" class="size-4 shrink-0" :class="eventTone(event)" />
          <component
            :is="eventTarget(event) ? 'RouterLink' : 'span'"
            v-bind="eventTarget(event) ? { to: eventTarget(event) } : {}"
            class="flex-1 min-w-0 truncate text-toned"
            :class="eventTarget(event) ? 'hover:text-highlighted hover:underline' : ''"
          >
            {{ event.message }}
          </component>
          <span v-if="event.actor && event.actor !== 'operator'" class="text-xs text-dimmed shrink-0">{{
            event.actor
          }}</span>
          <span class="text-xs text-muted shrink-0 tabular-nums">{{ timeAgo(event.timestamp) }}</span>
        </li>
      </ul>
    </div>
  </div>
</template>
