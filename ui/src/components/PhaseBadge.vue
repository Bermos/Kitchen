<script setup lang="ts">
import { computed } from "vue";
import { phaseTone } from "../lib/status";

const props = defineProps<{ phase?: string }>();

const color = computed(() => {
  const tone = phaseTone(props.phase);
  return tone === "neutral" ? "neutral" : tone;
});
</script>

<template>
  <UBadge v-if="phase" :color="color" variant="soft" size="sm" class="font-mono">{{ phase }}</UBadge>
  <span v-else class="text-dimmed">—</span>
</template>
