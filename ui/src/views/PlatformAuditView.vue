<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import DecisionsPanel from "../components/DecisionsPanel.vue";
import { api, type AuditRecord } from "../lib/api";
import { timeAgo } from "../lib/format";
import { useAsync } from "../lib/useAsync";

// The audit log: what the platform did, as evidence rather than as prose.
//
// It is not /platform/events (the cluster's warnings) and not the activity feed
// on the overview (the platform's story, written best-effort). Every row here
// is chained to the one before it, and a transition the platform could not
// record is a transition it refused to make — see docs/COMPLIANCE.md.
//
// The screen leads with the chain's verdict rather than with the rows, because
// the rows are only worth reading if the chain holds. The verdict is asked for
// rather than polled: verifying is a scan, and a number that changes on its own
// every thirty seconds invites nobody to look at it.

const route = useRoute();
const router = useRouter();

/** The filters the store itself applies — the three questions anyone asks of
 *  an audit log: what happened to this object, what did this person do, and
 *  what happened in this window. */
const FILTERS = ["kind", "name", "project", "actor"] as const;

const ranges = [
  { label: "Last 24 hours", value: 1440 },
  { label: "Last 7 days", value: 10080 },
  { label: "Last 30 days", value: 43200 },
  { label: "Last 90 days", value: 129600 },
  { label: "Everything kept", value: 0 },
];
const limits = [100, 250, 500, 1000];

function param(key: string): string {
  return (route.query[key] as string) ?? "";
}
const rangeMinutes = computed(() => Number(route.query.range ?? 10080) || 0);
const limit = computed(() => Number(route.query.limit ?? 100) || 100);

function apply(patch: Record<string, string | number | undefined>) {
  const query: Record<string, string> = {};
  for (const [key, value] of Object.entries({ ...route.query, ...patch })) {
    const text = value === undefined || value === null ? "" : String(value);
    if (text) query[key] = text;
  }
  void router.replace({ path: "/platform/audit", query });
}

function selection() {
  const query: Record<string, string | number> = { limit: limit.value };
  if (rangeMinutes.value > 0) {
    query.since = new Date(Date.now() - rangeMinutes.value * 60_000).toISOString();
  }
  for (const key of FILTERS) {
    const value = param(key);
    if (value) query[key] = value;
  }
  return query;
}

const records = useAsync(() => api.audit(selection()));
const compliance = useAsync(() => api.compliance());
// The URL is the selection, so a changed URL is a changed question — but only
// while this is still the screen being looked at.
watch(
  () => route.fullPath,
  (path) => {
    if (path.startsWith("/platform/audit")) void records.refresh();
  },
);

const verification = ref<Awaited<ReturnType<typeof api.verifyAudit>> | null>(null);
const verifyError = ref("");
const verifying = ref(false);

async function verify() {
  verifying.value = true;
  verifyError.value = "";
  try {
    verification.value = await api.verifyAudit(1);
  } catch (cause) {
    verifyError.value = cause instanceof Error ? cause.message : String(cause);
  } finally {
    verifying.value = false;
  }
}

const rows = computed(() => records.data.value ?? []);

const chips = computed(() => {
  const active: { key: string; value: string }[] = [];
  for (const key of FILTERS) {
    const value = param(key);
    if (value) active.push({ key, value });
  }
  return active;
});

function narrow(field: string, value: string) {
  apply({ [field]: param(field) === value ? undefined : value });
}

function clearAll() {
  void router.replace({ path: "/platform/audit" });
}

function time(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("en-GB");
}

/** Enough of a hash to tell two apart, which is all a row needs; the whole
 *  thing is in the title attribute for anyone checking it by hand. */
function short(hash: string): string {
  return hash ? hash.slice(0, 12) : "—";
}

/** The move a record describes, in the fewest words that stay accurate. */
function move(record: AuditRecord): string {
  if (record.fromState && record.toState) return `${record.fromState} → ${record.toState}`;
  if (record.toState) return `→ ${record.toState}`;
  if (record.fromState) return `${record.fromState} →`;
  return "";
}

const operationColour: Record<string, string> = {
  create: "text-success",
  update: "text-info",
  transition: "text-toned",
  delete: "text-error",
};

/** A gap between the last record and the anchor is the one edit the chain
 *  cannot see on its own: a tail cut off rehashes perfectly. */
