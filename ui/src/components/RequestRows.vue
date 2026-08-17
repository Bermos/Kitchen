<script setup lang="ts">
import { ref } from "vue";
import type { RequestRow } from "../lib/api";
import { correlatedLogsQuery, formatLatency, isHTTP2, statusClass } from "../lib/requests";

// The requests themselves: when, what was asked, what was answered, how long it
// took. One row per request the platform's edge served.
//
// A row expands into what the platform can add to it — the host it arrived on,
// the template the path was grouped under, the protocol — and into the one
// thing this screen exists to make one click: the log lines the same
// environment wrote in the thirty seconds either side of it. It also expands
// into what the platform cannot add, which matters just as much: no release, no
// query string, and a status that is transport-level for gRPC.

const props = withDefaults(
  defineProps<{
    rows: RequestRow[] | null;
    /** Whose lines the correlated view opens. */
    environment: string;
    loading?: boolean;
    /** What to say when there is nothing to show. */
    empty?: string;
    /** Cap the scroller; the crash report's fifty rows need no scroller. */
    scroll?: boolean;
  }>(),
  { rows: null, empty: "No requests match in this window.", scroll: true },
);

const expanded = ref<number | null>(null);

function time(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleTimeString("en-GB");
}

/** The seconds are the point on a request row — two 502s a minute apart and
 * two in the same second are different failures. */
function millis(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "" : `.${String(date.getMilliseconds()).padStart(3, "0")}`;
}
</script>

<template>
  <div
    class="rounded-md border border-default bg-muted font-mono text-xs leading-5 overflow-auto"
    :class="scroll ? 'max-h-[32rem] min-h-24' : ''"
  >
    <div v-if="!rows?.length" class="px-3 py-3 text-muted">
      {{ loading ? "Loading…" : empty }}
    </div>
    <table v-else class="w-full">
      <tbody>
        <template v-for="(row, i) in rows" :key="i">
          <tr class="hover:bg-elevated/50 align-top cursor-pointer" @click="expanded = expanded === i ? null : i">
            <td class="px-3 py-0.5 text-dimmed whitespace-nowrap select-none">
              {{ time(row.timestamp) }}<span class="text-dimmed/60">{{ millis(row.timestamp) }}</span>
            </td>
            <td class="px-2 py-0.5 text-muted whitespace-nowrap select-none">{{ row.method }}</td>
            <td class="px-2 py-0.5 whitespace-nowrap tabular-nums select-none" :class="statusClass(row.status)">
              {{ row.status }}
            </td>
            <td class="px-2 py-0.5 text-toned whitespace-nowrap tabular-nums text-right">
              {{ formatLatency(row.durationMs) }}
            </td>
            <td class="px-2 py-0.5 break-all w-full" :class="statusClass(row.status)">
              {{ row.path }}
              <span v-if="row.route && row.route !== row.path" class="text-dimmed ml-1">{{ row.route }}</span>
            </td>
          </tr>
          <tr v-if="expanded === i" class="bg-elevated/30">
            <td colspan="5" class="px-3 py-2 space-y-2">
              <div class="flex flex-wrap gap-x-4 gap-y-1 text-[11px]">
                <span class="text-dimmed">host <span class="text-toned">{{ row.host || "—" }}</span></span>
                <span class="text-dimmed">route <span class="text-toned">{{ row.route || "—" }}</span></span>
                <span class="text-dimmed">
                  protocol <span class="text-toned">{{ row.protocol || "—" }}</span>
                </span>
                <span class="text-dimmed">observed at the <span class="text-toned">{{ row.source || "edge" }}</span></span>
              </div>

              <div class="flex flex-wrap items-center gap-2 text-[11px]">
                <RouterLink
                  v-if="correlatedLogsQuery(environment, row.timestamp)"
                  :to="{ name: 'observability', query: correlatedLogsQuery(environment, row.timestamp)! }"
                  class="px-1.5 py-0.5 rounded border border-default hover:border-accented text-primary"
                  title="This environment's log lines, thirty seconds either side of this request"
                  @click.stop
                >
                  <UIcon name="i-lucide-scroll-text" class="size-3 align-[-2px]" />
                  logs ±30s
                </RouterLink>
                <!-- Reserved rather than speculative: the column exists so an
                     instrumented application's requests link straight to their
                     traces, and is empty until the edge can carry one. -->
                <RouterLink
                  v-if="row.traceId"
                  :to="{ name: 'traces', query: { trace: row.traceId, range: '1440' } }"
                  class="px-1.5 py-0.5 rounded border border-default hover:border-accented text-primary"
                  title="The whole request, as the application traced it"
                  @click.stop
                >
                  <UIcon name="i-lucide-git-fork" class="size-3 align-[-2px]" />
                  trace {{ row.traceId.slice(0, 12) }}
                </RouterLink>
              </div>

              <!-- What a request row cannot tell you, said where someone would
                   otherwise assume it could. -->
              <p class="text-[11px] text-dimmed leading-relaxed">
                The edge routes to a Service, so no row can say which release served it — the deploy marks on the charts
                are how a change lines up with a moment. Query strings are stripped before the row is written and never
                stored.<template v-if="isHTTP2(row.protocol)">
                  Over HTTP/2 the status is transport-level: a failed gRPC call is a 200 with a
                  <span class="text-toned">grpc-status</span> trailer the edge does not read.</template>
              </p>
            </td>
          </tr>
        </template>
      </tbody>
    </table>
  </div>
</template>
