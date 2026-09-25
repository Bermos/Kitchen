import type { ObservedEdge, TopologyEdge, TopologyGraph, TopologyNode, TopologyTraffic } from "./api";

/**
 * The architecture overview's arithmetic: which boxes are on the diagram,
 * where each one goes, and what the observed traffic says about each edge.
 *
 * It is here rather than in the view so that it can be tested without a
 * browser, and because the layout is the one part of the screen that is a
 * real algorithm rather than markup: a layered drawing, left to right, in the
 * direction every edge already points — from what would break to what it
 * would break on, which is also the way a request travels.
 */

export const NODE_WIDTH = 180;
export const NODE_HEIGHT = 52;
const LAYER_GAP = 76;
const ROW_GAP = 14;
/** Extra room between two projects' boxes in one column, so the grouping
 * reads without drawing a frame around it. */
const GROUP_GAP = 12;
const PADDING = 24;

type Kind = TopologyNode["kind"];

export const KIND_LABELS: Record<Kind, string> = {
  project: "Project",
  environment: "Environment",
  offering: "Offering",
  resource: "Resource",
  provider: "Connection",
  domain: "Domain",
  internet: "Internet",
  platform: "Platform",
  external: "External service",
};

export const KIND_ICONS: Record<Kind, string> = {
  project: "i-lucide-folder",
  environment: "i-lucide-layers",
  offering: "i-lucide-plug-zap",
  resource: "i-lucide-database",
  provider: "i-lucide-cable",
  domain: "i-lucide-link",
  internet: "i-lucide-globe",
  platform: "i-lucide-shield",
  external: "i-lucide-cloud",
};

export const EDGE_LABELS: Record<TopologyEdge["kind"], string> = {
  routes: "routes to",
  consumes: "binds",
  serves: "is answered by",
  uses: "uses",
  providedBy: "is provided by",
};

/**
 * The column a box goes in when nothing connects it to anything: roughly
 * where it would be if something did.
 */
const RESTING_LAYER: Record<Kind, number> = {
  internet: 0,
  domain: 1,
  platform: 1,
  project: 2,
  environment: 2,
  offering: 3,
  resource: 4,
  external: 4,
  provider: 5,
};

/** What goes under a box's name. */
export function subtitle(node: TopologyNode): string {
  const parts: string[] = [];
  switch (node.kind) {
    case "environment":
      parts.push(node.type ?? "environment");
      if (node.phase) parts.push(node.phase);
      if (node.idle) parts.push("idle");
      break;
    case "resource":
      parts.push(node.type ?? "resource");
      if (node.phase && node.phase !== "Bound") parts.push(node.phase);
      break;
    case "offering":
      parts.push(node.project ?? "", node.type ?? "http");
      if (node.phase) parts.push(node.phase);
      break;
    case "provider":
      parts.push(node.type ? `${node.type} connection` : "connection");
      break;
    case "domain":
      parts.push(node.phase ?? "domain");
      break;
    case "project":
      parts.push(node.foreign ? "another team's project" : "project");
      break;
    case "internet":
      parts.push("visitors");
      break;
    case "platform":
      parts.push("gateway, gates and interceptors");
      break;
    case "external":
      parts.push("off the platform");
      break;
  }
  return parts.filter(Boolean).join(" · ");
}

/** An edge the diagram draws: declared, or observed with nothing declaring it. */
export interface DrawnEdge {
  id: string;
  from: string;
  to: string;
  declared?: TopologyEdge;
  observed?: ObservedEdge;
}

/** What is on the diagram, before anything is placed. */
export interface Diagram {
  nodes: TopologyNode[];
  edges: DrawnEdge[];
}

export interface AssembleOptions {
  /** Whether preview environments are drawn. There can be dozens, and each
   * repeats its production's edges. */
  previews: boolean;
}

/**
 * The boxes and lines of one reading: the declared graph, with whatever the
 * traffic reached that it does not have, and the observed pairs no declared
 * edge explains.
 */
