<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import { api, type TopologyEdge, type TopologyNode } from "../lib/api";
import { environmentLink } from "../lib/links";
import { useFreshness } from "../lib/freshness";
import { useAsync, usePoll } from "../lib/useAsync";
import {
  EDGE_LABELS,
  KIND_ICONS,
  KIND_LABELS,
  NODE_HEIGHT,
  NODE_WIDTH,
  assemble,
  edgePath,
  flowLabel,
  flowOf,
  flowParticles,
  flowSeconds,
  flowTone,
  flowWidth,
  flowsByEdge,
  layout,
  lineage,
  quietBindings,
  rate,
  subtitle,
  type DrawnEdge,
  type Flow,
} from "../lib/topology";
import PageHeader from "../components/PageHeader.vue";
import PageSection from "../components/PageSection.vue";
import PhaseBadge from "../components/PhaseBadge.vue";

// The architecture overview (docs/spikes/service-topology-2026-09.md,
// "Topology"): every project the reader can see, what it is made of, and what
// it depends on — the declared graph off GET /topology — with the platform's
// observed traffic off GET /topology/traffic flowing along it.
//
// It is a Fleet screen because the question is across projects: a binding has
// two ends, and "who depends on this" is never answered from inside one
// project. Everything on it is the reader's own or named by something of
// theirs, and none of it is the cluster's vocabulary: an environment, not what
// runs it; a resource, not where its provider put it.
//
// The address is the state — `?project=`, `?window=`, `?previews=1` — so a
// link to "shop's dependencies, live" is a link somebody else can open.

const route = useRoute();
const router = useRouter();
const freshness = useFreshness();

function queryString(name: string): string {
  const value = route.query[name];
  return typeof value === "string" ? value : "";
}

const project = computed(() => queryString("project"));
const windowMinutes = computed(() => {
  const asked = queryString("window");
  if (asked === "") return 5;
  const minutes = Number(asked);
  return Number.isFinite(minutes) && minutes >= 0 ? minutes : 5;
});
const previews = computed(() => queryString("previews") === "1");

function setQuery(changes: Record<string, string | undefined>) {
  const query = { ...route.query };
  for (const [key, value] of Object.entries(changes)) {
    if (value === undefined || value === "") delete query[key];
    else query[key] = value;
  }
  void router.replace({ query });
}

const windows = [
  { label: "Traffic off", value: 0 },
  { label: "Last minute", value: 1 },
  { label: "Last 5 minutes", value: 5 },
  { label: "Last 15 minutes", value: 15 },
  { label: "Last hour", value: 60 },
];

const graph = useAsync(() => api.topology(project.value || undefined));
const traffic = useAsync(() =>
  windowMinutes.value > 0
    ? api.topologyTraffic({
        project: project.value || undefined,
        since: new Date(Date.now() - windowMinutes.value * 60000).toISOString(),
      })
    : Promise.resolve(null),
);
// The graph changes when somebody changes an object; the traffic changes all
// the time. Five seconds is as live as the store gets — the collector ships
// in batches — and fast enough that the particles are showing now.
usePoll(() => void graph.refresh(), 30000, () => true);
usePoll(() => void traffic.refresh(), 5000, () => windowMinutes.value > 0);
watch(project, () => {
  selected.value = null;
  void graph.refresh();
  void traffic.refresh();
});
watch(windowMinutes, () => void traffic.refresh());

// The select cannot carry an empty value, so "no project" has a name of its
// own that no project can have — a project name is a DNS label.
const ALL_PROJECTS = "*";

// The projects the reader can pick from: their own, not the foreign names.
const projectItems = computed(() => {
  const names = new Set<string>();
  for (const node of graph.data.value?.nodes ?? []) {
    if (node.kind === "project" && !node.foreign) names.add(node.name);
  }
  if (project.value) names.add(project.value);
  return [{ label: "All projects", value: ALL_PROJECTS }, ...[...names].sort().map((name) => ({ label: name, value: name }))];
});

