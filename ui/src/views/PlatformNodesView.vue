<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, type PlatformNode } from "../lib/api";
import { formatBytes, timeAgo, uptime } from "../lib/format";
import { freshness, latestObserved, formatFraction, nodePressure, NODE_SATURATION_FRACTION } from "../lib/platform";
import { useAsync, usePoll } from "../lib/useAsync";
import FillBar from "../components/FillBar.vue";
import Sparkline from "../components/Sparkline.vue";
import StatusDot from "../components/StatusDot.vue";

// What the cluster is made of, plus the column that earns the screen.
//
// A node whose collector died — or was never admitted, which is the Pod
// Security failure the platform namespace's own level exists to prevent — reads
// perfectly healthy everywhere else: its conditions are True, its pods are
// Running, and it simply stops contributing to every number the platform
// reports. Telemetry freshness is where that looks broken, so silence here is
// deliberately loud: a red badge, a tinted row, and a banner above the table.
//
// The third state is the one that is easy to get wrong. When the store could
// not be read at all, freshness is *unknown* for every node — neither fresh nor
// silent. Rendering that as health would be the same wrong answer this screen
// exists to prevent, arrived at from the other side; rendering it as silence
// would condemn a cluster over an unreachable database.

const route = useRoute();
const router = useRouter();

/** `?node=` narrows to one, which is where the findings' evidence links point. */
const node = computed(() => (route.query.node as string) || "");

const { data, error, loading, refresh } = useAsync(() => api.platformNodes({ node: node.value || undefined }));
watch(node, () => void refresh());
usePoll(() => void refresh(), 30_000, () => true);

// The collection layer's own side of the same question, read separately: a
// silent node with a CrashLoopBackOff collector and a silent node with no
// collector pod at all are different diagnoses, and only the second one is the
// admission failure — where there is no pod to inspect, and so nothing on any
// pod listing to find.
const ingest = useAsync(() => api.platformIngest());
usePoll(() => void ingest.refresh(), 30_000, () => true);
const collectors = computed(() => {
  const byNode = new Map<string, string>();
  for (const entry of ingest.data.value?.items ?? []) byNode.set(entry.node, entry.collector ?? "");
  return byNode;
});
/** Empty string means a node the ingest read covered and found no collector pod
 * on; undefined means the read has not answered for it at all. */
function collector(name: string): string | undefined {
  return collectors.value.get(name);
}

const nodes = computed(() => data.value?.items ?? []);
const telemetryMessage = computed(() => data.value?.telemetryMessage ?? "");
const usageMessage = computed(() => data.value?.usageMessage ?? "");

const silent = computed(() => nodes.value.filter((item) => freshness(item.telemetry, telemetryMessage.value).state === "silent"));
const notReady = computed(() => nodes.value.filter((item) => !item.ready));

const expanded = ref<string | null>(null);
function toggle(name: string) {
  expanded.value = expanded.value === name ? null : name;
}

function clearFilter() {
  void router.replace({ path: "/platform/nodes" });
}

/** The saturation series, where something reads host metrics back out of the
 * store. Buckets nothing was observed in are null and stay out of the shape:
 * the number beside it is the newest bucket that was actually measured. */
function series(points: { start: string; value: number | null }[] | undefined): number[] {
  return (points ?? []).filter((point) => point.value !== null).map((point) => (point.value as number) * 100);
}

function saturationTone(fraction: number | null): string {
  if (fraction === null) return "text-dimmed";
  return fraction >= NODE_SATURATION_FRACTION ? "text-error" : "text-toned";
}

/** The fullest filesystem on a node, which is the one worth a column. */
function fullest(item: PlatformNode) {
  const filesystems = item.usage?.filesystems ?? [];
  let worst: { mountPoint: string; fraction: number } | null = null;
  for (const filesystem of filesystems) {
    const fraction = filesystem.latest ?? latestObserved(filesystem.used);
    if (fraction === null || fraction === undefined) continue;
    if (!worst || fraction > worst.fraction) worst = { mountPoint: filesystem.mountPoint, fraction };
  }
  return worst;
}
</script>

