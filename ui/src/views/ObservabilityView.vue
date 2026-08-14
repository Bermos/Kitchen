<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRoute, useRouter } from "vue-router";
import { APIError, api, type LogLine } from "../lib/api";
import { usePoll } from "../lib/useAsync";
import StatusDot from "../components/StatusDot.vue";

// The observability view does not pretend the logs live anywhere but
// ClickHouse: the query bar takes a real ClickHouse boolean expression over
// the logs table, sent to GET /api/v1/logs and evaluated as written —
// read-only and capped on the operator's side.

const route = useRoute();
const router = useRouter();

const where = ref((route.query.where as string) || "1 = 1");
const limit = ref(200);
const limits = [200, 500, 1000, 5000];

const ranges = [
  { label: "Last 15 minutes", value: 15 },
  { label: "Last hour", value: 60 },
  { label: "Last 6 hours", value: 360 },
  { label: "Last 24 hours", value: 1440 },
  { label: "Last 7 days", value: 10080 },
  { label: "All retained", value: 0 },
];
const rangeMinutes = ref(60);

const lines = ref<LogLine[] | null>(null);
const error = ref<string | null>(null);
const loading = ref(false);
const liveTail = ref(false);

const columns = "timestamp · source · project · environment · build · pod · container · stream · message";

async function run() {
  loading.value = true;
  // The expression is part of the address, so a query can be linked to.
  void router.replace({ query: where.value === "1 = 1" ? {} : { where: where.value } });
  try {
    const since =
      rangeMinutes.value > 0 ? new Date(Date.now() - rangeMinutes.value * 60000).toISOString() : undefined;
    lines.value = await api.logs(where.value, { limit: limit.value, since });
    error.value = null;
  } catch (err) {
    if (err instanceof APIError && err.status === 401) {
      void router.push({ name: "login", query: { returnTo: route.fullPath } });
      return;
    }
    error.value = err instanceof Error ? err.message : String(err);
    lines.value = null;
  } finally {
    loading.value = false;
  }
}

onMounted(run);
usePoll(() => void run(), 5000, () => liveTail.value && !loading.value);

// Facets are counted client-side over the returned page: honest about what
// they are, and free.
const facets = computed(() => {
  const count = (pick: (line: LogLine) => string) => {
    const counts = new Map<string, number>();
    for (const line of lines.value ?? []) {
      const key = pick(line) || "—";
      counts.set(key, (counts.get(key) ?? 0) + 1);
    }
    return [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 8);
  };
  return {
    stream: count((l) => l.stream),
    source: count((l) => (l.environment ? `${l.environment}` : l.build ? `${l.build}` : l.source)),
  };
});

function addClause(clause: string) {
  where.value = where.value.trim() === "1 = 1" || !where.value.trim() ? clause : `${where.value} AND ${clause}`;
  void run();
}

function time(line: LogLine): string {
  const date = new Date(line.timestamp);
  return Number.isNaN(date.getTime()) ? "—" : date.toLocaleTimeString("en-GB");
}
</script>

<template>
  <div class="space-y-5">
    <div class="flex items-center justify-between gap-4 flex-wrap">
      <div>
        <h1 class="text-xl font-semibold text-highlighted">Observability</h1>
        <p class="text-xs text-muted mt-1">
          ClickHouse, queried as ClickHouse: a boolean expression over the
          <span class="font-mono">logs</span> table, run read-only.
        </p>
      </div>
      <div class="flex items-center gap-2">
        <USelect v-model="rangeMinutes" :items="ranges" size="sm" class="w-44" />
        <UButton
          size="sm"
          :color="liveTail ? 'success' : 'neutral'"
          :variant="liveTail ? 'soft' : 'subtle'"
          @click="liveTail = !liveTail"
        >
          <StatusDot :tone="liveTail ? 'success' : 'neutral'" :pulse="liveTail" class="mr-1" /> Live tail
        </UButton>
      </div>
    </div>

    <div class="flex items-stretch gap-2">
      <div
        class="flex-1 flex items-center gap-2 rounded-md border border-default bg-muted px-3 focus-within:border-accented"
      >
        <span class="font-mono text-xs text-info select-none">where</span>
        <input
          v-model="where"
          class="flex-1 bg-transparent py-2 font-mono text-sm text-highlighted outline-none placeholder:text-dimmed"
          :placeholder="`project = 'shop' AND stream = 'stderr'`"
          spellcheck="false"
          @keydown.enter="run"
        />
      </div>
      <USelect v-model="limit" :items="limits" size="sm" class="w-24" />
      <UButton icon="i-lucide-play" :loading="loading" @click="run">Run</UButton>
    </div>
    <p class="text-[11px] text-dimmed font-mono -mt-3">columns: {{ columns }}</p>

    <UAlert
      v-if="error"
      color="error"
      variant="soft"
      icon="i-lucide-triangle-alert"
      title="ClickHouse refused the query"
      :description="error"
      class="[&_p]:font-mono [&_p]:text-xs"
    />

    <div class="flex gap-4 items-start">
      <div class="flex-1 min-w-0 rounded-md border border-default bg-muted font-mono text-xs leading-5 overflow-auto max-h-[36rem]">
        <div v-if="!lines?.length" class="px-3 py-6 text-center text-muted">
          {{ loading ? "Running…" : "No lines match the expression in this window." }}
        </div>
        <table v-else class="w-full">
          <tbody>
            <tr v-for="(line, i) in lines" :key="i" class="hover:bg-elevated/50 align-top">
              <td class="px-3 py-0.5 text-dimmed whitespace-nowrap select-none">{{ time(line) }}</td>
              <td class="px-2 py-0.5 whitespace-nowrap" :class="line.stream === 'stderr' ? 'text-error' : 'text-muted'">
                {{ line.environment || line.build || line.source }}
              </td>
              <td
                class="px-2 py-0.5 whitespace-pre-wrap break-all w-full"
                :class="line.stream === 'stderr' ? 'text-error' : 'text-toned'"
              >
                {{ line.message }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>

      <aside v-if="lines?.length" class="w-56 shrink-0 space-y-4 text-xs">
        <div>
          <p class="text-muted mb-1.5">Stream</p>
          <button
            v-for="[value, count] in facets.stream"
            :key="value"
            class="flex items-center justify-between w-full px-2 py-1 rounded hover:bg-elevated text-left"
            @click="addClause(`stream = '${value}'`)"
          >
            <span class="font-mono" :class="value === 'stderr' ? 'text-error' : 'text-toned'">{{ value }}</span>
            <span class="text-dimmed font-mono">{{ count }}</span>
          </button>
        </div>
        <div>
          <p class="text-muted mb-1.5">Source</p>
          <div
            v-for="[value, count] in facets.source"
            :key="value"
            class="flex items-center justify-between px-2 py-1"
          >
            <span class="font-mono text-toned truncate" :title="value">{{ value }}</span>
            <span class="text-dimmed font-mono ml-2">{{ count }}</span>
          </div>
        </div>
        <p class="text-dimmed leading-relaxed">Counts are over the returned page, not the whole table.</p>
      </aside>
    </div>
  </div>
</template>
