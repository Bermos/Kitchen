/**
 * The addresses, and the ones that used to be.
 *
 * Half of this file is a regression test for links that are not in this
 * repository at all. `internal/signals/evidence.go` composes a finding's
 * evidence link out of these paths, the API serves it, and whoever the finding
 * reached clicks it — weeks later, from a chat message. So the old addresses
 * are not "nice to keep": they are the API's output, and the query on three of
 * them (`?section=`, `?node=`, `?namespace=&kind=&name=`) is the whole of what
 * the link is for.
 *
 * The redirect table this walks is the one in issue #469.
 */

import { createMemoryHistory, createRouter } from "vue-router";
import { describe, expect, it } from "vitest";
import { completedBy, routes, SCOPES } from "./routes";

// `resolve` matches without loading the lazy component, which is what keeps
// this a test of the table rather than of every screen in the dashboard.
const router = createRouter({ history: createMemoryHistory(), routes });

/** Where a path ends up: the address itself, or the one it redirects to. */
function follow(path: string): string {
  const resolved = router.resolve(path);
  const record = resolved.matched[resolved.matched.length - 1];
  const redirect = record?.redirect;
  if (typeof redirect !== "function") return resolved.fullPath;
  return router.resolve(redirect(resolved, router.currentRoute.value)).fullPath;
}

/** What the screen at a path is called, following redirects. */
function lands(path: string): string {
  return String(router.resolve(follow(path)).name);
}

describe("the table itself", () => {
  it("names a real screen everywhere", () => {
    // `screen()` throws at construction for a view that does not exist, so
    // this is really a test that the module loads — said out loud because a
    // typo in a file name is otherwise a blank page at runtime.
    expect(routes.length).toBeGreaterThan(20);
    for (const route of routes) {
      if ("redirect" in route) continue;
      expect(route.meta?.view, `${String(route.name)} names its view`).toMatch(/\.vue$/);
    }
  });

  it("gives every scope a root that opens something", () => {
    for (const scope of SCOPES) {
      expect(lands(scope.root), `${scope.id} opens`).not.toBe("not-found");
    }
  });

  it("puts every screen in a scope", () => {
    // The three that are not in one: the login round trip, which happens
    // before there is a shell, and the 404, which is a sentence inside one.
    const unscoped = routes
      .filter((route) => !("redirect" in route) && !route.meta?.scope)
      .map((route) => String(route.name));
    expect(unscoped.sort()).toEqual(["auth-callback", "login", "not-found"]);
  });
});

describe("the addresses that moved", () => {
  // Every row of #469's redirect table, in its order. The comment on each is
  // what breaks if it stops holding.
  const table: { from: string; to: string; why: string }[] = [
    // `EvidenceBuilds` — the build queue tile on the platform overview links
    // here too.
    { from: "/builds", to: "/deploys", why: "evidence: EvidenceBuilds" },
    { from: "/connections", to: "/platform/connections", why: "the operator's, and it was in the developer's nav" },
    // Not a move into a free address: /platform/storage was already the
    // operator's volume inventory, and is itself evidence.
    { from: "/volumes", to: "/platform/storage", why: "two inventories of one subject" },
    { from: "/platform/audit", to: "/compliance/audit", why: "the auditor is not necessarily an operator" },
    { from: "/settings", to: "/platform/settings", why: "where settings lived before /platform" },
    { from: "/compliance", to: "/compliance/audit", why: "the scope's root" },
  ];

  it.each(table)("$from → $to ($why)", ({ from, to }) => {
    expect(follow(from)).toBe(to);
  });

  it.each(table)("$from keeps its query", ({ from, to }) => {
    expect(follow(`${from}?node=node-b&kind=Pod`)).toBe(`${to}?node=node-b&kind=Pod`);
  });

  it("carries the queries the API actually emits", () => {
    // `eventsEvidence` composes this one, and the explorer is empty without
    // it: the finding quotes one message and the screen holds the rest.
    expect(follow("/platform/events?kind=Pod&name=shop-0&namespace=shop")).toBe(
      "/platform/events?kind=Pod&name=shop-0&namespace=shop",
    );
    // `nodeEvidence`.
    expect(follow("/platform/nodes?node=node-b")).toBe("/platform/nodes?node=node-b");
    // `environmentEvidence`, which names the section of the page to open at.
    for (const section of ["requests", "resources", "workload"]) {
      expect(follow(`/environments/shop-production?section=${section}`)).toBe(
        `/environments/shop-production?section=${section}`,
      );
    }
  });

  it("leaves the five platform screens where the findings say they are", () => {
    for (const path of [
      "/platform",
      "/platform/nodes",
      "/platform/workloads",
      "/platform/edge",
      "/platform/events",
      "/platform/storage",
    ]) {
      expect(follow(path)).toBe(path);
      expect(lands(path)).not.toBe("not-found");
    }
  });
});

describe("the two addresses that name an object", () => {
  // A build and an environment know which project they are in; an address
  // does not, so these cannot be redirected by the table. They open the screen
  // they always opened, which then corrects the address once the payload has
  // answered — see BuildView.vue and EnvironmentView.vue.
  it("opens the build screen from either address", () => {
    expect(lands("/builds/shop-42")).toBe("build");
    expect(lands("/projects/shop/deploys/shop-42")).toBe("project-build");
    expect(router.resolve("/builds/shop-42").params.name).toBe("shop-42");
    const scoped = router.resolve("/projects/shop/deploys/shop-42");
    expect(scoped.params).toEqual({ name: "shop", build: "shop-42" });
  });

  it("opens the environment screen from either address, section and all", () => {
    expect(lands("/environments/shop-production?section=requests")).toBe("environment");
    const scoped = router.resolve("/projects/shop/environments/shop-production?section=requests");
    expect(scoped.name).toBe("project-environment");
    expect(scoped.params).toEqual({ name: "shop", env: "shop-production" });
    expect(scoped.query).toEqual({ section: "requests" });
  });
});

describe("the addresses that name no project", () => {
  // These are not screens: they are a question about a project nobody named.
  // The shell opens the picker over the fleet dashboard and keeps the address
  // that was asked for; choosing a project finishes the sentence.
  const table: { from: string; to: string }[] = [
    { from: "/projects", to: "/projects/shop" },
    { from: "/observability", to: "/projects/shop/observability" },
    { from: "/traffic", to: "/projects/shop/observability?view=traffic" },
    { from: "/traces", to: "/projects/shop/observability?view=traces" },
  ];

  it.each(table)("$from completes to $to", ({ from, to }) => {
    const asked = router.resolve(from);
    const picker = asked.meta.picker;
    expect(picker, `${from} opens the picker`).toBeTruthy();
    expect(router.resolve(completedBy(picker!, asked, "shop")).fullPath).toBe(to);
  });

  it("keeps the question the pasted link was asking", () => {
    const asked = router.resolve("/traffic?range=15");
    const completed = router.resolve(completedBy(asked.meta.picker!, asked, "shop"));
    expect(completed.fullPath).toBe("/projects/shop/observability?range=15&view=traffic");
  });

  it("renders the fleet dashboard underneath", () => {
    for (const path of ["/projects", "/observability", "/traffic", "/traces"]) {
      expect(router.resolve(path).meta.view).toBe("OverviewView.vue");
    }
  });
});
