/**
 * The design guide, as a test.
 *
 * `docs/UI.md` is the guide; this is the half of it a machine can hold. The
 * dashboard drifted the way UIs drift — not by anybody deciding differently,
 * but by twenty-two screens each guessing at a shape nobody had written down —
 * and a written guide alone would have drifted the same way, one screen at a
 * time, with every step defensible.
 *
 * So the rules that can be checked are checked, on the same principle the CLI
 * checks that every command names a real route: the moment a rule stops being
 * enforced it stops being true, and a rule that is not true is worse than no
 * rule at all, because the next person reads it and believes it.
 *
 * What is *not* here matters as much. Nothing below has an opinion about what
 * a screen says, how it is laid out inside its sections, or which chart it
 * draws — those are judgement, and a test that pretended to make them would
 * only be in the way. What is here is the frame: one page width, one rhythm,
 * one header, one heading scale, one table, one palette, and the mode rule.
 */

import { parse } from "@vue/compiler-sfc";
import { describe, expect, it } from "vitest";

// The sources themselves, pulled in by the bundler rather than read off the
// disk: it keeps this test to the same module graph as everything else here,
// and means the dashboard needs no Node type declarations to typecheck.
const viewSources = import.meta.glob("../views/*.vue", { query: "?raw", import: "default", eager: true }) as Record<
  string,
  string
>;
const componentSources = import.meta.glob("../components/*.vue", {
  query: "?raw",
  import: "default",
  eager: true,
}) as Record<string, string>;

/**
 * Screens that are not pages: they render outside the shell's content column
 * and so own their own frame. Sign-in and the OAuth round trip happen before
 * there is a shell at all, and the 404 is a sentence in the middle of one.
 */
const STANDALONE = new Set(["LoginView.vue", "AuthCallbackView.vue", "NotFoundView.vue"]);

/**
 * The operator's own screens: the platform section, connections, and the
 * volumes written for storage the platform did not create. The mode
 * gate is per *screen*, not per block — a screen is the developer's or the
 * operator's — so these are wholly operator content and nothing inside them is
 * gated a second time. `docs/UI.md`, "The mode rule", is why.
 */
const OPERATOR_VIEWS = new Set([
  "PlatformView.vue",
  "PlatformAddonsView.vue",
  "PlatformNodesView.vue",
  "PlatformWorkloadsView.vue",
  "PlatformEdgeView.vue",
  "PlatformStorageView.vue",
  "PlatformEventsView.vue",
  "PlatformBackupView.vue",
  "PlatformAuditView.vue",
  "PlatformSettingsView.vue",
  "ConnectionsView.vue",
  "VolumesView.vue",
]);

/**
 * Components that are only ever mounted on an operator screen, or inside an
 * `<OperatorOnly>` on a developer one, *and* that speak the operator's
 * vocabulary in their own text.
 *
 * It is deliberately the shortest list that works rather than every component
 * an operator screen happens to use: a component that is clean today is
 * checked, and stays clean. Adding one means saying where its gate is.
 */
const OPERATOR_COMPONENTS = new Set([
  // Both are inside PlatformUpdatePanel, on the platform's settings screen.
  "ComponentChecklist.vue",
  "UpdateFlight.vue",
  // Also on the settings screen, and the one place the dashboard mentions
  // kubectl on purpose: what an operator cannot change from here.
  "OperatorsPanel.vue",
]);

/** The one page width a view may declare for itself: a form column. Anything
 * else is the shell's decision, made once in `AppShell.vue`. */
const FORM_WIDTH = "max-w-3xl";

/** The three table densities, and the one exception: a row that says the table
 * is empty is a paragraph in a table's clothing and is spaced like one. */
const CELL_PADDING_Y = new Set(["py-2", "py-1", "py-0.5"]);
const EMPTY_ROW_PADDING_Y = "py-8";

/**
 * The Kubernetes nouns. A developer screen never prints one — not because they
 * are secret (the API decides that, and it decides it by role) but because
 * they are the wrong answer to every question a developer is asking. See
 * docs/SCOPE.md: "the developer should never need the words namespace or
 * Deployment".
 *
 * They are matched as whole words in what a person actually reads: text, and
 * the attributes that become text. An expression like `pod.name` is not on the
 * list — a field name is not a label — so what this catches is a screen
 * *saying* Pod, which is exactly what leaks.
 */
