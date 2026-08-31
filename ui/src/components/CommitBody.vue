<script setup lang="ts">
/**
 * What a commit says under its subject, once somebody has asked for it.
 *
 * A message is written for whoever reads `git log`: paragraphs, a list of what
 * changed, trailers. It keeps the shape it was written in — the platform never
 * reflows it — so this is a `pre`, wrapped rather than scrolled sideways, and
 * given a height of its own so that a long body cannot push the page it is on
 * out of shape.
 */
defineProps<{ body?: string }>();
</script>

<template>
  <pre
    v-if="body"
    class="text-xs font-mono text-toned whitespace-pre-wrap break-words max-h-96 overflow-y-auto"
    >{{ body }}</pre
  >
</template>
