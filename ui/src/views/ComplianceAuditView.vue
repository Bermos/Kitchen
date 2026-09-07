<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import AccessReviewPanel from "../components/AccessReviewPanel.vue";
import AuditPackPanel from "../components/AuditPackPanel.vue";
import CriticalityPanel from "../components/CriticalityPanel.vue";
import DecisionsPanel from "../components/DecisionsPanel.vue";
import DriftPanel from "../components/DriftPanel.vue";
import ExceptionsPanel from "../components/ExceptionsPanel.vue";
import PageHeader from "../components/PageHeader.vue";
import { api, type AuditRecord } from "../lib/api";
import { anchorNote } from "../lib/audit";
import { timeAgo } from "../lib/format";
import { callerFor } from "../lib/me";
import { may } from "../lib/policy";
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
//
// It is the Compliance scope's screen rather than the Platform section's,
// because an auditor may be a third party rather than an operator and should
// not have to land in the operator's estate to read the evidence (#469). That
// is also why the admission here is the audit log's own — anybody who can see
// a project can read the records of it — and why the three blocks the API
// answers for an operator alone ask for themselves rather than the whole
// screen asking on their behalf. Gating the scope on the posture's
// requirement would refuse a member the routes the API is willing to answer.

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
  void router.replace({ path: "/compliance/audit", query });
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
/** The posture, the evidence pack and the access recertifications are the
 *  operator's three: everything else on this screen is answered for anybody
 *  who can see a project. A control nobody may use is not rendered. */
const mayReadPosture = computed(() => may("GET /api/v1/compliance", callerFor()));
const mayVerify = computed(() => may("GET /api/v1/audit/verify", callerFor()));
const mayExportPack = computed(() => may("GET /api/v1/projects/{name}/audit-pack", callerFor()));
const mayReview = computed(() => may("GET /api/v1/access/reviews", callerFor()));
const compliance = useAsync(() => api.compliance(), { immediate: mayReadPosture.value });
/** The conditions nobody has tended to, worst first. The API answers with
 * `untended: []` and a message where it cannot say — an installation with
 * nothing recording cannot answer how long anybody has been not looking, and
 * an empty list rendered as "all clear" would be that mistake in one word. */
const untended = computed(() => compliance.data.value?.incidents?.untended ?? []);
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
    if (path.startsWith("/compliance/audit")) void records.refresh();
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
  void router.replace({ path: "/compliance/audit" });
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

/** What the anchor says, in one line under the verdict. The gap itself is no
 *  longer computed here: the server compares the run against the anchor and
 *  reports a `truncated` or `unanchored` finding like any other break, so
 *  every reader of the endpoint gets the answer this screen used to work out
 *  for itself (#428). The sentence is in `lib/audit` so a test can hold it. */
const note = computed(() => anchorNote(verification.value));
</script>

