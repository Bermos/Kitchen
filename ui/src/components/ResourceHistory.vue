<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type ResourceSeries } from "../lib/api";
import { bucketLabel, compactCount, formatBytes } from "../lib/format";
import { useAsync, usePoll } from "../lib/useAsync";
import ResourceChart from "./ResourceChart.vue";

// What an environment has been doing, next to what it is doing.
//
// The workload panel above answers the instant — replicas ready, restarts so
// far, the requests the release asked for — and cannot answer anything else,
// because the API server keeps no history. These four charts are the questions
// that need one: was it always using this much memory, did it get OOMKilled
// overnight, when did it scale, is it anywhere near its limit.
//
// The whole panel is absent, rather than empty, on an installation without a
// telemetry store: a row of flat lines would claim the environment used
// nothing.

const props = defineProps<{ environment: string; live?: boolean }>();

const ranges = [
  { label: "Last hour", value: 60 },
  { label: "Last 6 hours", value: 360 },
  { label: "Last 24 hours", value: 1440 },
  { label: "Last 7 days", value: 10080 },
];
const rangeMinutes = ref(60);

const history = useAsync(() =>
  api.environmentMetrics(props.environment, {
    since: new Date(Date.now() - rangeMinutes.value * 60_000).toISOString(),
    points: 120,
  }),
);
watch([() => props.environment, rangeMinutes], () => void history.refresh());
// Only while something is moving, and never faster than a sample arrives.
usePoll(() => void history.refresh(), 30_000, () => props.live === true);

const series = computed<ResourceSeries | null>(() => history.data.value ?? null);
const points = computed(() => series.value?.points ?? []);

/** The events, shared by every chart: a restart is worth seeing against memory
 * as much as against the replica count. */
const marks = computed(() =>
  points.value.flatMap((point) => [
    ...(point.oomKills ? [{ start: point.start, count: point.oomKills, tone: "stroke-error", label: "OOM kill" }] : []),
    ...(point.restarts && !point.oomKills
      ? [{ start: point.start, count: point.restarts, tone: "stroke-warning/70", label: "restart" }]
      : []),
  ]),
);

const cpu = computed(() =>
  points.value.map((point) => ({ start: point.start, value: point.cpuCores, peak: point.cpuPeakCores })),
);
const memory = computed(() =>
  points.value.map((point) => ({ start: point.start, value: point.memoryBytes, peak: point.memoryPeakBytes })),
);
const replicas = computed(() => points.value.map((point) => ({ start: point.start, value: point.replicas })));
const restarts = computed(() =>
  points.value.map((point) => ({ start: point.start, value: point.restarts + point.oomKills })),
);

/** A window with nothing in it at all: sampling is off, or this environment
 * has not run inside it. Either way there is nothing to draw and saying so
 * beats four empty axes. */
const quiet = computed(() => points.value.every((point) => point.replicas === 0));

const cores = (value: number) => (value >= 1 ? `${value.toFixed(2)} cores` : `${Math.round(value * 1000)}m`);
const count = (value: number) => compactCount(value);

/** How coarse the answer actually is, which is not always what was asked for:
 * a wide window is drawn from the five-minute rollup. */
const resolution = computed(() => bucketLabel(series.value?.bucketSeconds));
</script>

<template>
  <!-- No telemetry store, or an operator too old to serve this: the panel is
       simply not part of the page. -->
  <div v-if="!history.error.value || series">
    <div class="flex items-center justify-between gap-3 mb-2 flex-wrap">
      <h2 class="text-sm font-medium text-highlighted">History</h2>
      <div class="flex items-center gap-2">
        <span v-if="series" class="text-[11px] text-dimmed font-mono">
          {{ resolution }}<template v-if="series.rollup"> · rollup</template>
        </span>
        <USelect v-model="rangeMinutes" :items="ranges" size="xs" class="w-32 sm:w-36" />
      </div>
    </div>

    <p v-if="series?.restarts || series?.oomKills" class="text-xs mb-2">
      <span :class="series.oomKills ? 'text-error' : 'text-warning'">
        {{ series.restarts }} restart{{ series.restarts === 1 ? "" : "s" }}
        <template v-if="series.oomKills"> · {{ series.oomKills }} OOM kill{{ series.oomKills === 1 ? "" : "s" }}</template>
      </span>
      <span class="text-dimmed"> in this window</span>
    </p>

    <p v-if="quiet && !history.loading.value" class="rounded-md border border-default bg-muted px-4 py-6 text-center text-xs text-muted">
      No samples in this window — the environment ran no pods, or the platform was not sampling.
    </p>
    <div v-else class="grid gap-3 lg:grid-cols-2">
      <ResourceChart label="CPU" :points="cpu" :format="cores" :limit="series?.cpuLimitCores ?? 0" :marks="marks" />
      <ResourceChart
        label="Memory"
        :points="memory"
        :format="formatBytes"
        :limit="series?.memoryLimitBytes ?? 0"
        :marks="marks"
        tone="text-info"
      />
      <ResourceChart label="Replicas" :points="replicas" :format="count" step tone="text-success" />
      <ResourceChart label="Restarts" :points="restarts" :format="count" step tone="text-warning" :marks="marks" />
    </div>
    <p class="text-[11px] text-dimmed mt-1.5">
      Solid is the mean across the environment's containers; the dashed line is the peak inside each bucket. Marks are
      restarts, red ones OOM kills.
    </p>
  </div>
</template>
