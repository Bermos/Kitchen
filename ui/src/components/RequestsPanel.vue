<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type RouteSort } from "../lib/api";
import { bucketLabel, compactCount } from "../lib/format";
import { renderClause } from "../lib/logquery";
import {
  anyHTTP2,
  deployMarks,
  edgeState,
  formatLatency,
  formatPercent,
  formatRate,
  formatSaturation,
  rawRetentionStart,
  saturation,
  type SignalTile,
} from "../lib/requests";
import { useAsync, usePoll } from "../lib/useAsync";
import GoldenSignals from "./GoldenSignals.vue";
import LogHistogram from "./LogHistogram.vue";
import RequestList from "./RequestList.vue";
import ResourceChart from "./ResourceChart.vue";
import RouteTable from "./RouteTable.vue";

// What the internet asked of this environment: the golden signals, the routes
// it asked them of, and the requests themselves.
//
// None of it costs the application anything — every request to every Kitchen
// application crosses the shared Gateway's proxy, so an application nobody
// instrumented still has traffic, error and latency numbers, observed where
// they all pass anyway.
//
// The section is honest about the two ways that can be nothing (§3.4 of the
// observability design). An environment with no route on the shared Gateway is
// not on the platform's edge at all, and four empty charts would describe the
// platform rather than the application: it gets the sentence that says so, and
// the header leads with what is real for every workload instead — what it
// wrote, what it used against its limits, and how often it restarted. An
// environment that *is* published and simply served nothing is a different
// sentence, and keeps its charts, because a gap in a traffic chart is the most
// interesting shape there is.

const props = defineProps<{
  environment: string;
  /** Whose activity feed the deploy marks come from. */
  project: string;
  /** Follow while something is moving — a deploying environment. */
  live?: boolean;
}>();

/** How many of the newest raw rows the protocol question is answered from. It
 * is a sample rather than a scan because the store cannot be asked "any" — and
 * a sample is enough for a footnote that is only ever added, never withheld on
 * the strength of not having seen one. */
const PROTOCOL_SAMPLE = 500;

const ranges = [
  { label: "Last 15 minutes", value: 15 },
  { label: "Last hour", value: 60 },
  { label: "Last 6 hours", value: 360 },
  { label: "Last 24 hours", value: 1440 },
  { label: "Last 7 days", value: 10080 },
];
const rangeMinutes = ref(60);

/** The route the table selected. It filters the header, the charts and the
 * request list — but never the table itself, which is what does the selecting. */
const selectedRoute = ref<string | null>(null);
const sort = ref<RouteSort>("requests");

/**
 * The window's start, held rather than recomputed per request: the header, the
 * charts, the routes and the listing are views of one window, and four reads
 * each taking their own `now` would show four windows that disagree.
 */
const windowStart = ref(new Date(Date.now() - rangeMinutes.value * 60_000).toISOString());

const scope = () => ({ since: windowStart.value, route: selectedRoute.value ?? undefined });

const summary = useAsync(() => api.requestSummary(props.environment, scope()));
const series = useAsync(() => api.requestSeries(props.environment, { ...scope(), buckets: 60 }));
// Deliberately unfiltered: the table is the picker, and a table narrowed to the
// row that was clicked has one row.
const routes = useAsync(() =>
  api.requestRoutes(props.environment, { since: windowStart.value, sort: sort.value, limit: 100 }),
);
// Saturation is the fourth golden signal, and it is the one the request
// pipeline cannot answer: it comes off the same metrics endpoint the history
// charts below read.
const resources = useAsync(() => api.environmentMetrics(props.environment, { since: windowStart.value, points: 60 }));
// The activity feed, for the deploy marks. A request row cannot name the
// release that served it, so lining a change up with a moment in time is the
// only honest way to see one.
const events = useAsync(() => api.events({ project: props.project, limit: 200 }));
// Whether this environment serves HTTP/2 at all — read separately from
// everything above, and deliberately so. See `http2` below.
const protocols = useAsync(() =>
  api.requests(props.environment, { since: rawRetentionStart(), limit: PROTOCOL_SAMPLE }),
);
// Only for an environment the edge does not reach: what it wrote, since that is
// what is real for a worker.
const logShape = useAsync(
  () =>
    api.logHistogram(
      {
        q: renderClause({ field: "environment", value: props.environment, negated: false }),
        since: windowStart.value,
      },
      60,
    ),
  { immediate: false },
);

