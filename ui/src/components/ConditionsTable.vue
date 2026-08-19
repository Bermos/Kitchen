<script setup lang="ts">
import type { Condition } from "../lib/api";
import { timeAgo } from "../lib/format";

defineProps<{ conditions?: Condition[] }>();

function tone(condition: Condition): string {
  return condition.status === "True" ? "text-success" : condition.status === "False" ? "text-error" : "text-warning";
}
</script>

<template>
  <div class="rounded-md border border-default bg-muted overflow-x-auto">
    <table class="w-full min-w-[36rem] text-sm">
      <thead>
        <tr class="text-left text-xs text-muted border-b border-default">
          <th class="px-3 py-2 font-medium">Condition</th>
          <th class="px-3 py-2 font-medium">Status</th>
          <th class="px-3 py-2 font-medium">Reason</th>
          <th class="px-3 py-2 font-medium">Message</th>
          <th class="px-3 py-2 font-medium text-right">Since</th>
        </tr>
      </thead>
      <tbody>
        <tr v-if="!conditions?.length">
          <td colspan="5" class="px-3 py-3 text-muted">No conditions reported yet.</td>
        </tr>
        <tr v-for="condition in conditions" :key="condition.type" class="border-b border-muted last:border-0">
          <td class="px-3 py-2 font-mono text-highlighted">{{ condition.type }}</td>
          <td class="px-3 py-2 font-mono" :class="tone(condition)">{{ condition.status }}</td>
          <td class="px-3 py-2 font-mono text-toned">{{ condition.reason || "—" }}</td>
          <td class="px-3 py-2 text-toned max-w-md truncate" :title="condition.message">
            {{ condition.message || "—" }}
          </td>
          <td class="px-3 py-2 text-right text-muted whitespace-nowrap">{{ timeAgo(condition.lastTransitionTime) }}</td>
        </tr>
      </tbody>
    </table>
  </div>
</template>
