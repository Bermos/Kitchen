<script setup lang="ts">
import { computed, nextTick, ref, watch } from "vue";
import type { LogLine, LogQuery } from "../lib/api";
import { useAsync, usePoll } from "../lib/useAsync";
import StatusDot from "./StatusDot.vue";

// The log endpoints are bounded queries (newest lines win, oldest first in the
// answer), so "live" is honest polling: re-run the same query while the thing
// the logs belong to is still moving.

const props = defineProps<{
  fetcher: (query: LogQuery) => Promise<LogLine[]>;
  /** Keep refreshing while true — a running build, a deploying environment. */
  live?: boolean;
}>();

const search = ref("");
const limit = ref(200);
const limits = [200, 500, 1000, 5000];

const { data, error, loading, refresh } = useAsync(() =>
  props.fetcher({ limit: limit.value, search: search.value.trim() || undefined }),
);

usePoll(() => void refresh(), 3000, () => props.live === true);
watch([() => props.live, limit], () => void refresh());

let debounce: ReturnType<typeof setTimeout> | undefined;
watch(search, () => {
  clearTimeout(debounce);
  debounce = setTimeout(() => void refresh(), 300);
});

const scroller = ref<HTMLElement | null>(null);
watch(data, async () => {
  // Follow the tail the way a terminal does, but only when already at it.
  const el = scroller.value;
  if (!el) return;
  const atTail = el.scrollHeight - el.scrollTop - el.clientHeight < 40;
  await nextTick();
  if (atTail) el.scrollTop = el.scrollHeight;
});

const unavailable = computed(() => error.value?.includes("telemetry store") ?? false);

function levelClass(line: LogLine): string {
  if (line.stream === "stderr") return "text-error";
  return "text-toned";
}

function time(line: LogLine): string {
  const date = new Date(line.timestamp);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleTimeString("en-GB");
}
</script>

<template>
  <div class="flex flex-col gap-2">
    <div class="flex items-center gap-2">
      <UInput
        v-model="search"
        icon="i-lucide-search"
        placeholder="Filter messages…"
        size="sm"
        class="w-64 font-mono"
      />
      <USelect v-model="limit" :items="limits" size="sm" class="w-24" />
      <UBadge v-if="live" color="success" variant="soft" size="sm" class="font-mono">
        <StatusDot tone="success" pulse class="mr-1" /> live
      </UBadge>
      <span class="flex-1" />
      <UButton
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
    <UAlert v-else-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <div
      v-else
      ref="scroller"
      class="rounded-md border border-default bg-muted font-mono text-xs leading-5 overflow-auto max-h-[32rem] min-h-24"
    >
      <div v-if="!data?.length" class="px-3 py-3 text-muted">
        {{ loading ? "Loading…" : "No log lines match." }}
      </div>
      <table v-else class="w-full">
        <tbody>
          <tr v-for="(line, i) in data" :key="i" class="hover:bg-elevated/50 align-top">
            <td class="px-3 py-0.5 text-dimmed whitespace-nowrap select-none">{{ time(line) }}</td>
            <td class="px-2 py-0.5 text-muted whitespace-nowrap">{{ line.container || line.source }}</td>
            <td class="px-2 py-0.5 whitespace-pre-wrap break-all w-full" :class="levelClass(line)">
              {{ line.message }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
