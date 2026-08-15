<script setup lang="ts">
import { computed, onMounted, onScopeDispose, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { APIError, api, type LogLine } from "../lib/api";
import { compactCount, formatBytes } from "../lib/format";
import { useAsync, usePoll } from "../lib/useAsync";
import Sparkline from "../components/Sparkline.vue";
import StatusDot from "../components/StatusDot.vue";

// The observability view does not pretend the logs live anywhere but
// ClickHouse: the query bar takes a real ClickHouse boolean expression over
// the logs table, sent to GET /api/v1/logs and evaluated as written —
// read-only and capped on the operator's side.

const route = useRoute();
const router = useRouter();

const where = ref((route.query.where as string) || "1 = 1");
const limit = ref(200);
const limits = [200, 500, 1000, 5000];

// Kitchen collects every container on every node, so the store also holds the
// logs of things Kitchen did not deploy — the CNI, the CSI sidecars, whatever
// else the cluster runs. They are worth having (a sick node is exactly when
// Kitchen looks broken) and they are not what someone opening this page is
// looking for, so they are scoped out unless asked for. The clause composes
// with the expression rather than editing it: the query bar stays the
// operator's to write.
const clusterClause = "source != 'cluster'";
const includeCluster = ref(route.query.cluster === "1");
const scoped = computed(() => {
  const expression = where.value.trim() || "1 = 1";
  return includeCluster.value ? expression : `(${expression}) AND ${clusterClause}`;
});

const ranges = [
  { label: "Last 15 minutes", value: 15 },
  { label: "Last hour", value: 60 },
  { label: "Last 6 hours", value: 360 },
  { label: "Last 24 hours", value: 1440 },
  { label: "Last 7 days", value: 10080 },
  { label: "All retained", value: 0 },
];
const rangeMinutes = ref(60);

const settings = useAsync(() => api.settings());
const lines = ref<LogLine[] | null>(null);
const error = ref<string | null>(null);
const loading = ref(false);
const liveTail = ref(false);

// The headline numbers over the same store: what the platform served, erred,
// and logged in the last 24 hours, per hour. Traffic rows read "—" until the
// flow pipeline is configured.
const metrics = useAsync(() => api.metricsOverview());
usePoll(() => void metrics.refresh(), 60000, () => true);
const headline = computed(() => {
  const m = metrics.data.value;
  if (!m) return null;
  const traffic = m.requests24h > 0 || m.requestsPerHour.some((v) => v > 0);
  return [
    { label: "requests · 24 h", value: traffic ? compactCount(m.requests24h) : "—", points: m.requestsPerHour },
    {
      label: "errors · 24 h",
      value: traffic ? compactCount(Math.round(m.requests24h * m.errorRate24h)) : "—",
      points: m.errorsPerHour,
      tone: "text-error",
    },
    {
      label: "p95",
      value: traffic && m.p95Ms24h > 0 ? `${Math.round(m.p95Ms24h)} ms` : "—",
      points: m.p95MsPerHour,
    },
    { label: "log lines · 24 h", value: compactCount(m.logLines24h), points: m.logLinesPerHour },
  ];
});

const columns = "timestamp · source · project · environment · build · pod · container · stream · level · message";

// The live tail prefers the streamed endpoint (SSE on the same /logs, the
// where expression applied to every new line) and falls back to re-running
// the query every few seconds when the stream cannot be established.
const streamBroken = ref(false);
let controller: AbortController | undefined;
const streaming = ref(false);

function stopStream() {
  controller?.abort();
  controller = undefined;
  streaming.value = false;
}
onScopeDispose(stopStream);

function startStream() {
  stopStream();
  const mine = new AbortController();
  controller = mine;
  streaming.value = true;
  const since = rangeMinutes.value > 0 ? new Date(Date.now() - rangeMinutes.value * 60000).toISOString() : undefined;
  lines.value = [];
  void api
    .streamLogs(
      scoped.value,
      { limit: limit.value, since },
      (line) => {
        if (controller !== mine || !lines.value) return;
        lines.value.push(line);
        if (lines.value.length > 5000) lines.value.splice(0, lines.value.length - 5000);
      },
      mine.signal,
    )
    .catch((err) => {
      if (controller !== mine) return;
      stopStream();
      if (err instanceof APIError && err.status === 400) {
        // The expression itself was refused — surface it, don't poll it.
        error.value = err.message;
        liveTail.value = false;
        return;
      }
      streamBroken.value = true;
      void run();
    });
}

function toggleCluster() {
  includeCluster.value = !includeCluster.value;
  void run();
}

function toggleLiveTail() {
  liveTail.value = !liveTail.value;
  if (liveTail.value && !streamBroken.value) startStream();
  else {
    stopStream();
    void run();
  }
}

async function run() {
  if (streaming.value) {
    startStream();
    return;
  }
  loading.value = true;
  // The expression and the scope are part of the address, so a query can be
  // linked to.
  const query: Record<string, string> = {};
  if (where.value !== "1 = 1") query.where = where.value;
  if (includeCluster.value) query.cluster = "1";
  void router.replace({ query });
  try {
    const since =
      rangeMinutes.value > 0 ? new Date(Date.now() - rangeMinutes.value * 60000).toISOString() : undefined;
    lines.value = await api.logs(scoped.value, { limit: limit.value, since });
    error.value = null;
  } catch (err) {
    if (err instanceof APIError && err.status === 401) {
      void router.push({ name: "login", query: { returnTo: route.fullPath } });
      return;
    }
    error.value = err instanceof Error ? err.message : String(err);
    lines.value = null;
  } finally {
    loading.value = false;
  }
}

onMounted(run);
usePoll(() => void run(), 5000, () => liveTail.value && !streaming.value && !loading.value);

// Facets are counted client-side over the returned page: honest about what
// they are, and free.
const facets = computed(() => {
  const count = (pick: (line: LogLine) => string) => {
    const counts = new Map<string, number>();
    for (const line of lines.value ?? []) {
      const key = pick(line) || "—";
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8);
  };
  return {
    level: count((l) => l.level ?? ""),
    stream: count((l) => l.stream),
    source: count((l) => (l.environment ? `${l.environment}` : l.build ? `${l.build}` : l.source)),
  };
});

function levelClass(line: LogLine): string {
  if (line.level === "error" || line.level === "fatal") return "text-error";
  if (line.level === "warn") return "text-warning";
  if (line.stream === "stderr") return "text-error";
  return "text-toned";
}

function addClause(clause: string) {
  where.value = where.value.trim() === "1 = 1" || !where.value.trim() ? clause : `${where.value} AND ${clause}`;
  void run();
}

function time(line: LogLine): string {
  const date = new Date(line.timestamp);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleTimeString("en-GB");
}
</script>

<template>
  <div class="space-y-5">
    <div class="flex items-center justify-between gap-4 flex-wrap">
      <div>
        <h1 class="text-xl font-semibold text-highlighted">Observability</h1>
        <p class="text-xs text-muted mt-1">
          ClickHouse<template v-if="settings.data.value?.logRetentionDays">
            · {{ settings.data.value.logRetentionDays }} day retention</template
          ><template v-if="metrics.data.value?.storeBytes">
            · {{ formatBytes(metrics.data.value.storeBytes) }} ·
            {{ Math.round(metrics.data.value.storeRowsPerSecond) }} rows/s</template
          >
          — queried as ClickHouse: a boolean expression over the
          <span class="font-mono">logs</span> table, run read-only.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <USelect v-model="rangeMinutes" :items="ranges" size="sm" class="w-44" />
        <UButton
          size="sm"
          :color="includeCluster ? 'primary' : 'neutral'"
          :variant="includeCluster ? 'soft' : 'subtle'"
          icon="i-lucide-server"
          :title="
            includeCluster
              ? 'Showing everything on the node, Kitchen\'s and the cluster\'s'
              : 'Showing Kitchen\'s own logs. The cluster\'s other pods are collected too.'
          "
          @click="toggleCluster"
        >
          Cluster
        </UButton>
        <UButton
          size="sm"
          :color="liveTail ? 'success' : 'neutral'"
          :variant="liveTail ? 'soft' : 'subtle'"
          @click="toggleLiveTail"
        >
          <StatusDot :tone="liveTail ? 'success' : 'neutral'" :pulse="liveTail" class="mr-1" />
          {{ streaming ? "Streaming" : "Live tail" }}
        </UButton>
      </div>
    </div>

    <!-- What the store saw in the last 24 hours, hourly. -->
    <div v-if="headline" class="grid grid-cols-2 lg:grid-cols-4 gap-3">
      <div
        v-for="tile in headline"
        :key="tile.label"
        class="rounded-md border border-default px-3 py-2 flex items-center justify-between gap-3"
      >
        <div class="min-w-0">
          <p class="text-[11px] text-muted truncate">{{ tile.label }}</p>
          <p class="text-sm font-semibold text-highlighted tabular-nums">{{ tile.value }}</p>
        </div>
        <Sparkline :points="tile.points" :width="72" :height="20" :tone="tile.tone" />
      </div>
    </div>

    <div class="flex items-stretch gap-2">
      <div
        class="flex-1 flex items-center gap-2 rounded-md border border-default bg-muted px-3 focus-within:border-accented"
      >
        <span class="font-mono text-xs text-info select-none">where</span>
        <input
          v-model="where"
          class="flex-1 bg-transparent py-2 font-mono text-sm text-highlighted outline-none placeholder:text-dimmed"
          :placeholder="`project = 'shop' AND stream = 'stderr'`"
          spellcheck="false"
          @keydown.enter="run"
        />
      </div>
      <USelect v-model="limit" :items="limits" size="sm" class="w-24" />
      <UButton icon="i-lucide-play" :loading="loading" @click="run">Run</UButton>
    </div>
    <p class="text-[11px] text-dimmed font-mono -mt-3">
      columns: {{ columns }}
      <template v-if="!includeCluster"> · scoped with AND {{ clusterClause }}</template>
    </p>

    <UAlert
      v-if="error"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      title="ClickHouse refused the query"
      :description="error"
      class="[&_p]:font-mono [&_p]:text-xs"
    />

    <div class="flex gap-4 items-start">
      <div class="flex-1 min-w-0 rounded-md border border-default bg-muted font-mono text-xs leading-5 overflow-auto max-h-[36rem]">
        <div v-if="!lines?.length" class="px-3 py-6 text-center text-muted">
          {{ loading ? "Running…" : "No lines match the expression in this window." }}
        </div>
        <table v-else class="w-full">
          <tbody>
            <tr v-for="(line, i) in lines" :key="i" class="hover:bg-elevated/50 align-top">
              <td class="px-3 py-0.5 text-dimmed whitespace-nowrap select-none">{{ time(line) }}</td>
              <td class="px-2 py-0.5 whitespace-nowrap" :class="line.stream === 'stderr' ? 'text-error' : 'text-muted'">
                {{ line.environment || line.build || line.source }}
              </td>
              <td class="px-2 py-0.5 whitespace-nowrap select-none" :class="levelClass(line)">
                {{ line.level || "" }}
              </td>
              <td class="px-2 py-0.5 whitespace-pre-wrap break-all w-full" :class="levelClass(line)">
                {{ line.message }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <aside v-if="lines?.length" class="w-56 shrink-0 space-y-4 text-xs">
        <div>
          <p class="text-muted mb-1.5">Level</p>
          <button
            v-for="[value, count] in facets.level"
            :key="value"
            class="flex items-center justify-between w-full px-2 py-1 rounded hover:bg-elevated text-left"
            :disabled="value === '—'"
            @click="value !== '—' && addClause(`level = '${value}'`)"
          >
            <span
              class="font-mono"
              :class="
                value === 'error' || value === 'fatal'
                  ? 'text-error'
                  : value === 'warn'
                    ? 'text-warning'
                    : 'text-toned'
              "
              >{{ value }}</span
            >
            <span class="text-dimmed font-mono">{{ count }}</span>
          </button>
        </div>
        <div>
          <p class="text-muted mb-1.5">Stream</p>
          <button
            v-for="[value, count] in facets.stream"
            :key="value"
            class="flex items-center justify-between w-full px-2 py-1 rounded hover:bg-elevated text-left"
            @click="addClause(`stream = '${value}'`)"
          >
            <span class="font-mono" :class="value === 'stderr' ? 'text-error' : 'text-toned'">{{ value }}</span>
            <span class="text-dimmed font-mono">{{ count }}</span>
          </button>
        </div>
        <div>
          <p class="text-muted mb-1.5">Source</p>
          <div
            v-for="[value, count] in facets.source"
            :key="value"
            class="flex items-center justify-between px-2 py-1"
          >
            <span class="font-mono text-toned truncate" :title="value">{{ value }}</span>
            <span class="text-dimmed font-mono ml-2">{{ count }}</span>
          </div>
        </div>
        <p class="text-dimmed leading-relaxed">Counts are over the returned page, not the whole table.</p>
      </aside>
    </div>
  </div>
</template>
