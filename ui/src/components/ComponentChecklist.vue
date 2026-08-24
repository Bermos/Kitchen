<script setup lang="ts">
import { computed } from "vue";
import type { ComponentStatus } from "../lib/api";
import { timeAgo } from "../lib/format";
import { checklist, componentDetail } from "../lib/updates";
import StatusDot from "./StatusDot.vue";

// The component survey as a checklist: every workload labelled part of Kitchen,
// its pods, and what the ones that are short are waiting for.
//
// It is the same `components[]` the platform's health strip counts and the
// settings page tables — this renders it as the list of things a
// `helm upgrade --wait` is waiting for, which is the one question a table of
// every column cannot answer at a glance.
//
// `seenAt` is not decoration. The survey is written by the operator, so during
// an upgrade the operator's own replacement freezes it: the numbers stay on
// screen and stop being a live claim, and saying when they were read is the
// difference between the two.

const props = defineProps<{
  components: ComponentStatus[];
  /** When this survey was read. */
  seenAt?: string;
  /** Whether it is frozen — the API is not answering just now. */
  stale?: boolean;
  /** Why there is no survey, where there is none. */
  message?: string;
}>();

const ordered = computed(() => checklist(props.components));
const waiting = computed(() => ordered.value.filter((component) => !component.healthy));
</script>

<template>
  <div class="space-y-2">
    <div class="flex items-baseline justify-between gap-3 flex-wrap">
      <p class="text-xs text-muted">
        Components
        <span v-if="components.length" class="text-dimmed">
          — {{ components.length - waiting.length }} of {{ components.length }} have the pods they want
        </span>
      </p>
      <p v-if="seenAt" class="text-[11px]" :class="stale ? 'text-warning' : 'text-dimmed'">
        {{ stale ? "last seen" : "read" }} {{ timeAgo(seenAt) }}
      </p>
    </div>

    <p v-if="!components.length" class="text-xs text-muted">
      {{ message || "No component survey has been read yet." }}
    </p>

    <template v-else>
      <div class="flex flex-wrap items-center gap-x-3 gap-y-1.5 font-mono text-xs">
        <span
          v-for="component in ordered"
          :key="component.name"
          class="inline-flex items-center gap-1.5"
          :title="`${component.kind} — ${componentDetail(component)}`"
        >
          <StatusDot :tone="component.healthy ? 'success' : 'warning'" />
          <span class="text-toned">{{ component.name }}</span>
          <span :class="component.healthy ? 'text-dimmed' : 'text-warning'">
            {{ component.available }}/{{ component.desired }}
          </span>
        </span>
      </div>

      <ul v-if="waiting.length" class="space-y-1">
        <li v-for="component in waiting" :key="component.name" class="text-[11px] text-toned">
          <span class="font-mono text-warning">{{ component.name }}</span>
          — {{ componentDetail(component) }}
        </li>
      </ul>
    </template>
  </div>
</template>
