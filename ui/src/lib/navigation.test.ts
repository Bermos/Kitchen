/**
 * Which project you are in, and how you get back to it.
 *
 * The rule this file exists to hold is the one that makes remembering safe at
 * all: **memory decides a destination, never a rendering**. So what is checked
 * here is that a place round-trips into an explicit address, that a screen
 * naming another project's object degrades rather than following you, and that
 * a remembered project which has gone is forgotten instead of navigated to.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";
import type { RouteLocationNormalizedLoaded } from "vue-router";
import {
  addressOf,
  forget,
  placeOf,
  projectScopeDestination,
  remember,
  SECTIONS,
  sectionOf,
  switched,
  type Place,
} from "./navigation";

/** Just enough of a resolved route for the functions that read one. */
function route(path: string, name: string, params: Record<string, string> = {}, query: Record<string, string> = {}) {
  return { path, name, params, query } as unknown as RouteLocationNormalizedLoaded;
}

/** A localStorage that works, and one that does not. The dashboard's tests run
 *  in node, which has neither — and a browser in a private window throws on
 *  the accessor itself, which is the case the second one stands for. */
function workingStorage() {
  const held = new Map<string, string>();
  return {
    getItem: (key: string) => held.get(key) ?? null,
    setItem: (key: string, value: string) => void held.set(key, value),
    removeItem: (key: string) => void held.delete(key),
  };
}
function throwingStorage() {
  return {
    getItem() {
      throw new Error("site data is blocked");
    },
    setItem() {
      throw new Error("site data is blocked");
    },
    removeItem() {
      throw new Error("site data is blocked");
    },
  };
}
function install(storage: unknown) {
  Object.defineProperty(globalThis, "localStorage", { value: storage, configurable: true, writable: true });
}

beforeEach(() => {
  install(workingStorage());
  forget();
});
afterEach(() => forget());

describe("what counts as a section", () => {
  it("is each of the six screens the scope is made of", () => {
    for (const section of SECTIONS) expect(sectionOf(section)).toBe(section);
    expect(SECTIONS).toHaveLength(6);
  });

  it("degrades a screen that names an object to the list it came from", () => {
    // A build belongs to the project you are leaving: there is no `blog`
    // equivalent of `shop-42`, and a remembered one ages out of the list.
    expect(sectionOf("project-build")).toBe("project-deploys");
    expect(sectionOf("project-environment")).toBe("project-environments");
  });

  it("is nothing for a screen that is not one project's", () => {
    for (const other of ["project-new", "projects", "overview", "platform", "compliance-audit", "", undefined, 7]) {
      expect(sectionOf(other)).toBeNull();
    }
  });
});

describe("reading a place off the address", () => {
  it("is the project and the screen", () => {
    expect(placeOf(route("/projects/shop/deploys", "project-deploys", { name: "shop" }))).toEqual({
      project: "shop",
      section: "project-deploys",
      pane: undefined,
    });
  });

  it("carries the settings pane and nothing else in the query", () => {
    // `?section=` names a pane of the destination screen — the vocabulary the
    // redirects and the API's evidence links already use, and every project
    // has the same panes. A log filter or a time range is a question about the
    // project being left.
    const asked = route("/projects/shop/settings", "project-settings", { name: "shop" }, {
      section: "domains",
      q: "failed",
      range: "15",
    });
    expect(placeOf(asked)).toEqual({ project: "shop", section: "project-settings", pane: "domains" });
  });

  it("degrades the two screens that name an object", () => {
    const build = placeOf(route("/projects/shop/deploys/shop-42", "project-build", { name: "shop", build: "shop-42" }));
    expect(build).toEqual({ project: "shop", section: "project-deploys", pane: undefined });
  });

  it("is nothing where there is no project to be in", () => {
    // The create screen names none, the legacy addresses put an *object* in
    // `params.name` and only their payload knows the project, and the fleet
    // dashboard is another scope entirely.
    expect(placeOf(route("/projects/new", "project-new"))).toBeNull();
    expect(placeOf(route("/builds/shop-42", "build", { name: "shop-42" }))).toBeNull();
    expect(placeOf(route("/environments/shop-production", "environment", { name: "shop-production" }))).toBeNull();
    expect(placeOf(route("/", "overview"))).toBeNull();
    expect(placeOf(route("/projects", "projects"))).toBeNull();
  });
});

