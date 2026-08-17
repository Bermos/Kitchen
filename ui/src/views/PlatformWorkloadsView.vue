<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, type PlatformPod, type PlatformWorkload } from "../lib/api";
import { timeAgo, uptime } from "../lib/format";
import { useAsync, usePoll } from "../lib/useAsync";
import StatusDot from "../components/StatusDot.vue";

// Every workload and every pod on the platform, applications and platform
// components alike — and, first, the workloads that have no pods at all.
//
// That order is the whole lesson. A workload whose pods were refused at
// admission has nothing to show: the pod never existed, so nothing is Pending
// and nothing is CrashLooping, and a listing of pods is a listing of the
// healthy ones. `kubectl get pods` looks clean and the rejection is a
// FailedCreate event on the workload — which is why this screen leads with the
// count that a pod listing can never contain, and why an empty list here says
// so in words rather than being an empty table somebody reads as calm.

const route = useRoute();
const router = useRouter();

/** Where a finding's evidence lands: `?namespace=` narrows the server's answer,
 * `?name=` picks the row out of it. */
const namespace = computed(() => (route.query.namespace as string) || "");
const named = computed(() => (route.query.name as string) || "");

const { data, error, loading, refresh } = useAsync(() =>
  api.platformWorkloads({ namespace: namespace.value || undefined }),
);
watch(namespace, () => void refresh());
usePoll(() => void refresh(), 30_000, () => true);

const search = ref("");

function matches(text: string[]): boolean {
  const needle = search.value.trim().toLowerCase();
  if (!needle) return true;
  return text.some((value) => value?.toLowerCase().includes(needle));
}

const workloads = computed(() =>
  (data.value?.items ?? []).filter((item) => matches([item.name, item.namespace, item.project ?? "", item.environment ?? "", item.component ?? ""])),
);
const pods = computed(() =>
  (data.value?.pods ?? []).filter((pod) => matches([pod.name, pod.namespace, pod.project ?? "", pod.environment ?? "", pod.node ?? ""])),
);

/** The lead: wants pods, has none. */
const withoutPods = computed(() => workloads.value.filter((item) => item.desired > 0 && item.pods === 0));
const eventsMessage = computed(() => data.value?.eventsMessage ?? "");

function subject(item: PlatformWorkload): string {
  if (item.project) return `${item.project} / ${item.environment || "—"}`;
  return item.component || "platform";
}

function workloadEvents(item: PlatformWorkload) {
  return { path: "/platform/events", query: { namespace: item.namespace, kind: item.kind, name: item.name } };
}

function podEvents(pod: PlatformPod) {
  return { path: "/platform/events", query: { namespace: pod.namespace, kind: "Pod", name: pod.name } };
}

function podTone(pod: PlatformPod) {
  if (pod.oomKilled || pod.phase === "Failed") return "error" as const;
  if (!pod.ready) return "warning" as const;
  return "success" as const;
}

function highlighted(name: string): boolean {
  return named.value !== "" && name === named.value;
}

function clearFilters() {
  void router.replace({ path: "/platform/workloads" });
}
</script>

