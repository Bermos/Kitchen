<script setup lang="ts">
import { computed, watch } from "vue";
import { api, type LogLine } from "../lib/api";
import { compactCount, formatBytes, timeAgo } from "../lib/format";
import { correlatedLogsQuery } from "../lib/requests";
import { useAsync, usePoll } from "../lib/useAsync";
import RequestRows from "./RequestRows.vue";
import ResourceChart from "./ResourceChart.vue";

// Everything the platform knows about a container that died, on one screen.
//
// The parts exist separately already — the termination is on the workload
// panel, the lines are in the log store, the memory series in the history
// charts, the cluster's warnings and the edge's requests in their own tables.
// What nobody has is the join, and the join is the whole feature: the exit code
// says *what*, the last lines before the termination instant say *why*, the
// memory climbing into its limit says whether the kernel was right, and the
// requests around the instant say what was being asked of it when it went.
//
// Nothing having crashed is an answer rather than an empty report, so that is
// what it renders — one line, not four empty sections that would read as "the
// platform lost the evidence".

const props = defineProps<{ environment: string; live?: boolean }>();

const { data, error, loading, refresh } = useAsync(() => api.environmentDiagnostics(props.environment));
watch(() => props.environment, () => void refresh());
usePoll(() => void refresh(), 20_000, () => props.live === true);

const report = computed(() => data.value?.report ?? null);
const crash = computed(() => report.value?.crash ?? null);

/** An installation without a telemetry store cannot assemble a report, and is
 * told so only where something actually crashed — whether anything did is the
 * API server's answer. Either way this is not the panel to shout it from. */
const unavailable = computed(() => (error.value ?? "").includes("telemetry store"));

const points = computed(() => report.value?.resources.points ?? []);
const memory = computed(() =>
  points.value.map((point) => ({ start: point.start, value: point.memoryBytes, peak: point.memoryPeakBytes })),
);
const restarts = computed(() => points.value.map((point) => ({ start: point.start, value: point.restarts + point.oomKills })));
/** Where in the window the restarts happened, rather than how many there have
 * ever been — the trajectory is the thing a crash loop is read by. */
const marks = computed(() =>
  points.value.flatMap((point) => [
    ...(point.oomKills ? [{ start: point.start, count: point.oomKills, tone: "stroke-error", label: "OOM kill" }] : []),
    ...(point.restarts && !point.oomKills
      ? [{ start: point.start, count: point.restarts, tone: "stroke-warning/70", label: "restart" }]
      : []),
  ]),
);

const headline = computed(() => {
  const detail = crash.value;
  if (!detail) return "";
  return detail.oomKilled
    ? "OOMKilled — the kernel stopped it for using more memory than its limit"
    : `Crashed — exit code ${detail.exitCode}${detail.reason ? ` (${detail.reason})` : ""}`;
});

const count = (value: number) => compactCount(value);

function time(iso: string | undefined): string {
  const date = iso ? new Date(iso) : null;
  return date && !Number.isNaN(date.getTime()) ? date.toLocaleTimeString("en-GB") : "—";
}

function levelClass(line: LogLine): string {
  if (line.level === "error" || line.level === "fatal") return "text-error";
  if (line.level === "warn") return "text-warning";
  if (line.stream === "stderr") return "text-error";
  return "text-toned";
}
</script>