<template>
  <div class="space-y-5">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <div class="flex items-center gap-2 text-xs text-muted mb-1">
          <RouterLink to="/platform" class="hover:text-highlighted">Platform</RouterLink>
          <span>/</span>
          <span class="text-toned">Nodes</span>
        </div>
        <h1 class="text-xl font-semibold text-highlighted">Nodes</h1>
        <p class="text-xs text-muted mt-1">
          What the cluster is made of — and, in the last column, which of its machines the platform is still hearing
          from.
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
      <div v-if="node" class="flex items-center gap-2 text-[11px]">
        <span class="text-dimmed">Filtered to</span>
        <button
          class="font-mono px-1.5 py-0.5 rounded border border-default text-toned hover:border-accented hover:text-error"
          title="Show every node again"
          @click="clearFilter"
        >
          {{ node }} ×
        </button>
      </div>

      <!-- Freshness is unknown, not fine. The store could not be read, so every
           node's last column is a question rather than an answer. -->
      <UAlert
        v-if="telemetryMessage"
        color="neutral"
        variant="soft"
        icon="i-lucide-eye-off"
        title="Telemetry freshness is unknown for every node"
        :description="telemetryMessage"
      />
      <!-- Silence, loudly. -->
      <UAlert
        v-else-if="silent.length"
        color="error"
        variant="soft"
        icon="i-lucide-signal-zero"
        :title="`${silent.length} node${silent.length === 1 ? ' has' : 's have'} shipped no telemetry`"
        :description="`${silent.map((item) => item.name).join(', ')} — everything running there is missing from every number this platform reports. A collector that died, or one that was never admitted: a DaemonSet whose pods are refused has no pods at all, so nothing else on any screen would show it.`"
      />

      <!-- The DaemonSet's own arithmetic, which catches the collector that
           never started: pods refused at admission leave nothing for a pod
           listing to show, so a non-zero desired with nothing available is the
           only trace of it. -->
      <UAlert
        v-if="ingest.data.value?.collector?.message"
        color="error"
        variant="soft"
        icon="i-lucide-package-x"
        title="The node collector is not running"
        :description="ingest.data.value.collector.message"
      />
      <UAlert
        v-else-if="ingest.data.value?.nodesWithoutCollector"
        color="warning"
        variant="soft"
        icon="i-lucide-package-x"
        :title="`${ingest.data.value.nodesWithoutCollector} node${ingest.data.value.nodesWithoutCollector === 1 ? ' has' : 's have'} no collector pod`"
        description="Nothing is collecting logs or metrics there. A pod refused at admission never exists, so this is the only place it shows."
      />

      <div class="grid grid-cols-2 sm:grid-cols-4 gap-3">
        <div class="rounded-md border border-default px-4 py-3">
          <p class="text-xs text-muted">Nodes</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums mt-1">{{ data?.nodes ?? "—" }}</p>
        </div>
        <div class="rounded-md border px-4 py-3" :class="notReady.length ? 'border-error/40 bg-error/5' : 'border-default'">
          <p class="text-xs text-muted">Ready</p>
          <p class="text-lg font-semibold tabular-nums mt-1" :class="notReady.length ? 'text-error' : 'text-highlighted'">
            {{ data ? `${data.readyNodes}/${data.nodes}` : "—" }}
          </p>
        </div>
        <div
          class="rounded-md border px-4 py-3"
          :class="telemetryMessage ? 'border-default' : data?.silentNodes ? 'border-error/40 bg-error/5' : 'border-default'"
        >
          <p class="text-xs text-muted">Silent</p>
          <p
            class="text-lg font-semibold tabular-nums mt-1"
            :class="telemetryMessage ? 'text-dimmed' : data?.silentNodes ? 'text-error' : 'text-highlighted'"
          >
            {{ telemetryMessage ? "unknown" : (data?.silentNodes ?? "—") }}
          </p>
          <p class="text-[11px] text-dimmed mt-0.5">
            {{ telemetryMessage ? "the store could not be asked" : "nothing received for ten minutes or more" }}
          </p>
        </div>
        <div class="rounded-md border border-default px-4 py-3">
          <p class="text-xs text-muted">Pods placed</p>
          <p class="text-lg font-semibold text-highlighted tabular-nums mt-1">
            {{ nodes.reduce((total, item) => total + item.pods, 0) }}
          </p>
        </div>
      </div>

      <div class="rounded-md border border-default overflow-x-auto">
        <table class="w-full text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-default bg-muted">
              <th class="px-4 py-2.5 font-medium">Node</th>
              <th class="px-4 py-2.5 font-medium">Telemetry</th>
              <th class="px-4 py-2.5 font-medium text-right">Pods</th>
              <th class="px-4 py-2.5 font-medium">CPU</th>
              <th class="px-4 py-2.5 font-medium">Memory</th>
              <th class="px-4 py-2.5 font-medium">Disk</th>
              <th class="px-4 py-2.5 font-medium">Kubelet</th>
            </tr>
          </thead>
          <tbody>
            <tr v-if="!nodes.length">
              <td colspan="7" class="px-4 py-8 text-center text-muted">
                {{ loading ? "Loading…" : node ? `No node named ${node}.` : "The cluster reported no nodes." }}
              </td>
            </tr>
            <template v-for="item in nodes" :key="item.name">
              <tr
                class="border-b border-muted last:border-0 cursor-pointer"
                :class="
                  freshness(item.telemetry, telemetryMessage).state === 'silent'
                    ? 'bg-error/5 hover:bg-error/10'
                    : 'hover:bg-elevated/40'
                "
                :title="expanded === item.name ? 'Hide this node' : 'Show conditions and filesystems'"
                @click="toggle(item.name)"
              >
                <td class="px-4 py-3">
                  <span class="inline-flex items-center gap-2">
                    <StatusDot :tone="item.ready ? 'success' : 'error'" />
                    <span class="font-mono text-highlighted">{{ item.name }}</span>
                  </span>
                  <p class="text-[11px] text-dimmed mt-0.5 pl-3.5">
                    {{ (item.roles ?? []).join(", ") || "worker" }}
                    <template v-if="!item.schedulable"> · <span class="text-warning">cordoned</span></template>
                    <template v-if="!item.ready"> · <span class="text-error">NotReady</span></template>
                    <template v-if="nodePressure(item.conditions).some((c) => c.type !== 'Ready')">
                      ·
                      <span class="text-warning">{{
                        nodePressure(item.conditions)
                          .filter((c) => c.type !== "Ready")
                          .map((c) => c.type)
                          .join(", ")
                      }}</span>
                    </template>
                  </p>
                </td>
                <!-- The column the screen exists for. -->
                <td class="px-4 py-3">
                  <span
                    class="inline-flex items-center gap-1.5 px-1.5 py-0.5 rounded font-mono text-xs"
                    :class="{
                      'bg-error/15 text-error font-semibold': freshness(item.telemetry, telemetryMessage).state === 'silent',
                      'text-toned': freshness(item.telemetry, telemetryMessage).state === 'fresh',
                      'bg-elevated text-dimmed': freshness(item.telemetry, telemetryMessage).state === 'unknown',
                    }"
                    :title="freshness(item.telemetry, telemetryMessage).detail"
                  >
                    <UIcon
                      v-if="freshness(item.telemetry, telemetryMessage).state === 'silent'"
                      name="i-lucide-signal-zero"
                      class="size-3.5"
                    />
                    <UIcon
                      v-else-if="freshness(item.telemetry, telemetryMessage).state === 'unknown'"
                      name="i-lucide-circle-help"
                      class="size-3.5"
                    />
                    {{ freshness(item.telemetry, telemetryMessage).label }}
                  </span>
                  <p
                    v-if="collector(item.name) !== undefined"
                    class="text-[11px] mt-0.5"
                    :class="collector(item.name) ? 'text-dimmed' : 'text-error'"
                    :title="
                      collector(item.name)
                        ? 'the collector pod on this node, as the kubelet reports it'
                        : 'no collector pod exists on this node — a pod refused at admission leaves nothing to inspect'
                    "
                  >
                    {{ collector(item.name) || "no collector pod" }}
                  </p>
                </td>
                <td class="px-4 py-3 text-right font-mono text-toned tabular-nums">
                  {{ item.pods }}<span class="text-dimmed">/{{ item.allocatable.pods || "?" }}</span>
                </td>
                <td class="px-4 py-3">
                  <div v-if="item.usage" class="flex items-center gap-2">
                    <Sparkline :points="series(item.usage.cpu)" :width="56" :height="18" />
                    <span class="font-mono text-xs tabular-nums" :class="saturationTone(latestObserved(item.usage.cpu))">
                      {{ formatFraction(latestObserved(item.usage.cpu)) }}
                    </span>
                  </div>
                  <span v-else class="text-xs text-dimmed" :title="usageMessage">—</span>
                  <p class="text-[11px] text-dimmed">{{ item.allocatable.cpu || "?" }} allocatable</p>
                </td>
                <td class="px-4 py-3">
                  <div v-if="item.usage" class="flex items-center gap-2">
                    <Sparkline :points="series(item.usage.memory)" :width="56" :height="18" tone="text-info" />
                    <span
                      class="font-mono text-xs tabular-nums"
                      :class="saturationTone(latestObserved(item.usage.memory))"
                    >
                      {{ formatFraction(latestObserved(item.usage.memory)) }}
                    </span>
                  </div>
                  <span v-else class="text-xs text-dimmed" :title="usageMessage">—</span>
                  <p class="text-[11px] text-dimmed">{{ item.allocatable.memory || "?" }} allocatable</p>
                </td>
                <td class="px-4 py-3">
                  <FillBar
                    :fraction="fullest(item)?.fraction ?? null"
                    :caption="fullest(item)?.mountPoint"
                    :unmeasured="usageMessage || '—'"
                    width="w-16"
                  />
                </td>
                <td class="px-4 py-3 font-mono text-xs text-dimmed whitespace-nowrap">
                  {{ item.kubeletVersion || "—" }}
                  <p class="text-[11px]">up {{ uptime(item.createdAt) }}</p>
                </td>
              </tr>

              <tr v-if="expanded === item.name" :key="`${item.name}-detail`" class="border-b border-muted last:border-0">
                <td colspan="7" class="px-4 py-3 bg-muted">
                  <div class="grid gap-4 lg:grid-cols-2">
                    <div>
                      <h3 class="text-xs font-medium text-highlighted mb-1.5">Conditions</h3>
                      <table class="w-full text-xs">
                        <tbody>
                          <tr v-for="condition in item.conditions ?? []" :key="condition.type" class="align-top">
                            <td class="py-1 pr-3 font-mono text-toned whitespace-nowrap">{{ condition.type }}</td>
                            <td
                              class="py-1 pr-3 font-mono whitespace-nowrap"
                              :class="
                                nodePressure([condition]).length ? 'text-error' : condition.status === 'True' ? 'text-success' : 'text-dimmed'
                              "
                            >
                              {{ condition.status }}
                            </td>
                            <td class="py-1 pr-3 text-dimmed whitespace-nowrap">{{ timeAgo(condition.since) }}</td>
                            <td class="py-1 text-toned break-words w-full">
                              {{ condition.message || condition.reason || "—" }}
                            </td>
                          </tr>
                          <tr v-if="!item.conditions?.length">
                            <td class="py-1 text-muted">The node reported no conditions.</td>
                          </tr>
                        </tbody>
                      </table>
                    </div>

                    <div>
                      <h3 class="text-xs font-medium text-highlighted mb-1.5">Filesystems</h3>
                      <p v-if="!item.usage?.filesystems?.length" class="text-xs text-muted">
                        {{ usageMessage || "No filesystem fill was reported for this node." }}
                      </p>
                      <table v-else class="w-full text-xs">
                        <tbody>
                          <tr v-for="filesystem in item.usage.filesystems" :key="filesystem.mountPoint">
                            <td class="py-1 pr-3 font-mono text-toned whitespace-nowrap">
                              {{ filesystem.mountPoint }}
                            </td>
                            <td class="py-1 pr-3 text-dimmed whitespace-nowrap">
                              {{ filesystem.capacityBytes ? formatBytes(filesystem.capacityBytes) : "—" }}
                            </td>
                            <td class="py-1 w-full">
                              <FillBar :fraction="filesystem.latest ?? latestObserved(filesystem.used)" width="w-32" />
                            </td>
                          </tr>
                        </tbody>
                      </table>
                    </div>
                  </div>

                  <div class="mt-3 flex flex-wrap gap-3 text-[11px]">
                    <RouterLink
                      :to="{ path: '/platform/events', query: { search: item.name } }"
                      class="text-primary hover:underline inline-flex items-center gap-1"
                    >
                      <UIcon name="i-lucide-list" class="size-3" /> events mentioning this node
                    </RouterLink>
                    <RouterLink
                      :to="{ path: '/platform/workloads' }"
                      class="text-primary hover:underline inline-flex items-center gap-1"
                    >
                      <UIcon name="i-lucide-boxes" class="size-3" /> what is running on the platform
                    </RouterLink>
                  </div>
                </td>
              </tr>
            </template>
          </tbody>
        </table>
      </div>

      <!-- An unmeasured node and an idle one must not draw the same chart, so
           where nothing reads the series the columns are dashes with the reason
           rather than flat lines at zero. -->
      <p v-if="usageMessage" class="text-[11px] text-warning">{{ usageMessage }}</p>
      <p class="text-[11px] text-dimmed leading-relaxed">
        Freshness is the store's answer to “what has this node's collector written lately”, looked back over an hour. A
        node that said nothing in that hour has no last-seen time at all, which is why silence reads as an absence
        rather than as an old timestamp. Saturation and filesystem fill come from the same query the
        <span class="font-mono">node.saturated</span> and <span class="font-mono">node.disk-filling</span> rules fire
        on, so this screen and the problems list cannot disagree about a number.
      </p>
    </template>
  </div>
</template>
