<script setup lang="ts">
import { computed } from "vue";
import type { Finding } from "../lib/api";
import { uptime } from "../lib/format";
import {
  evidenceLabel,
  evidenceLocation,
  firstClause,
  severityIcon,
  severityLabel,
  severityTone,
} from "../lib/signals";
import StatusDot from "./StatusDot.vue";

// One finding, rendered the same way wherever it appears: the operator's
// problems list and the developer's diagnostics strip are the same rows in two
// sizes, because they are the same catalogue evaluated the same way and
// narrowed differently.
//
// Five things, always: how bad it is, what it is, the numbers, how long it has
// been firing, and where to go and look. A finding without its evidence link is
// a sentence about a number nobody can check.

const props = defineProps<{
  finding: Finding;
  /** The strip's size: title and the headline clause on one line. */
  dense?: boolean;
}>();

const tone = computed(() => severityTone(props.finding.severity));
const target = computed(() => evidenceLocation(props.finding.evidence));
const label = computed(() => evidenceLabel(props.finding.evidence));
/** The detail's first clause is the headline number by contract, which is what
 * lets the strip render `title (12 restarts in 30m)` without knowing anything
 * about the rule behind it. */
const headline = computed(() => firstClause(props.finding.detail));
</script>

<template>
  <div class="flex items-start gap-3" :class="dense ? 'px-4 py-1.5 text-xs' : 'px-4 py-3 text-sm'">
    <UIcon
      :name="severityIcon(finding.severity)"
      class="shrink-0 mt-0.5"
      :class="[
        dense ? 'size-3.5' : 'size-4',
        tone === 'error' ? 'text-error' : tone === 'warning' ? 'text-warning' : tone === 'info' ? 'text-info' : 'text-dimmed',
      ]"
      :title="severityLabel(finding.severity)"
    />

    <div class="min-w-0 flex-1">
      <div class="flex items-baseline gap-2 flex-wrap">
        <span class="text-highlighted font-medium">{{ finding.title }}</span>
        <!-- The strip shows the headline clause and keeps the rest on hover;
             the list shows the whole detail below. Nothing is lost either way. -->
        <span v-if="dense && headline" class="text-toned truncate" :title="finding.detail">{{ headline }}</span>
        <!-- The rule's name, because a finding is a versioned rule and knowing
             which one fired is how it gets argued with. -->
        <span v-if="!dense" class="font-mono text-[11px] text-dimmed">{{ finding.signal }}</span>
      </div>
      <p v-if="!dense" class="text-toned mt-0.5 break-words">{{ finding.detail }}</p>
      <p v-if="!dense && finding.scope" class="text-[11px] text-dimmed mt-0.5 font-mono truncate">
        {{ finding.scope.kind }}<template v-if="finding.scope.name || finding.scope.node"> · </template>
        {{ [finding.scope.project, finding.scope.environment, finding.scope.namespace, finding.scope.node, finding.scope.name].filter(Boolean).join("/") }}
      </p>
    </div>

    <span
      class="shrink-0 text-dimmed tabular-nums whitespace-nowrap"
      :class="dense ? 'text-[11px]' : 'text-xs'"
      :title="`firing since ${finding.since}`"
    >
      {{ uptime(finding.since) }}
    </span>

    <RouterLink
      v-if="target"
      :to="target"
      class="shrink-0 text-primary hover:underline inline-flex items-center gap-1 whitespace-nowrap"
      :class="dense ? 'text-[11px]' : 'text-xs'"
      :title="finding.evidence"
    >
      {{ label }}
      <UIcon name="i-lucide-arrow-right" class="size-3" />
    </RouterLink>
    <StatusDot v-else :tone="tone" class="shrink-0 mt-1.5" />
  </div>
</template>
