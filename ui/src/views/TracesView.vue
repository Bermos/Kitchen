<script setup lang="ts">
import { computed, onMounted, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, type Span } from "../lib/api";
import { compactCount } from "../lib/format";
import { useAsync, usePoll } from "../lib/useAsync";

// Traces: what one request did, across everything it touched.
//
// The traffic view draws what Hubble saw — that one workload called another,
// and how long the call took. This draws what the application says it was
// doing, which is the only place the answer to "why was checkout slow" lives.
// The two are complementary and neither is derived from the other.
//
// A trace is opened by id, and the id is in the URL, so a slow request found
// here is a link — and so is the jump from a log line that carried one.

const route = useRoute();
const router = useRouter();

const ranges = [
  { label: "Last 15 minutes", value: 15 },
  { label: "Last hour", value: 60 },
  { label: "Last 6 hours", value: 360 },
  { label: "Last 24 hours", value: 1440 },
];
const rangeMinutes = ref(Number(route.query.range ?? 60));
const project = ref((route.query.project as string) ?? "");
const errorsOnly = ref(route.query.errors === "1");
const minDuration = ref(Number(route.query.slow ?? 0));
const slowOptions = [
  { label: "Any duration", value: 0 },
  { label: "Slower than 100 ms", value: 100 },
  { label: "Slower than 500 ms", value: 500 },
  { label: "Slower than 2 s", value: 2000 },
];

const projects = useAsync(() => api.projects());
const projectItems = computed(() => [
  { label: "All projects", value: "" },
  ...(projects.data.value ?? []).map((p) => ({ label: p.name, value: p.name })),
]);

const traces = useAsync(() =>
  api.traces({
    since: new Date(Date.now() - rangeMinutes.value * 60_000).toISOString(),
    project: project.value || undefined,
    errors: errorsOnly.value || undefined,
    minDuration: minDuration.value || undefined,
    limit: 100,
  }),
);
usePoll(() => void traces.refresh(), 15_000, () => selected.value === null);

/** The open trace. It is in the URL, so a trace is a link. */
const selected = ref<string | null>((route.query.trace as string) ?? null);
const detail = ref<Span[] | null>(null);
const detailError = ref<string | null>(null);
const detailLoading = ref(false);

function syncURL() {
  const params: Record<string, string> = {};
  if (rangeMinutes.value !== 60) params.range = String(rangeMinutes.value);
  if (project.value) params.project = project.value;
  if (errorsOnly.value) params.errors = "1";
  if (minDuration.value) params.slow = String(minDuration.value);
  if (selected.value) params.trace = selected.value;
  void router.replace({ query: params });
}

async function open(traceId: string) {
  selected.value = traceId;
  syncURL();
  detailLoading.value = true;
  detailError.value = null;
  try {
    const trace = await api.trace(traceId);
    detail.value = trace.spans;
  } catch (err) {
    detail.value = null;
    detailError.value = err instanceof Error ? err.message : String(err);
  } finally {
    detailLoading.value = false;
  }
}

function close() {
  selected.value = null;
  detail.value = null;
  detailError.value = null;
  syncURL();
}

function rerun() {
  syncURL();
  void traces.refresh();
}

watch([rangeMinutes, project, errorsOnly, minDuration], rerun);
onMounted(() => {
  if (selected.value) void open(selected.value);
});

// The waterfall. Every span is placed against the trace's own span of time, so
// a nested call reads as what it is: a bar inside its parent's bar.
const window = computed(() => {
  const spans = detail.value ?? [];
  if (!spans.length) return null;
  const start = Math.min(...spans.map((span) => new Date(span.timestamp).getTime()));
  const end = Math.max(...spans.map((span) => new Date(span.timestamp).getTime() + span.durationMs));
  return { start, end: end > start ? end : start + 1 };
});

/** Depth by walking parents, so an indented waterfall does not depend on the
 * spans arriving in tree order — they arrive in time order. */
const rows = computed(() => {
  const spans = detail.value ?? [];
  const parents = new Map(spans.map((span) => [span.spanId, span.parentSpanId]));
  const depthOf = (span: Span): number => {
    let depth = 0;
    let parent = span.parentSpanId;
    const guard = new Set<string>([span.spanId]);
    while (parent && parents.has(parent) && !guard.has(parent)) {
      guard.add(parent);
      depth += 1;
      parent = parents.get(parent);
    }
    return Math.min(depth, 8);
  };
  const span0 = window.value;
  return spans.map((span) => {
    const start = new Date(span.timestamp).getTime();
    const total = span0 ? span0.end - span0.start : 1;
    return {
      span,
      depth: depthOf(span),
      left: span0 ? ((start - span0.start) / total) * 100 : 0,
      width: span0 ? Math.max((span.durationMs / total) * 100, 0.4) : 0,
    };
  });
});

