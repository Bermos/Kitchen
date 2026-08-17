<script setup lang="ts">
import { computed } from "vue";
import { useRoute } from "vue-router";
import { api } from "../lib/api";
import { compactCount, formatBytes, formatDurationSeconds, timeAgo } from "../lib/format";
import { formatFraction } from "../lib/platform";
import { useAsync, usePoll } from "../lib/useAsync";
import FillBar from "../components/FillBar.vue";
import StatusDot from "../components/StatusDot.vue";

// Every volume the platform holds, what mounts it, and the health of the one
// database Kitchen runs itself.
//
// They are called volumes and not claims throughout, because `/claims` already
// means something else in this API — a ResourceClaim, the platform's own kind
// for a provisioned database — and two things called claims in one dashboard is
// one too many.
//
// The store's numbers come from the same query the `store.disk` rule fires on,
// so the bar on this screen and the finding on the problems list cannot
// disagree about how full it is. And `flows` is here as well as on the ingest
// reading because losing rows before they are written and running out of disk
// to write them to are the same problem seen from two ends.

const route = useRoute();
/** Where `pvc.pending` and `pvc.filling` evidence lands. */
const namespace = computed(() => (route.query.namespace as string) || "");
const claim = computed(() => (route.query.claim as string) || "");

const { data, error, loading, refresh } = useAsync(() => api.platformStorage());
usePoll(() => void refresh(), 60_000, () => true);

const volumes = computed(() => data.value?.items ?? []);
const usageMessage = computed(() => data.value?.usageMessage ?? "");
const store = computed(() => data.value?.store);
const flows = computed(() => data.value?.flows);

function highlighted(volume: { namespace: string; name: string }): boolean {
  if (!claim.value) return false;
  return volume.name === claim.value && (!namespace.value || volume.namespace === namespace.value);
}
</script>