export function assemble(graph: TopologyGraph, traffic: TopologyTraffic | null, options: AssembleOptions): Diagram {
  const nodes = new Map<string, TopologyNode>();
  for (const node of graph.nodes) nodes.set(node.id, node);
  for (const node of traffic?.nodes ?? []) if (!nodes.has(node.id)) nodes.set(node.id, node);

  const hidden = new Set<string>();
  if (!options.previews) {
    for (const node of nodes.values()) {
      if (node.kind === "environment" && node.type === "preview") hidden.add(node.id);
    }
  }

  const edges: DrawnEdge[] = [];
  for (const edge of graph.edges) {
    if (hidden.has(edge.from) || hidden.has(edge.to)) continue;
    edges.push({ id: edge.id, from: edge.from, to: edge.to, declared: edge });
  }
  // A declared pair is drawn by the edges it runs along — unless a focus on
  // one project left those out, in which case it is drawn on its own.
  const declaredIDs = new Set(graph.edges.map((edge) => edge.id));
  for (const edge of traffic?.edges ?? []) {
    if (edge.status === "declared" && (edge.along ?? []).every((id) => declaredIDs.has(id))) continue;
    if (hidden.has(edge.from) || hidden.has(edge.to)) continue;
    edges.push({ id: observedID(edge), from: edge.from, to: edge.to, observed: edge });
  }

  // A project's own box is its group label when it has anything else on the
  // diagram, and a box of its own only when an edge needs it — a foreign
  // project, or one with nothing deployed yet.
  const connected = new Set(edges.flatMap((edge) => [edge.from, edge.to]));
  const members = new Set<string>();
  for (const node of nodes.values()) if (node.kind !== "project" && node.project) members.add(node.project);
  // A box only the traffic reached is drawn only while something reaches it.
  const extra = new Set((traffic?.nodes ?? []).map((node) => node.id));

  const kept = [...nodes.values()].filter((node) => {
    if (hidden.has(node.id)) return false;
    if (connected.has(node.id)) return true;
    if (extra.has(node.id) && !graph.nodes.some((declared) => declared.id === node.id)) return false;
    if (node.kind === "project") return !members.has(node.name);
    if (node.kind === "domain") return false;
    return true;
  });
  return { nodes: kept, edges };
}

export function observedID(edge: ObservedEdge): string {
  return `observed:${edge.from}->${edge.to}`;
}

/** One box, placed. */
export interface Placed {
  node: TopologyNode;
  layer: number;
  x: number;
  y: number;
}

export interface Layout {
  placed: Map<string, Placed>;
  width: number;
  height: number;
}

/** The order boxes start in inside one column, before crossings are reduced:
 * a project's boxes together, and within a project by kind and then name. */
function initialKey(node: TopologyNode): string {
  return `${node.project ?? "~"}\u0000${RESTING_LAYER[node.kind]}\u0000${node.name}`;
}

/**
 * A layered drawing: longest path from the sources for the column, then a few
 * barycentre sweeps for the order inside each column.
 *
 * Cycles are real — two projects can bind each other's offerings — so the
 * edges that close one are left out of the layering (and still drawn).
 */
