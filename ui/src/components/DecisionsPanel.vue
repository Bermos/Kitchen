<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type Decision, type DecisionReplay } from "../lib/api";
import { firedSummary, shortDigest, verdictTone } from "../lib/decisions";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
import { useAsync } from "../lib/useAsync";

// The decision register: every verdict the policy engine reached, stored with
// the digests that reproduce it. The replay button is the register's whole
// point made clickable — re-run a historical decision from its stored inputs
// and watch the same verdict come out.

const verdicts = [
  { label: "Every verdict", value: "" },
  { label: "allowed", value: "allowed" },
  { label: "allowed-with-exception", value: "allowed-with-exception" },
  { label: "blocked", value: "blocked" },
];
const kinds = [
  { label: "Every kind", value: "" },
  { label: "promotion", value: "promotion" },
  { label: "rescan", value: "rescan" },
  { label: "replay", value: "replay" },
];

const verdict = ref("");
const kind = ref("");
const decisions = useAsync(() =>
  api.decisions({ verdict: verdict.value || undefined, kind: kind.value || undefined }),
);
function refine() {
  void decisions.refresh();
}

const rows = computed(() => decisions.data.value ?? []);

/** Which rows are opened out to their fired rules and digests. */
const expanded = ref<Set<string>>(new Set());
function toggle(id: string) {
  const open = new Set(expanded.value);
  if (open.has(id)) {
    open.delete(id);
  } else {
    open.add(id);
  }
  expanded.value = open;
}

// The route admits any account and the handler wants developer on the
// decision's project; this screen is the operator's, so the gate here is the
// table's word, and the API stays the authority.
const mayReplay = computed(() => may("POST /api/v1/decisions/{id}/replay", callerFor()));

const replaying = ref("");
const replays = ref<Record<string, DecisionReplay>>({});
const replayError = ref("");

async function replay(decision: Decision) {
  replaying.value = decision.id;
  replayError.value = "";
  try {
    replays.value = { ...replays.value, [decision.id]: await api.replayDecision(decision.id) };
    void decisions.refresh();
  } catch (cause) {
    replayError.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    replaying.value = "";
  }
}

function time(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("en-GB");
}
</script>

