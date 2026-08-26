<script setup lang="ts">
import type { RequestRoute, RouteSort } from "../lib/api";
import { compactCount } from "../lib/format";
import { formatLatency, formatPercent, GRPC_FOOTNOTE } from "../lib/requests";

// One row per route template — the per-path breakdown, which is only possible
// because paths are templated where they are collected and the set of templates
// is bounded there.
//
// Clicking a row filters the charts, the header and the request list to that
// route; clicking it again lets them go. The sort travels to the server rather
// than being applied here, because it decides which rows survive the limit: the
// ten busiest routes and the ten slowest are not the same ten.

const props = defineProps<{
  routes: RequestRoute[] | null;
  sort: RouteSort;
  /** The route the rest of the section is filtered to, if any. */
  selected: string | null;
  loading?: boolean;
  /** Whether the environment serves HTTP/2, which the error column has to
   * footnote: gRPC failures are not in these numbers. It is a fact about the
   * environment and not about the rows on screen — these rows come off the
   * rollups, which carry no protocol at all, so a table full of gRPC traffic
   * looks exactly like one with none. */
  grpc?: boolean;
}>();

const emit = defineEmits<{
  (event: "select", route: string | null): void;
  (event: "update:sort", sort: RouteSort): void;
}>();

/** The four sorts the API offers, on the columns they order. A column with no
 * sort is a column the store cannot order the limit by, and it stays a plain
 * heading rather than a button that quietly does nothing. */
const columns: { key: string; label: string; sort?: RouteSort; align: string }[] = [
  { key: "route", label: "Route", align: "text-left" },
  { key: "requests", label: "Requests", sort: "requests", align: "text-right" },
  { key: "rate", label: "Rate", align: "text-right" },
  { key: "errors", label: "5xx", sort: "errors", align: "text-right" },
  { key: "errorRate", label: "Error %", sort: "errorRate", align: "text-right" },
  { key: "p50", label: "p50", align: "text-right" },
  { key: "p95", label: "p95", sort: "p95", align: "text-right" },
  { key: "p99", label: "p99", align: "text-right" },
];

/** The overflow route: everything past the per-environment template budget. A
 * row rather than a rollup quietly growing a series per user id — and almost
 * always a sign the classifier missed an identifier scheme. */
const OVERFLOW = "/…";

function toggle(route: string) {
  emit("select", props.selected === route ? null : route);
}
</script>

<template>
  <div class="space-y-1.5">
    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[42rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th v-for="column in columns" :key="column.key" class="px-3 py-2 font-medium" :class="column.align">
              <button
                v-if="column.sort"
                class="inline-flex items-center gap-1 hover:text-highlighted"
                :class="sort === column.sort ? 'text-highlighted' : ''"
                :title="`Order the window by ${column.label.toLowerCase()}`"
                @click="emit('update:sort', column.sort)"
              >
                {{ column.label }}
                <UIcon v-if="sort === column.sort" name="i-lucide-chevron-down" class="size-3" />
              </button>
              <span v-else>{{ column.label }}</span>
              <!-- The error columns carry the footnote, because they are the
                   numbers a gRPC service would be misread by. -->
              <UIcon
                v-if="grpc && (column.key === 'errors' || column.key === 'errorRate')"
                name="i-lucide-asterisk"
                class="size-3 align-[-1px] text-warning"
                :title="GRPC_FOOTNOTE"
              />
            </th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!routes?.length">
            <td :colspan="columns.length" class="px-3 py-8 text-center text-muted text-sm">
              {{ loading ? "Loading…" : "No routes served anything in this window." }}
            </td>
          </tr>
          <tr
            v-for="row in routes ?? []"
            :key="row.route"
            class="border-b border-muted last:border-0 cursor-pointer"
            :class="selected === row.route ? 'bg-elevated' : 'hover:bg-elevated/40'"
            :title="selected === row.route ? 'Show every route again' : 'Filter this section to this route'"
            @click="toggle(row.route)"
          >
            <td class="px-3 py-2 font-mono text-xs text-highlighted break-all">
              {{ row.route || "/" }}
              <UIcon
                v-if="row.route === OVERFLOW"
                name="i-lucide-info"
                class="size-3 align-[-2px] ml-1 text-warning"
                title="Everything past this environment's 300-template budget lands here — usually a sign an identifier scheme was not recognised"
              />
            </td>
            <td class="px-3 py-2 text-right font-mono text-toned tabular-nums">{{ compactCount(row.requests) }}</td>
            <td class="px-3 py-2 text-right font-mono text-dimmed tabular-nums text-xs">
              {{ row.requestsPerSecond >= 1 ? `${row.requestsPerSecond.toFixed(1)}/s` : `${(row.requestsPerSecond * 60).toFixed(1)}/min` }}
            </td>
            <td class="px-3 py-2 text-right font-mono tabular-nums" :class="row.errors ? 'text-error' : 'text-dimmed'">
              {{ row.errors || "—" }}
            </td>
            <td class="px-3 py-2 text-right font-mono tabular-nums" :class="row.errors ? 'text-error' : 'text-dimmed'">
              {{ formatPercent(row.errorRate) }}
            </td>
            <td class="px-3 py-2 text-right font-mono text-dimmed tabular-nums text-xs">
              {{ formatLatency(row.p50Ms) }}
            </td>
            <td class="px-3 py-2 text-right font-mono text-toned tabular-nums">{{ formatLatency(row.p95Ms) }}</td>
            <td class="px-3 py-2 text-right font-mono text-dimmed tabular-nums text-xs">
              {{ formatLatency(row.p99Ms) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
    <p v-if="grpc" class="text-[11px] text-warning">{{ GRPC_FOOTNOTE }}</p>
  </div>
</template>