export function layout(nodes: TopologyNode[], edges: { from: string; to: string }[]): Layout {
  const ids = nodes.map((node) => node.id).sort();
  const byID = new Map(nodes.map((node) => [node.id, node]));
  const out = new Map<string, string[]>(ids.map((id) => [id, []]));
  const into = new Map<string, string[]>(ids.map((id) => [id, []]));
  for (const edge of edges) {
    if (!byID.has(edge.from) || !byID.has(edge.to) || edge.from === edge.to) continue;
    out.get(edge.from)!.push(edge.to);
    into.get(edge.to)!.push(edge.from);
  }

  // Break cycles: a depth-first walk in a fixed order, keeping the edges that
  // do not point back at something still on the stack.
  const state = new Map<string, 1 | 2>();
  const dag = new Map<string, string[]>(ids.map((id) => [id, []]));
  const visit = (id: string) => {
    state.set(id, 1);
    for (const next of out.get(id)!) {
      if (state.get(next) === 1) continue;
      dag.get(id)!.push(next);
      if (!state.has(next)) visit(next);
    }
    state.set(id, 2);
  };
  for (const id of ids) if (!state.has(id)) visit(id);

  // Longest path from the sources, in topological order.
  const indegree = new Map(ids.map((id) => [id, 0]));
  for (const targets of dag.values()) for (const to of targets) indegree.set(to, indegree.get(to)! + 1);
  const queue = ids.filter((id) => indegree.get(id) === 0);
  const order: string[] = [];
  const layer = new Map<string, number>(ids.map((id) => [id, 0]));
  while (queue.length) {
    const id = queue.shift()!;
    order.push(id);
    for (const to of dag.get(id)!) {
      layer.set(to, Math.max(layer.get(to)!, layer.get(id)! + 1));
      indegree.set(to, indegree.get(to)! - 1);
      if (indegree.get(to) === 0) queue.push(to);
    }
  }
  // A source sits right in front of what it points at rather than at the far
  // left, so a foreign project binding an offering is drawn beside it.
  for (const id of [...order].reverse()) {
    const hasInto = [...dag.values()].some((targets) => targets.includes(id));
    const targets = dag.get(id)!;
    if (!hasInto && targets.length) layer.set(id, Math.min(...targets.map((to) => layer.get(to)!)) - 1);
  }
  // A box nothing connects rests in the column its kind usually lands in.
  for (const id of ids) {
    if (!out.get(id)!.length && !into.get(id)!.length) layer.set(id, RESTING_LAYER[byID.get(id)!.kind]);
  }
  const lowest = Math.min(0, ...layer.values());
  for (const id of ids) layer.set(id, layer.get(id)! - lowest);

  // Columns, then crossings.
  const columns: string[][] = [];
  for (const id of ids) {
    const at = layer.get(id)!;
    (columns[at] ??= []).push(id);
  }
  for (let i = 0; i < columns.length; i++) columns[i] ??= [];
  for (const column of columns) {
    column.sort((a, b) => initialKey(byID.get(a)!).localeCompare(initialKey(byID.get(b)!)));
  }
  const position = new Map<string, number>();
  const reindex = () => columns.forEach((column) => column.forEach((id, index) => position.set(id, index)));
  reindex();
  const sweep = (column: string[], neighbours: Map<string, string[]>) => {
    const centre = new Map<string, number>();
    for (const id of column) {
      const near = neighbours.get(id)!.filter((other) => position.has(other));
      centre.set(
        id,
        near.length ? near.reduce((sum, other) => sum + position.get(other)!, 0) / near.length : position.get(id)!,
      );
    }
    column.sort((a, b) => centre.get(a)! - centre.get(b)! || position.get(a)! - position.get(b)!);
  };
  for (let pass = 0; pass < 4; pass++) {
    for (let i = 1; i < columns.length; i++) {
      sweep(columns[i], into);
      reindex();
    }
    for (let i = columns.length - 2; i >= 0; i--) {
      sweep(columns[i], out);
      reindex();
    }
  }

  // Coordinates: each column centred on the tallest.
  const heightOf = (column: string[]) => {
    let height = 0;
    column.forEach((id, index) => {
      if (index > 0) {
        height += ROW_GAP;
        if (byID.get(column[index - 1])!.project !== byID.get(id)!.project) height += GROUP_GAP;
      }
      height += NODE_HEIGHT;
    });
    return height;
  };
  const tallest = Math.max(NODE_HEIGHT, ...columns.map(heightOf));
  const placed = new Map<string, Placed>();
  columns.forEach((column, index) => {
    let y = PADDING + (tallest - heightOf(column)) / 2;
    column.forEach((id, row) => {
      if (row > 0) {
        y += ROW_GAP;
        if (byID.get(column[row - 1])!.project !== byID.get(id)!.project) y += GROUP_GAP;
      }
      placed.set(id, { node: byID.get(id)!, layer: index, x: PADDING + index * (NODE_WIDTH + LAYER_GAP), y });
      y += NODE_HEIGHT;
    });
  });

  return {
    placed,
    width: PADDING * 2 + Math.max(1, columns.length) * NODE_WIDTH + Math.max(0, columns.length - 1) * LAYER_GAP,
    height: PADDING * 2 + tallest,
  };
}

/** The curve between two placed boxes: out of the right side of one, into
 * the left side of the other. */
export function edgePath(from: Placed, to: Placed): string {
  const x1 = from.x + NODE_WIDTH;
  const y1 = from.y + NODE_HEIGHT / 2;
  const x2 = to.x;
  const y2 = to.y + NODE_HEIGHT / 2;
  const bend = Math.max(48, Math.abs(x2 - x1) / 2);
  return `M ${x1} ${y1} C ${x1 + bend} ${y1}, ${x2 - bend} ${y2}, ${x2} ${y2}`;
}

/** The traffic running along one declared edge in the window. */
export interface Flow {
  rps: number;
  flows: number;
  errors: number;
  drops: number;
  p95Ms: number;
  protocol: string;
}

