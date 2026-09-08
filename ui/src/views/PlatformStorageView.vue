<script setup lang="ts">
import { computed } from "vue";
import { useRoute } from "vue-router";
import { api, type PlatformVolume } from "../lib/api";
import { compactCount, formatBytes, formatDurationSeconds, timeAgo } from "../lib/format";
import { useFreshness } from "../lib/freshness";
import { FLOWS_LOST_FIRING, flowsUnderReporting, formatFraction } from "../lib/platform";
import { useAsync, usePoll } from "../lib/useAsync";
import FillBar from "../components/FillBar.vue";
import GrowVolumeModal from "../components/GrowVolumeModal.vue";
import PageHeader from "../components/PageHeader.vue";
import StatusDot from "../components/StatusDot.vue";
import WrittenVolumesPanel from "../components/WrittenVolumesPanel.vue";

// Every volume the platform holds, what mounts it, and the health of the one
// database Kitchen runs itself.
//
// They are called volumes and not claims throughout, because `/claims` already
// means something else in this API — a ResourceClaim, the platform's own kind
// for a provisioned database — and two things called claims in one dashboard is
// one too many.
//
// Since #469 it also holds what used to be the Volumes screen — the storage
// somebody pointed the platform at so that a project could mount something
// older than the cluster. Two inventories of one subject, one of which was
// sitting in the developer's navigation; and this address was already the
// operator's and already emitted as evidence, so the merge is into an
// occupied address rather than a move into a free one.
//
// The store's fill comes from the same reading the `store.disk` rule fires on —
// the kubelet's stats for the store's own claim — so the bar on this screen and
// the finding on the problems list cannot disagree about how full it is. It is
// the whole disk rather than the store's own tables, and the two are shown
// separately: the volume filled with something else is exactly the case where
// the database's size said everything was fine (#531).
//
// And `flows` is here as well as on the ingest reading because losing rows
// before they are written and running out of disk to write them to are the same
// problem seen from two ends.

const route = useRoute();
/** Where `pvc.pending` and `pvc.filling` evidence lands. */
const namespace = computed(() => (route.query.namespace as string) || "");
const claim = computed(() => (route.query.claim as string) || "");

const { data, error, loading, refresh } = useAsync(() => api.platformStorage());
// How old this screen is, and the reader's hold on it: every fetch above
// reports into it and the header renders it.
const freshness = useFreshness();
usePoll(() => void refresh(), 60_000, () => true);

const volumes = computed(() => data.value?.items ?? []);
const usageMessage = computed(() => data.value?.usageMessage ?? "");
const store = computed(() => data.value?.store);
const flows = computed(() => data.value?.flows);

/** What the store's bar says in place of a fill it has not got. An external
 * store is a disk the platform does not own and has no business judging; a
 * volume of the platform's own that nothing measured is a gap, and the API says
 * which by sending a message with it. A store that could not answer for itself
 * has its own line above this one, so the bar says only what it adds. */
const storeUnmeasured = computed(() => {
  const health = store.value;
  if (!health) return "—";
  if (health.usageMessage) return health.usageMessage;
  if (!health.claim && !health.message) {
    return `${formatBytes(health.bytesOnDisk)} on a volume the platform does not own`;
  }
  if (health.message) return "and how full its disk is was not read either";
  return "how full this volume is was not read";
});

/** The ledger's verdict, decided where `ingest.flows-lost` decides it. A
 * handful of dropped events is a momentary buffer overrun that no total will
 * ever show; the counts below say what was lost either way, which is the point
 * of the panel — but only the rule's own threshold paints it amber. */
const underReporting = computed(() => flowsUnderReporting(flows.value));
const ledger = computed(() => {
  const loss = flows.value;
  if (!loss) return "";
  if (underReporting.value) return "Hubble reported dropping events — request counts under-report by an unknown amount.";
  if (loss.lossless) return "Nothing was reported lost in the follower's trailing window.";
  return `Something was lost, below the ${FLOWS_LOST_FIRING} events in a window the platform calls under-reporting: what survived is correct, and there are simply that many fewer rows than there were requests.`;
});

/** Whether the size this volume was asked to be is larger than the size it is,
 * which is the platform's half of the work. */
function growing(volume: PlatformVolume): boolean {
  const desired = volume.resize?.desired;
  if (!desired || volume.resize?.phase !== "Growing") return false;
  return desired !== (volume.capacity || volume.requested);
}

