<script setup lang="ts">
/**
 * A titled block within a screen.
 *
 * The same drift as `PageHeader.vue`, one level down: thirty-odd `<h2>`
 * elements across the dashboard, half of them `font-medium` and half
 * `font-semibold`, some `mb-2`, some `mb-3`, some with a sentence under the
 * heading and some without. The heading scale is in `docs/UI.md` and is
 * enforced by `src/lib/design.test.ts`; this is where a section gets it
 * without anybody having to remember it.
 *
 * `id` is here rather than left to the caller because a finding's evidence
 * link scrolls to a section (`?section=workload` on the environment screen),
 * and an anchor that lives on a wrapper somebody added later is an anchor that
 * disappears in the next refactor.
 */
withDefaults(
  defineProps<{
    title: string;
    description?: string;
    /** The anchor a `?section=` in the URL scrolls to, when this section is
     * one a finding links at. */
    id?: string;
  }>(),
  { description: undefined, id: undefined },
);
</script>

<template>
  <section :id="id">
    <div class="flex items-start justify-between gap-3" :class="description || $slots.description ? 'mb-3' : 'mb-2'">
      <div class="min-w-0">
        <h2 class="text-sm font-medium text-highlighted">{{ title }}</h2>
        <p v-if="description || $slots.description" class="text-xs text-muted mt-1">
          <slot name="description">{{ description }}</slot>
        </p>
      </div>
      <div v-if="$slots.actions" class="flex items-center gap-2 shrink-0">
        <slot name="actions" />
      </div>
    </div>
    <slot />
  </section>
</template>
