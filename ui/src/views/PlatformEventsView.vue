<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, type K8sEvent, type PlatformEventQuery } from "../lib/api";
import { compactCount, timeAgo } from "../lib/format";
import { useAsync, usePoll } from "../lib/useAsync";
import PageHeader from "../components/PageHeader.vue";

// The cluster's Warning history — FailedScheduling, FailedCreate, FailedMount,
// OOMKilling — which Kubernetes expires about an hour after the fact and the
// operator records so that "what happened at 03:00" has an answer.
//
// It is not the activity feed: `/events` is the platform's story, written by
// the reconcilers about things Kitchen did; this is the cluster's, about things
// that happened to it.
//
// The whole selection lives in the URL, exactly as the log view's does, because
// this screen is mostly arrived at rather than opened: every other operator
// screen deep-links into it — "show me this pod's events" — and a filter that
// is not in the URL is a link that cannot be sent.

const route = useRoute();
const router = useRouter();

/** The filters the store itself applies. */
const FILTERS = ["project", "environment", "namespace", "kind", "name", "reason", "node"] as const;

const ranges = [
  { label: "Last hour", value: 60 },
  { label: "Last 6 hours", value: 360 },
  { label: "Last 24 hours", value: 1440 },
  { label: "Last 7 days", value: 10080 },
  { label: "Last 30 days", value: 43200 },
];
const limits = [100, 250, 500, 1000];

const search = ref((route.query.search as string) ?? "");

function param(key: string): string {
  return (route.query[key] as string) ?? "";
}
const rangeMinutes = computed(() => Number(route.query.range ?? 60) || 60);
const limit = computed(() => Number(route.query.limit ?? 100) || 100);

function apply(patch: Record<string, string | number | undefined>) {
  const query: Record<string, string> = {};
  for (const [key, value] of Object.entries({ ...route.query, ...patch })) {
    const text = value === undefined || value === null ? "" : String(value);
    if (text) query[key] = text;
  }
  void router.replace({ path: "/platform/events", query });
}

function selection(): PlatformEventQuery {
  const query: PlatformEventQuery = {
    since: new Date(Date.now() - rangeMinutes.value * 60_000).toISOString(),
    limit: limit.value,
  };
  for (const key of FILTERS) {
    const value = param(key);
    if (value) query[key] = value;
  }
  if (search.value.trim()) query.search = search.value.trim();
  return query;
}

const { data, error, loading, refresh } = useAsync(() => api.platformEvents(selection()));
// The URL is the selection, so a changed URL is a changed question — but only
// while this is still the screen being looked at.
watch(
  () => route.fullPath,
  (path) => {
    if (path.startsWith("/platform/events")) void refresh();
  },
);
usePoll(() => void refresh(), 30_000, () => true);

const events = computed(() => data.value?.items ?? []);
const facets = computed(() => data.value?.facets ?? []);

/** Every filter currently narrowing the screen, as removable chips. */
const chips = computed(() => {
  const active: { key: string; value: string }[] = [];
  for (const key of FILTERS) {
    const value = param(key);
    if (value) active.push({ key, value });
  }
  if (search.value.trim()) active.push({ key: "search", value: search.value.trim() });
  return active;
});

function drop(key: string) {
  if (key === "search") {
    search.value = "";
    apply({ search: undefined });
    return;
  }
  apply({ [key]: undefined });
}

function narrow(field: string, value: string) {
  apply({ [field]: param(field) === value ? undefined : value });
}

function clearAll() {
  search.value = "";
  void router.replace({ path: "/platform/events" });
}

function facetLabel(field: string): string {
  return field.charAt(0).toUpperCase() + field.slice(1);
}

function time(iso: string): string {
  const date = new Date(iso);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleString("en-GB");
}

function environmentOf(event: K8sEvent) {
  return event.environment ? { name: "environment", params: { name: event.environment } } : null;
}
</script>

