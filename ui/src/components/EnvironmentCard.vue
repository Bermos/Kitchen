<script setup lang="ts">
import { computed, watch } from "vue";
import { api, type Environment } from "../lib/api";
import { compactCount } from "../lib/format";
import { edgeState, formatLatency, formatPercent } from "../lib/requests";
import { phaseTone } from "../lib/status";
import { useAsync, usePoll } from "../lib/useAsync";
import PhaseBadge from "./PhaseBadge.vue";
import Sparkline from "./Sparkline.vue";
import StatusDot from "./StatusDot.vue";

// One environment at a glance: what it served in the last day, how much of it
// failed, how slow it was, and whether it is healthy — so a project owner sees
// production and every preview without opening any of them.
//
// The numbers come off the rollups, which is why a card per preview is
// affordable; the shape beside them comes off the same window. An environment
// the platform's edge does not reach says so rather than showing four zeroes.

const props = defineProps<{ environment: Environment }>();

const DAY_MINUTES = 24 * 60;
const since = () => new Date(Date.now() - DAY_MINUTES * 60_000).toISOString();

const traffic = useAsync(async () => {
  const window = { since: since() };
  const [summary, series] = await Promise.all([
    api.requestSummary(props.environment.name, window),
    // A day at 24 buckets is an hour a bar — the same resolution the overview's
    // per-project sparklines are drawn at.
    api.requestSeries(props.environment.name, { ...window, buckets: 24 }),
  ]);
  return { summary, series };
});
watch(() => props.environment.name, () => void traffic.refresh());
// Slower than the project page's own poll: these are day-wide aggregates, and
// re-asking them every ten seconds would be a cost with nothing to show for it.
usePoll(() => void traffic.refresh(), 60_000, () => true);

const summary = computed(() => traffic.data.value?.summary ?? null);
/** An installation with no telemetry store measures nothing, and a card of
 * dashes would claim it measured zero. It keeps its name and its phase. */
const measured = computed(() => summary.value !== null || !traffic.error.value);
const state = computed(() => edgeState(summary.value?.edge, summary.value?.requests));
const points = computed(() => (traffic.data.value?.series.points ?? []).map((point) => point.requests));

/** The card's dot: what is wrong first, whatever it is. A Live environment
 * serving 5xx is not a green environment. */
const tone = computed(() => {
  const phase = phaseTone(props.environment.phase);
  if (phase === "error" || phase === "warning") return phase;
  const rate = summary.value?.errorRate ?? 0;
  if (rate >= 0.05) return "error" as const;
  if (rate > 0) return "warning" as const;
  return phase;
});
</script>

<template>
  <RouterLink
    :to="{ name: 'environment', params: { name: environment.name } }"
    class="rounded-md border border-default bg-muted px-4 py-3 block hover:border-accented"
  >
    <div class="flex items-center gap-2 min-w-0">
      <StatusDot :tone="tone" />
      <span class="text-sm text-highlighted font-medium truncate">{{ environment.name }}</span>
      <span class="flex-1" />
      <PhaseBadge :phase="environment.phase" />
    </div>
    <p class="text-[11px] text-muted mt-0.5 truncate">
      {{ environment.type }}<template v-if="environment.preview"> · #{{ environment.preview.pullRequest }}</template>
      <template v-if="environment.preview"> · {{ environment.preview.branch }}</template>
    </p>

    <!-- Off the edge: the numbers below would be four zeroes about a workload
         that is not broken for having no HTTP traffic. -->
    <p v-if="measured && state.kind === 'off-edge'" class="text-[11px] text-dimmed mt-3">
      Not on the platform's edge — nothing publishes it on the shared Gateway, so there is no traffic to show. Its logs,
      its usage and its restarts are on its own page.
    </p>

    <template v-else-if="measured">
      <div class="flex items-end justify-between gap-3 mt-2">
        <div class="min-w-0">
          <p class="text-[11px] text-muted">Requests · 24 h</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums">
            {{ summary ? compactCount(summary.requests) : "—" }}
          </p>
        </div>
        <Sparkline :points="points" :width="80" :height="22" />
      </div>
      <div class="flex items-center gap-4 mt-2 text-[11px] font-mono">
        <span :class="(summary?.errors ?? 0) > 0 ? 'text-error' : 'text-dimmed'">
          {{ summary ? formatPercent(summary.errorRate) : "—" }} errors
        </span>
        <span class="text-toned">p95 {{ formatLatency(summary?.p95Ms) }}</span>
      </div>
      <p v-if="state.kind === 'quiet' && summary" class="text-[11px] text-dimmed mt-1">
        No traffic in the last day.
      </p>
    </template>
  </RouterLink>
</template>
