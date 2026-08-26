<script setup lang="ts">
import type { EdgeEntry } from "../lib/api";
import { compactCount } from "../lib/format";
import { formatLatency, formatPercent, formatRate } from "../lib/requests";

// One of the edge's five rankings. They are five reads rather than one list
// sorted five ways, because the sort decides which rows survive the limit: the
// ten busiest routes and the ten that fail most are rarely the same ten.
//
// The two ranked by error rate drop rows with too little traffic to rank, or
// the worst-performing host on the platform is whichever scanner asked once and
// got a 404 — which is why an empty table here is worth a sentence.

defineProps<{
  title: string;
  hint: string;
  entries: EdgeEntry[] | undefined;
  /** Which column this ranking is ordered by, so the eye lands on it. */
  by: "requests" | "errorRate" | "p95";
  empty: string;
  loading?: boolean;
}>();
</script>

<template>
  <!-- min-w-0 because this sits in a grid, whose items refuse to shrink below
       their content: without it the table's own minimum widens the page. -->
  <div class="min-w-0">
    <h3 class="text-xs font-medium text-highlighted mb-1.5">
      {{ title }} <span class="text-dimmed font-normal">— {{ hint }}</span>
    </h3>
    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[42rem] text-sm">
        <thead>
          <tr class="text-left text-[11px] text-muted border-b border-default bg-muted">
            <th class="px-3 py-1 font-medium">Key</th>
            <th class="px-3 py-1 font-medium">Environment</th>
            <th class="px-3 py-1 font-medium text-right" :class="by === 'requests' ? 'text-highlighted' : ''">Requests</th>
            <th class="px-3 py-1 font-medium text-right" :class="by === 'errorRate' ? 'text-highlighted' : ''">Errors</th>
            <th class="px-3 py-1 font-medium text-right" :class="by === 'p95' ? 'text-highlighted' : ''">p95</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!entries?.length">
            <td colspan="5" class="px-3 py-2 text-center text-xs text-muted">{{ loading ? "Loading…" : empty }}</td>
          </tr>
          <tr v-for="entry in entries ?? []" :key="entry.key" class="border-b border-muted last:border-0">
            <td class="px-3 py-1 font-mono text-xs text-highlighted break-all">{{ entry.key || "—" }}</td>
            <td class="px-3 py-1 text-xs">
              <RouterLink
                v-if="entry.environment"
                :to="{ name: 'environment', params: { name: entry.environment } }"
                class="text-primary hover:underline"
                >{{ entry.environment }}</RouterLink
              >
              <span v-else class="text-dimmed">unrouted</span>
            </td>
            <td class="px-3 py-1 text-right font-mono text-xs tabular-nums text-toned">
              {{ compactCount(entry.requests) }}
              <span class="text-dimmed">· {{ formatRate(entry.requestsPerSecond) }}</span>
            </td>
            <td
              class="px-3 py-1 text-right font-mono text-xs tabular-nums"
              :class="entry.errors ? 'text-error' : 'text-dimmed'"
            >
              {{ formatPercent(entry.errorRate) }}
              <span v-if="entry.errors" class="text-dimmed">· {{ compactCount(entry.errors) }}</span>
            </td>
            <td class="px-3 py-1 text-right font-mono text-xs tabular-nums text-toned">
              {{ formatLatency(entry.p95Ms) }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
