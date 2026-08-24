<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import DecisionsPanel from "../components/DecisionsPanel.vue";
import CriticalityPanel from "../components/CriticalityPanel.vue";
import DriftPanel from "../components/DriftPanel.vue";
import AccessReviewPanel from "../components/AccessReviewPanel.vue";
import AuditPackPanel from "../components/AuditPackPanel.vue";
import ExceptionsPanel from "../components/ExceptionsPanel.vue";
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
const FILTERS = ["kind", "name", "project", "actor", "privilegeClass"] as const;

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
  // A class implies the marking, so the two never both need spelling.
  if (param("privileged") === "true" && !query.privilegeClass) query.privileged = "true";
  return query;
}

/** Privileged-only is the supervisor's question — what moved a control rather
 *  than a workload — and it is one toggle rather than six filters. */
const privilegedOnly = computed(() => param("privileged") === "true" || param("privilegeClass") !== "");

function togglePrivileged() {
  if (privilegedOnly.value) {
    apply({ privileged: undefined, privilegeClass: undefined });
    return;
  }
  apply({ privileged: "true" });
}

const records = useAsync(() => api.audit(selection()));
const compliance = useAsync(() => api.compliance());
// The classification inventory: one request, exportable as it is.
const inventory = useAsync(() => api.complianceInventory());

function exportInventory() {
  const data = inventory.data.value;
  if (!data) return;
  const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = `kitchen-inventory-${data.generatedAt.slice(0, 10)}.json`;
  anchor.click();
  URL.revokeObjectURL(url);
}
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
        <!-- The supervisor's question, one toggle: what moved a control
             rather than a workload. Waivers, requirements, classifications,
             grants, credentials, and writes the platform did not make. -->
        <UButton size="xs" color="neutral" :variant="privilegedOnly ? 'solid' : 'subtle'" @click="togglePrivileged">
          Privileged only
        </UButton>
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
              <!-- A privileged record moved a control rather than a workload.
                   The class is a filter as well as a badge: one click narrows
                   the log to every waiver, or every credential rotation. -->
              <button
                v-if="record.privileged"
                class="ml-2 font-mono text-[11px] px-1 rounded border border-warning/40 text-warning hover:border-warning"
                :title="`Narrow to ${record.privilegeClass || 'privileged'} records`"
                @click="apply({ privileged: 'true', privilegeClass: record.privilegeClass || undefined })"
              >
                {{ record.privilegeClass || "privileged" }}
              </button>
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

    <!-- The evidence export sits between the log and everything derived from
         it, and above rather than below, because on the day somebody opens
         this screen with a deadline it is what they came to do. Every panel
         under it is one section of the pack shown on its own; this is all of
         them in one file, signed, for a window. -->
    <AuditPackPanel />

    <!-- Access recertification sits directly under the log, because it is the
         one control in this suite that is about the people reading the rest
         of it. Everything above records what the platform did; this records
         who was allowed to do it, and who last checked that. -->
    <AccessReviewPanel />

    <!-- The exception register sits right above the decisions it changes:
         every standing waiver, prominent and permanent, because the loudness
         is the design — an emergency deployment is allowed and seen, never
         blocked and worked around. -->
    <ExceptionsPanel />

    <!-- The decision register lives on the audit screen because it answers to
         the same standard: every verdict is a stored record, and a stored
         record is one that can be checked. -->
    <DecisionsPanel />

    <!-- Drift is the decision register read the other way round: not what was
         decided, but what those decisions would say today. It sits under them
         because it is derived from them, and above the inventory because
         "what is running that no longer meets its bar" is the question this
         whole screen is here to answer. -->
    <DriftPanel />

    <!-- The criticality mapping. It sits beside the classification inventory
         because they are the same kind of answer about two different
         institutional inputs — what the data is worth, and what its
         continuing to work is worth — and both are derived from the graph
         rather than maintained. -->
    <CriticalityPanel />

    <!-- The classification inventory: every environment and claim with its
         class, its data's provenance and its location, in one request. The
         absences are words — unclassified, undeclared, unknown — because a
         blank cell in an export invites a generous reading. -->
    <div class="space-y-2">
      <div class="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <h2 class="text-sm font-semibold text-highlighted">Data classification inventory</h2>
          <p class="text-xs text-muted mt-0.5">
            Where every environment's and claim's data stands: class, provenance, location.
            <template v-if="inventory.data.value?.defaultResidency">
              Platform residency (declared): {{ inventory.data.value.defaultResidency }}.
            </template>
          </p>
        </div>
        <UButton
          size="xs"
          color="neutral"
          variant="subtle"
          icon="i-lucide-download"
          :disabled="!inventory.data.value"
          @click="exportInventory"
        >
          Export JSON
        </UButton>
      </div>
      <UAlert
        v-if="inventory.error.value"
        color="error"
        variant="soft"
        icon="i-lucide-triangle-alert"
        :title="inventory.error.value"
      />
      <div v-else class="rounded-md border border-default overflow-x-auto">
        <table class="w-full text-xs">
          <thead>
            <tr class="border-b border-default text-left text-dimmed">
              <th class="px-3 py-2 font-medium">Project</th>
              <th class="px-3 py-2 font-medium">Name</th>
              <th class="px-3 py-2 font-medium">Kind</th>
              <th class="px-3 py-2 font-medium">Class</th>
              <th class="px-3 py-2 font-medium">Provenance</th>
              <th class="px-3 py-2 font-medium">Residency</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="item in inventory.data.value?.items ?? []"
              :key="`${item.kind}/${item.name}`"
              class="border-b border-default/50"
            >
              <td class="px-3 py-1.5 font-mono text-toned">{{ item.project }}</td>
              <td class="px-3 py-1.5 font-mono text-highlighted">{{ item.name }}</td>
              <td class="px-3 py-1.5 text-dimmed">{{ item.kind }} · {{ item.type }}</td>
              <td class="px-3 py-1.5" :class="item.dataClass === 'unclassified' ? 'text-dimmed' : 'text-toned'">
                {{ item.dataClass }}
              </td>
              <td class="px-3 py-1.5">
                <span v-if="!item.provenance" class="text-dimmed">—</span>
                <span v-else-if="item.provenance === 'production'" class="text-warning">production</span>
                <span v-else :class="item.provenance === 'undeclared' ? 'text-dimmed' : 'text-toned'">
                  {{ item.provenance }}
                </span>
              </td>
              <td class="px-3 py-1.5" :class="item.residency === 'unknown' ? 'text-dimmed' : 'text-toned'">
                {{ item.residency }}
              </td>
            </tr>
            <tr v-if="inventory.data.value && inventory.data.value.items.length === 0">
              <td colspan="6" class="px-3 py-3 text-center text-dimmed">Nothing to classify yet.</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
