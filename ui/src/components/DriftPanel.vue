<script setup lang="ts">
import { computed, ref } from "vue";
import { api, type DriftItem } from "../lib/api";
import { timeAgo } from "../lib/format";
import { useAsync } from "../lib/useAsync";

// Compliance drift: what is running right now that no longer meets its
// environment's bar.
//
// The screen exists to draw one distinction, so the table draws it in the
// status column and again on every rule: a rule that started failing *after*
// this release was promoted is a new finding about an artifact nobody
// touched, and a rule that fired at promotion too and was waived is an
// exception that ran out. They look identical on a blocked verdict and mean
// completely different things.

const all = ref(false);
const drift = useAsync(() => api.complianceDrift({ all: all.value }));

function refine() {
  all.value = !all.value;
  void drift.refresh();
}

const rows = computed<DriftItem[]>(() => drift.data.value?.items ?? []);
const answer = computed(() => drift.data.value);

/** Which rows are opened out to their rules. */
const expanded = ref<Set<string>>(new Set());
function key(item: DriftItem): string {
  return `${item.project}/${item.environment}/${item.release}`;
}
function toggle(item: DriftItem) {
  const open = new Set(expanded.value);
  const id = key(item);
  if (open.has(id)) open.delete(id);
  else open.add(id);
  expanded.value = open;
}

const statusLabels: Record<string, string> = {
  compliant: "compliant",
  waived: "waived",
  "newly-failing": "newly failing",
  "waived-at-promotion": "waiver expired",
  "not-evaluated": "not evaluated",
};

function statusTone(status: string): string {
  switch (status) {
    case "compliant":
      return "text-success";
    case "newly-failing":
      return "text-error";
    case "waived-at-promotion":
    case "waived":
      return "text-warning";
    default:
      return "text-dimmed";
  }
}

function time(iso?: string): string {
  if (!iso) return "—";
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("en-GB");
}
</script>

<template>
  <div class="space-y-2">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <p class="text-sm text-highlighted font-medium">Compliance drift</p>
        <p class="text-xs text-muted mt-0.5">
          What is deployed right now that would not be allowed to deploy today. Every deployed release is
          re-evaluated on a schedule against a current vulnerability database, through the same policy path a
          promotion uses — no rebuild, no redeploy.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <UButton size="xs" color="neutral" :variant="all ? 'solid' : 'subtle'" @click="refine">
          {{ all ? "All pairs" : "Drifting only" }}
        </UButton>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="drift.loading.value"
          aria-label="Refresh compliance drift"
          @click="drift.refresh"
        />
      </div>
    </div>

    <UAlert
      v-if="drift.error.value"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="drift.error.value"
    />

    <!-- Said before the table, because an empty table under a pass that is
         off means "nobody is looking", not "nothing is wrong". -->
    <UAlert
      v-if="answer && !answer.rescanning"
      color="warning"
      variant="soft"
      icon="i-lucide-clock-alert"
      :title="answer.message || 'Continuous re-evaluation is not running.'"
      description="Nothing below has been re-checked since it was promoted."
    />

    <div class="rounded-md border border-default overflow-x-auto">
      <table class="w-full text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-3 py-2 font-medium">Project</th>
            <th class="px-3 py-2 font-medium">Environment</th>
            <th class="px-3 py-2 font-medium">Release</th>
            <th class="px-3 py-2 font-medium">Status</th>
            <th class="px-3 py-2 font-medium">Scanned</th>
            <th class="px-3 py-2 font-medium">Findings</th>
            <th class="px-3 py-2 font-medium">Rules</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!rows.length">
            <td colspan="7" class="px-3 py-8 text-center text-muted text-sm">
              {{
                drift.loading.value
                  ? "Loading…"
                  : all
                    ? "Nothing is deployed against a policy bundle yet."
                    : "Nothing deployed is drifting."
              }}
            </td>
          </tr>
          <template v-for="item in rows" :key="key(item)">
            <tr class="border-b border-muted last:border-0 align-top hover:bg-elevated/40">
              <td class="px-3 py-2 text-xs">
                <RouterLink
                  :to="{ name: 'project', params: { name: item.project } }"
                  class="text-primary hover:underline"
                >
                  {{ item.project }}
                </RouterLink>
              </td>
              <td class="px-3 py-2 text-xs font-mono text-toned break-all">{{ item.environment }}</td>
              <td class="px-3 py-2 text-xs font-mono text-toned break-all">{{ item.release }}</td>
              <td class="px-3 py-2 text-xs font-mono" :class="statusTone(item.status)">
                {{ statusLabels[item.status] ?? item.status }}
              </td>
              <td class="px-3 py-2 text-xs text-dimmed font-mono whitespace-nowrap" :title="item.scannedAt">
                {{ time(item.scannedAt) }}
                <p v-if="item.scannedAt" class="text-[11px]">{{ timeAgo(item.scannedAt) }}</p>
              </td>
              <td class="px-3 py-2 text-xs font-mono text-toned">{{ item.findings ?? 0 }}</td>
              <td class="px-3 py-2 text-xs text-toned whitespace-nowrap">
                <button class="hover:underline" @click="toggle(item)">
                  {{ item.rules.length ? `${item.rules.length} rule(s)` : "—" }}
                </button>
              </td>
            </tr>
            <tr v-if="expanded.has(key(item))" class="border-b border-muted last:border-0">
              <td colspan="7" class="px-3 py-2 bg-elevated/30">
                <div class="space-y-1.5 text-xs">
                  <p class="text-toned">{{ item.message }}</p>
                  <p v-for="rule in item.rules" :key="rule.rule">
                    <span class="font-mono" :class="rule.since === 'rescan' ? 'text-error' : 'text-warning'">
                      {{ rule.rule }}
                    </span>
                    <span class="text-toned"> — {{ rule.message }}</span>
                    <span v-if="rule.since === 'rescan'" class="text-error">
                      (did not fire when this release was promoted)
                    </span>
                    <span v-else class="text-warning">
                      (fired at promotion, waived<template v-if="rule.exception"> by {{ rule.exception }}</template>)
                    </span>
                  </p>
                  <p class="font-mono text-dimmed break-all">
                    <span v-if="item.dataSnapshot">data {{ item.dataSnapshot }}</span>
                    <span v-if="item.artifact" class="ml-2">{{ item.artifact }}</span>
                    <span v-if="item.decisionID" class="ml-2">decision {{ item.decisionID }}</span>
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