const openTrace = computed(() => traces.data.value?.find((trace) => trace.traceId === selected.value) ?? null);
const expanded = ref<string | null>(null);

function attributesOf(span: Span): [string, string][] {
  return [
    ...Object.entries(span.attributes ?? {}),
    ...Object.entries(span.resource ?? {}).map(([key, value]) => [`resource.${key}`, value] as [string, string]),
  ].sort((a, b) => a[0].localeCompare(b[0]));
}

/** The trace's logs, which is the correlation the whole trace id column
 * exists for: the query language reaches the column by name. */
function logsLink(traceId: string) {
  return { name: "observability", query: { q: `traceId:${traceId}`, range: "1440", cluster: "1" } };
}

function ms(value: number): string {
  if (value >= 1000) return `${(value / 1000).toFixed(2)} s`;
  if (value >= 10) return `${Math.round(value)} ms`;
  return `${value.toFixed(1)} ms`;
}

function time(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleTimeString("en-GB");
}

function statusTone(span: Span): string {
  if (span.statusCode === "ERROR" || (span.httpStatus ?? 0) >= 500) return "text-error";
  if ((span.httpStatus ?? 0) >= 400) return "text-warning";
  return "text-toned";
}

function barTone(span: Span): string {
  if (span.statusCode === "ERROR" || (span.httpStatus ?? 0) >= 500) return "bg-error/70";
  if (span.kind === "CLIENT") return "bg-info/70";
  return "bg-primary/70";
}
</script>

