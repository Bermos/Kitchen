<script setup lang="ts">
import type { Alert } from "../lib/api";
import { uptime } from "../lib/format";

// Failures with nobody acting.
//
// It reads deliveries rather than findings, and that is the whole reason it is
// a second table: "nobody has touched this in four hours" is not a property of
// a condition, it is a property of what people did about it, and only the
// recorded history knows. An installation with nothing recording has no
// untended failures rather than none it can see — which is what `message`
// says, in the API's own words.
//
// A failure somebody is already fixing is not here by construction: an
// acknowledgement stops the clock, so an acknowledged condition is never
// untended.

defineProps<{
  items: Alert[];
  /** Why this answer is thinner than it should be, where it is. */
  message?: string;
}>();
</script>

<template>
  <div class="space-y-3">
    <UAlert v-if="message" color="warning" variant="soft" icon="i-lucide-info" :description="message" />

    <div v-if="items.length" class="rounded-md border border-default bg-muted overflow-x-auto">
      <table class="w-full min-w-[36rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default">
            <th class="px-3 py-2 font-medium">Condition</th>
            <th class="px-3 py-2 font-medium">Where</th>
            <th class="px-3 py-2 font-medium">Unacknowledged</th>
          </tr>
        </thead>
        <tbody class="divide-y divide-default">
          <tr v-for="alert in items" :key="`${alert.fingerprint}/${alert.audience}`">
            <td class="px-3 py-2">
              <p class="text-highlighted">{{ alert.title }}</p>
              <p class="text-xs text-muted break-words">{{ alert.detail }}</p>
            </td>
            <td class="px-3 py-2 font-mono text-xs text-toned">
              {{ [alert.scope?.project, alert.scope?.environment].filter(Boolean).join(" / ") || alert.scope?.kind }}
            </td>
            <td class="px-3 py-2 text-xs text-warning tabular-nums whitespace-nowrap">
              {{ alert.openedAt ? uptime(alert.openedAt) : "—" }}
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <p v-else-if="!message" class="text-xs text-muted flex items-center gap-2">
      <UIcon name="i-lucide-shield-check" class="size-4 text-success shrink-0" />
      <span>Nothing has been left unacknowledged past the escalation window.</span>
    </p>
  </div>
</template>