const OPERATOR_WORDS = [
  "pod",
  "pods",
  "node",
  "nodes",
  "namespace",
  "namespaces",
  "cluster",
  "clusters",
  "kubernetes",
  "kubectl",
  "kubelet",
  "manifest",
  "manifests",
  "statefulset",
  "daemonset",
  "replicaset",
  "configmap",
  "etcd",
  // The storage nouns. A volume claim may bind one that already exists
  // (#346), which puts the operator's whole vocabulary within reach of a
  // developer's form — so a screen offering that says "storage" and names
  // the object only behind the gate.
  "persistentvolume",
  "persistentvolumes",
  "persistentvolumeclaim",
  "persistentvolumeclaims",
  "storageclass",
  "storageclasses",
  "pvc",
  "pvcs",
];

/**
 * Phrases that contain one of those words and are not about Kubernetes at all.
 * Each is exact, and each says why — an allowlist without reasons becomes a
 * place to put anything that fails.
 */
const NOT_ABOUT_KUBERNETES = [
  // Log *clustering* — grouping like lines into patterns, which is the word
  // the literature uses and the one the button has to say.
  "No lines to cluster in this window.",
  // A map box for something the flows reached that this platform does not run.
  "off the platform",
];

/** Attributes whose value is read by a person rather than by the browser. */
const HUMAN_ATTRIBUTES = new Set([
  "title",
  "aria-label",
  "placeholder",
  "label",
  "description",
  "empty",
  "hint",
  "alt",
]);

/** Tailwind's own palette. The dashboard's colours are the tokens in
 * `assets/main.css`, so that a change of palette is one file. */
const PALETTE = /\b(?:text|bg|border|fill|stroke|ring|from|via|to)-(?:red|orange|amber|yellow|lime|green|emerald|teal|cyan|sky|blue|indigo|violet|purple|fuchsia|pink|rose|slate|gray|zinc|stone)-\d{2,3}\b/;

interface Node {
  type: number;
  tag?: string;
  content?: string | { content?: string };
  props?: Prop[];
  children?: Node[];
}
interface Prop {
  type: number;
  name: string;
  value?: { content: string };
  arg?: { content: string };
  exp?: { content: string };
}

const ELEMENT = 1;
const TEXT = 2;
const ATTRIBUTE = 6;
const DIRECTIVE = 7;

/** Every `.vue` file in one directory, as `{ name, source }`, name-ordered. */
function sources(glob: Record<string, string>): { name: string; source: string }[] {
  return Object.entries(glob)
    .map(([path, source]) => ({ name: path.slice(path.lastIndexOf("/") + 1), source }))
    .sort((a, b) => a.name.localeCompare(b.name));
}

const views = sources(viewSources);
const components = sources(componentSources);
const everything = [...views, ...components];

function templateOf(file: { name: string; source: string }): Node | undefined {
  const { descriptor } = parse(file.source, { filename: file.name });
  return descriptor.template?.ast as unknown as Node | undefined;
}

/** Static `class` on an element, as a list of Tailwind tokens. */
function classes(node: Node): string[] {
  const prop = node.props?.find((p) => p.type === ATTRIBUTE && p.name === "class");
  return prop?.value?.content.split(/\s+/).filter(Boolean) ?? [];
}

/** Every element in the tree, deepest last, each with the ancestors above it. */
function walk(node: Node | undefined, visit: (node: Node, ancestors: Node[]) => void, ancestors: Node[] = []): void {
  if (!node) return;
  if (node.type === ELEMENT) visit(node, ancestors);
  const next = node.type === ELEMENT ? [...ancestors, node] : ancestors;
  for (const child of node.children ?? []) walk(child, visit, next);
}

/** The element children of a template root, skipping whitespace and comments. */
function elementChildren(node: Node | undefined): Node[] {
  return (node?.children ?? []).filter((c) => c.type === ELEMENT);
}

function textOf(node: Node): string {
  return typeof node.content === "string" ? node.content : (node.content?.content ?? "");
}

