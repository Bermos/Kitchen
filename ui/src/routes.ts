import type {
  RouteLocationGeneric,
  RouteLocationRaw,
  RouteRecordRaw,
  RouteRecordRedirectOption,
  RouteRecordSingleView,
} from "vue-router";
import type { Route as PolicyRoute } from "./lib/policy";

/**
 * The dashboard's addresses, as data.
 *
 * They are here rather than in `router.ts` so that the table can be read by
 * something other than a browser: `router.ts` adds the history and the guards,
 * `design.test.ts` reads each screen's scope off it, and `routes.test.ts`
 * walks every address anybody has ever pasted.
 *
 * ## The four scopes
 *
 * The dashboard has four audiences and used to have one flat list of screens
 * for all of them, which meant every cross-project developer screen began with
 * a question the address could not answer — *which project?* — and the
 * operator's two inventories sat as items six and seven of the developer's
 * navigation (#469).
 *
 * So every screen belongs to exactly one scope, the scope is in the address,
 * and the shell shows which one you are in:
 *
 * | Scope | Root | Whose |
 * |---|---|---|
 * | Fleet | `/` | Everybody's, and the only place that spans projects |
 * | Project | `/projects/:name/…` | The developer's, scoped by the address |
 * | Platform | `/platform/…` | The operator's, and where they land on sign-in |
 * | Compliance | `/compliance/…` | The auditor's, who is not necessarily an operator |
 *
 * A screen's scope is what decides what may be *on* it — see docs/UI.md, "The
 * scope rule". That is why the scope lives on the route rather than in a
 * component: it is one fact, read by the shell, by the guard and by the test.
 */
export type Scope = "fleet" | "project" | "platform" | "compliance";

/** A scope as the shell's switcher renders it. */
export interface ScopeDefinition {
  id: Scope;
  label: string;
  /** Where picking this scope sends somebody who is not already in it. */
  root: string;
  /**
   * The API route that decides whether this scope exists for an account.
   *
   * Compliance is gated on the audit log rather than on the compliance posture
   * deliberately: four of the five routes behind that scope are answered for
   * anybody who can see a project, and gating the whole scope on the posture's
   * operator-only requirement would refuse a member the four the API is
   * willing to answer (#469, decision 1).
   */
  requires?: PolicyRoute;
}

export const SCOPES: ScopeDefinition[] = [
  { id: "fleet", label: "Fleet", root: "/" },
  { id: "project", label: "Project", root: "/projects" },
  { id: "platform", label: "Platform", root: "/platform", requires: "GET /api/v1/platform/signals" },
  { id: "compliance", label: "Compliance", root: "/compliance", requires: "GET /api/v1/audit" },
];

/**
 * Where a screen that needs a project but was opened without one completes to.
 *
 * `/observability` is not a screen: it is a question about a project nobody
 * named. Rather than guessing one or refusing, the shell opens the project
 * picker over the fleet dashboard and keeps the address that was asked for —
 * so a link somebody pasted still says what they meant, and choosing a project
 * finishes the sentence.
 */
export interface Picker {
  /** The route the choice completes to. */
  name: string;
  /** Query the completed address carries on top of the one already asked for. */
  query?: Record<string, string>;
}

declare module "vue-router" {
  interface RouteMeta {
    /** No session needed: the login round trip, and nothing else. */
    public?: boolean;
    /**
     * The API route this screen is *for*. The guard admits the navigation only
     * if the policy admits the call, so a screen and the requests it is made
     * of cannot disagree about who may open it — and adding a screen means
     * naming its route rather than remembering a role.
     *
     * Only screens with an admission requirement carry one. The project
     * screens do not: which project a request is about is resolved from the
     * object it names, so their answer is the payload's `role`, and it is the
     * controls on the page rather than the route that turn on it.
     */
    requires?: PolicyRoute;
    /** Which of the four scopes this address is in. */
    scope?: Scope;
    /** The `.vue` file under `views/`, so that a test can read a screen's
     * scope without executing the router. */
    view?: string;
    /** Set on an address that names no project and needs one. */
    picker?: Picker;
  }
}

// Every screen, by file name. Looking them up rather than writing the import
// inline is what lets a route carry the file's name as data: one spelling,
// checked at construction, and no route can name a screen that is not there.
const modules = import.meta.glob("./views/*.vue");

interface ScreenOptions {
  path: string;
  name: string;
  view: string;
  scope?: Scope;
  requires?: PolicyRoute;
  picker?: Picker;
  public?: boolean;
}

function screen(options: ScreenOptions): RouteRecordSingleView {
  const component = modules[`./views/${options.view}`];
  if (!component) throw new Error(`route ${options.name} names ${options.view}, which is not a view`);
  return {
    path: options.path,
    name: options.name,
    component: component as RouteRecordSingleView["component"],
    meta: {
      view: options.view,
      scope: options.scope,
      requires: options.requires,
      picker: options.picker,
      public: options.public,
    },
  };
}

