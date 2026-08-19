<script setup lang="ts">
import type { SignalTile } from "../lib/requests";
import Sparkline from "./Sparkline.vue";

// The four numbers an environment is read by, with the shape of the window
// next to each. It is deliberately the same tile the overview and the
// observability view already use — one dashboard, and a number that means the
// same thing should look the same wherever it is read.
//
// The tiles are given rather than computed here: which four they are depends on
// whether the platform's edge reaches this environment at all (§3.4), and the
// component that knows that is the one that asked.

defineProps<{ tiles: SignalTile[] }>();
</script>

<template>
  <div class="grid grid-cols-2 lg:grid-cols-4 gap-3">
    <div v-for="tile in tiles" :key="tile.label" class="rounded-md border border-default px-3 sm:px-4 py-3">
      <p class="text-xs text-muted truncate" :title="tile.label">{{ tile.label }}</p>
      <div class="flex items-end justify-between gap-3 mt-1">
        <span class="text-lg sm:text-xl font-semibold text-highlighted tabular-nums truncate">{{ tile.value }}</span>
        <!-- Two tiles to a phone's width leave the number and the shape of the
             window fighting over the same 150 pixels; the number wins. -->
        <Sparkline v-if="tile.points?.length" :points="tile.points" :tone="tile.tone" class="hidden sm:block" />
      </div>
      <p v-if="tile.detail" class="text-[11px] text-dimmed mt-1 truncate" :title="tile.detail">{{ tile.detail }}</p>
    </div>
  </div>
</template>
