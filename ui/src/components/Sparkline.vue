<script setup lang="ts">
import { computed } from "vue";

// A single-series line in miniature: magnitude over time, one hue, no axes.
// It never carries identity (the tile or column it sits in names it) and the
// number next to it carries the value — the line only shows the shape.

const props = withDefaults(
  defineProps<{
    points: number[];
    width?: number;
    height?: number;
    /** Text tone class carrying the stroke via currentColor. */
    tone?: string;
  }>(),
  { width: 88, height: 24, tone: "text-primary" },
);

const padding = 2;

const path = computed(() => {
  const points = props.points;
  if (!points.length) return "";
  const max = Math.max(...points);
  const innerW = props.width - padding * 2;
  const innerH = props.height - padding * 2;
  const step = points.length > 1 ? innerW / (points.length - 1) : 0;
  return points
    .map((value, i) => {
      const x = padding + i * step;
      // A window with no data at all draws as a flat baseline.
      const y = padding + (max > 0 ? innerH * (1 - value / max) : innerH);
      return `${x.toFixed(1)},${y.toFixed(1)}`;
    })
    .join(" ");
});

const empty = computed(() => !props.points.length || Math.max(...props.points) === 0);
</script>

<template>
  <svg
    :viewBox="`0 0 ${width} ${height}`"
    :width="width"
    :height="height"
    class="shrink-0"
    :class="empty ? 'text-dimmed' : tone"
    aria-hidden="true"
  >
    <polyline
      :points="path"
      fill="none"
      stroke="currentColor"
      stroke-width="1.5"
      stroke-linejoin="round"
      stroke-linecap="round"
    />
  </svg>
</template>