const edge = computed(() => summary.data.value?.edge ?? series.data.value?.edge);
const state = computed(() => edgeState(edge.value, summary.data.value?.requests));
const offEdge = computed(() => state.value.kind === "off-edge");
watch(offEdge, (off) => {
  if (off && !logShape.data.value) void logShape.refresh();
});

function reload() {
  windowStart.value = new Date(Date.now() - rangeMinutes.value * 60_000).toISOString();
  void summary.refresh();
  void series.refresh();
  void routes.refresh();
  void resources.refresh();
  void events.refresh();
  // Its own window, not this one: it is a question about the environment.
  void protocols.refresh();
  if (offEdge.value) void logShape.refresh();
}

watch([() => props.environment, rangeMinutes], ([environment], [previous]) => {
  if (environment !== previous) {
    // A route template belongs to the environment it was read off; carrying one
    // across would filter this environment by another's paths. The same goes
    // for what the last environment's listing was seen serving.
    selectedRoute.value = null;
    listingSawHTTP2.value = false;
  }
  reload();
});
// The route filter moves the header, the charts and the listing; the table and
// the resource series are the same either way.
watch(selectedRoute, () => {
  void summary.refresh();
  void series.refresh();
});
watch(sort, () => void routes.refresh());
// Never faster than a bucket, and only while something is moving.
usePoll(reload, 30_000, () => props.live === true);

const points = computed(() => series.data.value?.points ?? []);
const resourcePoints = computed(() => resources.data.value?.points ?? []);

const cpuSaturation = computed(() =>
  resourcePoints.value.length
    ? saturation(Math.max(...resourcePoints.value.map((p) => p.cpuPeakCores)), resources.data.value?.cpuLimitCores)
    : null,
);
const memorySaturation = computed(() =>
  resourcePoints.value.length
    ? saturation(Math.max(...resourcePoints.value.map((p) => p.memoryPeakBytes)), resources.data.value?.memoryLimitBytes)
    : null,
);

/** The fourth tile, whichever four are on screen: it is the one signal that is
 * true of every workload, edge or no edge. */
const saturationTile = computed<SignalTile>(() => {
  const capped = cpuSaturation.value !== null || memorySaturation.value !== null;
  return {
    label: "Saturation",
    value: capped ? `${formatSaturation(cpuSaturation.value)} · ${formatSaturation(memorySaturation.value)}` : "—",
    detail: capped
      ? "peak CPU · memory, against the release's limits"
      : "no CPU or memory limit set on this release, so there is no ceiling to measure against",
    points: resourcePoints.value.map((point) => point.memoryPeakBytes),
    tone: "text-info",
  };
});

const requestTiles = computed<SignalTile[]>(() => {
  const answer = summary.data.value;
  return [
    {
      label: "Requests",
      value: answer ? formatRate(answer.requestsPerSecond) : "—",
      detail: answer ? `${compactCount(answer.requests)} in this window` : "",
      points: points.value.map((point) => point.requestsPerSecond),
    },
    {
      label: "Error rate",
      value: answer ? formatPercent(answer.errorRate) : "—",
      detail: answer ? `${compactCount(answer.errors)} answered 500 or above` : "",
      points: points.value.map((point) => point.errorRate),
      tone: "text-error",
    },
    {
      label: "p95 latency",
      value: formatLatency(answer?.p95Ms),
      detail: answer ? `p50 ${formatLatency(answer.p50Ms)} · p99 ${formatLatency(answer.p99Ms)}` : "",
      points: points.value.map((point) => point.p95Ms),
    },
    saturationTile.value,
  ];
});

/** The header for a workload the golden signals do not fit: log volume and
 * error-line rate stand in for traffic and errors — for a queue worker they are
 * the liveness proxy — and restarts for the thing that actually goes wrong. */
