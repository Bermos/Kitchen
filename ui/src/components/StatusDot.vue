<script setup lang="ts">
import { computed } from "vue";
import type { Tone } from "../lib/status";

const props = defineProps<{ tone: Tone; pulse?: boolean }>();

const classes = computed(() => {
  const colors: Record<Tone, string> = {
    success: "bg-success",
    warning: "bg-warning",
    error: "bg-error",
    info: "bg-info",
    neutral: "bg-accented",
  };
  return [colors[props.tone], props.pulse ? "animate-pulse" : ""];
});
</script>

<template>
  <span class="inline-block size-1.5 rounded-full shrink-0" :class="classes" />
</template>
