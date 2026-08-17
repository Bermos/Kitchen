<script setup lang="ts">
import { computed } from "vue";
import { fillTone, formatFraction, VOLUME_FULL_FRACTION } from "../lib/platform";

// How full something is, against the threshold the platform's own rules fire
// on. One component for a node's disk, an application's volume and the
// telemetry store, so that 86% cannot look calm on one screen and alarming on
// another.
//
// Nothing measured draws no bar at all. An empty bar is a claim — "this is
// empty" — and the whole point of the `usageMessage` fields is that the
// platform does not always get to make it.

const props = withDefaults(
  defineProps<{
    /** 0..1, or null/undefined where nothing measured it. */
    fraction: number | null | undefined;
    /** What the bar is of, beside the percentage. */
    caption?: string;
    /** What to say instead of a bar where there is no measurement. */
    unmeasured?: string;
    width?: string;
  }>(),
  { unmeasured: "—", width: "w-24" },
);

const measured = computed(
  () => props.fraction !== null && props.fraction !== undefined && !Number.isNaN(props.fraction),
);
const tone = computed(() => fillTone(props.fraction));
const percent = computed(() => Math.min(Math.max((props.fraction ?? 0) * 100, 0), 100));
</script>

<template>
  <div v-if="measured" class="flex items-center gap-2 min-w-0">
    <span
      class="h-1.5 rounded-full bg-accented/40 overflow-hidden shrink-0"
      :class="width"
      :title="`${formatFraction(fraction)} used${caption ? ` — ${caption}` : ''}; the platform calls a volume filling at ${Math.round(VOLUME_FULL_FRACTION * 100)}%`"
    >
      <span
        class="block h-full rounded-full"
        :class="tone === 'error' ? 'bg-error' : tone === 'warning' ? 'bg-warning' : 'bg-success'"
        :style="{ width: `${percent}%` }"
      />
    </span>
    <span
      class="font-mono text-xs tabular-nums"
      :class="tone === 'error' ? 'text-error' : tone === 'warning' ? 'text-warning' : 'text-toned'"
    >
      {{ formatFraction(fraction) }}
    </span>
    <span v-if="caption" class="text-[11px] text-dimmed truncate">{{ caption }}</span>
  </div>
  <span v-else class="text-xs text-dimmed" :title="unmeasured">{{ unmeasured }}</span>
</template>