const workloadTiles = computed<SignalTile[]>(() => {
  const shape = logShape.data.value;
  const buckets = shape?.buckets ?? [];
  const errorLines = buckets.reduce((total, bucket) => total + bucket.errors, 0);
  const restarts = resources.data.value?.restarts ?? 0;
  const oomKills = resources.data.value?.oomKills ?? 0;
  return [
    {
      label: "Log lines",
      value: shape ? compactCount(shape.total) : "—",
      detail: "what it wrote in this window",
      points: buckets.map((bucket) => bucket.count),
    },
    {
      label: "Error lines",
      value: shape ? compactCount(errorLines) : "—",
      detail: shape?.total ? `${formatPercent(errorLines / shape.total)} of its output` : "nothing logged in this window",
      points: buckets.map((bucket) => bucket.errors),
      tone: "text-error",
    },
    {
      label: "Restarts",
      value: compactCount(restarts),
      detail: oomKills ? `${oomKills} OOM kill${oomKills === 1 ? "" : "s"} in this window` : "in this window",
      points: resourcePoints.value.map((point) => point.restarts + point.oomKills),
      tone: oomKills ? "text-error" : "text-warning",
    },
    saturationTile.value,
  ];
});

const marks = computed(() =>
  deployMarks(
    (events.data.value ?? []).filter((event) => event.environment === props.environment),
    points.value,
    series.data.value?.bucketSeconds ?? 0,
  ),
);

const traffic = computed(() => points.value.map((point) => ({ start: point.start, value: point.requestsPerSecond })));
const errorRate = computed(() => points.value.map((point) => ({ start: point.start, value: point.errorRate })));
const latency = computed(() =>
  points.value.map((point) => ({ start: point.start, base: point.p50Ms, value: point.p95Ms, peak: point.p99Ms })),
);

/**
 * Whether this *environment* serves HTTP/2, which is what the error column's
 * footnote is a statement about: a failed gRPC call is an HTTP 200 with a
 * trailer the edge does not read, so the route table's error counts are
 * transport-level for such a service whichever route is selected and whichever
 * page of the listing happens to be on screen. Driving the footnote off the
 * listing took it away from a table still full of the rows it warns about.
 *
 * Protocol lives only on the raw rows — the rollups the table reads carry no
 * protocol column at all (§5 of the observability design, and API.md says so
 * on the request row) — so this is a second, deliberately unfiltered read of
 * the listing endpoint over the whole span raw rows can answer, rather than
 * over the window the section is showing.
 *
 * A sample of the newest rows can only ever prove the footnote *needed*, never
 * that it is not, so what the listing below happens to show counts as well:
 * `http2` only ever goes up, and is reset when the environment changes.
 */
const listingSawHTTP2 = ref(false);
const http2 = computed(() => listingSawHTTP2.value || anyHTTP2(protocols.data.value?.items));

const resolution = computed(() => bucketLabel(series.data.value?.bucketSeconds));

/** An installation that runs without telemetry has no requests to show, and a
 * section of error alerts would be worse than no section at all. */
const unavailable = computed(() => (summary.error.value ?? "").includes("telemetry store"));
const failed = computed(() => (unavailable.value ? null : summary.error.value));

/** The window the summary answered, which is not always the one that was asked
 * for: these numbers come off indivisible buckets, so the start is snapped. */
function clock(iso: string | undefined): string {
  const date = iso ? new Date(iso) : null;
  return date && !Number.isNaN(date.getTime()) ? date.toLocaleTimeString("en-GB") : "—";
}
</script>