<template>
  <div class="space-y-6">
    <PageHeader title="Events" :breadcrumb="[{ label: 'Platform', to: '/platform' }, { label: 'Events' }]">
      <template #description>
        The cluster's warnings, kept past the hour Kubernetes keeps them — so “what happened at 03:00” has an answer.
      </template>
      <template #actions>
        <USelect
          :model-value="rangeMinutes"
          :items="ranges"
          size="xs"
          class="w-36"
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
          :loading="loading"
          aria-label="Refresh"
          @click="refresh"
        />
      </template>
    </PageHeader>

    <UInput
      v-model="search"
      icon="i-lucide-search"
      placeholder="Search the messages — a port, an object, a policy"
      size="sm"
      @keydown.enter="apply({ search: search.trim() || undefined })"
      @blur="apply({ search: search.trim() || undefined })"
    />

    <div v-if="chips.length" class="flex items-center gap-2 flex-wrap text-[11px]">
      <button
        v-for="chip in chips"
        :key="chip.key"
        class="font-mono px-1.5 py-0.5 rounded border border-default text-toned hover:border-accented hover:text-error"
        title="Remove this filter"
        @click="drop(chip.key)"
      >
        {{ chip.key }}:{{ chip.value }} ×
      </button>
      <button class="text-dimmed hover:text-highlighted" @click="clearAll">clear all</button>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <!-- The facets are a column beside the results only where there is room
         for one; narrower than that they follow the results down the page. -->
    <div v-else class="flex flex-col lg:flex-row gap-4 items-stretch lg:items-start">
      <div class="flex-1 min-w-0 space-y-2">
        <div class="rounded-md border border-default overflow-x-auto">
          <table class="w-full min-w-[52rem] text-sm">
            <thead>
              <tr class="text-left text-xs text-muted border-b border-default bg-muted">
                <th class="px-3 py-2 font-medium">When</th>
                <th class="px-3 py-2 font-medium">Reason</th>
                <th class="px-3 py-2 font-medium">Object</th>
                <th class="px-3 py-2 font-medium">Message</th>
                <th class="px-3 py-2 font-medium text-right">Count</th>
              </tr>
            </thead>
            <tbody>
              <tr v-if="!events.length">
                <td colspan="5" class="px-3 py-8 text-center text-muted text-sm">
                  {{
                    loading
                      ? "Loading…"
                      : "Nothing was warned about in this window. A quiet cluster and a filter that matches nothing look the same here — the chips above say which this is."
                  }}
                </td>
              </tr>
              <tr v-for="(event, i) in events" :key="i" class="border-b border-muted last:border-0 align-top hover:bg-elevated/40">
                <td class="px-3 py-2 text-xs text-dimmed font-mono whitespace-nowrap" :title="event.timestamp">
                  {{ time(event.timestamp) }}
                  <p class="text-[11px]">{{ timeAgo(event.timestamp) }}</p>
                </td>
                <td class="px-3 py-2">
                  <button
                    class="font-mono text-xs text-warning hover:underline"
                    title="Narrow to this reason"
                    @click="narrow('reason', event.reason)"
                  >
                    {{ event.reason }}
                  </button>
                </td>
                <!-- The object's name is the row's identity: it stays on one
                     line and the table scrolls, rather than the message column
                     shattering it down the page a character at a time. -->
                <td class="px-3 py-2 text-xs whitespace-nowrap">
                  <button
                    class="font-mono text-highlighted hover:underline text-left"
                    title="Narrow to this object"
                    @click="apply({ kind: event.kind, name: event.name, namespace: event.namespace })"
                  >
                    {{ event.kind }}/{{ event.name }}
                  </button>
                  <p class="text-[11px] text-dimmed font-mono">
                    {{ event.namespace || "cluster-scoped" }}
                    <template v-if="event.node"> · {{ event.node }}</template>
                  </p>
                  <RouterLink
                    v-if="environmentOf(event)"
                    :to="environmentOf(event)!"
                    class="text-[11px] text-primary hover:underline"
                  >
                    {{ event.project }} / {{ event.environment }}
                  </RouterLink>
                </td>
                <td class="px-3 py-2 text-xs text-toned break-words w-full">{{ event.message }}</td>
                <td class="px-3 py-2 text-right font-mono text-xs text-dimmed tabular-nums">
                  <template v-if="event.count > 1">×{{ event.count }}</template>
                </td>
              </tr>
            </tbody>
          </table>
        </div>

        <p v-if="data?.truncated" class="text-[11px] text-warning">
          The page stopped at {{ limit }} rows, so the facets describe this page rather than the whole window. Narrow
          the window or a facet to see the rest.
        </p>
        <p class="text-[11px] text-dimmed leading-relaxed">
          <span class="font-mono">count</span> on a row is Kubernetes' own repeat count for that event; the facet counts
          are rows, so the two deliberately do not add up to each other. This history is retained for as long as the
          store's one retention knob says, which is why an event Kubernetes forgot an hour after the fact is still here.
        </p>
      </div>

      <aside class="w-full lg:w-56 lg:shrink-0 space-y-4 text-xs">
        <div v-for="facet in facets" :key="facet.field">
          <p class="text-muted mb-1.5">
            {{ facetLabel(facet.field) }}
          </p>
          <p v-if="!facet.values?.length" class="text-dimmed px-2">—</p>
          <button
            v-for="value in facet.values ?? []"
            :key="value.value"
            class="flex items-center justify-between w-full px-2 py-1 rounded text-left hover:bg-elevated"
            :class="param(facet.field) === value.value ? 'bg-elevated' : ''"
            @click="narrow(facet.field, value.value)"
          >
            <span class="font-mono truncate" :class="facet.field === 'reason' ? 'text-warning' : 'text-toned'">
              {{ value.value || "—" }}
            </span>
            <span class="text-dimmed font-mono ml-2 tabular-nums">{{ compactCount(value.count) }}</span>
          </button>
        </div>
        <p class="text-dimmed leading-relaxed">Counts are over the rows on this page, not over the whole window.</p>
      </aside>
    </div>
  </div>
</template>