describe("switching to another project", () => {
  const here: Place = { project: "shop", section: "project-deploys" };

  it("keeps the screen you are on", () => {
    expect(switched(here, "blog")).toEqual({ name: "project-deploys", params: { name: "blog" } });
  });

  it("keeps the settings pane", () => {
    expect(switched({ project: "shop", section: "project-settings", pane: "domains" }, "blog")).toEqual({
      name: "project-settings",
      params: { name: "blog" },
      query: { section: "domains" },
    });
  });

  it("lands on the Overview from a screen that is nobody's section", () => {
    expect(switched(null, "blog")).toEqual({ name: "project", params: { name: "blog" } });
  });

  it("round-trips through an address", () => {
    expect(addressOf(here)).toEqual({ name: "project-deploys", params: { name: "shop" } });
  });
});

describe("where the scope switcher goes", () => {
  it("is where you already are, which is not memory at all", () => {
    remember({ project: "blog", section: "project-alerts" });
    const here: Place = { project: "shop", section: "project-settings", pane: "members" };
    expect(projectScopeDestination(here, ["shop", "blog"])).toEqual({
      name: "project-settings",
      params: { name: "shop" },
      query: { section: "members" },
    });
  });

  it("is the project you were last in, on the screen you were last on", () => {
    remember({ project: "shop", section: "project-deploys" });
    expect(projectScopeDestination(null, ["shop", "blog"])).toEqual({
      name: "project-deploys",
      params: { name: "shop" },
    });
  });

  it("asks when there is nothing to remember", () => {
    expect(projectScopeDestination(null, ["shop"])).toBe("/projects");
  });

  it("asks when the project has gone", () => {
    // Deleted, or a role somebody no longer holds. Either way the control must
    // not open something that answers 404.
    remember({ project: "gone", section: "project-deploys" });
    expect(projectScopeDestination(null, ["shop", "blog"])).toBe("/projects");
  });

  it("steps over a stale entry rather than deleting it", () => {
    // The shell asks from a computed, so this decides and changes nothing. A
    // project that comes back — re-created, or a role restored — is remembered
    // again rather than having been thrown away while it was gone.
    remember({ project: "gone", section: "project-deploys" });
    expect(projectScopeDestination(null, ["shop"])).toBe("/projects");
    expect(projectScopeDestination(null, ["shop", "gone"])).toEqual({
      name: "project-deploys",
      params: { name: "gone" },
    });
  });

  it("is cleared outright by signing out", () => {
    remember({ project: "shop", section: "project-deploys" });
    forget();
    expect(projectScopeDestination(null, ["shop"])).toBe("/projects");
  });

  it("keeps the memory while the inventory has not answered", () => {
    // An empty list is "cannot say yet", not "there are none" — forgetting on
    // it would lose the project on every cold load.
    remember({ project: "shop", section: "project-deploys" });
    expect(projectScopeDestination(null, [])).toEqual({ name: "project-deploys", params: { name: "shop" } });
  });

  it("ignores a route that is not one project's", () => {
    remember({ project: "shop", section: "project-deploys" });
    remember(null);
    expect(projectScopeDestination(null, ["shop"])).toEqual({ name: "project-deploys", params: { name: "shop" } });
  });
});

describe("when the browser will not hold it", () => {
  it("still remembers for this tab", () => {
    install(throwingStorage());
    forget();
    remember({ project: "shop", section: "project-alerts" });
    expect(projectScopeDestination(null, ["shop"])).toEqual({ name: "project-alerts", params: { name: "shop" } });
  });

  it("asks rather than throwing when it cannot read", () => {
    install(throwingStorage());
    forget();
    expect(projectScopeDestination(null, ["shop"])).toBe("/projects");
  });
});

describe("a stored value this build did not write", () => {
  it("drops a section that no longer exists rather than navigating to it", () => {
    install(workingStorage());
    forget();
    localStorage.setItem("kitchen.lastProject", JSON.stringify({ project: "shop", section: "project-previews" }));
    expect(projectScopeDestination(null, ["shop"])).toEqual({ name: "project", params: { name: "shop" } });
  });

  it("is nothing at all when it names no project", () => {
    install(workingStorage());
    forget();
    for (const junk of ["{not json", JSON.stringify({ section: "project-deploys" }), JSON.stringify(null), "7"]) {
      localStorage.setItem("kitchen.lastProject", junk);
      expect(projectScopeDestination(null, ["shop"])).toBe("/projects");
    }
  });
});
