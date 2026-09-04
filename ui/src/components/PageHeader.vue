<script setup lang="ts">
/**
 * The top of every screen, and the reason every screen's top is the same.
 *
 * The dashboard drifted here first: a page's title block was written again on
 * each new screen, so twenty-two of them ended up with three alignments
 * (`items-center`, `items-start`, neither), two gaps, a description on some
 * and not others, and a breadcrumb spelled out by hand wherever there was
 * one. None of that was a decision — it was twenty-two independent guesses at
 * the same shape.
 *
 * So the shape lives here and a screen names its parts. `docs/UI.md` is the
 * whole of the reasoning; `src/lib/design.test.ts` refuses a view that does
 * not use this.
 *
 * The slots exist because the parts vary and the frame does not:
 *
 * - `badges` — what the title *is*: a phase, an environment type, a data
 *   classification. Beside the title, because it qualifies the noun.
 * - `description` (or the `description` prop) — what the screen answers, in a
 *   sentence. Every screen owes one: a heading alone says what a page is
 *   called, never what it is for.
 * - `meta` — the small facts under it: a repository, a branch, when it was
 *   created.
 * - `actions` — what can be done to the thing named, right-aligned and
 *   wrapping under the title on a narrow viewport rather than squeezing it.
 *
 * And one part that is not a slot: `freshness`. A screen that polls says how
 * old it is and offers to hold still while it is read, and it says it in the
 * same place on every screen — see `docs/UI.md`, "The freshness control". The
 * object comes from `useFreshness()`; the header only places it.
 */
import type { RouteLocationRaw } from "vue-router";
import type { ScreenFreshness } from "../lib/freshness";
import FreshnessControl from "./FreshnessControl.vue";

/** One step of the trail. The last has no `to` — it is where you are. */
export interface Crumb {
  label: string;
  to?: RouteLocationRaw;
  /** Identifiers are set in mono, prose is not. The trail's last step is
   * usually an object's name, so this is how it is told apart from a section
   * name like "Platform". */
  mono?: boolean;
}

withDefaults(
  defineProps<{
    title: string;
    description?: string;
    breadcrumb?: Crumb[];
    /** The screen's freshness, on a screen that polls. */
    freshness?: ScreenFreshness;
  }>(),
  { description: undefined, breadcrumb: () => [], freshness: undefined },
);
</script>

<template>
  <div class="flex items-start justify-between gap-4 flex-wrap">
    <div class="min-w-0">
      <nav v-if="breadcrumb.length" class="flex items-center gap-2 text-xs text-muted mb-1" aria-label="Breadcrumb">
        <template v-for="(crumb, index) in breadcrumb" :key="index">
          <span v-if="index" aria-hidden="true">/</span>
          <RouterLink v-if="crumb.to" :to="crumb.to" class="hover:text-highlighted" :class="{ 'font-mono': crumb.mono }">
            {{ crumb.label }}
          </RouterLink>
          <span v-else class="text-toned" :class="{ 'font-mono': crumb.mono }" aria-current="page">
            {{ crumb.label }}
          </span>
        </template>
      </nav>

      <div class="flex items-center gap-3 flex-wrap">
        <h1 class="text-xl font-semibold text-highlighted">{{ title }}</h1>
        <slot name="badges" />
      </div>

      <p v-if="description || $slots.description" class="text-xs text-muted mt-1">
        <slot name="description">{{ description }}</slot>
      </p>

      <div v-if="$slots.meta" class="flex items-center gap-3 mt-1 text-xs text-muted flex-wrap">
        <slot name="meta" />
      </div>
    </div>

    <div v-if="$slots.actions || freshness" class="flex items-center gap-2 flex-wrap">
      <FreshnessControl v-if="freshness" :freshness="freshness" />
      <slot name="actions" />
    </div>
  </div>
</template>
