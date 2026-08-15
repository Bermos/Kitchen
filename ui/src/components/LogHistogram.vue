<script setup lang="ts">
import { computed, ref } from "vue";
import type { LogHistogram } from "../lib/api";
import { compactCount } from "../lib/format";

// The shape of the window, above the lines it belongs to. It is how someone
// sees that the errors started at 10:41 without reading a line, and it is how
// they select a range: drag across the bars and the window follows.
//
// Errors and warnings are stacked rather than drawn as their own series. The
// question the chart answers is "how much, and how much of it was bad", and two
// overlaid lines answer it worse than one bar with a red foot.

const props = defineProps<{
  histogram: LogHistogram | null;
  loading?: boolean;
}>();

const emit = defineEmits<{ (event: "select", range: { since: string; until: string }): void }>();

const height = 72;
const width = 1000;

const buckets = computed(() => props.histogram?.buckets ?? []);
const peak = computed(() => Math.max(1, ...buckets.value.map((bucket) => bucket.count)));
const barWidth = computed(() => (buckets.value.length ? width / buckets.value.length : 0));

/** The bar under the pointer, and the bar the drag started on. */
const hovered = ref<number | null>(null);
const anchor = ref<number | null>(null);

// While a drag is in progress the selection is the span between the anchor and
// the pointer, in either direction.
const selection = computed(() => {
  if (anchor.value === null || hovered.value === null) return null;
  return { from: Math.min(anchor.value, hovered.value), to: Math.max(anchor.value, hovered.value) };
});

function bar(bucket: { count: number; errors: number; warnings: number }) {
  const total = (bucket.count / peak.value) * height;
  const errors = bucket.count ? (bucket.errors / bucket.count) * total : 0;
  const warnings = bucket.count ? (bucket.warnings / bucket.count) * total : 0;
  return { total, errors, warnings, rest: total - errors - warnings };
}

function bucketAt(event: PointerEvent): number | null {
  const target = event.currentTarget as SVGElement | null;
  if (!target || !buckets.value.length) return null;
  const box = target.getBoundingClientRect();
  if (!box.width) return null;
  const index = Math.floor(((event.clientX - box.left) / box.width) * buckets.value.length);
  return Math.min(Math.max(index, 0), buckets.value.length - 1);
}

function onDown(event: PointerEvent) {
  anchor.value = bucketAt(event);
  hovered.value = anchor.value;
  // Capture the pointer so a drag that wanders off the chart still ends here,
  // rather than leaving the selection stuck mid-gesture.
  (event.currentTarget as SVGElement | null)?.setPointerCapture?.(event.pointerId);
}

function onMove(event: PointerEvent) {
  hovered.value = bucketAt(event);
}

function onLeave() {
  if (anchor.value === null) hovered.value = null;
}

// A drag selects the span it covers; a click selects the one bucket it landed
// on, which is the same gesture at zero width and is how you zoom into a spike.
function onUp(event: PointerEvent) {
  (event.currentTarget as SVGElement | null)?.releasePointerCapture?.(event.pointerId);
  const range = selection.value;
  anchor.value = null;
  if (!range || !props.histogram) return;
  const step = props.histogram.bucketSeconds * 1000;
  const from = buckets.value[range.from];
  const to = buckets.value[range.to];
  if (!from || !to) return;
  emit("select", {
    since: from.start,
    until: new Date(new Date(to.start).getTime() + step).toISOString(),
  });
}

const tooltip = computed(() => {
  if (hovered.value === null) return null;
  const bucket = buckets.value[hovered.value];
  if (!bucket) return null;
  return {
    at: new Date(bucket.start).toLocaleTimeString("en-GB"),
    count: compactCount(bucket.count),
    errors: bucket.errors,
  };
});

/** The axis: the window's ends, and its middle, which is enough to read it by. */
const axis = computed(() => {
  const histogram = props.histogram;
  if (!histogram || !buckets.value.length) return [];
  const at = (iso: string) => new Date(iso);
  const start = at(histogram.start);
  const end = at(histogram.end);
  const middle = new Date((start.getTime() + end.getTime()) / 2);
  const format = (date: Date) =>
    end.getTime() - start.getTime() > 36 * 3600 * 1000
      ? date.toLocaleDateString("en-GB", { day: "2-digit", month: "short" })
      : date.toLocaleTimeString("en-GB", { hour: "2-digit", minute: "2-digit" });
  return [format(start), format(middle), format(end)];
});
</script>

<template>
  <div class="rounded-md border border-default bg-muted px-3 py-2">
    <div class="flex items-baseline justify-between text-[11px] mb-1">
      <span class="text-muted">
        <template v-if="histogram?.total">{{ compactCount(histogram.total) }} lines</template>
        <template v-else-if="loading">Counting…</template>
        <template v-else>No lines in this window</template>
      </span>
      <span v-if="tooltip" class="font-mono text-toned tabular-nums">
        {{ tooltip.at }} · {{ tooltip.count }}
        <span v-if="tooltip.errors" class="text-error">· {{ tooltip.errors }} error</span>
      </span>
      <span v-else class="text-dimmed">drag to select a range</span>
    </div>

    <svg
      :viewBox="`0 0 ${width} ${height}`"
      preserveAspectRatio="none"
      class="w-full select-none touch-none"
      :style="{ height: `${height}px` }"
      :class="buckets.length ? 'cursor-crosshair' : 'cursor-default'"
      @pointerdown="onDown"
      @pointermove="onMove"
      @pointerup="onUp"
      @pointerleave="onLeave"
    >
      <rect
        v-if="selection"
        :x="selection.from * barWidth"
        :width="(selection.to - selection.from + 1) * barWidth"
        y="0"
        :height="height"
        class="fill-primary/15"
      />
      <g v-for="(bucket, i) in buckets" :key="bucket.start">
        <rect
          :x="i * barWidth"
          :width="Math.max(barWidth - 1, 0.5)"
          :y="height - bar(bucket).total"
          :height="bar(bucket).rest"
          class="fill-primary/70"
        />
        <rect
          v-if="bucket.warnings"
          :x="i * barWidth"
          :width="Math.max(barWidth - 1, 0.5)"
          :y="height - bar(bucket).errors - bar(bucket).warnings"
          :height="bar(bucket).warnings"
          class="fill-warning"
        />
        <rect
          v-if="bucket.errors"
          :x="i * barWidth"
          :width="Math.max(barWidth - 1, 0.5)"
          :y="height - bar(bucket).errors"
          :height="bar(bucket).errors"
          class="fill-error"
        />
      </g>
    </svg>

    <div v-if="axis.length" class="flex justify-between text-[10px] text-dimmed font-mono mt-1">
      <span v-for="(label, i) in axis" :key="i">{{ label }}</span>
    </div>
  </div>
</template>
