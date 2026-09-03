<script setup lang="ts">
import { computed, nextTick, onScopeDispose, ref, watch } from "vue";
import type { LogLine, LogQuery } from "../lib/api";
import { useAsync, usePoll } from "../lib/useAsync";
import StatusDot from "./StatusDot.vue";

// "Live" prefers a real tail: the same endpoint streamed as Server-Sent
// Events, lines arriving as they are collected. When the stream cannot be
// established (a proxy in the way, an old operator) the viewer falls back to
// what it did before — re-running the bounded query every few seconds.

const props = defineProps<{
  fetcher: (query: LogQuery) => Promise<LogLine[]>;
  /** Keep following while true — a running build, a deploying environment. */
  live?: boolean;
  /** Streaming variant of the fetcher; polling is the fallback without it. */
  streamer?: (query: LogQuery, onLine: (line: LogLine) => void, signal: AbortSignal) => Promise<void>;
  /** ClickHouse expression scoping this object, for the jump to Observability. */
  queryClause?: string;
  /** What to call the thing that wrote a line, keyed by the line's `run` — the
   * name of the Job behind it. A build that is several images is several Jobs,
   * and their lines are one merged tail; without this it is one anonymous
   * stream. Lines whose run is not named fall back to their container. */
  runLabels?: Record<string, string>;
}>();

const search = ref("");
const limit = ref(200);
const limits = [200, 500, 1000, 5000];

const query = (): LogQuery => ({ limit: limit.value, search: search.value.trim() || undefined });

const { data, error, loading, refresh } = useAsync(() => props.fetcher(query()));

// The stream, when one is up. `streamed` doubles as the flag: non-null means
// lines render from it instead of the polled page.
const streamed = ref<LogLine[] | null>(null);
const streamBroken = ref(false);
let controller: AbortController | undefined;

function stopStream() {
  controller?.abort();
  controller = undefined;
  streamed.value = null;
}

function startStream() {
  stopStream();
  if (!props.streamer) return;
  const mine = new AbortController();
  controller = mine;
  streamed.value = [];
  void props
    .streamer(
      query(),
      (line) => {
        if (controller !== mine || !streamed.value) return;
        streamed.value.push(line);
        // The tail is a window, not an archive: the full history is one
        // bounded query (or Observability) away.
        if (streamed.value.length > 5000) streamed.value.splice(0, streamed.value.length - 5000);
      },
      mine.signal,
    )
    .catch(() => {
      if (controller !== mine) return;
      // Fall back to polling for the rest of this view's life.
      streamBroken.value = true;
      stopStream();
      void refresh();
    });
}

const streaming = computed(() => streamed.value !== null);
const lines = computed(() => streamed.value ?? data.value);

watch(
  [() => props.live, limit],
  () => {
    if (props.live && props.streamer && !streamBroken.value) startStream();
    else {
      stopStream();
      void refresh();
    }
  },
  { immediate: true },
);
onScopeDispose(stopStream);

// Polling only carries the fallback: live without a working stream.
usePoll(() => void refresh(), 3000, () => props.live === true && !streaming.value);

let debounce: ReturnType<typeof setTimeout> | undefined;
watch(search, () => {
  clearTimeout(debounce);
  debounce = setTimeout(() => {
    if (streaming.value) startStream();
    else void refresh();
  }, 300);
});

const scroller = ref<HTMLElement | null>(null);
watch(
  () => lines.value?.length,
  async () => {
    // Follow the tail the way a terminal does, but only when already at it.
    const el = scroller.value;
    if (!el) return;
    const atTail = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
    await nextTick();
    if (atTail) el.scrollTop = el.scrollHeight;
  },
);

const unavailable = computed(() => error.value?.includes("telemetry store") ?? false);

function levelClass(line: LogLine): string {
  if (line.level === "error" || line.level === "fatal") return "text-error";
  if (line.level === "warn") return "text-warning";
  if (line.stream === "stderr") return "text-error";
  return "text-toned";
}

// Which of the merged streams a line came from: the caller's name for its
// Job where there is one, and the container otherwise — which is what every
// log the platform shows was labelled with before any of them were merged.
function origin(line: LogLine): string {
  return props.runLabels?.[line.run ?? ""] || line.container || line.source;
}

function time(line: LogLine): string {
  const date = new Date(line.timestamp);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleTimeString("en-GB");
}
</script>

<template>
  <div class="flex flex-col gap-2">
    <div class="flex items-center gap-2 flex-wrap">
      <UInput
        v-model="search"
        icon="i-lucide-search"
        placeholder="Filter messages…"
        size="sm"
        class="flex-1 min-w-40 sm:flex-none sm:w-64 font-mono"
      />
      <USelect v-model="limit" :items="limits" size="sm" class="w-24 shrink-0" />
      <UBadge v-if="live" color="success" variant="soft" size="sm" class="font-mono">
        <StatusDot tone="success" pulse class="mr-1" /> {{ streaming ? "streaming" : "live" }}
      </UBadge>
      <span class="flex-1" />
      <UButton
        v-if="queryClause"
        :to="{ name: 'observability', query: { where: queryClause } }"
        size="sm"
        color="neutral"
        variant="ghost"
        icon="i-lucide-database"
      >
        Query in Observability
      </UButton>
      <UButton
        v-if="!streaming"
        icon="i-lucide-refresh-cw"
        size="sm"
        color="neutral"
        variant="ghost"
        :loading="loading"
        aria-label="Refresh logs"
        @click="refresh"
      />
    </div>

    <UAlert
      v-if="unavailable"
      color="warning"
      variant="soft"
      icon="i-lucide-database-zap"
      title="No telemetry store"
      :description="error ?? undefined"
    />
    <UAlert v-else-if="error && !streaming" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <div
      v-else
      ref="scroller"
      class="rounded-md border border-default bg-muted font-mono text-xs leading-5 overflow-auto max-h-[32rem] min-h-24"
    >
      <div v-if="!lines?.length" class="px-3 py-3 text-muted">
        {{ loading ? "Loading…" : streaming ? "Waiting for lines…" : "No log lines match." }}
      </div>
      <table v-else class="w-full">
        <tbody>
          <tr v-for="(line, i) in lines" :key="i" class="hover:bg-elevated/50 align-top">
            <td class="px-3 py-0.5 text-dimmed whitespace-nowrap select-none">{{ time(line) }}</td>
            <td class="px-3 py-0.5 text-muted whitespace-nowrap">{{ origin(line) }}</td>
            <td v-if="line.level" class="px-3 py-0.5 whitespace-nowrap select-none" :class="levelClass(line)">
              {{ line.level }}
            </td>
            <td v-else class="px-3 py-0.5" />
            <td class="px-3 py-0.5 whitespace-pre-wrap break-all w-full" :class="levelClass(line)">
              {{ line.message }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
