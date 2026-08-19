<script setup lang="ts">
import { computed } from "vue";
import { api } from "../lib/api";
import { healthStrip } from "../lib/platform";
import { useAsync, usePoll } from "../lib/useAsync";
import FindingList from "../components/FindingList.vue";
import HealthStrip from "../components/HealthStrip.vue";

// The platform's front page: a health strip and the problems list.
//
// It is shaped as a list of findings rather than as a dashboard on purpose.
// This screen *is* the alert inbox docs/OBSERVABILITY.md §7 designs, minus
// persistence: today the catalogue is evaluated when the screen asks, and one
// day a background loop writes transitions and this reads them instead — same
// screen, same rows, same shape. Building it as a bespoke set of panels would
// mean rebuilding it then.
//
// The five reads are deliberately five: each tile of the strip has exactly one
// source, so a source that could not be read darkens its own tile and says why
// rather than making the whole screen an error page.

const status = useAsync(() => api.status());
const ingest = useAsync(() => api.platformIngest());
const storage = useAsync(() => api.platformStorage());
const edge = useAsync(() => api.platformEdge());
const signals = useAsync(() => api.platformSignals());

// The strip's sources are cheap reads of informer caches and one store query
// each; the catalogue is thirty-six rules over a whole snapshot, so it is asked
// for less often.
usePoll(() => {
  void status.refresh();
  void ingest.refresh();
  void storage.refresh();
  void edge.refresh();
}, 30_000, () => true);
usePoll(() => void signals.refresh(), 60_000, () => true);

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
}

const sections = [
  { label: "Nodes", to: "/platform/nodes", icon: "i-lucide-server", hint: "conditions, saturation, and who stopped reporting" },
  { label: "Workloads", to: "/platform/workloads", icon: "i-lucide-boxes", hint: "every pod — and the workloads with none" },
  { label: "Edge", to: "/platform/edge", icon: "i-lucide-globe", hint: "traffic, the Gateway, the tunnel, certificates" },
  { label: "Storage", to: "/platform/storage", icon: "i-lucide-hard-drive", hint: "volumes, and the store's own health" },
  { label: "Events", to: "/platform/events", icon: "i-lucide-list", hint: "the cluster's warning history" },
  { label: "Audit", to: "/platform/audit", icon: "i-lucide-shield-check", hint: "what the platform did, and whether the record holds" },
];
</script>

<template>
  <div class="space-y-6">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <h1 class="text-xl font-semibold text-highlighted">Platform</h1>
        <p class="text-xs text-muted mt-1">
          The cluster as the operator sees it, across every project — and everything currently wrong with it, worst
          first.
        </p>
      </div>
      <UButton
        icon="i-lucide-refresh-cw"
        color="neutral"
        variant="ghost"
        size="sm"
        :loading="loading"
        aria-label="Re-evaluate"
        @click="refresh"
      />
    </div>

    <HealthStrip :tiles="tiles" />

    <FindingList
      :answer="signals.data.value"
      :loading="signals.loading.value"
      :error="signals.error.value"
      title="Problems"
      empty="Nothing is firing. Every rule in the catalogue was evaluated against a snapshot of this platform and none of them matched."
    />

    <p class="text-[11px] text-dimmed leading-relaxed">
      The catalogue is evaluated when this screen asks rather than on a timer, so nothing here is stored and
      “evaluated <em>n</em> ago” is exactly how fresh it is. Each finding carries a fingerprint that is stable for the
      same underlying condition, which is what will let a later release record open and resolve transitions instead of
      re-announcing the same problem — this screen is that inbox, minus the persistence.
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
