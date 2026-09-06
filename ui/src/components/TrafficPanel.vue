<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { api, type TrafficEdge } from "../lib/api";
import { compactCount } from "../lib/format";
import { useAsync, usePoll } from "../lib/useAsync";

// One tab of a project's observability screen (#469). It was a cross-project
// screen with a project dropdown, which is the shape this dashboard is moving
// away from: the project is the address now, and this panel is handed it.
const props = defineProps<{ project: string }>();

// The traffic screen draws what the flow collector shipped: aggregated
// Hubble flow edges out of ClickHouse, via GET /api/v1/traffic. The map is a
// reading of the last window, not a live packet view — rates are averages
// over the window the store answered for.

const ranges = [
  { label: "Last 15 minutes", value: 15 },
  { label: "Last hour", value: 60 },
  { label: "Last 6 hours", value: 360 },
  { label: "Last 24 hours", value: 1440 },
];
const rangeMinutes = ref(60);
const mode = ref<"all" | "http" | "network">("all");
const dropsOnly = ref(false);

const { data, error, loading, refresh } = useAsync(() =>
  api.traffic({
    since: new Date(Date.now() - rangeMinutes.value * 60000).toISOString(),
    project: props.project || undefined,
  }),
);
// The screen's freshness is the host's — every fetch here reports into it
// through `useAsync`, and the one control lives in the one header.
usePoll(() => void refresh(), 15000, () => true);
watch([() => props.project, rangeMinutes], () => void refresh());
function rerun() {
  void refresh();
}

const edges = computed<TrafficEdge[]>(() => {
  let edges = data.value ?? [];
  if (mode.value === "http") edges = edges.filter((e) => e.protocol === "HTTP");
  if (mode.value === "network") edges = edges.filter((e) => e.protocol !== "HTTP");
  if (dropsOnly.value) edges = edges.filter((e) => e.drops > 0);
  return edges;
});

// The second line under a box on the map. The field is a namespace, which is
// the cluster's vocabulary and belongs in the Platform scope; what this screen
// needs from it is the one thing it says in a developer's — whether the other
// end of the edge is on this platform at all, which is exactly the case where
// it is missing.
function boxDetail(box: Node): string {
  return box.namespace ? "" : "off the platform";
}

// The map draws the busiest edges; the table below has all of them.
const mapLimit = 30;
const mapEdges = computed(() => edges.value.slice(0, mapLimit));

interface Node {
  key: string;
  name: string;
  namespace: string;
  column: number;
  x: number;
  y: number;
}

const nodeWidth = 168;
const nodeHeight = 40;
const columnX = [30, 316, 602];
const mapWidth = 800;

// Nodes settle into three columns by their role in the window: pure sources
// (the gateway, cloudflared) on the left, workloads that both receive and
// call in the middle, pure destinations (stores, the outside world) on the
// right. It is a heuristic, and an honest one — the platform's shape happens
// to be a left-to-right story.
const layout = computed(() => {
  const sources = new Set(mapEdges.value.map((e) => `${e.sourceNamespace ?? ""}/${e.source}`));
  const destinations = new Set(mapEdges.value.map((e) => `${e.destinationNamespace ?? ""}/${e.destination}`));
  const keys = [...new Set([...sources, ...destinations])];

  const nodes = new Map<string, Node>();
  const perColumn: number[] = [0, 0, 0];
  for (const key of keys) {
    const [namespace, ...rest] = key.split("/");
    const column = sources.has(key) && destinations.has(key) ? 1 : sources.has(key) ? 0 : 2;
    const node: Node = {
      key,
      name: rest.join("/"),
      namespace,
      column,
      x: columnX[column],
      y: 24 + perColumn[column] * (nodeHeight + 18),
    };
    perColumn[column] += 1;
    nodes.set(key, node);
  }
  const height = Math.max(...perColumn, 1) * (nodeHeight + 18) + 30;
  return { nodes, height };
});