describe("the page frame", () => {
  it.each(views.filter((v) => !STANDALONE.has(v.name)))("$name is one page in the shell's column", (view) => {
    const roots = elementChildren(templateOf(view));
    expect(roots, `${view.name}: a view is one root element — the page`).toHaveLength(1);

    const cls = classes(roots[0]);
    expect(cls, `${view.name}: a page's sections are spaced by the page, not by each section`).toContain("space-y-6");

    // The shell caps the content column once, in AppShell.vue. A view that
    // caps itself again is a page of a different width from every other page,
    // which is how the dashboard came to have three.
    for (const width of cls.filter((c) => c.startsWith("max-w-"))) {
      expect(width, `${view.name}: a page is the shell's width, or ${FORM_WIDTH} if it is a form`).toBe(FORM_WIDTH);
    }
  });

  it.each(views.filter((v) => !STANDALONE.has(v.name)))("$name opens with a PageHeader", (view) => {
    const headers: Node[] = [];
    walk(templateOf(view), (node) => {
      if (node.tag === "PageHeader") headers.push(node);
    });
    expect(headers.length, `${view.name}: every page has a title, a sentence saying what it answers, and one shape`)
      .toBe(1);
  });

  it("no screen writes its own <h1>", () => {
    const offenders: string[] = [];
    for (const file of everything) {
      // PageHeader owns the page title; the three standalone screens render
      // outside the shell and have none — see STANDALONE.
      if (file.name === "PageHeader.vue" || STANDALONE.has(file.name)) continue;
      walk(templateOf(file), (node) => {
        if (node.tag === "h1") offenders.push(file.name);
      });
    }
    expect(offenders, "the page title belongs to PageHeader, so that every page has exactly one").toEqual([]);
  });
});