<template>
  <div v-if="!unavailable && (data || error)">
    <UAlert
      v-if="error"
      color="warning"
      variant="soft"
      icon="i-lucide-triangle-alert"
      title="The crash report could not be assembled"
      :description="error"
    />

    <!-- Nothing crashed: the endpoint's own sentence, and no shell around it. -->
    <p
      v-else-if="!data?.crashed"
      class="rounded-md border border-default bg-muted px-4 py-2.5 text-xs text-muted flex items-center gap-2"
    >
      <UIcon name="i-lucide-shield-check" class="size-4 text-success shrink-0" />
      <span class="flex-1">{{ data?.message }}</span>
      <span v-if="data?.restarts" class="text-warning font-mono shrink-0">
        {{ data.restarts }} restart{{ data.restarts === 1 ? "" : "s" }} on the pods running now
      </span>
    </p>

    <div v-else-if="report && crash" class="rounded-md border border-error/40 overflow-hidden">
      <div class="px-5 py-4 bg-error/5 border-b border-error/30">
        <div class="flex items-start justify-between gap-4 flex-wrap">
          <div>
            <h2 class="text-sm font-semibold text-error flex items-center gap-2">
              <UIcon :name="crash.oomKilled ? 'i-lucide-memory-stick' : 'i-lucide-skull'" class="size-4" />
              {{ headline }}
            </h2>
            <p class="text-xs text-muted mt-1 font-mono">
              {{ crash.pod }} · {{ crash.container }} · {{ timeAgo(crash.finishedAt) }}
              <template v-if="crash.previous"> · the run before the current one</template>
            </p>
          </div>
          <UButton
            icon="i-lucide-refresh-cw"
            size="xs"
            color="neutral"
            variant="ghost"
            :loading="loading"
            aria-label="Refresh the crash report"
            @click="refresh"
          />
        </div>
        <p v-if="crash.waiting" class="text-xs text-warning mt-2 font-mono">{{ crash.waiting }}</p>
        <p v-if="crash.message" class="text-xs text-toned mt-1 font-mono break-all">{{ crash.message }}</p>

        <div class="grid gap-4 grid-cols-2 sm:grid-cols-4 mt-3 text-sm">
          <div>
            <p class="text-[11px] text-muted">Exit code</p>
            <p class="font-mono text-highlighted">
              {{ crash.exitCode }}<span v-if="crash.signal" class="text-dimmed"> · signal {{ crash.signal }}</span>
            </p>
          </div>
          <div>
            <p class="text-[11px] text-muted">Reason</p>
            <p class="font-mono" :class="crash.oomKilled ? 'text-error' : 'text-toned'">{{ crash.reason || "—" }}</p>
          </div>
          <div>
            <p class="text-[11px] text-muted">Restarts · this container</p>
            <p class="font-mono text-warning">{{ crash.restarts }}</p>
          </div>
          <div>
            <p class="text-[11px] text-muted">Restarts · environment</p>
            <p class="font-mono text-warning">{{ data?.restarts ?? 0 }}</p>
          </div>
        </div>
      </div>

      <div class="px-5 py-4 space-y-4">
        <div class="grid gap-3 lg:grid-cols-2">
          <ResourceChart
            label="Memory into the limit"
            :points="memory"
            :format="formatBytes"
            :limit="report.resources.memoryLimitBytes"
            :marks="marks"
            tone="text-info"
          />
          <ResourceChart
            label="Restart trajectory"
            :points="restarts"
            :format="count"
            step
            tone="text-warning"
            :marks="marks"
          />
        </div>

        <div>
          <h3 class="text-xs font-medium text-highlighted mb-1.5">
            Last lines before it went
            <span class="text-dimmed font-normal">— this container's own, up to {{ time(crash.finishedAt) }}</span>
          </h3>
          <div class="rounded-md border border-default bg-muted font-mono text-xs leading-5 overflow-auto max-h-72">
            <p v-if="!report.logs.length" class="px-3 py-3 text-muted">
              It wrote nothing in the half hour before it died, which is itself a finding.
            </p>
            <table v-else class="w-full">
              <tbody>
                <tr v-for="(line, i) in report.logs" :key="i" class="align-top">
                  <td class="px-3 py-0.5 text-dimmed whitespace-nowrap select-none">{{ time(line.timestamp) }}</td>
                  <td v-if="line.level" class="px-2 py-0.5 whitespace-nowrap select-none" :class="levelClass(line)">
                    {{ line.level }}
                  </td>
                  <td v-else class="px-2 py-0.5" />
                  <td class="px-2 py-0.5 whitespace-pre-wrap break-all w-full" :class="levelClass(line)">
                    {{ line.message }}
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
          <RouterLink
            v-if="correlatedLogsQuery(environment, crash.finishedAt, 300)"
            :to="{ name: 'observability', query: correlatedLogsQuery(environment, crash.finishedAt, 300)! }"
            class="text-[11px] text-primary hover:underline inline-flex items-center gap-1 mt-1"
          >
            <UIcon name="i-lucide-scroll-text" class="size-3" />
            every line this environment wrote around the crash
          </RouterLink>
        </div>

        <div>
          <h3 class="text-xs font-medium text-highlighted mb-1.5">
            What the cluster said
            <span class="text-dimmed font-normal">— Warnings, which run past the crash because a loop keeps announcing itself</span>
          </h3>
          <div class="rounded-md border border-default overflow-x-auto">
            <p v-if="!report.events.length" class="px-4 py-2.5 text-xs text-muted">
              No Warning events for this environment in the window.
            </p>
            <table v-else class="w-full text-xs">
              <tbody>
                <tr v-for="(event, i) in report.events" :key="i" class="border-b border-muted last:border-0 align-top">
                  <td class="px-3 py-1.5 text-dimmed font-mono whitespace-nowrap">{{ time(event.timestamp) }}</td>
                  <td class="px-2 py-1.5 font-mono text-warning whitespace-nowrap">{{ event.reason }}</td>
                  <td class="px-2 py-1.5 font-mono text-dimmed whitespace-nowrap">
                    {{ event.kind }}<template v-if="event.name">/{{ event.name }}</template>
                  </td>
                  <td class="px-2 py-1.5 text-toned w-full break-all">{{ event.message }}</td>
                  <td class="px-3 py-1.5 text-dimmed font-mono text-right whitespace-nowrap">
                    <template v-if="event.count > 1">×{{ event.count }}</template>
                  </td>
                </tr>
              </tbody>
            </table>
          </div>
        </div>

        <div>
          <h3 class="text-xs font-medium text-highlighted mb-1.5">
            What the edge was serving
            <span class="text-dimmed font-normal">— the thirty seconds either side of the instant</span>
          </h3>
          <RequestRows
            :rows="report.requests"
            :environment="environment"
            empty="The edge served nothing in the seconds around it — either nothing was asked, or nothing reaches this environment through it."
          />
        </div>

        <p class="text-[11px] text-dimmed leading-relaxed">
          Assembled over {{ time(report.since) }} – {{ time(report.until) }}. The lines and the memory series stop at the
          termination instant, because they are what led up to it; the events run past it; the requests are the seconds
          either side. The report is all-or-nothing — a section that came back empty is a section that was empty.
        </p>
      </div>
    </div>
  </div>
</template>
