<script setup lang="ts">
import { computed, ref } from "vue";

// One measurement of an environment over its window: a filled line, the limit
// it runs under as a dashed ceiling, and the events that happened along the way
// as marks on the baseline.
//
// It is a chart rather than a sparkline because the questions here need a
// scale: "was it always using this much memory" is unanswerable without one,
// and "did it ever come near its limit" is unanswerable without the limit
// drawn next to it. Everything else stays as spare as the log histogram — no
// axes, no gridlines, the value under the pointer in the header.

const props = withDefaults(
  defineProps<{
    label: string;
    /** `value` is the headline series; `peak` the higher one drawn dashed
     * above it, `base` the lower one drawn faintly beneath. Two of the three
     * are optional because most measurements are one line and a spike — the
     * third is for latency, where p50 under p95 under p99 is the shape, and the
     * distance between them is the whole reading. */
    points: { start: string; value: number; peak?: number; base?: number }[];
    /** Renders a value for the header and the tooltip. */
    format: (value: number) => string;
    /** What the series are, in a word, beside the label: `p50 · p95 · p99`. */
    hint?: string;
    /** The ceiling this measurement runs under, drawn as a dashed line when
     * it is set. Zero means no limit, which is a fact and not a limit of
     * nothing. */
    limit?: number;
    /** Marks on the baseline: restarts, OOM kills. */
    marks?: { start: string; count: number; tone: string; label: string }[];
    tone?: string;
    /** Stepped rendering, for a count that changes at an instant rather than
     * drifting — a replica count is 2, then 3, never 2.4. */
    step?: boolean;
  }>(),
  { tone: "text-primary", limit: 0, step: false },
);

const height = 64;
const width = 1000;
const padding = 4;

const hovered = ref<number | null>(null);

/** The scale: the tallest thing drawn, which includes the limit when the limit
 * is close enough to be worth seeing. A chart whose ceiling is off-screen does
 * not answer "how close was it". */
const peak = computed(() => {
  const values = props.points.flatMap((point) => [point.value, point.peak ?? 0]);
  const highest = Math.max(0, ...values);
  if (props.limit > 0 && props.limit <= highest * 4) return Math.max(highest, props.limit);
  return highest;
});

const scale = computed(() => (peak.value > 0 ? peak.value : 1));

function x(index: number): number {
  const step = props.points.length > 1 ? width / (props.points.length - 1) : 0;
  return index * step;
}

function y(value: number): number {
  const inner = height - padding * 2;
  return padding + inner * (1 - Math.min(value / scale.value, 1));
}

/** The line itself. Stepped series hold their value until the next point,
 * which is what a replica count actually does. */
function line(pick: (point: { value: number; peak?: number; base?: number }) => number): string {
  const points = props.points;
  if (!points.length) return "";
  const parts: string[] = [];
  points.forEach((point, i) => {
    const value = pick(point);
    if (i === 0) {
      parts.push(`M ${x(i).toFixed(1)} ${y(value).toFixed(1)}`);
      return;
    }
    if (props.step) parts.push(`L ${x(i).toFixed(1)} ${y(pick(points[i - 1])).toFixed(1)}`);
    parts.push(`L ${x(i).toFixed(1)} ${y(value).toFixed(1)}`);
  });
  return parts.join(" ");
}

const path = computed(() => line((point) => point.value));
const peakPath = computed(() =>
  props.points.some((point) => (point.peak ?? 0) > point.value) ? line((point) => point.peak ?? point.value) : "",
);
const basePath = computed(() =>
  props.points.some((point) => point.base !== undefined) ? line((point) => point.base ?? point.value) : "",
);

/** The same line closed against the baseline, so the magnitude reads at a
 * glance rather than only the shape. */
const area = computed(() => {
  if (!path.value || props.points.length < 2) return "";
  return `${path.value} L ${width} ${height - padding} L 0 ${height - padding} Z`;
});

const limitY = computed(() => (props.limit > 0 && props.limit <= scale.value ? y(props.limit) : null));

const empty = computed(() => !props.points.length || peak.value === 0);

/** The newest point is what the header shows when nothing is hovered: this is
 * a history, but the current value is still the first thing anyone reads. */
const current = computed(() => {
  const point = hovered.value !== null ? props.points[hovered.value] : props.points[props.points.length - 1];
  return point ?? null;
});

