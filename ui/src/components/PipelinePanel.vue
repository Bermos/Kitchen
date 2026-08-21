<script setup lang="ts">
import { computed } from "vue";
import type { Environment, Promotion, PromotionStage } from "../lib/api";
import { timeAgo } from "../lib/format";
import { newestFirst, promotionTone, stageRows } from "../lib/promotions";

// The pipeline: one artifact — the same image digest, never rebuilt — moving
// through the project's stages, judged at each boundary by that environment's
// own requirements. The columns say where it is; a blocked one says exactly
// which rules stand in the way, because "blocked" without names is a support
// ticket.

const props = defineProps<{
  stages: PromotionStage[];
  environments: Environment[];
  promotions: Promotion[];
}>();

const rows = computed(() => stageRows(props.stages, props.environments, props.promotions));
const recent = computed(() => newestFirst(props.promotions).slice(0, 8));
</script>

<template>
  <div class="space-y-3">
    <div>
      <p class="text-sm text-highlighted font-medium">Pipeline</p>
      <p class="text-xs text-muted mt-0.5">
        One artifact moves through the stages in order, never rebuilt; each environment's requirements decide
        whether it may land there.
      </p>
    </div>

    <!-- The stage columns, in promotion order. -->
    <div v-if="rows.length" class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
      <div v-for="row in rows" :key="row.stage.name" class="rounded-md border border-default bg-muted px-4 py-3">
        <div class="flex items-center justify-between gap-2">
          <p class="text-xs font-medium text-highlighted uppercase tracking-wide">{{ row.stage.name }}</p>
          <UBadge v-if="row.stage.autoPromote" color="neutral" variant="soft" size="sm">auto</UBadge>
        </div>
        <RouterLink
          v-if="row.environment"
          :to="{ name: 'environment', params: { name: row.stage.environment } }"
          class="block font-mono text-sm text-toned hover:underline mt-1 truncate"
          >{{ row.stage.environment }}</RouterLink
        >
        <p v-else class="font-mono text-sm text-dimmed mt-1 truncate">
          {{ row.stage.environment }} <span class="text-xs">(not created yet)</span>
        </p>
        <p class="text-xs text-muted mt-2 mb-0.5">Running</p>
        <p class="font-mono text-sm text-highlighted truncate">{{ row.release || "—" }}</p>
        <template v-if="row.promotion">
          <p class="text-xs text-muted mt-2 mb-0.5">Latest promotion</p>
          <div class="flex items-center gap-2 min-w-0">
            <UBadge :color="promotionTone(row.promotion.phase)" variant="soft" size="sm">
              {{ row.promotion.phase }}
            </UBadge>
            <span class="font-mono text-xs text-toned truncate">{{ row.promotion.release }}</span>
          </div>
          <!-- A blocked stage names what is missing, by rule id; the message
               carries the rules' own words. -->
          <div v-if="row.promotion.phase === 'Blocked'" class="mt-1.5 space-y-0.5">
            <p v-for="rule in row.promotion.unmetRules" :key="rule" class="text-xs text-error font-mono">
              {{ rule }}
            </p>
            <p v-if="row.promotion.message" class="text-xs text-muted">{{ row.promotion.message }}</p>
          </div>
        </template>
      </div>
    </div>

    <!-- The promotions themselves, newest first: what was asked, by whom,
         and what became of it. -->
    <div v-if="recent.length" class="rounded-md border border-default overflow-x-auto">
      <table class="w-full min-w-[42rem] text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-3 py-2 font-medium">When</th>
            <th class="px-3 py-2 font-medium">Release</th>
            <th class="px-3 py-2 font-medium">Environment</th>
            <th class="px-3 py-2 font-medium">Trigger</th>
            <th class="px-3 py-2 font-medium">Requested by</th>
            <th class="px-3 py-2 font-medium">Phase</th>
          </tr>
        </thead>
        <tbody>
          <tr v-for="p in recent" :key="p.name" class="border-b border-muted last:border-0">
            <td class="px-3 py-2 text-xs text-muted whitespace-nowrap" :title="p.name">
              {{ timeAgo(p.createdAt) }}
            </td>
            <td class="px-3 py-2 font-mono text-xs text-toned">{{ p.release }}</td>
            <td class="px-3 py-2 font-mono text-xs text-toned">{{ p.environment }}</td>
            <td class="px-3 py-2 text-xs text-muted">{{ p.trigger }}</td>
            <td class="px-3 py-2 text-xs text-muted truncate max-w-40" :title="p.requestedBy">
              {{ p.requestedBy }}
            </td>
            <td class="px-3 py-2">
              <div class="flex items-center gap-2 min-w-0">
                <UBadge :color="promotionTone(p.phase)" variant="soft" size="sm">{{ p.phase }}</UBadge>
                <span
                  v-if="p.unmetRules?.length"
                  class="font-mono text-xs text-error truncate"
                  :title="p.message"
                  >{{ p.unmetRules.join(", ") }}</span
                >
              </div>
            </td>
          </tr>
        </tbody>
      </table>
    </div>
  </div>
</template>
