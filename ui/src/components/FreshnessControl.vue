<script setup lang="ts">
/**
 * How old this screen is, and the button that stops it moving.
 *
 * Five pollers were running on the overview with nothing on the screen saying
 * so: rows reordered themselves under the cursor, and a metrics store that had
 * stopped answering left confident numbers from twenty minutes ago. This is
 * the one place a screen admits both — the age of the oldest thing on it, and
 * whether it is still being told the truth.
 *
 * It renders from the screen's `ScreenFreshness` object (`lib/freshness.ts`),
 * which
 * every `useAsync` on the screen reports into, so it covers the panels inside
 * a view as well as the view's own fetches. `PageHeader` places it; a screen
 * only has to hand it over.
 */
import { computed } from "vue";
import type { ScreenFreshness } from "../lib/freshness";
import type { Tone } from "../lib/status";
import StatusDot from "./StatusDot.vue";

const props = defineProps<{ freshness: ScreenFreshness }>();

const tones: Record<string, Tone> = {
  loading: "neutral",
  live: "success",
  paused: "info",
  stale: "warning",
};
const tone = computed<Tone>(() => tones[props.freshness.state.value] ?? "neutral");
const text = computed(() => {
  const colours: Record<Tone, string> = {
    success: "text-muted",
    warning: "text-warning",
    info: "text-info",
    error: "text-error",
    neutral: "text-dimmed",
  };
  return colours[tone.value];
});

const queued = computed(() => props.freshness.queued.value);
const action = computed(() => {
  if (!props.freshness.paused.value) return "Pause while I read";
  if (queued.value > 0) return `Apply ${queued.value} change${queued.value === 1 ? "" : "s"}`;
  return "Resume";
});
// The one thing the label cannot say in four words: what pausing does, and
// that it lets go by itself so a screen left paused does not quietly rot.
const explanation = computed(() =>
  props.freshness.paused.value
    ? "Held while you read — newer data is waiting and will be applied in a few minutes if you do not."
    : "Hold the screen still while you read it. Newer data queues and is applied when you resume.",
);

function toggle() {
  if (props.freshness.paused.value) props.freshness.resume();
  else props.freshness.pause();
}
</script>

<template>
  <div class="flex items-center gap-2">
    <span
      class="inline-flex items-center gap-1.5 rounded-md border border-default px-2 py-1 text-xs tabular-nums"
      :class="text"
      role="status"
      aria-live="polite"
    >
      <!-- No pulse: this dot is on every screen at all times, and a dot that
           never stops moving is the thing the eye learns to ignore. -->
      <StatusDot :tone="tone" />
      {{ freshness.label.value }}
    </span>
    <UButton
      size="xs"
      color="neutral"
      :variant="freshness.paused.value ? 'soft' : 'subtle'"
      :icon="freshness.paused.value ? 'i-lucide-play' : 'i-lucide-pause'"
      :title="explanation"
      @click="toggle"
    >
      {{ action }}
    </UButton>
  </div>
</template>