const diagram = computed(() =>
  graph.data.value ? assemble(graph.data.value, traffic.data.value, { previews: previews.value }) : null,
);
const placement = computed(() => (diagram.value ? layout(diagram.value.nodes, diagram.value.edges) : null));
const flows = computed(() => flowsByEdge(traffic.data.value));
const previewCount = computed(
  () => graph.data.value?.nodes.filter((node) => node.kind === "environment" && node.type === "preview").length ?? 0,
);

// ── Selection ─────────────────────────────────────────────────────────────
const selected = ref<string | null>(null);
const lit = computed(() => (selected.value && diagram.value ? lineage(selected.value, diagram.value.edges) : null));
const selectedNode = computed<TopologyNode | null>(
  () => diagram.value?.nodes.find((node) => node.id === selected.value) ?? null,
);
function toggle(id: string) {
  selected.value = selected.value === id ? null : id;
}
function dimmed(id: string): boolean {
  return lit.value !== null && !lit.value.has(id);
}
function edgeDimmed(edge: DrawnEdge): boolean {
  return lit.value !== null && !(lit.value.has(edge.from) && lit.value.has(edge.to));
}

// ── Drawing ───────────────────────────────────────────────────────────────
const zoom = ref(1);
const canvas = ref<HTMLElement | null>(null);
// The first drawing is fitted to the width it has, so the whole architecture
// is on screen before anybody reaches for the zoom; after that the zoom is
// the reader's, and a poll that adds a box does not take it back.
let fitted = false;
watch(placement, (drawn) => {
  if (fitted || !drawn || !canvas.value) return;
  fitted = true;
  const room = canvas.value.clientWidth - 2;
  if (room > 0 && drawn.width > room) zoom.value = Math.max(0.7, Math.floor((room / drawn.width) * 20) / 20);
});
function zoomBy(step: number) {
  zoom.value = Math.min(1.5, Math.max(0.4, Math.round((zoom.value + step) * 20) / 20));
}

// Particles are the part of the screen that moves on its own, so a reader who
// has asked the system for less motion gets the rates and the colours and no
// travelling dots.
const reducedMotion =
  typeof window !== "undefined" && window.matchMedia?.("(prefers-reduced-motion: reduce)").matches === true;

interface Line {
  edge: DrawnEdge;
  key: string;
  path: string;
  flow: Flow | null;
  /** How the line is drawn: in effect, waiting, or observed with nothing
   * declaring it. */
  style: "declared" | "waiting" | "undeclared" | "incidental";
  labelX: number;
  labelY: number;
}

const lines = computed<Line[]>(() => {
  const placed = placement.value?.placed;
  if (!placed || !diagram.value) return [];
  return diagram.value.edges.flatMap((edge, index) => {
    const from = placed.get(edge.from);
    const to = placed.get(edge.to);
    if (!from || !to) return [];
    let style: Line["style"] = "declared";
    let flow: Flow | null = null;
    if (edge.declared) {
      style = edge.declared.state ? "waiting" : "declared";
      flow = flows.value.get(edge.declared.id) ?? null;
    } else if (edge.observed) {
      const status = edge.observed.status;
      style = status === "undeclared" ? "undeclared" : status === "declared" ? "declared" : "incidental";
      flow = flowOf(edge.observed);
    }
    return [
      {
        edge,
        key: `topology-edge-${index}`,
        path: edgePath(from, to),
        flow,
        style,
        labelX: (from.x + NODE_WIDTH + to.x) / 2,
        labelY: (from.y + to.y + NODE_HEIGHT) / 2 - 6,
      },
    ];
  });
});

const TONE_STROKE = { error: "stroke-error", warning: "stroke-warning", primary: "stroke-primary" } as const;
const TONE_FILL = { error: "fill-error", warning: "fill-warning", primary: "fill-primary" } as const;

