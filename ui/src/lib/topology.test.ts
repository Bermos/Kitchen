import { describe, expect, it } from "vitest";
import type { TopologyGraph, TopologyTraffic } from "./api";
import {
  assemble,
  flowLabel,
  flowSeconds,
  flowTone,
  flowWidth,
  flowsByEdge,
  layout,
  lineage,
  quietBindings,
  rate,
  subtitle,
} from "./topology";

const graph: TopologyGraph = {
  nodes: [
    { id: "internet", kind: "internet", name: "Internet" },
    { id: "project/shop", kind: "project", name: "shop", project: "shop" },
    { id: "project/pricing", kind: "project", name: "pricing", project: "pricing", foreign: true },
    {
      id: "environment/shop-production",
      kind: "environment",
      name: "shop-production",
      project: "shop",
      type: "production",
      phase: "Live",
    },
    {
      id: "environment/shop-pr-7",
      kind: "environment",
      name: "shop-pr-7",
      project: "shop",
      type: "preview",
      phase: "Live",
    },
    { id: "resource/shop-db", kind: "resource", name: "shop-db", project: "shop", type: "postgres", phase: "Bound" },
    { id: "provider/neon", kind: "provider", name: "neon", type: "neon" },
    { id: "offering/pricing/api", kind: "offering", name: "api", project: "pricing", foreign: true, type: "http" },
  ],
  edges: [
    {
      id: "routes:internet->environment/shop-production",
      kind: "routes",
      from: "internet",
      to: "environment/shop-production",
    },
    { id: "routes:internet->environment/shop-pr-7", kind: "routes", from: "internet", to: "environment/shop-pr-7" },
    {
      id: "uses:environment/shop-production->resource/shop-db",
      kind: "uses",
      from: "environment/shop-production",
      to: "resource/shop-db",
    },
    {
      id: "uses:environment/shop-pr-7->resource/shop-db",
      kind: "uses",
      from: "environment/shop-pr-7",
      to: "resource/shop-db",
    },
    {
      id: "providedBy:resource/shop-db->provider/neon",
      kind: "providedBy",
      from: "resource/shop-db",
      to: "provider/neon",
    },
    {
      id: "consumes:environment/shop-production->offering/pricing/api",
      kind: "consumes",
      from: "environment/shop-production",
      to: "offering/pricing/api",
    },
    {
      id: "serves:offering/pricing/api->project/pricing",
      kind: "serves",
      from: "offering/pricing/api",
      to: "project/pricing",
    },
  ],
};

const traffic: TopologyTraffic = {
  since: "2026-09-25T10:00:00Z",
  until: "2026-09-25T10:05:00Z",
  nodes: [
    { id: "project/blog", kind: "project", name: "blog", project: "blog", foreign: true },
    { id: "external/api.stripe.com", kind: "external", name: "api.stripe.com" },
  ],
  edges: [
    {
      from: "internet",
      to: "environment/shop-production",
      protocol: "HTTP",
      flows: 600,
      rps: 2,
      errors: 0,
      drops: 0,
      p95Ms: 40,
      status: "declared",
      along: ["routes:internet->environment/shop-production"],
    },
    {
      from: "project/blog",
      to: "environment/shop-production",
      protocol: "HTTP",
      flows: 10,
      rps: 0.03,
      errors: 2,
      drops: 0,
      p95Ms: 12,
      status: "undeclared",
    },
  ],
};

describe("assemble", () => {
  it("leaves previews out unless asked, with their edges", () => {
    const diagram = assemble(graph, null, { previews: false });
    expect(diagram.nodes.map((n) => n.id)).not.toContain("environment/shop-pr-7");
    expect(diagram.edges.some((e) => e.from === "environment/shop-pr-7" || e.to === "environment/shop-pr-7")).toBe(
      false,
    );
    const everything = assemble(graph, null, { previews: true });
    expect(everything.nodes.map((n) => n.id)).toContain("environment/shop-pr-7");
  });

  it("draws a project as a box only when an edge needs it", () => {
    const ids = assemble(graph, null, { previews: false }).nodes.map((n) => n.id);
    // shop has boxes of its own, so its name is the group, not a box
    expect(ids).not.toContain("project/shop");
    // pricing is foreign and answers an offering, so it is a box
    expect(ids).toContain("project/pricing");
  });

  it("adds the observed pairs nothing declares, and the boxes only they reach", () => {
    const diagram = assemble(graph, traffic, { previews: false });
    const observed = diagram.edges.filter((e) => e.observed);
    expect(observed.map((e) => e.from)).toEqual(["project/blog"]);
    const ids = diagram.nodes.map((n) => n.id);
    expect(ids).toContain("project/blog");
    // reached by nothing in this reading, so not drawn
    expect(ids).not.toContain("external/api.stripe.com");
  });

  it("draws a declared pair on its own when a focus left its declared edges out", () => {
    const focused = { nodes: graph.nodes, edges: graph.edges.filter((e) => e.kind !== "routes") };
    const diagram = assemble(focused, traffic, { previews: false });
    expect(diagram.edges.some((e) => e.observed?.status === "declared" && e.from === "internet")).toBe(true);
    // and not when the edges are there to carry it
    const whole = assemble(graph, traffic, { previews: false });
    expect(whole.edges.some((e) => e.observed?.status === "declared")).toBe(false);
  });
});

