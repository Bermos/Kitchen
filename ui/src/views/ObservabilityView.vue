<script setup lang="ts">
import { computed, onMounted, onScopeDispose, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import {
  APIError,
  api,
  type LogFacet,
  type LogHistogram as LogHistogramData,
  type LogLine,
  type LogPattern,
  type LogSelection,
} from "../lib/api";
import { compactCount, formatBytes } from "../lib/format";
import { clausesOf, hasClause, isEditable, removeClause, toggleClause, type Clause } from "../lib/logquery";
import { useAsync, usePoll } from "../lib/useAsync";
import LogHistogram from "../components/LogHistogram.vue";
import Sparkline from "../components/Sparkline.vue";
import StatusDot from "../components/StatusDot.vue";

// The observability view asks one selection four ways: the lines, when they
// happened, what else is in them, and what they are actually saying. The
// selection is the query bar plus the window, and it is entirely in the URL, so
// what is on screen is always a link.
//
// The query bar speaks Kitchen's log query language — `level:error
// service:shop` — and can be switched to raw ClickHouse, which is genuinely
// more powerful and is not the front door. Neither has a default: an empty bar
// asks for everything in the window, which is the question someone opening this
// page is asking.

const route = useRoute();
const router = useRouter();

type Mode = "query" | "clickhouse";
const mode = ref<Mode>(route.query.where ? "clickhouse" : "query");
const query = ref((route.query.q as string) ?? "");
const where = ref((route.query.where as string) ?? "");
const limit = ref(Number(route.query.limit) || 200);
const limits = [200, 500, 1000, 5000];
const tab = ref<"lines" | "patterns">(route.query.view === "patterns" ? "patterns" : "lines");

// Kitchen collects every container on every node, so the store also holds the
// logs of things Kitchen did not deploy — the CNI, the CSI sidecars, whatever
// else the cluster runs. They are worth having (a sick node is exactly when
// Kitchen looks broken) and they are not what someone opening this page is
// looking for, so they are scoped out unless asked for. The clause rides in the
// query language even in ClickHouse mode: the two surfaces compose with AND
// server-side, so the bar stays the operator's to write.
const clusterClause: Clause = { field: "source", value: "cluster", negated: true };
const includeCluster = ref(route.query.cluster === "1");

const ranges = [
  { label: "Last 15 minutes", value: 15 },
  { label: "Last hour", value: 60 },
  { label: "Last 6 hours", value: 360 },
  { label: "Last 24 hours", value: 1440 },
  { label: "Last 7 days", value: 10080 },
  { label: "All retained", value: 0 },
];
const rangeMinutes = ref(Number(route.query.range ?? 60));
// A range dragged out of the histogram is absolute, and stops following the
// clock until a preset is chosen again.
const pinned = ref<{ since: string; until: string } | null>(
  route.query.since && route.query.until
    ? { since: route.query.since as string, until: route.query.until as string }
    : null,
);

const settings = useAsync(() => api.settings());
const lines = ref<LogLine[] | null>(null);
const histogram = ref<LogHistogramData | null>(null);
const facets = ref<LogFacet[]>([]);
const patterns = ref<LogPattern[]>([]);
const error = ref<string | null>(null);
const loading = ref(false);
const liveTail = ref(false);
const expanded = ref<number | null>(null);

/** What every request this page makes is asked over. */
function selection(): LogSelection {
  const scoped = includeCluster.value ? query.value : toQueryWithCluster();
  const window = pinned.value ?? {
    since: rangeMinutes.value > 0 ? new Date(Date.now() - rangeMinutes.value * 60000).toISOString() : undefined,
    until: undefined,
  };
  return {
    q: scoped.trim() || undefined,
    where: mode.value === "clickhouse" ? where.value.trim() || undefined : undefined,
    since: window.since,
    until: window.until,
  };
}

function toQueryWithCluster(): string {
  return hasClause(query.value, clusterClause) ? query.value : toggleClause(query.value, clusterClause);
}

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

// The live tail prefers the streamed endpoint (SSE on the same /logs, the
// selection applied to every new line) and falls back to re-running the query
// every few seconds when the stream cannot be established.
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
  lines.value = [];
  void api
    .streamLogs(
      selection(),
      limit.value,
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
        // The query itself was refused — surface it, don't poll it.
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

function setMode(next: Mode) {
  if (mode.value === next) return;
  mode.value = next;
  void run();
}

/** The selection, in the address bar. A query on screen is always a link. */
function syncURL() {
  const params: Record<string, string> = {};
  if (query.value.trim()) params.q = query.value.trim();
  if (mode.value === "clickhouse" && where.value.trim()) params.where = where.value.trim();
  if (includeCluster.value) params.cluster = "1";
  if (limit.value !== 200) params.limit = String(limit.value);
  if (tab.value !== "lines") params.view = tab.value;
  if (pinned.value) {
    params.since = pinned.value.since;
    params.until = pinned.value.until;
  } else if (rangeMinutes.value !== 60) {
    params.range = String(rangeMinutes.value);
  }
  void router.replace({ query: params });
}

/**
 * Ask the selection. `full` asks it every way — which is what a new query or a
 * new window means. The polling fallback for the live tail asks only for the
 * lines: the chart and the facets over an unchanged selection are the same
 * chart and the same facets, and re-asking them every five seconds is four
 * aggregates a second the store does not owe anyone.
 */
async function run(full: unknown = true) {
  if (streaming.value) {
    startStream();
    return;
  }
  loading.value = true;
  expanded.value = null;
  const asked = selection();
  try {
    if (!full) {
      lines.value = await api.logs(asked, limit.value);
      error.value = null;
      return;
    }
    syncURL();
    // The rest go out together: they are views of one selection, and showing
    // them at different ages would make them disagree.
    const [answered, counted, faceted, clustered] = await Promise.all([
      api.logs(asked, limit.value),
      api.logHistogram(asked),
      api.logFacets(asked),
      tab.value === "patterns" ? api.logPatterns(asked) : Promise.resolve([] as LogPattern[]),
    ]);
    lines.value = answered;
    histogram.value = counted;
    facets.value = faceted;
    patterns.value = clustered;
    error.value = null;
  } catch (err) {
    if (err instanceof APIError && err.status === 401) {
      void router.push({ name: "login", query: { returnTo: route.fullPath } });
      return;
    }
    error.value = err instanceof Error ? err.message : String(err);
    lines.value = null;
    histogram.value = null;
  } finally {
    loading.value = false;
  }
}

onMounted(run);
usePoll(() => void run(false), 5000, () => liveTail.value && !streaming.value && !loading.value);

watch(tab, (next) => {
  if (next === "patterns" && !patterns.value.length) void run();
  else syncURL();
});

/** A preset range releases whatever the histogram pinned. */
function chooseRange(minutes: number) {
  // The "Selected range" entry only exists to show what is pinned; choosing it
  // again is not a change.
  if (minutes < 0) return;
  rangeMinutes.value = minutes;
  pinned.value = null;
  void run();
}

function onHistogramSelect(range: { since: string; until: string }) {
  pinned.value = range;
  void run();
}

// Facets narrow by clicking. Each value is a toggle, so the way out of a filter
// is the thing that put you in it — and it only works on a query flat enough to
// edit without changing what it means.
const editable = computed(() => isEditable(query.value));
const activeClauses = computed(() => clausesOf(query.value));

function narrow(field: string, value: string) {
  if (!editable.value) return;
  query.value = toggleClause(query.value, { field, value, negated: false });
  void run();
}

function drop(clause: Clause) {
  query.value = removeClause(query.value, clause);
  void run();
}

function isActive(field: string, value: string): boolean {
  return hasClause(query.value, { field, value, negated: false });
}

function facetLabel(field: string): string {
  return field.charAt(0).toUpperCase() + field.slice(1);
}

function levelClass(level: string | undefined, stream?: string): string {
  if (level === "error" || level === "fatal") return "text-error";
  if (level === "warn") return "text-warning";
  if (stream === "stderr") return "text-error";
  return "text-toned";
}

function time(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleTimeString("en-GB");
}

function fieldsOf(line: LogLine): [string, string][] {
  return Object.entries(line.fields ?? {}).sort((a, b) => a[0].localeCompare(b[0]));
}

const placeholder = computed(() =>
  mode.value === "query" ? `level:error service:shop` : `project = 'shop' AND stream = 'stderr'`,
);
</script>

<template>
  <div class="space-y-4">
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
        </p>
      </div>
      <div class="flex items-center gap-2">
        <USelect
          :model-value="pinned ? -1 : rangeMinutes"
          :items="pinned ? [{ label: 'Selected range', value: -1 }, ...ranges] : ranges"
          size="sm"
          class="w-44"
          @update:model-value="chooseRange"
        />
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
        <button
          class="font-mono text-xs shrink-0 select-none"
          :class="mode === 'query' ? 'text-info' : 'text-warning'"
          :title="
            mode === 'query'
              ? 'Kitchen query syntax. Click to write ClickHouse instead.'
              : 'A ClickHouse expression, evaluated as written. Click to go back to query syntax.'
          "
          @click="setMode(mode === 'query' ? 'clickhouse' : 'query')"
        >
          {{ mode === "query" ? "search" : "where" }}
        </button>
        <input
          v-if="mode === 'query'"
          v-model="query"
          class="flex-1 bg-transparent py-2 font-mono text-sm text-highlighted outline-none placeholder:text-dimmed"
          :placeholder="placeholder"
          spellcheck="false"
          @keydown.enter="run"
        />
        <input
          v-else
          v-model="where"
          class="flex-1 bg-transparent py-2 font-mono text-sm text-highlighted outline-none placeholder:text-dimmed"
          :placeholder="placeholder"
          spellcheck="false"
          @keydown.enter="run"
        />
      </div>
      <USelect v-model="limit" :items="limits" size="sm" class="w-24" @update:model-value="run" />
      <UButton icon="i-lucide-play" :loading="loading" @click="run">Run</UButton>
    </div>

    <!-- What the query is currently narrowed by, and the way back out. -->
    <div class="flex items-center gap-2 flex-wrap -mt-2 text-[11px]">
      <button
        v-for="clause in activeClauses"
        :key="`${clause.negated}${clause.field}${clause.value}`"
        class="font-mono px-1.5 py-0.5 rounded border border-default text-toned hover:border-accented hover:text-error"
        title="Remove"
        @click="drop(clause)"
      >
        {{ clause.negated ? "−" : "" }}{{ clause.field }}:{{ clause.value }} ×
      </button>
      <span v-if="mode === 'query'" class="text-dimmed">
        <template v-if="!activeClauses.length">
          <span class="font-mono">level:error</span> · <span class="font-mono">service:shop</span> ·
          <span class="font-mono">http.status:&gt;=500</span> · <span class="font-mono">-source:cluster</span>
        </template>
      </span>
      <span v-else class="text-dimmed font-mono">
        columns: timestamp · source · project · environment · build · pod · container · stream · level · traceId ·
        spanId · message · fields
      </span>
    </div>

    <UAlert
      v-if="error"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="mode === 'query' ? 'The query could not be run' : 'ClickHouse refused the query'"
      :description="error"
      class="[&_p]:font-mono [&_p]:text-xs"
    />

    <LogHistogram :histogram="histogram" :loading="loading" @select="onHistogramSelect" />

    <div class="flex gap-4 items-start">
      <div class="flex-1 min-w-0 space-y-2">
        <div class="flex items-center gap-1 text-xs">
          <button
            v-for="view in (['lines', 'patterns'] as const)"
            :key="view"
            class="px-2 py-1 rounded capitalize"
            :class="tab === view ? 'bg-elevated text-highlighted' : 'text-muted hover:text-toned'"
            @click="tab = view"
          >
            {{ view }}
          </button>
          <span v-if="tab === 'patterns'" class="text-dimmed ml-2">
            the newest lines in the window, collapsed to templates
          </span>
        </div>

        <div
          v-if="tab === 'lines'"
          class="rounded-md border border-default bg-muted font-mono text-xs leading-5 overflow-auto max-h-[36rem]"
        >
          <div v-if="!lines?.length" class="px-3 py-6 text-center text-muted">
            {{ loading ? "Running…" : "No lines match in this window." }}
          </div>
          <table v-else class="w-full">
            <tbody>
              <template v-for="(line, i) in lines" :key="i">
                <tr class="hover:bg-elevated/50 align-top cursor-pointer" @click="expanded = expanded === i ? null : i">
                  <td class="px-3 py-0.5 text-dimmed whitespace-nowrap select-none">{{ time(line.timestamp) }}</td>
                  <td
                    class="px-2 py-0.5 whitespace-nowrap"
                    :class="line.stream === 'stderr' ? 'text-error' : 'text-muted'"
                  >
                    {{ line.environment || line.build || line.source }}
                  </td>
                  <td
                    class="px-2 py-0.5 whitespace-nowrap select-none"
                    :class="levelClass(line.level, line.stream)"
                  >
                    {{ line.level || "" }}
                  </td>
                  <td
                    class="px-2 py-0.5 whitespace-pre-wrap break-all w-full"
                    :class="levelClass(line.level, line.stream)"
                  >
                    {{ line.message }}
                    <!-- A line that carries a trace id is a line with a whole
                         request behind it; the mark is what says so before it
                         is expanded. -->
                    <UIcon
                      v-if="line.traceId"
                      name="i-lucide-git-fork"
                      class="size-3 align-[-2px] ml-1 text-dimmed"
                      title="Part of a traced request"
                    />
                  </td>
                </tr>
                <!-- A JSON line's own fields, and each of them a filter — plus
                     the way out to the request the line belongs to. -->
                <tr v-if="expanded === i && (fieldsOf(line).length || line.traceId)" class="bg-elevated/30">
                  <td colspan="4" class="px-3 py-2 space-y-2">
                    <div v-if="line.traceId" class="flex items-center gap-2">
                      <RouterLink
                        :to="{ name: 'traces', query: { trace: line.traceId, range: '1440' } }"
                        class="px-1.5 py-0.5 rounded border border-default hover:border-accented text-primary"
                        title="Open the whole request this line came out of"
                      >
                        <UIcon name="i-lucide-git-fork" class="size-3 align-[-2px]" />
                        trace {{ line.traceId.slice(0, 12) }}
                      </RouterLink>
                      <button
                        class="px-1.5 py-0.5 rounded border border-default hover:border-accented text-toned"
                        :disabled="!editable"
                        title="Every line of this request"
                        @click.stop="narrow('traceId', line.traceId!)"
                      >
                        its other lines
                      </button>
                    </div>
                    <div v-if="fieldsOf(line).length" class="flex flex-wrap gap-1">
                      <button
                        v-for="[key, value] in fieldsOf(line)"
                        :key="key"
                        class="px-1.5 py-0.5 rounded border border-default hover:border-accented text-left"
                        :disabled="!editable"
                        :title="editable ? `Filter on ${key}` : 'Simplify the query to filter by clicking'"
                        @click.stop="narrow(key, value)"
                      >
                        <span class="text-info">{{ key }}</span>
                        <span class="text-dimmed">:</span>
                        <span class="text-toned">{{ value }}</span>
                      </button>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>

        <div v-else class="rounded-md border border-default bg-muted overflow-auto max-h-[36rem]">
          <div v-if="!patterns.length" class="px-3 py-6 text-center text-muted text-xs">
            {{ loading ? "Clustering…" : "No lines to cluster in this window." }}
          </div>
          <table v-else class="w-full text-xs">
            <tbody>
              <tr v-for="pattern in patterns" :key="pattern.pattern" class="hover:bg-elevated/50 align-top">
                <td class="px-3 py-1.5 text-right tabular-nums font-mono text-highlighted whitespace-nowrap">
                  {{ compactCount(pattern.count) }}
                </td>
                <td class="px-2 py-1.5 font-mono whitespace-nowrap select-none" :class="levelClass(pattern.level)">
                  {{ pattern.level || "" }}
                </td>
                <td class="px-2 py-1.5 w-full">
                  <p class="font-mono break-all" :class="levelClass(pattern.level)">{{ pattern.pattern }}</p>
                  <p class="font-mono text-[11px] text-dimmed break-all mt-0.5">{{ pattern.sample }}</p>
                </td>
                <td class="px-3 py-1.5 text-dimmed font-mono whitespace-nowrap text-right">
                  {{ time(pattern.lastSeen) }}
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <aside class="w-56 shrink-0 space-y-4 text-xs">
        <div v-for="facet in facets" :key="facet.field">
          <p class="text-muted mb-1.5">{{ facetLabel(facet.field) }}</p>
          <p v-if="!facet.values.length" class="text-dimmed px-2">—</p>
          <button
            v-for="value in facet.values"
            :key="value.value"
            class="flex items-center justify-between w-full px-2 py-1 rounded text-left hover:bg-elevated disabled:hover:bg-transparent"
            :class="isActive(facet.field, value.value) ? 'bg-elevated' : ''"
            :disabled="!editable"
            :title="editable ? 'Narrow to this' : 'Simplify the query to filter by clicking'"
            @click="narrow(facet.field, value.value)"
          >
            <span class="font-mono truncate" :class="levelClass(facet.field === 'level' ? value.value : undefined)">
              {{ value.value }}
            </span>
            <span class="text-dimmed font-mono ml-2 tabular-nums">{{ compactCount(value.count) }}</span>
          </button>
        </div>
        <p class="text-dimmed leading-relaxed">Counts are over the whole window, not the returned page.</p>
      </aside>
    </div>
  </div>
</template>
