<script setup lang="ts">
import type { HealthTile } from "../lib/platform";
import StatusDot from "./StatusDot.vue";

// The platform's front page in seven numbers: nodes, components, ingest, store,
// edge, certificates, builds — each green or naming its problem.
//
// A tile is never green by default. Where its source could not be read it says
// so and takes the neutral dot, because the difference between "checked and
// fine" and "not checked" is the difference this whole screen exists to make.

defineProps<{ tiles: HealthTile[] }>();
</script>

<template>
  <div class="grid grid-cols-2 sm:grid-cols-3 lg:grid-cols-4 gap-3">
    <component
      :is="tile.to ? 'RouterLink' : 'div'"
      v-for="tile in tiles"
      :key="tile.key"
      v-bind="tile.to ? { to: tile.to } : {}"
      class="rounded-md border px-4 py-3 block"
      :class="[
        tile.state === 'problem' && tile.tone === 'error'
          ? 'border-error/40 bg-error/5'
          : tile.state === 'problem'
            ? 'border-warning/40 bg-warning/5'
            : 'border-default',
        tile.to ? 'hover:border-accented' : '',
      ]"
    >
      <p class="text-xs text-muted flex items-center gap-2">
        <StatusDot :tone="tile.tone" />
        {{ tile.label }}
      </p>
      <p class="text-lg font-semibold text-highlighted tabular-nums truncate mt-1" :title="tile.value">
        {{ tile.value }}
      </p>
      <p
        class="text-[11px] mt-0.5 line-clamp-2"
        :class="tile.state === 'ok' ? 'text-dimmed' : tile.state === 'unknown' ? 'text-muted' : 'text-toned'"
        :title="tile.detail"
      >
        {{ tile.detail }}
      </p>
    </component>
  </div>
</template>