/** Every declared edge's share of the observed traffic. A pair that runs
 * over two edges — a binding, then the environment answering it — counts on
 * both. */
export function flowsByEdge(traffic: TopologyTraffic | null): Map<string, Flow> {
  const flows = new Map<string, Flow>();
  for (const edge of traffic?.edges ?? []) {
    for (const id of edge.along ?? []) {
      const flow = flows.get(id) ?? { rps: 0, flows: 0, errors: 0, drops: 0, p95Ms: 0, protocol: edge.protocol };
      flow.rps += edge.rps;
      flow.flows += edge.flows;
      flow.errors += edge.errors;
      flow.drops += edge.drops;
      flow.p95Ms = Math.max(flow.p95Ms, edge.p95Ms);
      if (edge.protocol === "HTTP") flow.protocol = "HTTP";
      flows.set(id, flow);
    }
  }
  return flows;
}

/** The observed edge's own numbers, as a Flow. */
export function flowOf(edge: ObservedEdge): Flow {
  return {
    rps: edge.rps,
    flows: edge.flows,
    errors: edge.errors,
    drops: edge.drops,
    p95Ms: edge.p95Ms,
    protocol: edge.protocol,
  };
}

/** How long one dash takes to travel the edge: busier edges move faster. */
export function flowSeconds(rps: number): number {
  const speed = 1 + Math.log10(1 + Math.max(0, rps) * 10);
  return Math.round(Math.min(3, Math.max(0.35, 3 / speed)) * 100) / 100;
}

/** How thick a busy edge is drawn. */
export function flowWidth(rps: number): number {
  return Math.round((1.5 + Math.min(4, Math.log10(1 + Math.max(0, rps)) * 1.8)) * 10) / 10;
}

/** How many particles travel an edge at once: one for a trickle, up to three
 * for a flood. */
export function flowParticles(rps: number): number {
  if (rps >= 50) return 3;
  if (rps >= 5) return 2;
  return 1;
}

export type FlowTone = "error" | "warning" | "primary";

/** A flow's colour: failing answers above one in twenty, anything dropped,
 * or healthy. */
export function flowTone(flow: Flow): FlowTone {
  if (flow.flows > 0 && flow.errors / flow.flows > 0.05) return "error";
  if (flow.drops > 0 || flow.errors > 0) return "warning";
  return "primary";
}

/** A rate, in the fewest characters that still say it. */
export function rate(rps: number): string {
  if (rps <= 0) return "0/s";
  if (rps < 0.01) return "<0.01/s";
  if (rps >= 10) return `${Math.round(rps)}/s`;
  return `${rps.toFixed(rps >= 1 ? 1 : 2)}/s`;
}

/** One line about a flow: its rate, then whatever is worth knowing. */
export function flowLabel(flow: Flow): string {
  const parts = [rate(flow.rps)];
  if (flow.protocol === "HTTP" && flow.p95Ms > 0) parts.push(`p95 ${Math.round(flow.p95Ms)}ms`);
  if (flow.errors > 0) parts.push(`${flow.errors} 5xx`);
  if (flow.drops > 0) parts.push(`${flow.drops} dropped`);
  return parts.join(" · ");
}

/**
 * Everything upstream and downstream of one box — what it depends on,
 * transitively, and what depends on it. Selecting a box lights this up, which
 * is the blast-radius question the diagram exists to answer.
 */
export function lineage(id: string, edges: { from: string; to: string }[]): Set<string> {
  const seen = new Set<string>([id]);
  const walk = (start: string, forward: boolean) => {
    const stack = [start];
    const visited = new Set<string>([start]);
    while (stack.length) {
      const current = stack.pop()!;
      for (const edge of edges) {
        const [here, there] = forward ? [edge.from, edge.to] : [edge.to, edge.from];
        if (here !== current || visited.has(there)) continue;
        visited.add(there);
        seen.add(there);
        stack.push(there);
      }
    }
  };
  walk(id, true);
  walk(id, false);
  return seen;
}

/**
 * Bindings nothing used in the window: the spike's dead edges, which are the
 * one kind of blast radius anybody actually deletes. Only a binding in effect
 * can be quiet — one waiting on a grant was never going to carry anything.
 */
export function quietBindings(graph: TopologyGraph, flows: Map<string, Flow>): TopologyEdge[] {
  return graph.edges.filter((edge) => edge.kind === "consumes" && !edge.state && !(flows.get(edge.id)?.flows ?? 0));
}
