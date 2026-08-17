<script setup lang="ts">
import { computed, onScopeDispose, ref, watch } from "vue";
import { api, type RequestListQuery, type RequestRow } from "../lib/api";
import { anyHTTP2, MAX_RAW_RETENTION_DAYS, rawRetentionDays } from "../lib/requests";
import { useAsync, usePoll } from "../lib/useAsync";
import RequestRows from "./RequestRows.vue";
import StatusDot from "./StatusDot.vue";

// The requests themselves, newest first, filtered the three ways someone asks
// for them: which route, which class of answer, which verb.
//
// "Live" prefers a real tail — the same endpoint streamed as Server-Sent
// Events, exactly as the log viewer does it — and falls back to re-running the
// bounded query every few seconds when the stream cannot be established. The
// server sends its page oldest first for the tail, so rows are prepended and
// the newest stays at the top, which is the order the plain listing answers in.

const props = defineProps<{
  environment: string;
  /** The window the section is reading, shared with the charts. */
  since?: string;
  until?: string;
  /** The route the table selected, or null for all of them. */
  route?: string | null;
}>();

const emit = defineEmits<{
  /** That something in the listing was served over HTTP/2 — the only place the
   * platform can tell anyone it might be looking at gRPC, and evidence the
   * section above adds to its own for the error column's footnote. It is
   * emitted only when true: a page with no HTTP/2 row is not evidence the
   * environment has none, and the footnote is a statement about the
   * environment. */
  (event: "http2"): void;
}>();

const statuses = [
  { label: "Any status", value: "" },
  { label: "2xx", value: "2xx" },
  { label: "3xx", value: "3xx" },
  { label: "4xx", value: "4xx" },
  { label: "5xx — errors", value: "5xx" },
];
const methods = ["", "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"].map((value) => ({
  label: value || "Any method",
  value,
}));
const limits = [200, 500, 1000, 5000];

const status = ref("");
const method = ref("");
const limit = ref(200);
const live = ref(false);

function query(): RequestListQuery {
  return {
    since: props.since,
    until: props.until,
    route: props.route ?? undefined,
    status: status.value || undefined,
    method: method.value || undefined,
    limit: limit.value,
  };
}

const { data, error, loading, refresh } = useAsync(() => api.requests(props.environment, query()));

// The stream, when one is up. `streamed` doubles as the flag: non-null means
// rows render from it instead of the fetched page.
const streamed = ref<RequestRow[] | null>(null);
const streamBroken = ref(false);
let controller: AbortController | undefined;

function stopStream() {
  controller?.abort();
  controller = undefined;
  streamed.value = null;
}
onScopeDispose(stopStream);

function startStream() {
  stopStream();
  const mine = new AbortController();
  controller = mine;
  streamed.value = [];
  void api
    .streamRequests(
      props.environment,
      // A tail has no end: an upper bound would stop it the moment it caught up.
      { ...query(), until: undefined },
      (row) => {
        if (controller !== mine || !streamed.value) return;
        streamed.value.unshift(row);
        // The tail is a window, not an archive — the bounded listing and the
        // route table are what a whole window is read from.
        if (streamed.value.length > 5000) streamed.value.length = 5000;
      },
      mine.signal,
    )
    .catch(() => {
      if (controller !== mine) return;
      // Fall back to polling for the rest of this panel's life.
      streamBroken.value = true;
      stopStream();
      void refresh();
    });
}

const streaming = computed(() => streamed.value !== null);
const rows = computed(() => streamed.value ?? data.value?.items ?? null);

function reload() {
  if (live.value && !streamBroken.value) startStream();
  else {
    stopStream();
    void refresh();
  }
}

function toggleLive() {
  live.value = !live.value;
  reload();
}

watch([() => props.environment, () => props.route, () => props.since, () => props.until, status, method, limit], reload);
// Polling only carries the fallback: live without a working stream.
usePoll(() => void refresh(), 5000, () => live.value && !streaming.value);

const http2 = computed(() => anyHTTP2(rows.value));
watch(
  http2,
  (seen) => {
    if (seen) emit("http2");
  },
  { immediate: true },
);

// The platform's one retention knob, which is what raw rows are kept for
// `min(7, retentionDays)` days of. The number is worth a request of its own:
// an install that keeps three days of telemetry keeps three days of requests,
// and a listing that came back empty for that reason should say the reason
// rather than a week nobody configured.
const settings = useAsync(() => api.settings());
const retentionDays = computed(() => settings.data.value?.logRetentionDays);
/** What the raw rows actually reach back, and whether that was read or assumed. */
const rawDays = computed(() => rawRetentionDays(retentionDays.value));
const rawDaysKnown = computed(() => retentionDays.value !== undefined);

/** Raw rows are kept for the shorter of a week and the platform's retention,
 * while the aggregates behind the charts are kept for all of it — so a listing
 * reaches back less far than the summary above it. Saying so beats an empty
 * table nobody can explain. */
const beyondRawRetention = computed(() => {
  if (!props.since) return false;
  const since = new Date(props.since).getTime();
  return !Number.isNaN(since) && Date.now() - since > rawDays.value * 24 * 3600 * 1000;
});

const retentionNote = computed(() => {
  const days = `${rawDays.value} day${rawDays.value === 1 ? "" : "s"}`;
  const kept = rawDaysKnown.value
    ? `Requests themselves are kept for ${days} — the shorter of a week and this platform's ${retentionDays.value}-day retention.`
    : `Requests themselves are kept for at most ${MAX_RAW_RETENTION_DAYS} days, and for less where this platform's retention is shorter — which could not be read just now.`;
  return `${kept} The charts and the route table above read aggregates kept for the whole of that retention, so this window is complete up there and truncated down here.`;
});
</script>

<template>
  <div class="flex flex-col gap-2">
    <div class="flex items-center gap-2 flex-wrap">
      <USelect v-model="status" :items="statuses" size="sm" class="w-36" />
      <USelect v-model="method" :items="methods" size="sm" class="w-32" />
      <USelect v-model="limit" :items="limits" size="sm" class="w-24" />
      <UBadge v-if="route" color="primary" variant="soft" size="sm" class="font-mono">{{ route }}</UBadge>
      <span class="flex-1" />
      <UButton size="sm" :color="live ? 'success' : 'neutral'" :variant="live ? 'soft' : 'subtle'" @click="toggleLive">
        <StatusDot :tone="live ? 'success' : 'neutral'" :pulse="live" class="mr-1" />
        {{ streaming ? "Streaming" : "Live tail" }}
      </UButton>
      <UButton
        v-if="!streaming"
        icon="i-lucide-refresh-cw"
        size="sm"
        color="neutral"
        variant="ghost"
        :loading="loading"
        aria-label="Refresh requests"
        @click="refresh"
      />
    </div>

    <UAlert
      v-if="error && !streaming"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="error"
    />
    <template v-else>
      <RequestRows
        :rows="rows"
        :environment="environment"
        :loading="loading"
        :empty="streaming ? 'Waiting for requests…' : 'No requests match in this window.'"
      />
      <p v-if="beyondRawRetention" class="text-[11px] text-dimmed">{{ retentionNote }}</p>
    </template>
  </div>
</template>