function baseStroke(line: Line): string {
  switch (line.style) {
    case "waiting":
      return "stroke-info";
    case "undeclared":
      return "stroke-warning";
    default:
      return "stroke-[var(--ui-text-dimmed)]";
  }
}

function dash(line: Line): string | undefined {
  if (line.style === "waiting") return "2 4";
  if (line.style === "undeclared") return "6 4";
  if (line.style === "incidental") return "3 3";
  return undefined;
}

function lineTitle(line: Line): string {
  const name = (id: string) => nameOf(id);
  const verb = line.edge.declared ? EDGE_LABELS[line.edge.declared.kind] : "called";
  const parts = [`${name(line.edge.from)} ${verb} ${name(line.edge.to)}`];
  if (line.edge.declared?.state) parts.push(line.edge.declared.reason || line.edge.declared.state);
  if (line.style === "undeclared") parts.push("nothing declares this call");
  if (line.flow && line.flow.flows > 0) parts.push(flowLabel(line.flow));
  return parts.join(" — ");
}

function nameOf(id: string): string {
  const node = diagram.value?.nodes.find((candidate) => candidate.id === id);
  if (!node) return id;
  return node.kind === "offering" ? `${node.project}/${node.name}` : node.name;
}

// ── What the diagram says in words ────────────────────────────────────────
const undeclared = computed(() => (traffic.data.value?.edges ?? []).filter((edge) => edge.status === "undeclared"));
const quiet = computed<TopologyEdge[]>(() =>
  graph.data.value && traffic.data.value ? quietBindings(graph.data.value, flows.value) : [],
);
const totalRate = computed(() =>
  (traffic.data.value?.edges ?? []).filter((edge) => edge.from === "internet").reduce((sum, edge) => sum + edge.rps, 0),
);

interface Neighbour {
  id: string;
  edge: DrawnEdge;
  verb: string;
  flow: Flow | null;
}

const neighbours = computed<{ out: Neighbour[]; in: Neighbour[] }>(() => {
  const id = selected.value;
  const result = { out: [] as Neighbour[], in: [] as Neighbour[] };
  if (!id) return result;
  for (const line of lines.value) {
    const verb = line.edge.declared
      ? EDGE_LABELS[line.edge.declared.kind]
      : line.style === "undeclared"
        ? "calls, undeclared"
        : "calls";
    if (line.edge.from === id) result.out.push({ id: line.edge.to, edge: line.edge, verb, flow: line.flow });
    if (line.edge.to === id) result.in.push({ id: line.edge.from, edge: line.edge, verb, flow: line.flow });
  }
  return result;
});

const error = computed(() => graph.error.value);
// A telemetry store that is not there is a fact about the installation, not a
// failure of this screen: the declared half still stands on its own.
const trafficNote = computed(() =>
  traffic.error.value ? "No flow data reaches this screen — what is drawn is what is declared." : null,
);
</script>

