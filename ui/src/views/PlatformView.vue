<script setup lang="ts">
import { computed } from "vue";
import { api } from "../lib/api";
import { useFreshness } from "../lib/freshness";
import { healthStrip } from "../lib/platform";
import { correlations } from "../lib/signals";
import { useAsync, usePoll } from "../lib/useAsync";
import CorrelationRow from "../components/CorrelationRow.vue";
import FindingList from "../components/FindingList.vue";
import HealthStrip from "../components/HealthStrip.vue";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import UntendedTable from "../components/UntendedTable.vue";

// The platform's front page: a health strip, then the two questions an
// operator actually arrives with.
//
// **Is this one problem?** is the first, and it is the only question no
// project-scoped screen can ever be asked. The Correlated table answers it,
// one row per correlation, each carrying the rung it was raised at — and the
// project rows belonging to a correlation fold into it and say so, because a
// failure that is part of one incident is one row and not four.
//
// **Is anybody acting on the rest?** is the second, and the Untended table is
// where a failure nobody has acknowledged past the escalation window ends up.
// It reads the deliveries rather than the findings, because "nobody has
// touched this in four hours" is a fact only the recorded history holds.
//
// Everything else is still the problems list underneath, which is the alert
// inbox docs/OBSERVABILITY.md §7 designs.
//
// The reads are deliberately separate: each tile of the strip has exactly one
// source, so a source that could not be read darkens its own tile and says why
// rather than making the whole screen an error page.

const status = useAsync(() => api.status());
const ingest = useAsync(() => api.platformIngest());
const storage = useAsync(() => api.platformStorage());
const edge = useAsync(() => api.platformEdge());
const signals = useAsync(() => api.platformSignals());
// The same conditions asked the other question: what has anybody done about
// them. It is a second read rather than a field on the first because the two
// are different answers — a finding is what is wrong, and a delivery is who
// was told and whether they replied.
const alerts = useAsync(() => api.alerts());

// How old this screen is, and the reader's hold on it: every fetch above
// reports into it and the header renders it.
const freshness = useFreshness();

// The strip's sources are cheap reads of informer caches and one store query
// each; the catalogue is thirty-odd rules over a whole snapshot, so it is asked
// for less often.
usePoll(() => {
  void status.refresh();
  void ingest.refresh();
  void storage.refresh();
  void edge.refresh();
}, 30_000, () => true);
usePoll(() => {
  void signals.refresh();
  void alerts.refresh();
}, 60_000, () => true);

// The round split in two: the correlations with the rows they fold up, and
// everything not part of one.
const split = computed(() => correlations(signals.data.value?.items));

// What is left for the problems list: the same answer with the correlations
// and their folded rows taken out, so that one incident is one row on this
// screen rather than one row and its four symptoms.
const remaining = computed(() => {
  const answer = signals.data.value;
  if (!answer) return null;
  return { ...answer, items: split.value.rest };
});

// Failures with nobody acting: past the escalation window, unacknowledged, and
// on the operator's own row. A failure somebody is already fixing is not here
// — that is what the tier model does to it, and it is the whole point of the
// table.
const untended = computed(() =>
  (alerts.data.value?.items ?? []).filter((alert) => alert.untended && !alert.symptom),
);

const tiles = computed(() =>
  healthStrip({
    status: status.data.value,
    ingest: ingest.data.value,
    storage: storage.data.value,
    edge: edge.data.value,
  }),
);

const loading = computed(() => signals.loading.value || status.loading.value);

function refresh() {
  void status.refresh();
  void ingest.refresh();
  void storage.refresh();
  void edge.refresh();
  void signals.refresh();
  void alerts.refresh();
}

const sections = [
  { label: "Nodes", to: "/platform/nodes", icon: "i-lucide-server", hint: "conditions, saturation, and who stopped reporting" },
  { label: "Workloads", to: "/platform/workloads", icon: "i-lucide-boxes", hint: "every pod — and the workloads with none" },
  { label: "Edge", to: "/platform/edge", icon: "i-lucide-globe", hint: "traffic, the Gateway, the tunnel, certificates" },
  { label: "Addons", to: "/platform/addons", icon: "i-lucide-puzzle", hint: "what this platform installs into its own cluster" },
  { label: "Storage", to: "/platform/storage", icon: "i-lucide-hard-drive", hint: "volumes, and the store's own health" },
  { label: "Events", to: "/platform/events", icon: "i-lucide-list", hint: "the cluster's warning history" },
  { label: "Policy", to: "/platform/policy", icon: "i-lucide-sliders-horizontal", hint: "what this installation counts as worth hearing" },
  { label: "Connections", to: "/platform/connections", icon: "i-lucide-plug", hint: "the forges this platform builds from" },
  // The one tile here that leaves the scope. The audit log is the auditor's
  // rather than the operator's (#469), and an operator following this arrives
  // in the Compliance scope — which is where the register they are looking
  // for is, and where the switcher will say they are.
  { label: "Audit", to: "/compliance/audit", icon: "i-lucide-shield-check", hint: "what the platform did, and whether the record holds" },
];
</script>

<template>
  <div class="space-y-6">
    <PageHeader :freshness="freshness" title="Platform">
      <template #description>
        The cluster as the operator sees it, across every project — and everything currently wrong with it, worst first.
      </template>
      <template #actions>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="loading"
          aria-label="Re-evaluate"
          @click="refresh"
        />
      </template>
    </PageHeader>

    <HealthStrip :tiles="tiles" />

    <PageSection
      v-if="split.correlated.length"
      title="Correlated"
      description="Several projects failing at once, raised at the highest confidence this evaluation could reach. A rung is never withheld for being unexplained — that several things broke together at 04:05 with nothing to explain it is worth knowing on its own."
    >
      <div class="rounded-md border border-default divide-y divide-muted">
        <CorrelationRow
          v-for="entry in split.correlated"
          :key="entry.finding.fingerprint"
          :correlation="entry"
        />
      </div>
    </PageSection>

    <PageSection
      v-if="untended.length || alerts.data.value?.message"
      title="Untended"
      description="Open past the escalation window with nobody acknowledging them. A failure somebody is already acting on is not here."
    >
      <UntendedTable :items="untended" :message="alerts.data.value?.message" />
    </PageSection>

    <FindingList
      :answer="remaining"
      :loading="signals.loading.value"
      :error="signals.error.value"
      title="Problems"
      empty="Nothing is firing. Every rule in the catalogue was evaluated against a snapshot of this platform and none of them matched."
    />

    <p class="text-[11px] text-dimmed leading-relaxed">
      The catalogue runs on a timer in the operator, which records when each condition opened and resolved; where that
      is not running — switched off, no telemetry store, or a round that has not landed recently enough — this screen
      evaluates one itself when it asks. Either way “evaluated <em>n</em> ago” is exactly how fresh this is. Each
      finding carries a fingerprint that is stable for the same underlying condition, which is what makes one round
      diffable against the last — this screen is that inbox, minus acknowledgement.
    </p>

    <div>
      <h2 class="text-sm font-medium text-highlighted mb-2">The screens behind it</h2>
      <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <RouterLink
          v-for="section in sections"
          :key="section.to"
          :to="section.to"
          class="rounded-md border border-default px-4 py-3 hover:border-accented"
        >
          <p class="text-sm text-highlighted font-medium flex items-center gap-2">
            <UIcon :name="section.icon" class="size-4 text-dimmed" />
            {{ section.label }}
          </p>
          <p class="text-xs text-muted mt-0.5">{{ section.hint }}</p>
        </RouterLink>
      </div>
    </div>
  </div>
</template>