<template>
  <div class="space-y-5">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <div class="flex items-center gap-2 text-xs text-muted mb-1">
          <RouterLink to="/platform" class="hover:text-highlighted">Platform</RouterLink>
          <span>/</span>
          <span class="text-toned">Storage</span>
        </div>
        <h1 class="text-xl font-semibold text-highlighted">Storage</h1>
        <p class="text-xs text-muted mt-1">
          Every volume on the platform and what mounts it, plus the telemetry store's own disk.
        </p>
      </div>
      <UButton
        icon="i-lucide-refresh-cw"
        color="neutral"
        variant="ghost"
        size="sm"
        :loading="loading"
        aria-label="Refresh"
        @click="refresh"
      />
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <template v-else>
      <div class="grid grid-cols-3 gap-3">
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
        <h2 class="text-sm font-medium text-highlighted mb-2">Volumes</h2>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-4 py-2.5 font-medium">Volume</th>
                <th class="px-4 py-2.5 font-medium">For</th>
                <th class="px-4 py-2.5 font-medium">Phase</th>
                <th class="px-4 py-2.5 font-medium">Class</th>
                <th class="px-4 py-2.5 font-medium text-right">Size</th>
                <th class="px-4 py-2.5 font-medium">Used</th>
                <th class="px-4 py-2.5 font-medium">Mounted by</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!volumes.length">
                <td colspan="7" class="px-4 py-8 text-center text-muted">
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
                  <td class="px-4 py-2.5">
                    <span class="inline-flex items-center gap-2">
                      <StatusDot :tone="volume.bound ? 'success' : 'error'" />
                      <span class="font-mono text-xs text-highlighted break-all">{{ volume.name }}</span>
                    </span>
                    <p class="text-[11px] text-dimmed pl-3.5">{{ volume.namespace }}</p>
                  </td>
                  <td class="px-4 py-2.5 text-xs">
                    <RouterLink
                      v-if="volume.project"
                      :to="{ name: 'project', params: { name: volume.project } }"
                      class="text-primary hover:underline"
                      >{{ volume.project }}</RouterLink
                    >
                    <span v-else class="text-dimmed">the platform</span>
                  </td>
                  <td class="px-4 py-2.5 text-xs" :class="volume.bound ? 'text-toned' : 'text-error'">
                    {{ volume.phase }}
                  </td>
                  <td class="px-4 py-2.5 font-mono text-xs text-dimmed">{{ volume.storageClass || "—" }}</td>
                  <td class="px-4 py-2.5 text-right font-mono text-xs tabular-nums text-toned">
                    {{ volume.capacity || volume.requested || "—" }}
                  </td>
                  <td class="px-4 py-2.5">
                    <FillBar
                      :fraction="volume.usage?.usedFraction ?? null"
                      :caption="volume.usage ? formatBytes(volume.usage.usedBytes) : undefined"
                      :unmeasured="usageMessage || '—'"
                    />
                  </td>
                  <td class="px-4 py-2.5 font-mono text-[11px] text-dimmed break-all">
                    {{ (volume.pods ?? []).join(", ") || "nothing" }}
                  </td>
                </tr>
                <tr v-if="volume.message" :key="`${volume.namespace}/${volume.name}-message`" class="border-b border-muted last:border-0">
                  <td colspan="7" class="px-4 pb-2.5 text-xs" :class="volume.bound ? 'text-muted' : 'text-error'">
                    {{ volume.message }}
                  </td>
                </tr>
              </template>
            </tbody>
          </table>
        </div>
      </div>

      <div class="grid gap-4 lg:grid-cols-2">
        <div>
          <h2 class="text-sm font-medium text-highlighted mb-2">The telemetry store</h2>
          <div class="rounded-md border border-default px-4 py-3 space-y-2">
            <p v-if="store?.message" class="text-xs text-warning">{{ store.message }}</p>
            <template v-else-if="store">
              <FillBar
                :fraction="store.capacityBytes ? (store.usedFraction ?? 0) : null"
                :caption="
                  store.capacityBytes
                    ? `${formatBytes(store.bytesOnDisk)} of ${formatBytes(store.capacityBytes)}`
                    : undefined
                "
                :unmeasured="`${formatBytes(store.bytesOnDisk)} on a volume the platform does not own`"
                width="w-40"
              />
              <div class="grid grid-cols-2 gap-3 text-xs pt-1">
                <div>
                  <p class="text-[11px] text-muted">Ingest</p>
                  <p class="font-mono" :class="store.rowsPerSecond > 0 ? 'text-toned' : 'text-warning'">
                    {{ store.rowsPerSecond.toFixed(store.rowsPerSecond < 10 ? 1 : 0) }} rows/s
                  </p>
                </div>
                <div>
                  <p class="text-[11px] text-muted">Retention</p>
                  <p class="font-mono text-toned">
                    {{ store.retentionDays ? `${store.retentionDays} days` : "—" }}
                  </p>
                </div>
                <div class="col-span-2">
                  <p class="text-[11px] text-muted">Volume</p>
                  <p class="font-mono text-toned break-all">{{ store.claim || "external" }}</p>
                </div>
              </div>
              <p class="text-[11px] text-dimmed">
                Retention is the one knob every table's TTL is derived from — the horizon past which the store
                deliberately holds nothing.
              </p>
            </template>
            <p v-else class="text-xs text-muted">{{ loading ? "Loading…" : "No telemetry store on this installation." }}</p>
          </div>
        </div>

        <div>
          <h2 class="text-sm font-medium text-highlighted mb-2">What the flow stream lost</h2>
          <div
            class="rounded-md border px-4 py-3"
            :class="flows && !flows.lossless ? 'border-warning/40 bg-warning/5' : 'border-default'"
          >
            <template v-if="flows">
              <p class="text-xs flex items-center gap-2" :class="flows.lossless ? 'text-muted' : 'text-warning'">
                <StatusDot :tone="flows.lossless ? 'success' : 'warning'" />
                <span>
                  {{
                    flows.lossless
                      ? "Nothing was reported lost in the follower's trailing window."
                      : "Hubble reported dropping events — request counts under-report by an unknown amount."
                  }}
                </span>
              </p>
              <div class="grid grid-cols-3 gap-3 text-xs mt-2">
                <div>
                  <p class="text-[11px] text-muted">Events lost</p>
                  <p class="font-mono" :class="flows.events ? 'text-warning' : 'text-toned'">
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
        the problems list are the same number.
      </p>
    </template>
  </div>
</template>