<template>
  <div class="space-y-5">
    <div class="flex items-start justify-between gap-4 flex-wrap">
      <div>
        <div class="flex items-center gap-2 text-xs text-muted mb-1">
          <RouterLink to="/platform" class="hover:text-highlighted">Platform</RouterLink>
          <span>/</span>
          <span class="text-toned">Workloads</span>
        </div>
        <h1 class="text-xl font-semibold text-highlighted">Workloads</h1>
        <p class="text-xs text-muted mt-1">
          Everything the cluster is running, and first of all the things it is not running that it was asked to.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <UInput v-model="search" size="sm" icon="i-lucide-search" placeholder="Filter by name or namespace" class="w-64" />
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
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <template v-else>
      <div v-if="namespace || named" class="flex items-center gap-2 text-[11px] flex-wrap">
        <span class="text-dimmed">Filtered to</span>
        <button
          class="font-mono px-1.5 py-0.5 rounded border border-default text-toned hover:border-accented hover:text-error"
          title="Show everything again"
          @click="clearFilters"
        >
          {{ [namespace, named].filter(Boolean).join(" / ") }} ×
        </button>
      </div>

      <!-- The lead. An empty list here is an answer, not an empty table. -->
      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2 flex items-center gap-2">
          Workloads with no pods at all
          <span
            v-if="data"
            class="font-mono text-xs px-1.5 py-0.5 rounded"
            :class="withoutPods.length ? 'bg-error/15 text-error' : 'bg-elevated text-dimmed'"
            >{{ withoutPods.length }}</span
          >
        </h2>

        <UAlert
          v-if="eventsMessage"
          color="warning"
          variant="soft"
          icon="i-lucide-eye-off"
          title="The cluster's warnings could not be read"
          :description="`${eventsMessage} A workload with no pods is still shown, but without the FailedCreate event that says why.`"
          class="mb-3"
        />

        <div
          v-if="!withoutPods.length"
          class="rounded-md border border-default bg-muted px-4 py-3 text-xs text-muted flex items-start gap-2"
        >
          <UIcon name="i-lucide-shield-check" class="size-4 text-success shrink-0 mt-px" />
          <span>
            {{ loading && !data ? "Loading…" : "Every workload that wants pods has them." }}
            This is the check nothing else on this screen can make: a workload whose pods are refused at admission has
            none to list, so it is absent from the pod table below rather than red in it.
          </span>
        </div>

        <div v-else class="rounded-md border border-error/40 overflow-hidden divide-y divide-error/20">
          <div
            v-for="item in withoutPods"
            :key="`${item.namespace}/${item.name}`"
            class="px-4 py-3 bg-error/5"
            :class="highlighted(item.name) ? 'ring-1 ring-inset ring-error/40' : ''"
          >
            <div class="flex items-start justify-between gap-4 flex-wrap">
              <div class="min-w-0">
                <p class="text-sm">
                  <span class="font-mono text-highlighted">{{ item.kind }}/{{ item.name }}</span>
                  <span class="text-dimmed font-mono text-xs"> · {{ item.namespace }}</span>
                </p>
                <p class="text-[11px] text-muted mt-0.5">
                  {{ subject(item) }} — wants {{ item.desired }} pod{{ item.desired === 1 ? "" : "s" }} and has none
                </p>
              </div>
              <RouterLink
                :to="workloadEvents(item)"
                class="text-xs text-primary hover:underline inline-flex items-center gap-1 shrink-0"
              >
                its events <UIcon name="i-lucide-arrow-right" class="size-3" />
              </RouterLink>
            </div>

            <template v-if="item.admission">
              <p v-if="item.admission.suspect" class="text-xs text-error mt-2 font-medium">
                {{ item.admission.suspect }}
              </p>
              <p class="text-[11px] text-toned mt-1 font-mono break-words">{{ item.admission.message }}</p>
              <p class="text-[11px] text-dimmed mt-1">
                {{ item.admission.reason }} · ×{{ item.admission.count }} · last {{ timeAgo(item.admission.at) }}
              </p>
            </template>
            <p v-else class="text-[11px] text-dimmed mt-2">
              No FailedCreate warning was recorded for it, so the refusal — if there was one — is not in the last window
              of the cluster's events.
            </p>
          </div>
        </div>
      </div>

      <div class="grid grid-cols-3 sm:grid-cols-6 gap-3">
        <div class="rounded-md border border-default px-3 py-2.5">
          <p class="text-[11px] text-muted">Pods</p>
          <p class="text-base font-semibold text-highlighted tabular-nums">{{ data?.totals.pods ?? "—" }}</p>
        </div>
        <div class="rounded-md border border-default px-3 py-2.5">
          <p class="text-[11px] text-muted">Running</p>
          <p class="text-base font-semibold text-highlighted tabular-nums">{{ data?.totals.running ?? "—" }}</p>
        </div>
        <div class="rounded-md border border-default px-3 py-2.5">
          <p class="text-[11px] text-muted">Pending</p>
          <p class="text-base font-semibold tabular-nums" :class="data?.totals.pending ? 'text-warning' : 'text-highlighted'">
            {{ data?.totals.pending ?? "—" }}
          </p>
        </div>
        <div class="rounded-md border border-default px-3 py-2.5">
          <p class="text-[11px] text-muted">Not ready</p>
          <p class="text-base font-semibold tabular-nums" :class="data?.totals.notReady ? 'text-warning' : 'text-highlighted'">
            {{ data?.totals.notReady ?? "—" }}
          </p>
        </div>
        <div class="rounded-md border border-default px-3 py-2.5">
          <p class="text-[11px] text-muted">Restarts</p>
          <p class="text-base font-semibold tabular-nums" :class="data?.totals.restarts ? 'text-warning' : 'text-highlighted'">
            {{ data?.totals.restarts ?? "—" }}
          </p>
        </div>
        <div class="rounded-md border border-default px-3 py-2.5">
          <p class="text-[11px] text-muted">OOM kills</p>
          <p class="text-base font-semibold tabular-nums" :class="data?.totals.oomKills ? 'text-error' : 'text-highlighted'">
            {{ data?.totals.oomKills ?? "—" }}
          </p>
        </div>
      </div>

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">
          Every workload
          <span class="text-dimmed font-normal text-xs"
            >— {{ data?.workloads ?? 0 }} of them, {{ data?.unhealthy ?? 0 }} unhealthy, worst first</span
          >
        </h2>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-4 py-2.5 font-medium">Workload</th>
                <th class="px-4 py-2.5 font-medium">For</th>
                <th class="px-4 py-2.5 font-medium text-right">Desired</th>
                <th class="px-4 py-2.5 font-medium text-right">Ready</th>
                <th class="px-4 py-2.5 font-medium text-right">Available</th>
                <th class="px-4 py-2.5 font-medium text-right">Pods</th>
                <th class="px-4 py-2.5 font-medium text-right">Events</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!workloads.length">
                <td colspan="7" class="px-4 py-8 text-center text-muted">
                  {{ loading ? "Loading…" : "No workloads match." }}
                </td>
              </tr>
              <tr
                v-for="item in workloads"
                :key="`${item.namespace}/${item.kind}/${item.name}`"
                class="border-b border-muted last:border-0"
                :class="[
                  item.desired > 0 && item.pods === 0 ? 'bg-error/5' : 'hover:bg-elevated/40',
                  highlighted(item.name) ? 'ring-1 ring-inset ring-primary/40' : '',
                ]"
              >
                <td class="px-4 py-2.5">
                  <span class="inline-flex items-center gap-2">
                    <StatusDot :tone="item.healthy ? 'success' : item.pods === 0 && item.desired > 0 ? 'error' : 'warning'" />
                    <span class="font-mono text-highlighted">{{ item.name }}</span>
                  </span>
                  <p class="text-[11px] text-dimmed pl-3.5">{{ item.kind }} · {{ item.namespace }}</p>
                </td>
                <td class="px-4 py-2.5 text-xs text-toned">
                  <RouterLink
                    v-if="item.environment"
                    :to="{ name: 'environment', params: { name: item.environment } }"
                    class="hover:underline text-primary"
                    >{{ subject(item) }}</RouterLink
                  >
                  <span v-else>{{ subject(item) }}</span>
                </td>
                <td class="px-4 py-2.5 text-right font-mono text-toned tabular-nums">{{ item.desired }}</td>
                <td class="px-4 py-2.5 text-right font-mono tabular-nums" :class="item.ready < item.desired ? 'text-warning' : 'text-toned'">
                  {{ item.ready }}
                </td>
                <td class="px-4 py-2.5 text-right font-mono tabular-nums" :class="item.available < item.desired ? 'text-warning' : 'text-toned'">
                  {{ item.available }}
                </td>
                <!-- The column the replica counts cannot give you: zero
                     available is pods failing *or* pods never created. -->
                <td
                  class="px-4 py-2.5 text-right font-mono tabular-nums font-semibold"
                  :class="item.desired > 0 && item.pods === 0 ? 'text-error' : 'text-toned'"
                  :title="item.desired > 0 && item.pods === 0 ? 'This workload wants pods and has none — nothing failing, nothing pending, nothing at all' : ''"
                >
                  {{ item.pods }}
                </td>
                <td class="px-4 py-2.5 text-right">
                  <RouterLink :to="workloadEvents(item)" class="text-xs text-primary hover:underline">events</RouterLink>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

      <div>
        <h2 class="text-sm font-medium text-highlighted mb-2">
          Pods <span class="text-dimmed font-normal text-xs">— worst first, so a limit only ever drops healthy ones</span>
        </h2>
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-4 py-2.5 font-medium">Pod</th>
                <th class="px-4 py-2.5 font-medium">For</th>
                <th class="px-4 py-2.5 font-medium">Node</th>
                <th class="px-4 py-2.5 font-medium">Phase</th>
                <th class="px-4 py-2.5 font-medium text-right">Restarts</th>
                <th class="px-4 py-2.5 font-medium">Up</th>
                <th class="px-4 py-2.5 font-medium">Detail</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!pods.length">
                <td colspan="7" class="px-4 py-8 text-center text-muted">
                  {{ loading ? "Loading…" : "No pods match." }}
                </td>
              </tr>
              <tr
                v-for="pod in pods"
                :key="`${pod.namespace}/${pod.name}`"
                class="border-b border-muted last:border-0 hover:bg-elevated/40"
              >
                <td class="px-4 py-2.5">
                  <span class="inline-flex items-center gap-2">
                    <StatusDot :tone="podTone(pod)" />
                    <span class="font-mono text-xs text-highlighted truncate">{{ pod.name }}</span>
                  </span>
                  <p class="text-[11px] text-dimmed pl-3.5">{{ pod.namespace }}</p>
                </td>
                <td class="px-4 py-2.5 text-xs">
                  <RouterLink
                    v-if="pod.environment"
                    :to="{ name: 'environment', params: { name: pod.environment } }"
                    class="text-primary hover:underline"
                    >{{ pod.project }} / {{ pod.environment }}</RouterLink
                  >
                  <span v-else class="text-dimmed font-mono">{{ pod.workload || "—" }}</span>
                </td>
                <td class="px-4 py-2.5">
                  <RouterLink
                    v-if="pod.node"
                    :to="{ path: '/platform/nodes', query: { node: pod.node } }"
                    class="font-mono text-xs text-primary hover:underline"
                    >{{ pod.node }}</RouterLink
                  >
                  <span v-else class="text-dimmed text-xs">unscheduled</span>
                </td>
                <td class="px-4 py-2.5 text-xs" :class="pod.phase === 'Failed' ? 'text-error' : pod.ready ? 'text-toned' : 'text-warning'">
                  {{ pod.phase }}<template v-if="!pod.ready && pod.phase === 'Running'"> · not ready</template>
                </td>
                <td class="px-4 py-2.5 text-right font-mono tabular-nums" :class="pod.restarts ? 'text-warning' : 'text-dimmed'">
                  {{ pod.restarts }}<span v-if="pod.oomKilled" class="text-error" title="killed for exceeding its memory limit"> · OOM</span>
                </td>
                <td class="px-4 py-2.5 text-xs text-muted whitespace-nowrap">{{ uptime(pod.startedAt) }}</td>
                <td class="px-4 py-2.5 text-xs text-toned max-w-xs">
                  <span class="block truncate" :title="pod.message">{{ pod.message || "—" }}</span>
                  <RouterLink :to="podEvents(pod)" class="text-[11px] text-primary hover:underline">its events</RouterLink>
                </td>
              </tr>
            </tbody>
          </table>
        </div>
        <p v-if="data?.truncated" class="text-[11px] text-warning mt-1.5">
          The pod listing was cut at the limit. It is sorted worst first, so what the cut dropped is pods that are
          running normally.
        </p>
      </div>
    </template>
  </div>
</template>
