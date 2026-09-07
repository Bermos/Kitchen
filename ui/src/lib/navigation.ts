/**
 * Which project you are in, and how you get back to it.
 *
 * The dashboard scoped every developer screen by the address (#469), which is
 * what makes a pasted link mean the same thing for every reader. What it did
 * not do is remember: the scope switcher carried the project only while you
 * were already in the Project scope, so Project → Platform → Project always
 * ended at the picker, and moving from one project's Deploys to another's
 * meant going out to its Overview and back down.
 *
 * ## The line this file is drawn along
 *
 * **Memory may decide a destination; it may never decide a rendering.**
 *
 * `routes.ts` rejected "a dropdown's last value" outright, and rightly:
 * `/projects/shop/deploys` has to mean shop for every reader, forever. That
 * rule is about what an address *renders*. Where a switcher points when you
 * have not named a project is a different question — the address it produces
 * is still explicit, still shareable, and still means one thing.
 *
 * So: nothing here is ever consulted to resolve a route. It is consulted to
 * build the `to` of a control somebody is about to click, and `/projects`
 * keeps its picker rather than redirecting, because a redirect would make one
 * address mean two things.
 *
 * ## One table, used twice
 *
 * `SECTIONS` is the whole mechanism. Switching project keeps the section you
 * are on, and leaving the scope remembers it — the same map, read from both
 * ends, so the two features cannot drift into disagreeing about what a screen
 * is.
 */

import type { RouteLocationNormalizedLoaded, RouteLocationRaw } from "vue-router";

/**
 * The six screens the Project scope is made of, and the only things worth
 * remembering or switching between. Each is one route name and each exists
 * for every project, which is what makes carrying one across a switch safe.
 */
export const SECTIONS = [
  "project",
  "project-deploys",
  "project-environments",
  "project-observability",
  "project-alerts",
  "project-settings",
] as const;

export type Section = (typeof SECTIONS)[number];

/**
 * The screens that name an *object* rather than a section, and the section
 * each belongs to.
 *
 * A build and an environment belong to the project you are leaving: there is
 * no `blog` equivalent of `shop-42`, and remembering one would send you back
 * to a build that has since aged out of the list. Both degrade to the list
 * they came from, which is the screen that answers the same question about
 * whichever project you land in.
 */
const DEGRADES: Record<string, Section> = {
  "project-build": "project-deploys",
  "project-environment": "project-environments",
};

/** The section a route name is, or the one it degrades to, or null for
 *  anything that is not one of a project's screens — the create screen and
 *  the scope's own picker included. */
export function sectionOf(name: unknown): Section | null {
  if (typeof name !== "string") return null;
  if ((SECTIONS as readonly string[]).includes(name)) return name as Section;
  return DEGRADES[name] ?? null;
}

/** Where somebody is: a project, and which of its screens. */
export interface Place {
  project: string;
  section: Section;
  /** The pane of a screen that has panes. Below. */
  pane?: string;
}

/**
 * The one query key that survives a switch.
 *
 * `?section=` names a *pane of the destination screen* — it is the vocabulary
 * `movedProjectSection` maps onto and the API emits as a finding's evidence,
 * and every project's Settings has the same panes. Everything else in a query
 * is a question about the project being left: a log filter, a time range, a
 * build phase. Carrying those across would answer somebody's question with
 * another project's data.
 */
function paneOf(query: RouteLocationNormalizedLoaded["query"]): string | undefined {
  const pane = query.section;
  return typeof pane === "string" && pane ? pane : undefined;
}

/**
 * Where this route is, or null if it is not one project's screen.
 *
 * The project comes from the path rather than from `params.name` alone: the
 * two legacy addresses in this scope — `/builds/:name`, `/environments/:name`
 * — put an *object* in that parameter, and only the payload knows which
 * project it belongs to. They correct their own address once it answers, so
 * this returns null until they do rather than reading the object as a project.
 */
export function placeOf(route: RouteLocationNormalizedLoaded): Place | null {
  if (!route.path.startsWith("/projects/")) return null;
  const project = route.params.name;
  if (typeof project !== "string" || !project) return null;
  const section = sectionOf(route.name);
  if (!section) return null;
  return { project, section, pane: paneOf(route.query) };
}

/** The address a place names. */
export function addressOf(place: Place): RouteLocationRaw {
  return {
    name: place.section,
    params: { name: place.project },
    ...(place.pane ? { query: { section: place.pane } } : {}),
  };
}

/** The same screen, on another project — what the switcher navigates to. */
export function switched(place: Place | null, project: string): RouteLocationRaw {
  return addressOf({ project, section: place?.section ?? "project", pane: place?.pane });
}

// ── What the browser remembers ──────────────────────────────────────────────

const STORAGE_LAST_PROJECT = "kitchen.lastProject";

/**
 * Storage is a convenience and never a dependency.
 *
 * A private window, a browser set to block site data, and a thumbnail capture
 * all throw on the accessor itself rather than answering null, so both halves
 * are wrapped and both fall back to the in-memory copy. The worst case is a
 * switcher that has forgotten, which is exactly where this feature started.
 */
let remembered: Place | null = null;

function persist(place: Place | null): void {
  try {
    if (place) localStorage.setItem(STORAGE_LAST_PROJECT, JSON.stringify(place));
    else localStorage.removeItem(STORAGE_LAST_PROJECT);
  } catch {
    // The in-memory copy is already set; this tab still works.
  }
}

function stored(): Place | null {
  if (remembered) return remembered;
  try {
    const raw = localStorage.getItem(STORAGE_LAST_PROJECT);
    if (!raw) return null;
    const parsed: unknown = JSON.parse(raw);
    if (!parsed || typeof parsed !== "object") return null;
    const { project, section, pane } = parsed as Record<string, unknown>;
    // A value written by an older build, or edited by hand: a section this
    // build does not have is dropped to the project's Overview rather than
    // navigated to, which would be a 404 out of a control that promised a
    // project.
    if (typeof project !== "string" || !project) return null;
    remembered = {
      project,
      section: sectionOf(section) ?? "project",
      pane: typeof pane === "string" && pane ? pane : undefined,
    };
    return remembered;
  } catch {
    return null;
  }
}

/** Record where somebody is, for the next time they come back to this scope.
 *  A route that is not one project's screen changes nothing — leaving the
 *  scope is what this exists to survive. */
export function remember(place: Place | null): void {
  if (!place) return;
  remembered = place;
  persist(place);
}

/** Forget it. Signing out does, because the next person at this browser has
 *  no business being told which project the last one was working on. */
export function forget(): void {
  remembered = null;
  persist(null);
}

/**
 * Where the scope switcher's Project entry goes.
 *
 * The project in the address wins, because that is not memory at all — it is
 * where you already are. Failing that it is the remembered one, and failing
 * *that* it is `/projects`, which asks. `visible` is the projects this account
 * can currently see: a remembered project that is not among them is passed
 * over, so the control never opens something that answers 404.
 *
 * An empty `visible` means the inventory has not answered yet, not that there
 * are no projects — so it is treated as "cannot say", and the memory stands
 * until a list arrives to contradict it.
 *
 * It reads and decides and changes nothing, because the shell calls it from a
 * computed: a stale entry is stepped over rather than deleted, and overwritten
 * by the next project screen somebody opens. What clears it outright is
 * signing out, which is `forget`.
 */
export function projectScopeDestination(here: Place | null, visible: string[]): RouteLocationRaw {
  if (here) return addressOf(here);
  const last = stored();
  if (!last) return "/projects";
  if (visible.length && !visible.includes(last.project)) return "/projects";
  return addressOf(last);
}