const truncated = computed(() => {
  const result = verification.value;
  if (!result || result.truncated) return 0;
  return Math.max(0, result.anchor - result.to);
});
</script>

<template>
  <div class="space-y-4">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <div class="flex items-center gap-2 text-xs text-muted mb-1">
          <RouterLink to="/platform" class="hover:text-highlighted">Platform</RouterLink>
          <span>/</span>
          <span class="text-toned">Audit</span>
        </div>
        <h1 class="text-xl font-semibold text-highlighted">Audit</h1>
        <p class="text-xs text-muted mt-1">
          Every state transition the platform made, chained so that a record edited, removed or slipped in afterwards
          says so.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <USelect
          :model-value="rangeMinutes"
          :items="ranges"
          size="xs"
          class="w-40"
          @update:model-value="(value: number) => apply({ range: value })"
        />
        <USelect
          :model-value="limit"
          :items="limits"
          size="xs"
          class="w-24"
          @update:model-value="(value: number) => apply({ limit: value })"
        />
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="records.loading.value"
          aria-label="Refresh"
          @click="records.refresh"
        />
      </div>
    </div>

    <!-- The chain's verdict, first, because the rows below are only worth
         reading if it holds. -->
    <div class="rounded-md border border-default px-4 py-3 space-y-2">
      <div class="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <p class="text-sm text-highlighted font-medium">The chain</p>
          <p class="text-xs text-muted mt-0.5">
            <template v-if="compliance.data.value?.audit.recording">
              Recording. {{ compliance.data.value.audit.sequence.toLocaleString("en-GB") }} records, kept
              {{ compliance.data.value.audit.retentionDays }} days.
            </template>
            <template v-else-if="compliance.data.value">
              <span class="text-warning">Not recording.</span>
              {{ compliance.data.value.audit.message || "The platform is producing no evidence." }}
            </template>
            <template v-else>Reading the platform's compliance status…</template>
          </p>
          <p v-if="compliance.data.value && !compliance.data.value.policy.storing" class="text-xs mt-0.5">
            <span class="text-warning">Decisions are not being stored.</span>
            <span class="text-muted"> {{ compliance.data.value.policy.message }}</span>
          </p>
        </div>
        <UButton size="xs" color="neutral" variant="subtle" :loading="verifying" @click="verify">
          Verify the chain
        </UButton>
      </div>

      <UAlert v-if="verifyError" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="verifyError" />

      <template v-else-if="verification">
        <div v-if="verification.intact" class="text-xs text-success flex items-center gap-1.5">
          <UIcon name="i-lucide-shield-check" class="size-4" />
          <span>
            Records {{ verification.from }}–{{ verification.to }} re-derive to the hashes stored beside them.
            <template v-if="verification.truncated">
              This is the first page of the chain; ask again from {{ verification.to + 1 }} for the rest.
            </template>
          </span>
        </div>
        <div v-else class="space-y-1.5">
          <p class="text-xs text-error flex items-center gap-1.5">
            <UIcon name="i-lucide-shield-alert" class="size-4" />
            {{ verification.findings.length }} break(s) over records {{ verification.from }}–{{ verification.to }}.
          </p>
          <p v-for="finding in verification.findings" :key="`${finding.sequence}-${finding.break}`" class="text-xs">
            <span class="font-mono text-error">#{{ finding.sequence }} {{ finding.break }}</span>
            <span class="text-toned"> — {{ finding.detail }}</span>
          </p>
        </div>
        <p v-if="truncated > 0" class="text-xs text-error">
          The chain verifies up to {{ verification.to }}, but the platform's own anchor says it ends at
          {{ verification.anchor }}: {{ truncated }} record(s) have been cut off the end. A rewritten tail rehashes
          perfectly, so this is the only place that shows.
        </p>
      </template>
      <p v-else class="text-[11px] text-dimmed">
        Verifying walks the chain from the first record and re-derives every hash. It is asked for rather than run on a
        timer, because it is a scan and because a number that changes on its own invites nobody to check it.
      </p>
    </div>

    <div v-if="chips.length" class="flex items-center gap-2 flex-wrap text-[11px]">
      <button
        v-for="chip in chips"
        :key="chip.key"
        class="font-mono px-1.5 py-0.5 rounded border border-default text-toned hover:border-accented hover:text-error"
        title="Remove this filter"
        @click="apply({ [chip.key]: undefined })"
      >
        {{ chip.key }}:{{ chip.value }} ×
      </button>
      <button class="text-dimmed hover:text-highlighted" @click="clearAll">clear all</button>
    </div>

    <UAlert
      v-if="records.error.value"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      :title="records.error.value"
    />

    <div v-else class="rounded-md border border-default overflow-x-auto">
      <table class="w-full text-sm">
        <thead>
          <tr class="text-left text-xs text-muted border-b border-default bg-muted">
            <th class="px-3 py-2 font-medium text-right">#</th>
            <th class="px-3 py-2 font-medium">When</th>
            <th class="px-3 py-2 font-medium">Actor</th>
            <th class="px-3 py-2 font-medium">Object</th>
            <th class="px-3 py-2 font-medium">What</th>
            <th class="px-3 py-2 font-medium">Hash</th>
          </tr>
        </thead>
        <tbody>
          <tr v-if="!rows.length">
            <td colspan="6" class="px-3 py-8 text-center text-muted text-sm">
              {{
                records.loading.value
                  ? "Loading…"
                  : "Nothing was recorded in this window. A quiet platform and a filter that matches nothing look the same here — the chips above say which this is."
              }}
            </td>
          </tr>
          <tr
            v-for="record in rows"
            :key="record.sequence"
            class="border-b border-muted last:border-0 align-top hover:bg-elevated/40"
          >
            <td class="px-3 py-2 text-right font-mono text-xs text-dimmed tabular-nums">{{ record.sequence }}</td>
            <td class="px-3 py-2 text-xs text-dimmed font-mono whitespace-nowrap" :title="record.timestamp">
              {{ time(record.timestamp) }}
              <p class="text-[11px]">{{ timeAgo(record.timestamp) }}</p>
            </td>
            <td class="px-3 py-2 text-xs">
              <button
                class="font-mono hover:underline break-all text-left"
                :class="record.actorKind === 'user' ? 'text-highlighted' : 'text-dimmed'"
                title="Narrow to this actor"
                @click="narrow('actor', record.actor)"
              >
                {{ record.actor }}
              </button>
              <p class="text-[11px] text-dimmed">{{ record.actorKind }}</p>
            </td>
            <td class="px-3 py-2 text-xs">
              <button
                class="font-mono text-highlighted hover:underline break-all text-left"
                title="Narrow to this object"
                @click="apply({ kind: record.kind, name: record.name })"
              >
                {{ record.kind }}/{{ record.name }}
              </button>
              <RouterLink
                v-if="record.project"
                :to="{ name: 'project', params: { name: record.project } }"
                class="block text-[11px] text-primary hover:underline"
              >
                {{ record.project }}
              </RouterLink>
            </td>
            <td class="px-3 py-2 text-xs w-full">
              <span class="font-mono" :class="operationColour[record.operation] ?? 'text-toned'">
                {{ record.operation }}
              </span>
              <span v-if="move(record)" class="font-mono text-dimmed ml-2">{{ move(record) }}</span>
              <p class="text-toned break-words">{{ record.reason }}</p>
              <p v-if="record.details" class="text-[11px] text-dimmed font-mono break-all">{{ record.details }}</p>
            </td>
            <td class="px-3 py-2 text-[11px] font-mono text-dimmed whitespace-nowrap" :title="record.hash">
              {{ short(record.hash) }}
              <p class="text-dimmed/70" :title="record.prevHash">← {{ short(record.prevHash) }}</p>
            </td>
          </tr>
        </tbody>
      </table>
    </div>

    <p class="text-[11px] text-dimmed leading-relaxed">
      The hashes are shown because hiding them would be asking to be believed, and the point of a chain is that it does
      not have to be: every record's hash covers the record before it and every field of its own content, so a row
      edited afterwards no longer hashes to the hash stored beside it. What this cannot catch on its own is a tail
      rewritten whole — the anchor above is what bounds that.
    </p>

    <!-- The decision register lives on the audit screen because it answers to
         the same standard: every verdict is a stored record, and a stored
         record is one that can be checked. -->
    <DecisionsPanel />
  </div>
</template>