describe("the freshness control", () => {
  /** Whether an element binds a prop, statically or with `v-bind`. */
  function binds(node: Node, name: string): boolean {
    return (node.props ?? []).some(
      (p) =>
        (p.type === ATTRIBUTE && p.name === name) ||
        (p.type === DIRECTIVE && p.name === "bind" && p.arg?.content === name),
    );
  }

  // A screen that polls is a screen whose data moves on its own, and every one
  // of them owes the reader the same three things: how old this is, a way to
  // hold it still while they read, and an admission when a source has stopped
  // answering. `PageHeader` renders all three from the object; a view only has
  // to hand it over. docs/UI.md, "The freshness control", is the reasoning.
  const polled = views.filter((v) => !STANDALONE.has(v.name) && /\busePoll\(/.test(v.source));

  it("is on every screen that polls", () => {
    expect(polled.length, "the dashboard polls; if nothing does, this rule has lost its subject").toBeGreaterThan(0);
  });

  it.each(polled)("$name says how old it is", (view) => {
    const headers: Node[] = [];
    walk(templateOf(view), (node) => {
      if (node.tag === "PageHeader") headers.push(node);
    });
    for (const header of headers) {
      expect(
        binds(header, "freshness"),
        `${view.name}: a screen that polls hands PageHeader its freshness — :freshness="freshness" from useFreshness()`,
      ).toBe(true);
    }
    expect(
      /\buseFreshness\(/.test(view.source),
      `${view.name}: the object comes from useFreshness(), so that the panels inside the screen age it too`,
    ).toBe(true);
  });

  it("is placed by the header and nowhere else", () => {
    const offenders = everything
      .filter((file) => file.name !== "PageHeader.vue" && file.name !== "FreshnessControl.vue")
      .filter((file) => {
        let found = false;
        walk(templateOf(file), (node) => {
          if (node.tag === "FreshnessControl") found = true;
        });
        return found;
      })
      .map((file) => file.name);
    expect(offenders, "one screen, one age, in the one place every screen puts it").toEqual([]);
  });
});

describe("the heading scale", () => {
  const SCALE: Record<string, { size: string; weight: string }> = {
    h2: { size: "text-sm", weight: "font-medium" },
    h3: { size: "text-xs", weight: "font-medium" },
  };

  it.each(everything)("$name keeps to it", (file) => {
    walk(templateOf(file), (node) => {
      const want = node.tag ? SCALE[node.tag] : undefined;
      if (!want) return;
      const cls = classes(node);
      expect(cls, `${file.name}: <${node.tag}> is ${want.size}`).toContain(want.size);
      expect(cls, `${file.name}: <${node.tag}> is ${want.weight}`).toContain(want.weight);
    });
  });
});

describe("tables", () => {
  it.each(everything)("$name uses the one table", (file) => {
    const name = file.name;
    walk(templateOf(file), (table, ancestors) => {
      if (table.tag !== "table") return;

      // A table is the one thing on these screens that is reliably wider than
      // the column it is in — a commit subject, a phase, a duration and a time
      // — and a table that says so with `min-w-*` will push the whole page
      // sideways unless something holds it. A table with no minimum shrinks to
      // fit and needs nothing.
      if (classes(table).some((c) => c.startsWith("min-w-"))) {
        const scrolls = ancestors.some((a) =>
          classes(a).some((c) => c === "overflow-x-auto" || c === "overflow-auto"),
        );
        expect(scrolls, `${name}: a table with a minimum width scrolls in its own box, never the page`).toBe(true);
      }

      const densities = new Set<string>();
      const gutters = new Set<string>();
      walk(table, (cell, within) => {
        if (cell.tag !== "th" && cell.tag !== "td") return;
        // A table inside an expanded row is its own table, with its own shape
        // and its own density. Only this one's cells are this one's.
        if (within.some((a) => a !== table && a.tag === "table")) return;
        const cls = classes(cell);

        // Two table shapes, and a table is one or the other throughout. A
        // **boxed** table draws its own edge, so every cell carries the `px-3`
        // gutter. A **flush** table sits inside a block that is already padded
        // — the conditions under a node, the evidence under a requirement —
        // and takes its left edge from that block, so no cell sets a left
        // gutter and the columns line up with the prose above them. Mixing the
        // two puts one column half a gutter out.
        for (const token of cls.filter((c) => c.startsWith("px-"))) {
          expect(token, `${name}: the gutter is px-3`).toBe("px-3");
        }
        gutters.add(cls.some((c) => c === "px-3" || c.startsWith("pl-")) ? "boxed" : "flush");

        // A cell that spans the table is not a cell in a column — it is the
        // "nothing here" line, or a row expanded into a block — so it is
        // spaced as a block and does not set the table's density.
        if ((cell.props ?? []).some((prop) => prop.name === "colspan")) return;

        const y = cls.filter((c) => c.startsWith("py-"));
        expect(y.length, `${name}: a cell has exactly one vertical padding`).toBe(1);
        if (y[0] === EMPTY_ROW_PADDING_Y) return;
        expect(CELL_PADDING_Y, `${name}: ${y[0]} is not one of the three densities`).toContain(y[0]);
        densities.add(y[0]);
      });

      expect(
        densities.size,
        `${name}: one table is one density — a header at ${[...densities][0]} over a body at ${[...densities][1]} is the drift this rule exists for`,
      ).toBeLessThanOrEqual(1);
      expect(gutters.size, `${name}: a table is boxed or flush throughout, never half of each`).toBeLessThanOrEqual(1);
    });
  });
});

describe("the palette", () => {
  it.each(everything)("$name takes its colours from the tokens", (file) => {
    const template = file.source.slice(file.source.indexOf("<template>"));
    const found = template.match(new RegExp(PALETTE, "g"));
    expect(found, `${file.name}: colours are the tokens in assets/main.css, not Tailwind's palette`).toBeNull();
  });
});

describe("the mode rule", () => {
  /** Whether an element gates its subtree on operator mode. */
  function gates(node: Node): boolean {
    if (node.tag === "OperatorOnly") return true;
    return (node.props ?? []).some(
      (p) =>
        p.type === DIRECTIVE &&
        (p.name === "if" || p.name === "show") &&
        (p.exp?.content ?? "").includes("operatorMode"),
    );
  }

  /** Every operator word a piece of rendered text says. */
  function operatorWordsIn(text: string): string[] {
    let rest = text;
    for (const phrase of NOT_ABOUT_KUBERNETES) rest = rest.split(phrase).join(" ");
    const words = new Set(rest.toLowerCase().match(/[a-z]+/g) ?? []);
    return OPERATOR_WORDS.filter((w) => words.has(w));
  }

  const developerScreens = [
    ...views.filter((v) => !OPERATOR_VIEWS.has(v.name) && !STANDALONE.has(v.name)),
    ...components.filter((c) => !OPERATOR_COMPONENTS.has(c.name)),
  ];

  it.each(developerScreens)("$name says nothing about Kubernetes outside an operator gate", (file) => {
    const leaks: string[] = [];
    const check = (text: string, gated: boolean) => {
      if (gated) return;
      const words = operatorWordsIn(text);
      if (words.length) leaks.push(`${words.join(", ")} — in ${JSON.stringify(text.trim().slice(0, 80))}`);
    };

    walk(templateOf(file), (node, ancestors) => {
      const gated = gates(node) || ancestors.some(gates);
      for (const prop of node.props ?? []) {
        if (prop.type === ATTRIBUTE && HUMAN_ATTRIBUTES.has(prop.name) && prop.value) {
          check(prop.value.content, gated);
        }
      }
      for (const child of node.children ?? []) {
        if (child.type === TEXT) check(textOf(child), gated);
      }
    });

    expect(
      leaks,
      `${file.name}: role decides what is permitted, mode decides what is rendered — wrap this in <OperatorOnly> (docs/UI.md)`,
    ).toEqual([]);
  });
});
