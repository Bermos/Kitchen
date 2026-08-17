<script setup lang="ts">
import { computed } from "vue";
import type { SignalsAnswer } from "../lib/api";
import { timeAgo } from "../lib/format";
import { hasSomethingToSay, problemsSentence, sortFindings, unreadableSentence } from "../lib/signals";
import FindingRow from "./FindingRow.vue";

// One evaluated round, on screen. The operator's problems list and the
// environment page's diagnostics strip are this component twice — same
// catalogue, same shape, same rules about what an empty list means.
//
// Those rules are the whole of it:
//
//   - `unreadable` is not an empty problems list. Every input the evaluation
//     could not read is named, with its reason, above the findings — because a
//     platform reporting no problems because it could not check anything is the
//     exact failure this design exists to prevent.
//   - "nothing is wrong" is only said when nothing went unread. Where something
//     did, the sentence says so instead.
//   - a finding whose severity is `unknown` is a rule that could not be
//     evaluated; it renders as neither health nor alarm (see severityTone).

const props = withDefaults(
  defineProps<{
    answer: SignalsAnswer | null;
    loading?: boolean;
    error?: string | null;
    /** `list` is the operator's problems list; `strip` is the environment page's
     * diagnostics strip, which renders nothing at all when there is nothing to
     * say. */
    variant?: "list" | "strip";
    /** The heading, for the list. */
    title?: string;
    /** What to say when the round is genuinely clean. */
    empty?: string;
  }>(),
  {
    variant: "list",
    title: "Problems",
    empty: "Nothing is firing. Every rule in the catalogue was evaluated and none of them matched.",
  },
);

const findings = computed(() => sortFindings(props.answer?.items));
const unreadable = computed(() => props.answer?.unreadable ?? []);
const counts = computed(() => props.answer?.counts);
/** A strip is a thing that appears when there is a problem; nothing to say is
 * no strip, not an empty one. */
const silent = computed(() => props.variant === "strip" && !props.error && !hasSomethingToSay(props.answer));

/** The panel's own tone: the worst thing in it. An unreadable input is not an
 * alarm, but it is not calm either. */
const frame = computed(() => {
  if (props.error || counts.value?.critical) return "border-error/40";
  if (counts.value?.warning || unreadable.value.length) return "border-warning/40";
  return "border-default";
});

const headline = computed(() => {
  const total = counts.value;
  if (!total) return "";
  const parts: string[] = [];
  if (total.critical) parts.push(`${total.critical} critical`);
  if (total.warning) parts.push(`${total.warning} warning`);
  if (total.info) parts.push(`${total.info} info`);
  return parts.join(" · ");
});
</script>

<template>
  <div v-if="!silent" class="rounded-md border overflow-hidden" :class="frame">
    <div class="px-4 py-2.5 border-b border-default bg-muted flex items-center justify-between gap-3 flex-wrap">
      <h2 class="text-xs font-medium text-muted flex items-center gap-2">
        <UIcon
          v-if="variant === 'strip'"
          name="i-lucide-stethoscope"
          class="size-3.5"
          :class="counts?.critical ? 'text-error' : counts?.warning ? 'text-warning' : 'text-dimmed'"
        />
        {{ variant === "strip" ? (error ? "Diagnostics" : problemsSentence(counts)) : title }}
      </h2>
      <div class="flex items-center gap-3 text-[11px]">
        <span v-if="headline" class="font-mono text-dimmed">{{ headline }}</span>
        <span v-if="answer?.evaluatedAt" class="text-dimmed" :title="answer.evaluatedAt">
          evaluated {{ timeAgo(answer.evaluatedAt) }}
        </span>
      </div>
    </div>

    <UAlert
      v-if="error"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      title="The catalogue could not be evaluated"
      :description="error"
      class="rounded-none border-0"
    />

    <template v-else>
      <!-- Rendered above the findings and never among them: these are rules
           that did not run, not conditions that fired. -->
      <div v-if="unreadable.length" class="px-4 py-3 bg-warning/5 border-b border-warning/30">
        <p class="text-xs text-warning flex items-start gap-2">
          <UIcon name="i-lucide-eye-off" class="size-4 shrink-0 mt-px" />
          <span>{{ unreadableSentence(unreadable, findings.length) }}</span>
        </p>
        <ul class="mt-2 space-y-1">
          <li v-for="failure in unreadable" :key="failure.input" class="text-[11px] flex items-start gap-2">
            <span class="font-mono text-toned shrink-0">{{ failure.input }}</span>
            <span class="text-dimmed break-words">{{ failure.reason }}</span>
          </li>
        </ul>
      </div>

      <div v-if="findings.length" class="divide-y divide-muted">
        <FindingRow
          v-for="finding in findings"
          :key="finding.fingerprint"
          :finding="finding"
          :dense="variant === 'strip'"
        />
      </div>

      <!-- The clean answer, said only where it is true: an empty list beside an
           unreadable input means nobody looked, and that sentence is above. -->
      <p
        v-else-if="!unreadable.length"
        class="px-4 py-3 text-xs text-muted flex items-center gap-2"
      >
        <UIcon name="i-lucide-shield-check" class="size-4 text-success shrink-0" />
        <span>{{ loading && !answer ? "Evaluating…" : empty }}</span>
      </p>
    </template>
  </div>
</template>