interface DrawnEdge {
  edge: TrafficEdge;
  path: string;
  labelX: number;
  labelY: number;
  tone: string;
  dashed: boolean;
}

const drawnEdges = computed<DrawnEdge[]>(() => {
  const { nodes } = layout.value;
  return mapEdges.value.flatMap((edge) => {
    const from = nodes.get(`${edge.sourceNamespace ?? ""}/${edge.source}`);
    const to = nodes.get(`${edge.destinationNamespace ?? ""}/${edge.destination}`);
    if (!from || !to) return [];
    const x1 = from.x + nodeWidth;
    const y1 = from.y + nodeHeight / 2;
    const x2 = to.x;
    const y2 = to.y + nodeHeight / 2;
    const bend = Math.max(40, (x2 - x1) / 2);
    return [
      {
        edge,
        path: `M ${x1} ${y1} C ${x1 + bend} ${y1}, ${x2 - bend} ${y2}, ${x2} ${y2}`,
        labelX: (x1 + x2) / 2,
        labelY: (y1 + y2) / 2 - 5,
        tone: edge.drops > 0 ? "stroke-error" : edge.errors > 0 ? "stroke-warning" : "stroke-accented",
        dashed: edge.protocol !== "HTTP",
      },
    ];
  });
});

function edgeLabel(edge: TrafficEdge): string {
  const rate = edge.rps >= 10 ? Math.round(edge.rps).toString() : edge.rps.toFixed(edge.rps >= 0.1 ? 1 : 2);
  const parts = [`${rate}/s`];
  if (edge.protocol === "HTTP" && edge.p95Ms > 0) parts.push(`p95 ${Math.round(edge.p95Ms)}ms`);
  if (edge.errors > 0) parts.push(`${edge.errors} 5xx`);
  if (edge.drops > 0) parts.push(`${edge.drops} dropped`);
  return parts.join(" · ");
}
</script>