<template>
  <div class="space-y-6">
    <PageHeader :freshness="freshness" title="Architecture">
      <template #description>
        What the projects you can see are made of and what each one depends on — declared by the objects themselves,
        with the traffic the platform observed flowing along it.
      </template>
      <template #actions>
        <UButton
          icon="i-lucide-refresh-cw"
          color="neutral"
          variant="ghost"
          size="sm"
          aria-label="Refresh"
          :loading="graph.loading.value"
          @click="
            graph.refresh();
            traffic.refresh();
          "
        />
      </template>
    </PageHeader>

    <div class="flex items-center gap-2 flex-wrap">
      <USelect
        :model-value="project || ALL_PROJECTS"
        :items="projectItems"
        size="sm"
        class="w-44"
        aria-label="Project"
        @update:model-value="(value: string) => setQuery({ project: value === ALL_PROJECTS ? undefined : value })"
      />
      <USelect
        :model-value="windowMinutes"
        :items="windows"
        size="sm"
        class="w-44"
        aria-label="Traffic window"
        @update:model-value="(value: number) => setQuery({ window: String(value) })"
      />
      <UButton
        v-if="previewCount"
        size="sm"
        icon="i-lucide-git-pull-request"
        :color="previews ? 'primary' : 'neutral'"
        :variant="previews ? 'soft' : 'subtle'"
        @click="setQuery({ previews: previews ? undefined : '1' })"
      >
        Previews · {{ previewCount }}
      </UButton>
      <span class="flex-1" />
      <span v-if="traffic.data.value" class="text-xs text-muted font-mono">
        {{ rate(totalRate) }} from the internet
      </span>
      <div class="flex items-center gap-1">
        <UButton icon="i-lucide-zoom-out" color="neutral" variant="ghost" size="sm" aria-label="Zoom out" @click="zoomBy(-0.1)" />
        <span class="text-xs text-muted font-mono w-10 text-center">{{ Math.round(zoom * 100) }}%</span>
        <UButton icon="i-lucide-zoom-in" color="neutral" variant="ghost" size="sm" aria-label="Zoom in" @click="zoomBy(0.1)" />
      </div>
    </div>

    <UAlert v-if="error" color="error" variant="soft" icon="i-lucide-triangle-alert" :title="error" />
    <p v-if="trafficNote && windowMinutes > 0" class="text-xs text-muted">{{ trafficNote }}</p>

    <div class="grid gap-6 lg:grid-cols-[minmax(0,1fr)_20rem]">
      <!-- The diagram. Boxes are elements over one drawing of the lines, so
           that a box is a button a keyboard can reach and the lines can move. -->
      <div
        ref="canvas"
        class="self-start rounded-md border border-default bg-muted/30 overflow-auto max-h-[72vh] min-h-64"
      >
        <div
          v-if="!diagram || !placement || !diagram.nodes.length"
          class="px-6 py-14 text-center text-sm text-muted space-y-2"
        >
          <p>{{ graph.loading.value && !graph.data.value ? "Loading…" : "Nothing to draw yet." }}</p>
          <p v-if="graph.data.value" class="text-xs text-dimmed max-w-xl mx-auto">
            No project you can see has an environment, a resource or an offering yet — each one appears here as soon as
            it exists.
          </p>
        </div>
        <div
          v-else
          :style="{ width: `${placement.width * zoom}px`, height: `${placement.height * zoom}px` }"
          class="relative"
        >
          <div
            class="absolute left-0 top-0 origin-top-left"
            :style="{ width: `${placement.width}px`, height: `${placement.height}px`, transform: `scale(${zoom})` }"
          >
            <svg :width="placement.width" :height="placement.height" class="absolute left-0 top-0" aria-hidden="true">
              <g v-for="line in lines" :key="line.key" class="transition-opacity" :class="{ 'opacity-15': edgeDimmed(line.edge) }">
                <path
                  :id="line.key"
                  :d="line.path"
                  fill="none"
                  :stroke-width="line.style === 'undeclared' ? 1.75 : 1.25"
                  :stroke-dasharray="dash(line)"
                  :class="baseStroke(line)"
                >
                  <title>{{ lineTitle(line) }}</title>
                </path>
                <template v-if="line.flow && line.flow.rps > 0">
                  <path
                    :d="line.path"
                    fill="none"
                    stroke-linecap="round"
                    :stroke-width="flowWidth(line.flow.rps)"
                    class="topology-flow opacity-60"
                    :class="[TONE_STROKE[flowTone(line.flow)], { 'topology-still': reducedMotion }]"
                    :style="{ animationDuration: `${flowSeconds(line.flow.rps)}s` }"
                  >
                    <title>{{ lineTitle(line) }}</title>
                  </path>
                  <template v-if="!reducedMotion">
                    <circle
                      v-for="particle in flowParticles(line.flow.rps)"
                      :key="particle"
                      r="2.75"
                      :class="TONE_FILL[flowTone(line.flow)]"
                    >
                      <animateMotion
                        :dur="`${flowSeconds(line.flow.rps) * 3}s`"
                        :begin="`-${((particle - 1) * flowSeconds(line.flow.rps) * 3) / flowParticles(line.flow.rps)}s`"
                        repeatCount="indefinite"
                      >
                        <mpath :href="`#${line.key}`" />
                      </animateMotion>
                    </circle>
                  </template>
                  <text
                    :x="line.labelX"
                    :y="line.labelY"
                    text-anchor="middle"
                    class="fill-[var(--ui-text-muted)] text-[9.5px] font-mono"
                  >
                    {{ rate(line.flow.rps) }}
                  </text>
                </template>
              </g>
            </svg>

            <button
              v-for="spot in placement.placed.values()"
              :key="spot.node.id"
              type="button"
              class="absolute rounded-md border text-left px-2.5 py-1.5 transition-opacity bg-default hover:border-accented focus-visible:outline-2 focus-visible:outline-primary"
              :class="[
                selected === spot.node.id ? 'border-primary ring-1 ring-primary' : 'border-default',
                { 'opacity-30': dimmed(spot.node.id), 'border-dashed': spot.node.foreign },
              ]"
              :style="{ left: `${spot.x}px`, top: `${spot.y}px`, width: `${NODE_WIDTH}px`, height: `${NODE_HEIGHT}px` }"
              :aria-label="`${KIND_LABELS[spot.node.kind]} ${spot.node.name}`"
              :aria-pressed="selected === spot.node.id"
              @click="toggle(spot.node.id)"
            >
              <span class="flex items-center gap-1.5 min-w-0">
                <UIcon :name="KIND_ICONS[spot.node.kind]" class="size-3.5 shrink-0 text-muted" />
                <span class="truncate text-xs font-medium font-mono text-highlighted">{{ spot.node.name }}</span>
                <span
                  v-if="spot.node.phase && spot.node.kind === 'environment'"
                  class="ml-auto size-1.5 shrink-0 rounded-full"
                  :class="{
                    'bg-success': spot.node.phase === 'Live',
                    'bg-error': spot.node.phase === 'Degraded',
                    'bg-warning': spot.node.phase === 'Deploying',
                    'bg-accented': !['Live', 'Degraded', 'Deploying'].includes(spot.node.phase),
                  }"
                />
              </span>
              <span class="block truncate text-[10.5px] text-dimmed mt-0.5">{{ subtitle(spot.node) }}</span>
            </button>
          </div>
        </div>
      </div>

      <!-- What the selected box is, and everything touching it. -->
      <aside class="rounded-md border border-default p-4 space-y-4 self-start text-sm">
        <template v-if="selectedNode">
          <div class="space-y-1">
            <p class="text-xs text-muted flex items-center gap-1.5">
              <UIcon :name="KIND_ICONS[selectedNode.kind]" class="size-3.5" />
              {{ KIND_LABELS[selectedNode.kind] }}
              <span v-if="selectedNode.project && selectedNode.kind !== 'project'">· {{ selectedNode.project }}</span>
            </p>
            <p class="font-mono text-highlighted break-all">{{ selectedNode.name }}</p>
            <p class="text-xs text-dimmed">{{ subtitle(selectedNode) }}</p>
          </div>

          <div class="flex items-center gap-2 flex-wrap">
            <PhaseBadge v-if="selectedNode.phase" :phase="selectedNode.phase" />
            <UBadge v-if="selectedNode.idle" color="neutral" variant="soft" size="sm">idle</UBadge>
            <UBadge v-if="selectedNode.foreign" color="neutral" variant="outline" size="sm">not yours</UBadge>
          </div>

          <p v-if="selectedNode.foreign" class="text-xs text-muted">
            A project you hold no role on, drawn because something of yours names it. Its name is all this screen
            knows about it.
          </p>

          <div v-if="selectedNode.processes?.length" class="space-y-1">
            <h3 class="text-xs font-medium text-muted">Processes</h3>
            <p v-for="process in selectedNode.processes" :key="process.name" class="text-xs font-mono text-toned">
              {{ process.name }} <span class="text-dimmed">{{ process.type }}</span>
            </p>
          </div>

          <div v-if="neighbours.out.length" class="space-y-1">
            <h3 class="text-xs font-medium text-muted">Depends on</h3>
            <button
              v-for="neighbour in neighbours.out"
              :key="neighbour.edge.id"
              type="button"
              class="w-full text-left text-xs rounded px-1.5 py-1 hover:bg-elevated/60"
              @click="selected = neighbour.id"
            >
              <span class="text-dimmed">{{ neighbour.verb }}</span>
              <span class="font-mono text-toned ml-1">{{ nameOf(neighbour.id) }}</span>
              <span v-if="neighbour.edge.declared?.state" class="block text-info">
                {{ neighbour.edge.declared.reason || neighbour.edge.declared.state }}
              </span>
              <span v-if="neighbour.flow && neighbour.flow.flows" class="block font-mono text-muted">
                {{ flowLabel(neighbour.flow) }}
              </span>
            </button>
          </div>
          <div v-if="neighbours.in.length" class="space-y-1">
            <h3 class="text-xs font-medium text-muted">Depended on by</h3>
            <button
              v-for="neighbour in neighbours.in"
              :key="neighbour.edge.id"
              type="button"
              class="w-full text-left text-xs rounded px-1.5 py-1 hover:bg-elevated/60"
              @click="selected = neighbour.id"
            >
              <span class="font-mono text-toned">{{ nameOf(neighbour.id) }}</span>
              <span class="text-dimmed ml-1">{{ neighbour.verb }}</span>
              <span v-if="neighbour.flow && neighbour.flow.flows" class="block font-mono text-muted">
                {{ flowLabel(neighbour.flow) }}
              </span>
            </button>
          </div>

          <div class="flex flex-wrap gap-2 pt-1">
            <UButton
              v-if="selectedNode.kind === 'environment' && selectedNode.project"
              :to="environmentLink(selectedNode.name, selectedNode.project)"
              size="xs"
              color="neutral"
              variant="subtle"
              icon="i-lucide-arrow-up-right"
            >
              Open environment
            </UButton>
            <UButton
              v-if="selectedNode.project && !selectedNode.foreign"
              :to="{ name: 'project', params: { name: selectedNode.project } }"
              size="xs"
              color="neutral"
              variant="subtle"
              icon="i-lucide-folder"
            >
              Open project
            </UButton>
            <UButton
              v-if="selectedNode.url"
              :to="selectedNode.url"
              target="_blank"
              size="xs"
              color="neutral"
              variant="subtle"
              icon="i-lucide-external-link"
            >
              Visit
            </UButton>
          </div>
        </template>
        <template v-else>
          <p class="text-xs text-muted">
            Pick a box to see what it depends on and what depends on it — everything upstream and downstream of it stays
            lit.
          </p>
          <div class="space-y-2 text-xs text-muted">
            <h3 class="text-xs font-medium text-muted">Reading the lines</h3>
            <p class="flex items-center gap-2">
              <svg width="36" height="8" aria-hidden="true"><line x1="0" y1="4" x2="36" y2="4" class="stroke-[var(--ui-text-dimmed)]" stroke-width="1.5" /></svg>
              declared: a route, a binding, a resource in use
            </p>
            <p class="flex items-center gap-2">
              <svg width="36" height="8" aria-hidden="true">
                <line x1="0" y1="4" x2="36" y2="4" class="stroke-primary topology-flow" stroke-width="3" />
              </svg>
              traffic along it, thicker when busier
            </p>
            <p class="flex items-center gap-2">
              <svg width="36" height="8" aria-hidden="true">
                <line x1="0" y1="4" x2="36" y2="4" class="stroke-info" stroke-width="1.5" stroke-dasharray="2 4" />
              </svg>
              waiting: a binding not yet granted or resolved
            </p>
            <p class="flex items-center gap-2">
              <svg width="36" height="8" aria-hidden="true">
                <line x1="0" y1="4" x2="36" y2="4" class="stroke-warning" stroke-width="1.75" stroke-dasharray="6 4" />
              </svg>
              undeclared: a call nothing says may happen
            </p>
            <p class="flex items-center gap-2">
              <svg width="36" height="8" aria-hidden="true">
                <line
                  x1="0"
                  y1="4"
                  x2="36"
                  y2="4"
                  class="stroke-[var(--ui-text-dimmed)]"
                  stroke-width="1.5"
                  stroke-dasharray="3 3"
                />
              </svg>
              through the platform, or off it
            </p>
          </div>
        </template>
      </aside>
    </div>

    <PageSection
      v-if="traffic.data.value"
      title="Undeclared calls"
      description="Calls between projects in this window that no binding explains. Today they work; they are what breaks the day the platform enforces its bindings."
    >
      <p v-if="!undeclared.length" class="text-xs text-dimmed">
        Every call between projects in this window runs along a declared binding.
      </p>
      <div v-else class="rounded-md border border-default overflow-x-auto">
        <table class="w-full min-w-[36rem] text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-default bg-muted">
              <th class="px-3 py-1 font-medium">From</th>
              <th class="px-3 py-1 font-medium">To</th>
              <th class="px-3 py-1 font-medium text-right">Rate</th>
              <th class="px-3 py-1 font-medium text-right">5xx</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="edge in undeclared"
              :key="`${edge.from}->${edge.to}`"
              class="border-b border-muted last:border-0 hover:bg-elevated/40 cursor-pointer"
              @click="selected = edge.from"
            >
              <td class="px-3 py-1 font-mono text-xs text-toned">{{ nameOf(edge.from) }}</td>
              <td class="px-3 py-1 font-mono text-xs text-toned">{{ nameOf(edge.to) }}</td>
              <td class="px-3 py-1 text-right font-mono text-xs text-toned">{{ rate(edge.rps) }}</td>
              <td class="px-3 py-1 text-right font-mono text-xs" :class="edge.errors ? 'text-error' : 'text-dimmed'">
                {{ edge.errors || "—" }}
              </td>
            </tr>
          </tbody>
        </table>
      </div>
    </PageSection>

    <PageSection
      v-if="traffic.data.value && quiet.length"
      title="Quiet bindings"
      description="Bindings nothing used in this window. A binding nobody calls is a dependency that could be removed — or a caller that has stopped working."
    >
      <div class="rounded-md border border-default overflow-x-auto">
        <table class="w-full min-w-[36rem] text-sm">
          <thead>
            <tr class="text-left text-xs text-muted border-b border-default bg-muted">
              <th class="px-3 py-1 font-medium">Consumer</th>
              <th class="px-3 py-1 font-medium">Offering</th>
            </tr>
          </thead>
          <tbody>
            <tr
              v-for="edge in quiet"
              :key="edge.id"
              class="border-b border-muted last:border-0 hover:bg-elevated/40 cursor-pointer"
              @click="selected = edge.from"
            >
              <td class="px-3 py-1 font-mono text-xs text-toned">{{ nameOf(edge.from) }}</td>
              <td class="px-3 py-1 font-mono text-xs text-toned">{{ nameOf(edge.to) }}</td>
            </tr>
          </tbody>
        </table>
      </div>
    </PageSection>
  </div>
</template>

<style scoped>
/* The live half: dashes that travel the way the traffic does. Their speed is
   set per line from its rate; a reader who asked for less motion keeps the
   thickness and the colour and loses the travel. */
.topology-flow {
  stroke-dasharray: 4 10;
  animation: topology-flow 1s linear infinite;
}
.topology-still {
  animation: none;
}
@keyframes topology-flow {
  to {
    stroke-dashoffset: -14;
  }
}
@media (prefers-reduced-motion: reduce) {
  .topology-flow {
    animation: none;
  }
}
</style>
