import { createRouter, createWebHistory } from "vue-router";
import { isAuthenticated } from "./lib/auth";
import { callerFor, forgetMe, loadMe } from "./lib/me";
import { may, type Route as PolicyRoute } from "./lib/policy";

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
  }
}

export const router = createRouter({
  history: createWebHistory(),
  routes: [
    { path: "/login", name: "login", component: () => import("./views/LoginView.vue"), meta: { public: true } },
    {
      path: "/auth/callback",
      name: "auth-callback",
      component: () => import("./views/AuthCallbackView.vue"),
      meta: { public: true },
    },
    { path: "/", name: "overview", component: () => import("./views/OverviewView.vue") },
    {
      // Every signed-in account's own screen, and so one with no `requires`:
      // it asks the identity provider about the account behind the session
      // rather than the API about anything the platform authorises. There is
      // no role that could be too low for it — see views/AccountView.vue.
      path: "/account",
      name: "account",
      component: () => import("./views/AccountView.vue"),
    },
    { path: "/projects/:name", name: "project", component: () => import("./views/ProjectView.vue") },
    { path: "/builds", name: "builds", component: () => import("./views/BuildsView.vue") },
    {
      path: "/observability",
      name: "observability",
      component: () => import("./views/ObservabilityView.vue"),
    },
    { path: "/traffic", name: "traffic", component: () => import("./views/TrafficView.vue") },
    { path: "/traces", name: "traces", component: () => import("./views/TracesView.vue") },
    // The operator's own section. These paths are exactly the ones the API's
    // findings emit as evidence (`internal/signals/evidence.go`), so renaming
    // one here silently breaks every link on the problems list.
    //
    // They are the operator's in the guard as well as in the sidebar. A
    // pasted evidence link still lands where it says it does for the operator
    // it was pasted to; for a member it is a redirect rather than a screen of
    // refused requests, which is the same thing the API would have said, said
    // once instead of six times.
    {
      path: "/platform",
      name: "platform",
      component: () => import("./views/PlatformView.vue"),
      meta: { requires: "GET /api/v1/platform/signals" },
    },
    {
      path: "/platform/nodes",
      name: "platform-nodes",
      component: () => import("./views/PlatformNodesView.vue"),
      meta: { requires: "GET /api/v1/platform/nodes" },
    },
    {
      path: "/platform/workloads",
      name: "platform-workloads",
      component: () => import("./views/PlatformWorkloadsView.vue"),
      meta: { requires: "GET /api/v1/platform/workloads" },
    },
    {
      path: "/platform/edge",
      name: "platform-edge",
      component: () => import("./views/PlatformEdgeView.vue"),
      meta: { requires: "GET /api/v1/platform/edge" },
    },
    {
      path: "/platform/storage",
      name: "platform-storage",
      component: () => import("./views/PlatformStorageView.vue"),
      meta: { requires: "GET /api/v1/platform/storage" },
    },
    {
      path: "/platform/events",
      name: "platform-events",
      component: () => import("./views/PlatformEventsView.vue"),
      meta: { requires: "GET /api/v1/platform/events" },
    },
    {
      // Backing the platform up is the operator's: the archive is every
      // credential the installation holds. Restoring has no screen at all —
      // it happens into a cluster whose accounts are gone, so there is nobody
      // left to log in. See docs/BACKUP.md.
      path: "/platform/backup",
      name: "platform-backup",
      component: () => import("./views/PlatformBackupView.vue"),
      meta: { requires: "POST /api/v1/platform/backup" },
    },
    {
      // The platform's own configuration — the `Kitchen` singleton — which is
      // as platform-scoped as anything under this prefix and used to sit in
      // the general navigation, where the one thing it told a developer was
      // that the platform has settings they may not read.
      path: "/platform/settings",
      name: "platform-settings",
      component: () => import("./views/PlatformSettingsView.vue"),
      meta: { requires: "GET /api/v1/settings" },
    },
    {
      // The audit log itself is filtered to what the caller can see, but this
      // screen is built around the compliance posture and the chain
      // verification beside it, and both of those are the operator's.
      path: "/platform/audit",
      name: "platform-audit",
      component: () => import("./views/PlatformAuditView.vue"),
      meta: { requires: "GET /api/v1/compliance" },
    },
    { path: "/builds/:name", name: "build", component: () => import("./views/BuildView.vue") },
    {
      path: "/environments/:name",
      name: "environment",
      component: () => import("./views/EnvironmentView.vue"),
    },
    {
      // Choosing a connection is everybody's; managing one is the operator's,
      // and this screen is the managing. The route it names is the read of a
      // single connection — the thing every row on it opens.
      path: "/connections",
      name: "connections",
      component: () => import("./views/ConnectionsView.vue"),
      meta: { requires: "GET /api/v1/connections/{name}" },
    },
    // Where the settings screen lived before it moved under the platform
    // prefix it belongs to. A bookmark is the whole reason this is here.
    { path: "/settings", redirect: { name: "platform-settings" } },
    { path: "/:pathMatch(.*)*", name: "not-found", component: () => import("./views/NotFoundView.vue") },
  ],
});

// Everything except the login round trip needs a signed-in session: the API
// answers 401 to anonymous callers, so there is nothing to render without one.
//
// And it needs to know *who* is signed in before it renders anything, because
// the platform role decides which of these routes exist for this account.
// `loadMe` is one request per session rather than per navigation; the await is
// only ever real on the first one.
router.beforeEach(async (to) => {
  if (to.meta.public) return true;
  if (!isAuthenticated.value) {
    // Signing out is what ends an account, and this is where the dashboard
    // finds out. Holding the old role would decide the first screen the next
    // person to sign in sees.
    forgetMe();
    return { name: "login", query: to.fullPath === "/" ? {} : { returnTo: to.fullPath } };
  }
  await loadMe();
  if (to.meta.requires && !may(to.meta.requires, callerFor())) {
    // The overview is every account's screen. `denied` is what it needs to
    // say why the address in the location bar is not the one that opened —
    // otherwise a bookmarked platform link looks like a broken dashboard.
    return { name: "overview", query: { denied: to.fullPath } };
  }
  return true;
});