<template>
  <div class="space-y-2">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <p class="text-sm text-highlighted font-medium">Decisions</p>
        <p class="text-xs text-muted mt-0.5">
          Every verdict the policy engine reached — promotions, scheduled re-evaluations, replays — stored with the
          bundle digest and input digest it can be reproduced from.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <USelect v-model="kind" :items="kinds" size="xs" class="w-32" @update:model-value="refine" />
        <USelect v-model="verdict" :items="verdicts" size="xs" class="w-44" @update:model-value="refine" />
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="decisions.loading.value"
          aria-label="Refresh decisions"
          @click="decisions.refresh"
        />
      </div>
    </div>

    <UAlert
      v-if="decisions.error.value"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="decisions.error.value"
    />
    <UAlert v-if="replayError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="replayError" />

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-3 py-2 font-medium">When</th>
            <th class="px-3 py-2 font-medium">Kind</th>
            <th class="px-3 py-2 font-medium">Project</th>
            <th class="px-3 py-2 font-medium">Environment</th>
            <th class="px-3 py-2 font-medium">Release</th>
            <th class="px-3 py-2 font-medium">Verdict</th>
            <th class="px-3 py-2 font-medium">Rules</th>
            <th class="px-3 py-2"></th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!rows.length">
            <td colspan="8" class="px-3 py-8 text-center text-muted text-sm">
              {{
                decisions.loading.value
                  ? "Loading…"
                  : "No decisions stored. They appear when an environment declares requirements and something is promoted against them."
              }}
            </td>
          </tr>
          <template v-for="decision in rows" :key="decision.id">
            <tr class="border-b border-muted last:border-0 align-top hover:bg-elevated/40">
              <td class="px-3 py-2 text-xs text-dimmed font-mono whitespace-nowrap" :title="decision.timestamp">
                {{ time(decision.timestamp) }}
                <p class="text-[11px]">{{ timeAgo(decision.timestamp) }}</p>
              </td>
              <td class="px-3 py-2 text-xs font-mono text-toned">{{ decision.kind }}</td>
              <td class="px-3 py-2 text-xs">
                <RouterLink
                  v-if="decision.project"
                  :to="{ name: 'project', params: { name: decision.project } }"
                  class="text-primary hover:underline"
                >
                  {{ decision.project }}
                </RouterLink>
                <span v-else class="text-dimmed">—</span>
              </td>
              <td class="px-3 py-2 text-xs font-mono text-toned break-all">{{ decision.environment || "—" }}</td>
              <td class="px-3 py-2 text-xs font-mono text-toned break-all">{{ decision.release || "—" }}</td>
              <td class="px-3 py-2 text-xs font-mono" :class="verdictTone(decision.verdict)">
                {{ decision.verdict }}
              </td>
              <td class="px-3 py-2 text-xs text-toned whitespace-nowrap">
                <button class="hover:underline" @click="toggle(decision.id)">
                  {{ firedSummary(decision.rulesFired) }}
                </button>
              </td>
              <td class="px-3 py-2 text-right whitespace-nowrap">
                <UButton
                  v-if="mayReplay"
                  size="xs"
                  color="neutral"
                  variant="subtle"
                  :loading="replaying === decision.id"
                  title="Re-evaluate this decision from its stored inputs"
                  @click="replay(decision)"
                >
                  Replay
                </UButton>
              </td>
            </tr>
            <tr v-if="expanded.has(decision.id) || replays[decision.id]" class="border-b border-muted last:border-0">
              <td colspan="8" class="px-3 py-2 bg-elevated/30">
                <div class="space-y-1.5 text-xs">
                  <p class="font-mono text-dimmed break-all">
                    <span :title="decision.bundleDigest">bundle {{ shortDigest(decision.bundleDigest) }}</span>
                    <span class="mx-2" :title="decision.inputDigest">input {{ shortDigest(decision.inputDigest) }}</span>
                    <span v-if="decision.dataSnapshot">data {{ decision.dataSnapshot }}</span>
                    <span v-if="decision.decidedBy" class="ml-2 text-dimmed/70">by {{ decision.decidedBy }}</span>
                  </p>
                  <p v-if="!(decision.rulesFired ?? []).length" class="text-dimmed">No rules fired.</p>
                  <p v-for="rule in decision.rulesFired ?? []" :key="rule.rule + (rule.message ?? '')">
                    <span class="font-mono" :class="rule.waived ? 'text-warning' : 'text-error'">{{ rule.rule }}</span>
                    <span class="text-toned"> — {{ rule.message }}</span>
                    <span v-if="rule.waived" class="text-warning"> (waived by {{ rule.exception }})</span>
                  </p>
                  <p v-if="replays[decision.id]" class="flex items-center gap-1.5">
                    <UIcon
                      :name="replays[decision.id].match ? 'i-lucide-shield-check' : 'i-lucide-shield-alert'"
                      class="size-4"
                      :class="replays[decision.id].match ? 'text-success' : 'text-error'"
                    />
                    <span :class="replays[decision.id].match ? 'text-success' : 'text-error'">
                      <template v-if="replays[decision.id].match">
                        Replayed: {{ replays[decision.id].replay.verdict }} — the stored decision reproduces.
                      </template>
                      <template v-else>
                        Replayed: {{ replays[decision.id].original.verdict }} then,
                        {{ replays[decision.id].replay.verdict }} now — the stored decision does not reproduce.
                      </template>
                    </span>
                  </p>
                </div>
              </td>
            </tr>
          </template>
        </tbody>
      </table>
    </div>
  </div>
</template>