<template>
  <div v-if="!unavailable">
    <div class="flex items-center justify-between gap-3 mb-2 flex-wrap">
      <h2 class="text-sm font-medium text-highlighted">Signals</h2>
      <div class="flex items-center gap-2">
        <!-- The window these numbers are true of is the one the store could
             answer, not the one that was asked for: they come off indivisible
             buckets, so the start is snapped to the rollup's resolution. -->
        <span
          v-if="summary.data.value"
          class="text-[11px] text-dimmed font-mono"
          :title="`answered over ${clock(summary.data.value.since)} – ${clock(summary.data.value.until)}, snapped to the ${summary.data.value.rollup} rollup's buckets`"
        >
          {{ resolution }}<template v-if="summary.data.value.rollup"> · {{ summary.data.value.rollup }} rollup</template>
        </span>
        <USelect v-model="rangeMinutes" :items="ranges" size="xs" class="w-36 sm:w-40" />
        <UButton
          icon="i-lucide-refresh-cw"
          size="xs"
          color="neutral"
          variant="ghost"
          :loading="summary.loading.value"
          aria-label="Refresh the signals"
          @click="reload"
        />
      </div>
    </div>

    <UAlert v-if="failed" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="failed" class="mb-3" />

    <div v-else class="space-y-3">
      <!-- A route that could not be read is not evidence about the
           application: the numbers stand, and the screen says the check did not
           happen rather than declaring anything off the edge. -->
      <p v-if="state.caveat" class="text-[11px] text-warning">{{ state.caveat }}</p>

      <GoldenSignals :tiles="offEdge ? workloadTiles : requestTiles" />

      <!-- Nothing publishes this environment: say so, and lead with what is
           real for such a workload instead. -->
      <template v-if="offEdge">
        <UAlert
          color="neutral"
          variant="soft"
          icon="i-lucide-unplug"
          title="No HTTP traffic reaches this environment through the platform's edge"
          :description="state.message"
        />
        <h3 class="text-xs font-medium text-highlighted">
          What it wrote <span class="text-dimmed font-normal">— the liveness a worker actually has</span>
        </h3>
        <LogHistogram :histogram="logShape.data.value" :loading="logShape.loading.value" />
        <p class="text-[11px] text-dimmed">
          Its usage against the release's limits and its restarts are in History below, and the lines themselves are in
          Runtime logs at the foot of this page. A worker's connection to its broker is real traffic too — it just never
          crosses the Gateway, so the traffic map sees it as flows rather than as requests.
        </p>
      </template>

      <template v-else>
        <div v-if="selectedRoute" class="flex items-center gap-2 text-[11px]">
          <span class="text-dimmed">Filtered to</span>
          <button
            class="font-mono px-1.5 py-0.5 rounded border border-default text-toned hover:border-accented hover:text-error"
            title="Show every route again"
            @click="selectedRoute = null"
          >
            {{ selectedRoute }} ×
          </button>
          <span class="text-dimmed">— the header, the charts and the list below; the table stays whole.</span>
        </div>

        <p v-if="state.kind === 'quiet'" class="text-xs text-muted">
          No traffic in this window. This environment is published on the shared Gateway — nothing was asked of it
          between {{ clock(summary.data.value?.since) }} and {{ clock(summary.data.value?.until) }}.
        </p>

        <div class="grid gap-3 lg:grid-cols-3">
          <ResourceChart label="Traffic" hint="requests/s" :points="traffic" :format="formatRate" :marks="marks" />
          <ResourceChart
            label="Errors"
            hint="5xx share"
            :points="errorRate"
            :format="formatPercent"
            :marks="marks"
            tone="text-error"
          />
          <ResourceChart
            label="Latency"
            hint="p50 · p95 · p99"
            :points="latency"
            :format="formatLatency"
            :marks="marks"
            tone="text-info"
          />
        </div>
        <p class="text-[11px] text-dimmed">
          Marks are deploys from the activity feed, amber ones rollbacks. They are lined up by time rather than joined:
          the edge routes to a Service, so no request can name the release that served it.
        </p>

        <h3 class="text-xs font-medium text-highlighted pt-1">
          Routes
          <span class="text-dimmed font-normal">— one row per template; click one to filter everything else by it</span>
        </h3>
        <RouteTable
          :routes="routes.data.value?.items ?? null"
          :sort="sort"
          :selected="selectedRoute"
          :loading="routes.loading.value"
          :grpc="http2"
          @select="(route) => (selectedRoute = route)"
          @update:sort="(next) => (sort = next)"
        />

        <h3 class="text-xs font-medium text-highlighted pt-1">
          Requests
          <span class="text-dimmed font-normal">— newest first; a row opens onto the lines written around it</span>
        </h3>
        <RequestList
          :environment="environment"
          :since="windowStart"
          :route="selectedRoute"
          @http2="listingSawHTTP2 = true"
        />
      </template>
    </div>
  </div>
</template>