<template>
  <div class="space-y-4">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <h1 class="text-xl font-semibold text-highlighted">Traces</h1>
        <p class="text-xs text-muted mt-1">
          What one request did, as the application reported it. Add an OpenTelemetry SDK and it exports here on its
          own — every environment is given the endpoint.
        </p>
      </div>
      <div class="flex items-center gap-2 flex-wrap">
        <USelect v-model="project" :items="projectItems" size="sm" class="w-36 sm:w-40" />
        <USelect v-model="minDuration" :items="slowOptions" size="sm" class="w-36 sm:w-44" />
        <USelect v-model="rangeMinutes" :items="ranges" size="sm" class="w-36 sm:w-40" />
        <UButton
          size="sm"
          :color="errorsOnly ? 'error' : 'neutral'"
          :variant="errorsOnly ? 'soft' : 'subtle'"
          icon="i-lucide-triangle-alert"
          title="Only traces something failed in"
          @click="errorsOnly = !errorsOnly"
        >
          Errors
        </UButton>
      </div>
    </div>

    <UAlert
      v-if="traces.error.value"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      title="The traces could not be read"
      :description="traces.error.value"
    />

    <div class="flex flex-col lg:flex-row gap-4 items-stretch lg:items-start">
      <!-- The list. Narrow once a trace is open, because the waterfall is the
           thing being read at that point — and where there is no room for two
           columns it gives way entirely, since a phone showing both shows the
           waterfall a scroll below the fold. The waterfall's × brings it back. -->
      <div class="min-w-0" :class="selected ? 'hidden lg:block lg:w-80 lg:shrink-0' : 'flex-1'">
        <div class="rounded-md border border-default bg-muted overflow-x-auto">
          <p v-if="!traces.data.value?.length" class="px-4 py-10 text-center text-sm text-muted">
            <template v-if="traces.loading.value">Loading…</template>
            <template v-else>
              No traces in this window.
              <span class="block text-xs text-dimmed mt-1">
                Traces come from the application: add your language's OpenTelemetry SDK and it will find the endpoint
                the platform already set for it.
              </span>
            </template>
          </p>
          <!-- Three columns need more than a phone is wide; the fourth is
               already dropped while a trace is open, and so is the floor. -->
          <table v-else class="w-full text-sm" :class="selected ? '' : 'min-w-[26rem]'">
            <tbody>
              <tr
                v-for="trace in traces.data.value"
                :key="trace.traceId"
                class="border-b border-muted last:border-0 cursor-pointer hover:bg-elevated/50"
                :class="trace.traceId === selected ? 'bg-elevated' : ''"
                @click="open(trace.traceId)"
              >
                <td class="px-3 py-2 min-w-0">
                  <p class="font-mono text-xs truncate" :class="trace.errors ? 'text-error' : 'text-highlighted'">
                    {{ trace.name || "(unnamed)" }}
                  </p>
                  <p class="text-[11px] text-dimmed truncate">
                    {{ trace.service }}
                    <template v-if="trace.environment"> · {{ trace.environment }}</template>
                    · {{ time(trace.timestamp) }}
                  </p>
                </td>
                <td class="px-3 py-2 text-right whitespace-nowrap">
                  <p class="font-mono text-xs tabular-nums text-toned">{{ ms(trace.durationMs) }}</p>
                  <p class="text-[11px] text-dimmed">
                    {{ compactCount(trace.spans) }} span{{ trace.spans === 1 ? "" : "s" }}
                    <template v-if="trace.services > 1"> · {{ trace.services }} svc</template>
                  </p>
                </td>
                <td v-if="!selected" class="px-3 py-2 w-24 text-right whitespace-nowrap">
                  <UBadge v-if="trace.errors" color="error" variant="soft" size="sm">{{ trace.errors }} failed</UBadge>
                  <span v-else-if="trace.httpStatus" class="font-mono text-xs text-dimmed">{{ trace.httpStatus }}</span>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <!-- The waterfall. -->
      <div v-if="selected" class="flex-1 min-w-0 space-y-2">
        <div class="flex items-center justify-between gap-3 flex-wrap">
          <div class="min-w-0">
            <p class="text-sm font-medium text-highlighted truncate">
              {{ openTrace?.name || "Trace" }}
            </p>
            <p class="font-mono text-[11px] text-dimmed truncate">{{ selected }}</p>
          </div>
          <div class="flex items-center gap-2">
            <UButton
              :to="logsLink(selected)"
              size="xs"
              color="neutral"
              variant="subtle"
              icon="i-lucide-scroll-text"
              title="Every log line that carried this trace id"
            >
              Logs
            </UButton>
            <UButton size="xs" color="neutral" variant="ghost" icon="i-lucide-x" aria-label="Close" @click="close" />
          </div>
        </div>

        <UAlert
          v-if="detailError"
          color="error"
          variant="soft"
          icon="i-lucide-triangle-alert"
          title="The trace could not be read"
          :description="detailError"
        />

        <div v-else class="rounded-md border border-default bg-muted overflow-x-auto">
          <p v-if="detailLoading" class="px-4 py-10 text-center text-sm text-muted">Loading…</p>
          <table v-else class="w-full min-w-[36rem] text-xs">
            <tbody>
              <template v-for="row in rows" :key="row.span.spanId">
                <tr
                  class="border-b border-muted last:border-0 hover:bg-elevated/50 cursor-pointer align-middle"
                  @click="expanded = expanded === row.span.spanId ? null : row.span.spanId"
                >
                  <td class="px-3 py-1.5 max-w-64">
                    <p class="truncate font-mono" :class="statusTone(row.span)" :style="{ paddingLeft: `${row.depth * 12}px` }">
                      {{ row.span.name }}
                    </p>
                    <p class="truncate text-[11px] text-dimmed" :style="{ paddingLeft: `${row.depth * 12}px` }">
                      {{ row.span.service }}<template v-if="row.span.kind"> · {{ row.span.kind.toLowerCase() }}</template>
                    </p>
                  </td>
                  <td class="px-2 py-1.5 w-full">
                    <div class="relative h-3 rounded-sm bg-elevated/60">
                      <div
                        class="absolute top-0 h-3 rounded-sm"
                        :class="barTone(row.span)"
                        :style="{ left: `${row.left}%`, width: `${row.width}%` }"
                      />
                    </div>
                  </td>
                  <td class="px-3 py-1.5 text-right font-mono tabular-nums whitespace-nowrap text-toned">
                    {{ ms(row.span.durationMs) }}
                  </td>
                </tr>
                <tr v-if="expanded === row.span.spanId" class="bg-elevated/30">
                  <td colspan="3" class="px-3 py-2">
                    <p v-if="row.span.statusMessage" class="text-error font-mono mb-1.5">
                      {{ row.span.statusMessage }}
                    </p>
                    <div class="flex flex-wrap gap-1">
                      <span
                        v-for="[key, value] in attributesOf(row.span)"
                        :key="key"
                        class="px-1.5 py-0.5 rounded border border-default"
                      >
                        <span class="text-info">{{ key }}</span>
                        <span class="text-dimmed">:</span>
                        <span class="text-toned">{{ value }}</span>
                      </span>
                      <span v-if="!attributesOf(row.span).length" class="text-dimmed">no attributes</span>
                    </div>
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </div>
    </div>
  </div>
</template>