/** Whether the storage driver is still catching up: the claim asks for more
 * than it has been given. The row draws both numbers for exactly this case —
 * one of them is what an operator was promised and the other is what is
 * actually there, and a row showing one of them cannot say which. */
function resizing(volume: PlatformVolume): boolean {
  if (volume.resize?.phase !== "Resizing") return false;
  return Boolean(volume.requested && volume.capacity && volume.requested !== volume.capacity);
}

/** What the platform is doing about this volume's size, in one line, or nothing
 * where there is nothing to say. A volume at the size it was asked to be says
 * nothing at all: a row that reports agreement on every line is a row nobody
 * reads the exceptions out of. */
function resizeNote(volume: PlatformVolume): string {
  const resize = volume.resize;
  if (!resize) return "";
  if (resize.message) return resize.message;
  return resize.desired && (resize.phase === "Growing" || resize.phase === "Resizing")
    ? `growing to ${resize.desired}`
    : "";
}

function highlighted(volume: { namespace: string; name: string }): boolean {
  if (!claim.value) return false;
  return volume.name === claim.value && (!namespace.value || volume.namespace === namespace.value);
}
</script>

<template>
  <div class="space-y-6">
    <PageHeader :freshness="freshness" title="Storage" :breadcrumb="[{ label: 'Platform', to: '/platform' }, { label: 'Storage' }]">
      <template #description>
        Every volume on the platform and what mounts it, the storage somebody wrote for projects to mount, and the
        telemetry store's own disk.
      </template>
      <template #actions>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          :loading="loading"
          aria-label="Refresh"
          @click="refresh"
        />
      </template>
    </PageHeader>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <template v-else>
      <div class="grid grid-cols-2 sm:grid-cols-3 gap-3">
        <div class="rounded-md border border-default px-4 py-3">
          <p class="text-xs text-muted">Volumes</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums mt-1">{{ data?.volumes ?? "—" }}</p>
        </div>
        <div class="rounded-md border px-4 py-3" :class="data?.unbound ? 'border-error/40 bg-error/5' : 'border-default'">
          <p class="text-xs text-muted">Unbound</p>
          <p class="text-lg font-semibold tabular-nums mt-1" :class="data?.unbound ? 'text-error' : 'text-highlighted'">
            {{ data?.unbound ?? "—" }}
          </p>
          <p class="text-[11px] text-dimmed mt-0.5">nothing that needs one can start</p>
        </div>
        <div
          class="rounded-md border px-4 py-3"
          :class="usageMessage ? 'border-default' : data?.filling ? 'border-warning/40 bg-warning/5' : 'border-default'"
        >
          <p class="text-xs text-muted">Filling</p>
          <p
            class="text-lg font-semibold tabular-nums mt-1"
            :class="usageMessage ? 'text-dimmed' : data?.filling ? 'text-warning' : 'text-highlighted'"
          >
            {{ usageMessage ? "unknown" : (data?.filling ?? "—") }}
          </p>
          <p class="text-[11px] text-dimmed mt-0.5">
            {{ usageMessage ? "how full each volume is was not read" : "past 85% used" }}
          </p>
        </div>
      </div>

      <!-- Nothing read the fill: a measured zero and an unmeasured one are
           different answers, and the tile above says which this is. -->
      <UAlert
        v-if="usageMessage"
        color="neutral"
        variant="soft"
        icon="i-lucide-eye-off"
        title="Volume fill is unknown"
        :description="usageMessage"
      />

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">Volumes projects claimed</h2>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[48rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-3 py-2 font-medium">Volume</th>
                <th class="px-3 py-2 font-medium">For</th>
                <th class="px-3 py-2 font-medium">Phase</th>
                <th class="px-3 py-2 font-medium">Class</th>
                <th class="px-3 py-2 font-medium text-right">Size</th>
                <th class="px-3 py-2 font-medium">Used</th>
                <th class="px-3 py-2 font-medium">Mounted by</th>
                <th class="px-3 py-2 font-medium"><span class="sr-only">Grow</span></th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!volumes.length">
                <td colspan="8" class="px-3 py-8 text-center text-muted">
                  {{ loading ? "Loading…" : "This platform holds no volumes." }}
                </td>
              </tr>
              <template v-for="volume in volumes" :key="`${volume.namespace}/${volume.name}`">
                <tr
                  class="border-b border-muted last:border-0"
                  :class="[
                    volume.bound ? 'hover:bg-elevated/40' : 'bg-error/5',
                    highlighted(volume) ? 'ring-1 ring-inset ring-primary/40' : '',
                  ]"
                >
                  <td class="px-3 py-2">
                    <span class="inline-flex items-center gap-2">
                      <StatusDot :tone="volume.bound ? 'success' : 'error'" />
                      <span class="font-mono text-xs text-highlighted break-all">{{ volume.name }}</span>
                    </span>
                    <p class="text-[11px] text-dimmed pl-3.5">{{ volume.namespace }}</p>
                  </td>
                  <td class="px-3 py-2 text-xs">
                    <RouterLink
                      v-if="volume.project"
                      :to="{ name: 'project', params: { name: volume.project } }"
                      class="text-primary hover:underline"
                      >{{ volume.project }}</RouterLink
                    >
                    <span v-else class="text-dimmed">the platform</span>
                  </td>
                  <td class="px-3 py-2 text-xs" :class="volume.bound ? 'text-toned' : 'text-error'">
                    {{ volume.phase }}
                  </td>
                  <td class="px-3 py-2 font-mono text-xs text-dimmed">{{ volume.storageClass || "—" }}</td>
                  <td class="px-3 py-2 text-right font-mono text-xs tabular-nums text-toned">
                    {{ volume.capacity || volume.requested || "—" }}
                    <!-- The size it was asked to be, drawn only while it differs from the size it is. -->
                    <!-- A volume being grown is the platform working, not a
                         caution: docs/UI.md gives a working state neither
                         colour, and the API classifies the finding as
                         information for the same reason. -->
                    <p v-if="resizing(volume)" class="text-[11px] text-muted">
                      {{ volume.requested }} asked of the driver
                    </p>
                    <p v-else-if="growing(volume)" class="text-[11px] text-muted">&rarr; {{ volume.resize?.desired }}</p>
                    <p v-else-if="volume.expandable === false" class="text-[11px] text-dimmed">fixed size</p>
                  </td>
                  <td class="px-3 py-2">
                    <FillBar
                      :fraction="volume.usage?.usedFraction ?? null"
                      :caption="volume.usage ? formatBytes(volume.usage.usedBytes) : undefined"
                      :unmeasured="usageMessage || '—'"
                    />
                  </td>
                  <td class="px-3 py-2 font-mono text-[11px] text-dimmed break-all">
                    {{ (volume.pods ?? []).join(", ") || "nothing" }}
                  </td>
                  <td class="px-3 py-2 text-right">
                    <!-- Only the platform's own volumes: a project's claim is not one this platform grows,
                         and a button whose Save is always a refusal is a screen that lies. -->
                    <GrowVolumeModal v-if="volume.resize" :volume="volume" @grown="refresh" />
                  </td>
                </tr>
                <tr v-if="volume.message" :key="`${volume.namespace}/${volume.name}-message`" class="border-b border-muted last:border-0">
                  <td colspan="8" class="px-3 pb-2.5 text-xs" :class="volume.bound ? 'text-muted' : 'text-error'">
                    {{ volume.message }}
                  </td>
                </tr>
                <tr
                  v-if="resizeNote(volume)"
                  :key="`${volume.namespace}/${volume.name}-resize`"
                  class="border-b border-muted last:border-0"
                >
                  <td
                    colspan="8"
                    class="px-3 pb-2.5 text-xs"
                    :class="volume.resize?.phase === 'Blocked' ? 'text-warning' : 'text-muted'"
                  >
                    {{ resizeNote(volume) }}
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </div>

      <!-- What was the Volumes screen: storage that existed before the
           cluster did, written so a project can bind it. -->
      <WrittenVolumesPanel />

      <div class="grid gap-4 lg:grid-cols-2">
        <div>
          <h2 class="text-sm font-medium text-highlighted mb-2">The telemetry store</h2>
          <div class="rounded-md border border-default px-4 py-3 space-y-2">
            <!-- The store's own reading and its disk's fail apart: the fill
                 comes from the kubelet, so a store that cannot answer for
                 itself has not stopped anyone measuring its volume, and a full
                 disk is the thing worth saying while it is unreachable. -->
            <p v-if="store?.message" class="text-xs text-warning">{{ store.message }}</p>
            <template v-if="store">
              <!-- Two numbers, and they answer two questions. The bar is how
                   full the disk is, which is everything written to it; the
                   telemetry's own size below is how much of that is ours, and
                   the one retention governs. -->
              <FillBar
                :fraction="store.usage?.usedFraction ?? null"
                :caption="
                  store.usage
                    ? `${formatBytes(store.usage.usedBytes)} of ${formatBytes(store.usage.capacityBytes)} used`
                    : undefined
                "
                :unmeasured="storeUnmeasured"
                width="w-40"
              />
              <div class="grid grid-cols-2 gap-3 text-xs pt-1">
                <!-- Both of these are the store's own account of itself, and
                     are left out rather than shown as zero when it could not
                     give one. -->
                <div v-if="!store.message">
                  <p class="text-[11px] text-muted">Telemetry on it</p>
                  <p class="font-mono text-toned">{{ formatBytes(store.bytesOnDisk) }}</p>
                </div>
                <div v-if="!store.message">
                  <p class="text-[11px] text-muted">Ingest</p>
                  <p class="font-mono" :class="store.rowsPerSecond > 0 ? 'text-toned' : 'text-warning'">
                    {{ store.rowsPerSecond.toFixed(store.rowsPerSecond < 10 ? 1 : 0) }} rows/s
                  </p>
                </div>
                <div>
                  <p class="text-[11px] text-muted">Longest retention</p>
                  <p class="font-mono text-toned">
                    {{ store.retentionDays ? `${store.retentionDays} days` : "—" }}
                  </p>
                </div>
                <div>
                  <p class="text-[11px] text-muted">Volume</p>
                  <p class="font-mono text-toned break-all">{{ store.claim || "external" }}</p>
                </div>
              </div>
              <p v-if="!store.message" class="text-[11px] text-dimmed">
                The bar is the whole disk, as the kubelet measures it; the telemetry is what the store's own tables
                occupy, and it is the part retention governs — the horizon past which the store deliberately holds
                nothing. The rest of the disk belongs to whatever else writes there.
              </p>
            </template>
            <p v-else class="text-xs text-muted">{{ loading ? "Loading…" : "No telemetry store on this installation." }}</p>
          </div>
        </div>

        <div>
          <h2 class="text-sm font-medium text-highlighted mb-2">What the flow stream lost</h2>
          <div class="rounded-md border px-4 py-3" :class="underReporting ? 'border-warning/40 bg-warning/5' : 'border-default'">
            <template v-if="flows">
              <p class="text-xs flex items-center gap-2" :class="underReporting ? 'text-warning' : 'text-muted'">
                <StatusDot :tone="underReporting ? 'warning' : 'success'" />
                <span>{{ ledger }}</span>
              </p>
              <div class="grid grid-cols-2 sm:grid-cols-3 gap-3 text-xs mt-2">
                <div>
                  <p class="text-[11px] text-muted">Events lost</p>
                  <!-- Amber at the rule's own number, not at the first event:
                       the count is worth reading at any size, and worth acting
                       on only where the problems list agrees. -->
                  <p class="font-mono" :class="flows.events >= FLOWS_LOST_FIRING ? 'text-warning' : 'text-toned'">
                    {{ compactCount(flows.events) }}
                  </p>
                </div>
                <div>
                  <p class="text-[11px] text-muted">Notices</p>
                  <p class="font-mono text-toned">{{ compactCount(flows.notices) }}</p>
                </div>
                <div>
                  <p class="text-[11px] text-muted">Reconnects</p>
                  <p class="font-mono" :class="flows.reconnects ? 'text-warning' : 'text-toned'">
                    {{ compactCount(flows.reconnects) }}
                  </p>
                </div>
              </div>
              <p class="text-[11px] text-dimmed mt-2">
                Counted over the last {{ formatDurationSeconds(flows.windowSeconds) }}<template v-if="flows.latest"
                  >, most recently {{ timeAgo(flows.latest) }}</template
                >. The follower runs on the leader alone, so a replica that never followed reports no loss because it
                did no following.
              </p>
            </template>
            <p v-else class="text-xs text-muted">
              No flow follower is running on the replica that answered, so there is no loss ledger to show.
            </p>
          </div>
        </div>
      </div>

      <p class="text-[11px] text-dimmed leading-relaxed">
        An unbound volume names its own suspect: a claim Pending with no storage class is waiting for the cluster's
        default, and a cluster without one is the first-install hang the prerequisites warn about. Fill is measured at
        {{ formatFraction(0.85) }} — the same threshold the <span class="font-mono">pvc.filling</span> and
        <span class="font-mono">store.disk</span> rules fire on, so a bar that has just turned amber and a finding on
        the problems list are the same number. Growing one of the platform's own volumes expands it and rewrites the
        declaration behind it, which is why the platform does it rather than the chart; a volume whose storage allows no
        expansion is fixed at the size it was made, and says so.
      </p>
    </template>
  </div>
</template>