/**
 * A redirect that keeps the question.
 *
 * Vue Router drops the query string on a redirect written as a path, and three
 * of the addresses below are emitted by the API as a finding's evidence with
 * their query carrying the whole of what the link is for — `?section=`,
 * `?node=`, `?namespace=&kind=&name=`. A redirect that drops it lands the
 * reader on the right screen showing the wrong thing, which is worse than a
 * 404 because nobody notices. So every redirect here is a function.
 */
export function moved(path: string): RouteRecordRedirectOption {
  return (to: RouteLocationGeneric): RouteLocationRaw => ({ path, query: to.query, hash: to.hash });
}

/**
 * Where a section of the project page went.
 *
 * `/projects/:name` was nine tabs and eleven cards of settings, and the tab was
 * never in the address — so the way anybody linked at a part of it was
 * `?section=`, which the environment screen already uses and which
 * `internal/signals/evidence.go` emits for that one. Six screens later those
 * names still have to land somewhere sensible rather than opening the Overview
 * and quietly showing the wrong thing.
 *
 * So every name the old page's tabs and cards went by is mapped here, once, and
 * `router.ts` applies it. The rest of the query is kept: a link is a question,
 * and a redirect that drops it lands the reader on the right screen with the
 * wrong answer.
 */
const PROJECT_SECTIONS: Record<string, { name: string; section?: string }> = {
  // The two dashboards that became the Deploys timeline.
  deployments: { name: "project-deploys" },
  releases: { name: "project-deploys" },
  builds: { name: "project-deploys" },
  // Previews and environments were two tabs over one list.
  previews: { name: "project-environments" },
  environments: { name: "project-environments" },
  // Everything else is a pane of Settings, under its own name there.
  domains: { name: "project-settings", section: "domains" },
  resources: { name: "project-settings", section: "resources" },
  claims: { name: "project-settings", section: "resources" },
  variables: { name: "project-settings", section: "variables" },
  env: { name: "project-settings", section: "variables" },
  files: { name: "project-settings", section: "files" },
  secrets: { name: "project-settings", section: "secrets" },
  people: { name: "project-settings", section: "members" },
  members: { name: "project-settings", section: "members" },
  keys: { name: "project-settings", section: "keys" },
  notifications: { name: "project-settings", section: "notifications" },
  processes: { name: "project-settings", section: "processes" },
  workloads: { name: "project-settings", section: "processes" },
  git: { name: "project-settings", section: "source" },
  image: { name: "project-settings", section: "source" },
  build: { name: "project-settings", section: "source" },
  runtime: { name: "project-settings", section: "runtime" },
  health: { name: "project-settings", section: "runtime" },
  security: { name: "project-settings", section: "security" },
  data: { name: "project-settings", section: "continuity" },
  continuity: { name: "project-settings", section: "continuity" },
  criticality: { name: "project-settings", section: "continuity" },
  settings: { name: "project-settings" },
  danger: { name: "project-settings", section: "danger" },
};

/**
 * The screen a `?section=` on the project page now names, or null when it names
 * nothing this page ever had — in which case the Overview is the honest answer
 * and the query is left alone.
 */
export function movedProjectSection(to: RouteLocationGeneric): RouteLocationRaw | null {
  if (to.name !== "project") return null;
  const asked = to.query.section;
  const target = typeof asked === "string" ? PROJECT_SECTIONS[asked] : undefined;
  if (!target) return null;
  const { section: _dropped, ...rest } = to.query;
  return {
    name: target.name,
    params: { name: to.params.name },
    query: target.section ? { ...rest, section: target.section } : rest,
    hash: to.hash,
  };
}

/**
 * The address a picker's choice completes to.
 *
 * The query already asked for is kept and the picker's own is laid over it, so
 * `/traffic?range=15` becomes `/projects/shop/observability?range=15&view=traffic`.
 */
export function completedBy(picker: Picker, to: RouteLocationGeneric, project: string): RouteLocationRaw {
  return {
    name: picker.name,
    params: { name: project },
    query: { ...to.query, ...(picker.query ?? {}) },
    hash: to.hash,
  };
}

