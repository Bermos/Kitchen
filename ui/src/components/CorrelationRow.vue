<script setup lang="ts">
import { computed, ref } from "vue";
import { uptime } from "../lib/format";
import {
  confidenceIcon,
  confidenceLabel,
  confidenceMeaning,
  confidenceTone,
  type Correlation,
} from "../lib/signals";
import FindingRow from "./FindingRow.vue";

// One correlation: several projects failing at once, and the rows it stands in
// front of.
//
// The badge is the row's subject rather than its decoration. An operator
// handed "three projects degraded" with no qualifier goes looking for the
// shared cause the evaluation already failed to find, so the rung says what is
// known *and* what is not: at `Coincidence` the detail ends with "nothing
// explains it yet", which is the sentence this whole screen exists to be able
// to print.
//
// The folded rows are collapsed and not hidden. A failure that is part of one
// incident is one row on this screen — that is the fold — but the reader still
// has to be able to see which projects, and clicking is a cheaper price than
// scrolling past four rows of the same outage.

const props = defineProps<{ correlation: Correlation }>();

const open = ref(false);
const rung = computed(() => props.correlation.finding.confidence ?? "coincidence");
const projects = computed(() => props.correlation.finding.projects ?? []);
const folded = computed(() => props.correlation.folded);
</script>

<template>
  <div>
    <div class="px-4 py-3 flex items-start gap-3">
      <UIcon
        :name="confidenceIcon(rung)"
        class="size-4 shrink-0 mt-0.5"
        :class="
          confidenceTone(rung) === 'error'
            ? 'text-error'
            : confidenceTone(rung) === 'warning'
              ? 'text-warning'
              : 'text-dimmed'
        "
        :title="confidenceMeaning(rung)"
      />

      <div class="min-w-0 flex-1">
        <div class="flex items-baseline gap-2 flex-wrap">
          <span class="text-sm text-highlighted font-medium">{{ correlation.finding.title }}</span>
          <span
            class="rounded px-1.5 py-0.5 text-[11px]"
            :class="
              confidenceTone(rung) === 'error'
                ? 'bg-error/10 text-error'
                : confidenceTone(rung) === 'warning'
                  ? 'bg-warning/10 text-warning'
                  : 'bg-elevated text-toned'
            "
          >
            {{ confidenceLabel(rung) }}
          </span>
          <span class="font-mono text-[11px] text-dimmed">{{ correlation.finding.signal }}</span>
        </div>
        <p class="text-sm text-toned mt-0.5 break-words">{{ correlation.finding.detail }}</p>
        <div v-if="projects.length" class="flex items-center gap-1.5 flex-wrap mt-1">
          <span
            v-for="project in projects"
            :key="project"
            class="rounded bg-elevated px-1.5 py-0.5 text-[11px] font-mono text-toned"
          >
            {{ project }}
          </span>
        </div>
        <button
          v-if="folded.length"
          type="button"
          class="text-[11px] text-primary hover:underline mt-1.5 inline-flex items-center gap-1"
          @click="open = !open"
        >
          <UIcon :name="open ? 'i-lucide-chevron-down' : 'i-lucide-chevron-right'" class="size-3" />
          {{ folded.length }} project {{ folded.length === 1 ? "row folds" : "rows fold" }} into this
        </button>
      </div>

      <span
        class="shrink-0 text-xs text-dimmed tabular-nums whitespace-nowrap"
        :title="`firing since ${correlation.finding.since}`"
      >
        {{ uptime(correlation.finding.since) }}
      </span>
    </div>

    <div v-if="open && folded.length" class="border-t border-default bg-muted divide-y divide-muted">
      <FindingRow v-for="row in folded" :key="row.fingerprint" :finding="row" dense />
    </div>
  </div>
</template>
