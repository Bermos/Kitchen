<script setup lang="ts">
/**
 * The control that shows a commit's body, on a row that has one.
 *
 * It is one button in two tables, which is the whole reason it is a component:
 * what it says to a screen reader and on hover is the same sentence in both,
 * and a row's own click means something else — it opens the build.
 */
defineProps<{ open: boolean }>();
defineEmits<{ toggle: [] }>();
</script>

<template>
  <UButton
    :icon="open ? 'i-lucide-chevron-up' : 'i-lucide-ellipsis'"
    color="neutral"
    variant="ghost"
    size="xs"
    class="shrink-0"
    :aria-label="open ? 'Hide the commit message' : 'Show the whole commit message'"
    :title="open ? 'Hide the commit message' : 'Show the whole commit message'"
    @click.stop="$emit('toggle')"
  />
</template>