<template>
  <div class="space-y-6">
    <div class="flex items-start justify-between gap-3 flex-wrap">
      <p class="text-xs text-muted max-w-2xl">
        The service map, aggregated from Cilium's Hubble flows — one edge per talking pair in the window.
      </p>
      <USelect v-model="rangeMinutes" :items="ranges" size="sm" class="w-36 sm:w-44" @change="rerun" />
    </div>

    <div class="flex items-center gap-2 flex-wrap">
      <UButton
        v-for="chip in [
          { value: 'all' as const, label: 'All' },
          { value: 'http' as const, label: 'HTTP' },
          { value: 'network' as const, label: 'Network' },
        ]"
        :key="chip.value"
        size="xs"
        :color="mode === chip.value ? 'primary' : 'neutral'"
        :variant="mode === chip.value ? 'soft' : 'subtle'"
        @click="mode = chip.value"
      >
        {{ chip.label }}
      </UButton>
      <UButton
        size="xs"
        :color="dropsOnly ? 'error' : 'neutral'"
        :variant="dropsOnly ? 'soft' : 'subtle'"
        icon="i-lucide-shield-x"
        @click="dropsOnly = !dropsOnly"
      >
        Drops only
      </UButton>
      <span class="flex-1" />
      <span v-if="edges.length > mapLimit" class="text-[11px] text-dimmed">
        map shows the {{ mapLimit }} busiest of {{ edges.length }} edges — the table has all of them
      </span>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />

    <div
      v-if="!edges.length"
      class="rounded-md border border-default px-6 py-14 text-center text-sm text-muted space-y-2"
    >
      <p>{{ loading ? "Loading…" : dropsOnly ? "Nothing was dropped in this window." : "No flow data in this window." }}</p>
      <!-- An empty answer is an answer and says why. It used to say it twice:
           an instruction addressed to an operator, and a sentence for everyone
           else. The instruction is not something the reader of a project's
           screen can act on and naming what they cannot do is not telling them
           anything, so what is left is the fact (#469). -->
      <p v-if="!loading && !dropsOnly" class="text-xs text-dimmed max-w-xl mx-auto">
        No flow data reaches this project — the platform is not measuring flows yet, and until it is there is nothing
        for this map to draw.
      </p>
    </div>

    <template v-else>
      <!-- The map: sources → workloads → destinations, the busiest edges. -->
      <div class="rounded-md border border-default overflow-x-auto">
        <svg :viewBox="`0 0 ${mapWidth} ${layout.height}`" class="w-full min-w-[640px]" :style="{ maxHeight: '480px' }">
          <g v-for="drawn in drawnEdges" :key="`${drawn.edge.source}->${drawn.edge.destination}`">
            <path
              :d="drawn.path"
              fill="none"
              stroke-width="1.5"
              class="opacity-70"
              :class="drawn.tone"
              :stroke-dasharray="drawn.dashed ? '4 3' : undefined"
            />
            <text
              :x="drawn.labelX"
              :y="drawn.labelY"
              text-anchor="middle"
              class="fill-[var(--ui-text-muted)] text-[9px] font-mono"
            >
              {{ edgeLabel(drawn.edge) }}
            </text>
          </g>
          <g v-for="node in layout.nodes.values()" :key="node.key">
            <rect
              :x="node.x"
              :y="node.y"
              :width="nodeWidth"
              :height="nodeHeight"
              rx="6"
              class="fill-[var(--ui-bg-muted)] stroke-[var(--ui-border)]"
            />
            <text :x="node.x + 10" :y="node.y + 17" class="fill-[var(--ui-text-highlighted)] text-[10.5px] font-medium">
              {{ node.name.length > 26 ? `${node.name.slice(0, 25)}…` : node.name }}
            </text>
            <text :x="node.x + 10" :y="node.y + 31" class="fill-[var(--ui-text-dimmed)] text-[9px] font-mono">
              {{ boxDetail(node) }}
            </text>
          </g>
        </svg>
      </div>

      <!-- Every edge, as numbers. -->
      <div class="rounded-md border border-default overflow-x-auto">
        <table class="w-full min-w-[48rem] text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-default bg-muted">
              <th class="px-3 py-2 font-medium">Edge</th>
              <th class="px-3 py-2 font-medium">Protocol</th>
              <th class="px-3 py-2 font-medium text-right">Rate</th>
              <th class="px-3 py-2 font-medium text-right">Flows</th>
              <th class="px-3 py-2 font-medium text-right">5xx</th>
              <th class="px-3 py-2 font-medium text-right">Dropped</th>
              <th class="px-3 py-2 font-medium text-right">p95</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="edge in edges"
              :key="`${edge.sourceNamespace}/${edge.source}->${edge.destinationNamespace}/${edge.destination}`"
              class="border-b border-muted last:border-0 hover:bg-elevated/40"
            >
              <td class="px-3 py-2 font-mono text-xs">
                <span class="text-toned">{{ edge.source }}</span>
                <span class="text-dimmed mx-1.5">→</span>
                <span class="text-toned">{{ edge.destination }}</span>
              </td>
              <td class="px-3 py-2 text-xs text-muted">{{ edge.protocol }}</td>
              <td class="px-3 py-2 text-right font-mono text-xs text-toned">
                {{ edge.rps >= 10 ? Math.round(edge.rps) : edge.rps.toFixed(2) }}/s
              </td>
              <td class="px-3 py-2 text-right font-mono text-xs text-toned">{{ compactCount(edge.flows) }}</td>
              <td class="px-3 py-2 text-right font-mono text-xs" :class="edge.errors ? 'text-error' : 'text-dimmed'">
                {{ edge.errors || "—" }}
              </td>
              <td class="px-3 py-2 text-right font-mono text-xs" :class="edge.drops ? 'text-error' : 'text-dimmed'">
                {{ edge.drops || "—" }}
              </td>
              <td class="px-3 py-2 text-right font-mono text-xs text-toned">
                {{ edge.protocol === "HTTP" && edge.p95Ms > 0 ? `${Math.round(edge.p95Ms)} ms` : "—" }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </template>
  </div>
</template>