function pointAt(event: PointerEvent): number | null {
  const target = event.currentTarget as SVGElement | null;
  if (!target || !props.points.length) return null;
  const box = target.getBoundingClientRect();
  if (!box.width) return null;
  const index = Math.round(((event.clientX - box.left) / box.width) * (props.points.length - 1));
  return Math.min(Math.max(index, 0), props.points.length - 1);
}

const marksAt = computed(() => {
  const byStart = new Map(props.points.map((point, i) => [point.start, i]));
  return (props.marks ?? [])
    .filter((mark) => mark.count > 0 && byStart.has(mark.start))
    .map((mark) => ({ ...mark, x: x(byStart.get(mark.start) as number) }));
});

function time(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
}
</script>

<template>
  <div class="rounded-md border border-default bg-muted px-3 py-2">
    <div class="flex items-baseline justify-between text-[11px] mb-1 gap-2">
      <span class="text-muted truncate">
        {{ label }}<span v-if="hint" class="text-dimmed ml-1">{{ hint }}</span>
      </span>
      <span class="font-mono tabular-nums shrink-0" :class="empty ? 'text-dimmed' : 'text-highlighted'">
        <template v-if="current">
          <span v-if="hovered !== null" class="text-dimmed mr-1">{{ time(current.start) }}</span>
          <!-- All three, where there are three: a p95 alone says nothing about
               how far the tail runs past it. -->
          <span v-if="current.base !== undefined" class="text-dimmed">{{ format(current.base) }} · </span>
          {{ format(current.value) }}
          <span v-if="current.base !== undefined && current.peak !== undefined" class="text-dimmed">
            · {{ format(current.peak) }}</span
          >
          <span v-if="limit > 0" class="text-dimmed">/ {{ format(limit) }}</span>
        </template>
        <template v-else>—</template>
      </span>
    </div>

    <svg
      :viewBox="`0 0 ${width} ${height}`"
      preserveAspectRatio="none"
      class="w-full select-none touch-none"
      :style="{ height: `${height}px` }"
      :class="empty ? 'text-dimmed' : tone"
      @pointermove="hovered = pointAt($event)"
      @pointerleave="hovered = null"
    >
      <!-- The ceiling, where there is one and it is in view. -->
      <line
        v-if="limitY !== null"
        x1="0"
        :x2="width"
        :y1="limitY"
        :y2="limitY"
        class="stroke-error/50"
        stroke-width="1"
        stroke-dasharray="4 4"
        vector-effect="non-scaling-stroke"
      />
      <path v-if="area" :d="area" fill="currentColor" class="opacity-15" />
      <!-- The lower series, where there is one: the median under the tail. -->
      <path
        v-if="basePath"
        :d="basePath"
        fill="none"
        stroke="currentColor"
        stroke-width="1"
        stroke-dasharray="1 3"
        class="opacity-60"
        vector-effect="non-scaling-stroke"
      />
      <!-- The peak inside each bucket, where it differs from the mean: a mean
           that never touched the limit says nothing about a spike that did. -->
      <path
        v-if="peakPath"
        :d="peakPath"
        fill="none"
        stroke="currentColor"
        stroke-width="1"
        stroke-dasharray="3 3"
        class="opacity-50"
        vector-effect="non-scaling-stroke"
      />
      <path
        v-if="path"
        :d="path"
        fill="none"
        stroke="currentColor"
        stroke-width="1.5"
        stroke-linejoin="round"
        vector-effect="non-scaling-stroke"
      />
      <line
        v-for="(mark, i) in marksAt"
        :key="`${mark.label}${i}`"
        :x1="mark.x"
        :x2="mark.x"
        :y1="padding"
        :y2="height - padding"
        :class="mark.tone"
        stroke-width="1"
        vector-effect="non-scaling-stroke"
      >
        <title>{{ mark.count }} {{ mark.label }} at {{ time(mark.start) }}</title>
      </line>
      <line
        v-if="hovered !== null"
        :x1="x(hovered)"
        :x2="x(hovered)"
        y1="0"
        :y2="height"
        class="stroke-toned/40"
        stroke-width="1"
        vector-effect="non-scaling-stroke"
      />
    </svg>
  </div>
</template>