describe("layout", () => {
  it("draws every edge left to right when there is no cycle", () => {
    const diagram = assemble(graph, traffic, { previews: true });
    const { placed } = layout(diagram.nodes, diagram.edges);
    for (const edge of diagram.edges) {
      const from = placed.get(edge.from)!;
      const to = placed.get(edge.to)!;
      expect(from.layer, `${edge.from} -> ${edge.to}`).toBeLessThan(to.layer);
    }
  });

  it("puts a source right in front of what it points at", () => {
    const diagram = assemble(graph, traffic, { previews: false });
    const { placed } = layout(diagram.nodes, diagram.edges);
    expect(placed.get("project/blog")!.layer).toBe(placed.get("environment/shop-production")!.layer - 1);
  });

  it("survives a cycle", () => {
    const nodes = [
      { id: "a", kind: "environment" as const, name: "a" },
      { id: "b", kind: "environment" as const, name: "b" },
    ];
    const { placed } = layout(nodes, [
      { from: "a", to: "b" },
      { from: "b", to: "a" },
    ]);
    expect(placed.size).toBe(2);
    expect(placed.get("a")!.layer).not.toBe(placed.get("b")!.layer);
  });

  it("never stacks two boxes on each other", () => {
    const diagram = assemble(graph, traffic, { previews: true });
    const boxes = [...layout(diagram.nodes, diagram.edges).placed.values()];
    const spots = new Set(boxes.map((b) => `${b.x},${b.y}`));
    expect(spots.size).toBe(boxes.length);
  });
});

describe("flows", () => {
  it("credits a pair to every declared edge it runs along", () => {
    const flows = flowsByEdge(traffic);
    expect(flows.get("routes:internet->environment/shop-production")?.rps).toBe(2);
    expect(flows.size).toBe(1);
  });

  it("finds the bindings nothing used", () => {
    const quiet = quietBindings(graph, flowsByEdge(traffic));
    expect(quiet.map((e) => e.id)).toEqual(["consumes:environment/shop-production->offering/pricing/api"]);
  });

  it("moves busier edges faster and draws them thicker", () => {
    expect(flowSeconds(100)).toBeLessThan(flowSeconds(1));
    expect(flowSeconds(0)).toBeLessThanOrEqual(3);
    expect(flowWidth(100)).toBeGreaterThan(flowWidth(1));
  });

  it("colours by the share of failures, not their count", () => {
    const base = { rps: 1, p95Ms: 0, protocol: "HTTP", drops: 0 };
    expect(flowTone({ ...base, flows: 1000, errors: 1 })).toBe("warning");
    expect(flowTone({ ...base, flows: 10, errors: 2 })).toBe("error");
    expect(flowTone({ ...base, flows: 10, errors: 0 })).toBe("primary");
  });

  it("says a rate in few characters", () => {
    expect(rate(0)).toBe("0/s");
    expect(rate(0.001)).toBe("<0.01/s");
    expect(rate(0.25)).toBe("0.25/s");
    expect(rate(3.14)).toBe("3.1/s");
    expect(rate(42.4)).toBe("42/s");
    expect(flowLabel({ rps: 2, flows: 10, errors: 1, drops: 0, p95Ms: 40.2, protocol: "HTTP" })).toBe(
      "2.0/s · p95 40ms · 1 5xx",
    );
  });
});

describe("lineage", () => {
  it("is everything upstream and downstream, and nothing beside", () => {
    const seen = lineage("resource/shop-db", graph.edges);
    expect(seen.has("provider/neon")).toBe(true);
    expect(seen.has("environment/shop-production")).toBe(true);
    expect(seen.has("internet")).toBe(true);
    // shop-production's *other* dependency is not upstream of the database
    expect(seen.has("offering/pricing/api")).toBe(false);
  });
});

describe("subtitle", () => {
  it("names what a box is without the cluster's words", () => {
    expect(subtitle(graph.nodes[3])).toBe("production · Live");
    expect(subtitle(graph.nodes[5])).toBe("postgres");
    expect(subtitle(graph.nodes[2])).toBe("another team's project");
  });
});