export const routes: RouteRecordRaw[] = [
  screen({ path: "/login", name: "login", view: "LoginView.vue", public: true }),
  screen({ path: "/auth/callback", name: "auth-callback", view: "AuthCallbackView.vue", public: true }),

  // ── Fleet ────────────────────────────────────────────────────────────────
  // The only scope that spans projects, and the three questions worth asking
  // across all of them: what exists, what needs acting on, what shipped.
  screen({ path: "/", name: "overview", view: "OverviewView.vue", scope: "fleet" }),
  // Alerts is the second, and it is in this scope rather than in Platform
  // because both audiences have it: the API answers it for anybody with a
  // token and narrows it to what they may see — a member's own projects'
  // deliveries and the symptom rows they are owed, an operator's whole estate.
  // It carries no `requires` for the same reason.
  screen({ path: "/alerts", name: "alerts", view: "AlertsView.vue", scope: "fleet" }),
  screen({ path: "/deploys", name: "deploys", view: "BuildsView.vue", scope: "fleet" }),
  screen({
    // Every signed-in account's own screen, and so one with no `requires`:
    // it asks the identity provider about the account behind the session
    // rather than the API about anything the platform authorises. There is
    // no role that could be too low for it — see views/AccountView.vue.
    path: "/account",
    name: "account",
    view: "AccountView.vue",
    scope: "fleet",
  }),

  // ── Project ──────────────────────────────────────────────────────────────
  // Every developer screen, scoped by the address rather than by a dropdown's
  // current value — so a pasted link is a screen somebody else can open and
  // see what you saw.
  screen({
    // The scope's own root names no project, so it is the picker over the
    // fleet dashboard: the switcher has somewhere to send you before you have
    // said which project you mean.
    path: "/projects",
    name: "projects",
    view: "OverviewView.vue",
    scope: "project",
    picker: { name: "project" },
  }),
  // The six screens the project scope is made of. They were one file and nine
  // tabs — health, releases, builds, previews, domains, claims, variables,
  // people and eleven cards of settings, mounted together so that opening the
  // members panel started the metrics pollers (#470). Each is now an address,
  // which is what makes "the fork policy is here" a link somebody can send.
  screen({ path: "/projects/:name", name: "project", view: "ProjectView.vue", scope: "project" }),
  screen({
    path: "/projects/:name/deploys",
    name: "project-deploys",
    view: "ProjectDeploysView.vue",
    scope: "project",
  }),
  screen({
    path: "/projects/:name/deploys/:build",
    name: "project-build",
    view: "BuildView.vue",
    scope: "project",
  }),
  screen({
    path: "/projects/:name/environments",
    name: "project-environments",
    view: "ProjectEnvironmentsView.vue",
    scope: "project",
  }),
  screen({
    path: "/projects/:name/environments/:env",
    name: "project-environment",
    view: "EnvironmentView.vue",
    scope: "project",
  }),
  screen({
    // Logs, patterns, traffic and traces are one question asked four ways
    // about one project, so they are four tabs of one screen rather than
    // three cross-project screens each starting by asking which project.
    path: "/projects/:name/observability",
    name: "project-observability",
    view: "ObservabilityView.vue",
    scope: "project",
  }),
  screen({
    // What is asking for somebody about this project, with the evidence and
    // the controls beside it. It is the fleet's `/alerts` narrowed to one
    // project — the same rows through the same component, so that the same
    // condition cannot say two different things depending on which list it is
    // read from.
    path: "/projects/:name/alerts",
    name: "project-alerts",
    view: "ProjectAlertsView.vue",
    scope: "project",
  }),
  screen({
    // Forms, at one width, behind a left rail — and the one project screen
    // that polls nothing, which is the whole complaint behind "a change to the
    // members panel loads the metrics pollers".
    path: "/projects/:name/settings",
    name: "project-settings",
    view: "ProjectSettingsView.vue",
    scope: "project",
  }),

  // ── Platform ─────────────────────────────────────────────────────────────
  // The operator's own estate. These paths are exactly the ones the API's
  // findings emit as evidence (`internal/signals/evidence.go`), so renaming
  // one here silently breaks every link on the problems list.
  //
  // They are the operator's in the guard as well as in the switcher. A pasted
  // evidence link still lands where it says it does for the operator it was
  // pasted to; for a member it is a redirect rather than a screen of refused
  // requests, which is the same thing the API would have said, said once
  // instead of six times.
  screen({
    path: "/platform",
    name: "platform",
    view: "PlatformView.vue",
    scope: "platform",
    requires: "GET /api/v1/platform/signals",
  }),
  screen({
    path: "/platform/nodes",
    name: "platform-nodes",
    view: "PlatformNodesView.vue",
    scope: "platform",
    requires: "GET /api/v1/platform/nodes",
  }),
  screen({
    path: "/platform/workloads",
    name: "platform-workloads",
    view: "PlatformWorkloadsView.vue",
    scope: "platform",
    requires: "GET /api/v1/platform/workloads",
  }),
  screen({
    path: "/platform/edge",
    name: "platform-edge",
    view: "PlatformEdgeView.vue",
    scope: "platform",
    requires: "GET /api/v1/platform/edge",
  }),
  screen({
    path: "/platform/addons",
    name: "platform-addons",
    view: "PlatformAddonsView.vue",
    scope: "platform",
    requires: "GET /api/v1/addons",
  }),
  screen({
    // Every volume the platform holds, and — since #469 folded the Volumes
    // screen into it — the storage an operator wrote so that a project could
    // mount something older than the cluster. Two inventories of the same
    // subject, and one of them was in the developer's navigation.
    path: "/platform/storage",
    name: "platform-storage",
    view: "PlatformStorageView.vue",
    scope: "platform",
    requires: "GET /api/v1/platform/storage",
  }),
  screen({
    path: "/platform/events",
    name: "platform-events",
    view: "PlatformEventsView.vue",
    scope: "platform",
    requires: "GET /api/v1/platform/events",
  }),
  screen({
    // Choosing a connection is everybody's; managing one is the operator's,
    // and this screen is the managing. The route it names is the read of a
    // single connection — the thing every row on it opens.
    path: "/platform/connections",
    name: "platform-connections",
    view: "ConnectionsView.vue",
    scope: "platform",
    requires: "GET /api/v1/connections/{name}",
  }),
  screen({
    // Backing the platform up is the operator's: the archive is every
    // credential the installation holds. Restoring has no screen at all —
    // it happens into a cluster whose accounts are gone, so there is nobody
    // left to log in. See docs/BACKUP.md.
    path: "/platform/backup",
    name: "platform-backup",
    view: "PlatformBackupView.vue",
    scope: "platform",
    requires: "POST /api/v1/platform/backup",
  }),
  screen({
    // The platform's own configuration — the `Kitchen` singleton — which is
    // as platform-scoped as anything under this prefix and used to sit in
    // the general navigation, where the one thing it told a developer was
    // that the platform has settings they may not read.
    path: "/platform/settings",
    name: "platform-settings",
    view: "PlatformSettingsView.vue",
    scope: "platform",
    requires: "GET /api/v1/settings",
  }),

  // ── Compliance ───────────────────────────────────────────────────────────
  // The auditor's, who may be a third party rather than the operator — which
  // is the whole reason this is a scope of its own and not a section of
  // Platform: somebody who is not an operator should not have to land in the
  // operator's estate to read the evidence (#469, decision 1).
  screen({
    // The audit log, the chain's verdict, the exception register, the
    // criticality register, drift and the classification inventory. The
    // requirement is the audit log's own — anybody who can see a project can
    // read the records of it — and the parts of the screen that are the
    // operator's ask for themselves.
    path: "/compliance/audit",
    name: "compliance-audit",
    view: "ComplianceAuditView.vue",
    scope: "compliance",
    requires: "GET /api/v1/audit",
  }),

  // ── Where these screens used to live ─────────────────────────────────────
  // Every one of these is an address somebody has pasted, and the first five
  // are emitted by the API as a finding's evidence. They keep their query
  // (see `moved`), and `routes.test.ts` walks the whole table.
  //
  // The two that name an object rather than a screen — a build, an
  // environment — cannot be redirected here at all: the new address contains
  // the project, and only the object knows which project it is in. Those two
  // render the screen they always did and correct the address once the
  // payload has said so, which is a redirect that had to ask a question first.
  { path: "/builds", redirect: moved("/deploys") },
  screen({ path: "/builds/:name", name: "build", view: "BuildView.vue", scope: "project" }),
  screen({ path: "/environments/:name", name: "environment", view: "EnvironmentView.vue", scope: "project" }),
  screen({
    path: "/observability",
    name: "observability",
    view: "OverviewView.vue",
    scope: "project",
    picker: { name: "project-observability" },
  }),
  screen({
    path: "/traffic",
    name: "traffic",
    view: "OverviewView.vue",
    scope: "project",
    picker: { name: "project-observability", query: { view: "traffic" } },
  }),
  screen({
    path: "/traces",
    name: "traces",
    view: "OverviewView.vue",
    scope: "project",
    picker: { name: "project-observability", query: { view: "traces" } },
  }),
  { path: "/connections", redirect: moved("/platform/connections") },
  // Not a move into a free address: `/platform/storage` was already the
  // operator's inventory of volumes, and already evidence. The two screens
  // are one screen now.
  { path: "/volumes", redirect: moved("/platform/storage") },
  { path: "/platform/audit", redirect: moved("/compliance/audit") },
  // Where the settings screen lived before it moved under the platform
  // prefix it belongs to. A bookmark is the whole reason this is here.
  { path: "/settings", redirect: moved("/platform/settings") },
  { path: "/compliance", redirect: moved("/compliance/audit") },

  screen({ path: "/:pathMatch(.*)*", name: "not-found", view: "NotFoundView.vue" }),
];