<template>
  <div class="space-y-6">
    <PageHeader title="Audit" :breadcrumb="[{ label: 'Compliance', to: '/compliance' }, { label: 'Audit' }]">
      <template #description>
        Every state transition the platform made, chained so that a record edited, removed or slipped in afterwards says
        so.
      </template>
      <template #actions>
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
      </template>
    </PageHeader>

    <!-- The chain's verdict, first, because the rows below are only worth
         reading if it holds. Both halves of it — what the platform says about
         its own recording, and re-deriving the hashes — are answered for an
         operator alone, so for anybody else this block is not here rather
         than here and empty. -->
    <div v-if="mayReadPosture || mayVerify" class="rounded-md border border-default px-4 py-3 space-y-2">
      <div class="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <p class="text-sm text-highlighted font-medium">The chain</p>
          <p class="text-xs text-muted mt-0.5">
            <template v-if="compliance.data.value?.audit.recording">
              Recording.
              <template v-if="compliance.data.value.audit.anchored">
                {{ compliance.data.value.audit.sequence.toLocaleString("en-GB") }} records, kept
                {{ compliance.data.value.audit.retentionDays }} days.
              </template>
              <template v-else>
                Kept {{ compliance.data.value.audit.retentionDays }} days.
                <span class="text-error">The chain has no anchor, so the platform cannot say where it ends.</span>
              </template>
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
        <UButton v-if="mayVerify" size="xs" color="neutral" variant="subtle" :loading="verifying" @click="verify">
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
        <p v-if="note" class="text-xs" :class="note.bad ? 'text-error' : 'text-dimmed'">
          {{ note.text }}
        </p>
      </template>
      <p v-else class="text-[11px] text-dimmed">
        Verifying walks the chain from the first record and re-derives every hash. It is asked for rather than run on a
        timer, because it is a scan and because a number that changes on its own invites nobody to check it.
      </p>
    </div>

    <!-- The escalation ladder's last rung, reported here rather than only on
         the alerts screen. An outage nobody has looked at for hours is not a
         louder alert — nothing is more broken at hour four than at hour one —
         it is a fact about the institution, and this is where facts about the
         institution are reported, with names on them. -->
    <div
      v-if="mayReadPosture && untended.length"
      class="rounded-md border border-error/40 px-4 py-3 space-y-2"
    >
      <p class="text-sm text-highlighted font-medium">Untended incidents</p>
      <p class="text-xs text-muted">
        {{ untended.length }} condition{{ untended.length === 1 ? "" : "s" }} open long past the point where somebody
        should have looked. Each was delivered, escalated and still acknowledged by nobody.
      </p>
      <ul class="space-y-1">
        <li v-for="incident in untended" :key="`${incident.fingerprint}#${incident.audience}`" class="text-xs">
          <RouterLink
            :to="incident.project ? { name: 'project-alerts', params: { name: incident.project } } : '/alerts'"
            class="text-error hover:underline"
          >
            {{ incident.title }}
          </RouterLink>
          <span class="text-muted"> — {{ incident.note || incident.signal }}</span>
        </li>
      </ul>
    </div>
    <p
      v-else-if="mayReadPosture && compliance.data.value?.incidents?.message"
      class="text-xs text-warning"
    >
      {{ compliance.data.value.incidents.message }}
    </p>

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
    <AuditPackPanel v-if="mayExportPack" />

    <!-- Access recertification sits directly under the log, because it is the
         one control in this suite that is about the people reading the rest
         of it. Everything above records what the platform did; this records
         who was allowed to do it, and who last checked that. -->
    <AccessReviewPanel v-if="mayReview" />

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
         class, its data's provenance and its location, in one request, and
         beside them every image running here that this platform did not
         build. The absences are words — unclassified, undeclared, unknown —
         because a blank cell in an export invites a generous reading. -->
    <div class="space-y-2">
      <div class="flex items-start justify-between gap-4 flex-wrap">
        <div>
          <h2 class="text-sm font-medium text-highlighted">Data classification inventory</h2>
          <p class="text-xs text-muted mt-0.5">
            Where every environment's and claim's data stands: class, provenance, location — and
            every image running here that somebody else built, with who admitted it.
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
              <th class="px-3 py-1 font-medium">Project</th>
              <th class="px-3 py-1 font-medium">Name</th>
              <th class="px-3 py-1 font-medium">Kind</th>
              <th class="px-3 py-1 font-medium">Class</th>
              <th class="px-3 py-1 font-medium">Provenance</th>
              <th class="px-3 py-1 font-medium">Residency</th>
              <th class="px-3 py-1 font-medium">Upstream</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="item in inventory.data.value?.items ?? []"
              :key="`${item.kind}/${item.name}`"
              class="border-b border-default/50"
            >
              <td class="px-3 py-1 font-mono text-toned">{{ item.project }}</td>
              <td class="px-3 py-1 font-mono text-highlighted">{{ item.name }}</td>
              <td class="px-3 py-1 text-dimmed">{{ item.kind }} · {{ item.type }}</td>
              <td class="px-3 py-1" :class="item.dataClass === 'unclassified' ? 'text-dimmed' : 'text-toned'">
                {{ item.dataClass }}
              </td>
              <td class="px-3 py-1">
                <span v-if="!item.provenance" class="text-dimmed">—</span>
                <span v-else-if="item.provenance === 'production'" class="text-warning">production</span>
                <span v-else :class="item.provenance === 'undeclared' ? 'text-dimmed' : 'text-toned'">
                  {{ item.provenance }}
                </span>
              </td>
              <td class="px-3 py-1" :class="item.residency === 'unknown' ? 'text-dimmed' : 'text-toned'">
                {{ item.residency }}
              </td>
              <!-- The outsourcing facts, on the rows that have them. An
                   unsigned upstream is said in words, because "none" means
                   the vendor publishes no signature and that is the ordinary
                   state of most published images rather than a failed
                   check. -->
              <td class="px-3 py-1">
                <span v-if="!item.upstream" class="text-dimmed">—</span>
                <div v-else class="space-y-0.5">
                  <div class="font-mono text-toned">{{ item.upstream }}</div>
                  <div class="text-dimmed">
                    admitted by {{ item.admittedBy }} ·
                    <span v-if="item.signature === 'verified'" class="text-success">signature verified</span>
                    <span v-else-if="item.signature === 'none'">vendor publishes no signature</span>
                    <span v-else class="text-warning">signature {{ item.signature }}</span>
                  </div>
                </div>
              </td>
            </tr>
            <tr v-if="inventory.data.value && inventory.data.value.items.length === 0">
              <td colspan="7" class="px-3 py-2 text-center text-dimmed">Nothing to classify yet.</td>
            </tr>
          </tbody>
        </table>
      </div>
    </div>
  </div>
</template>
