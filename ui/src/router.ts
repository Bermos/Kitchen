import { createRouter, createWebHistory } from "vue-router";
import { isAuthenticated } from "./lib/auth";

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
    { path: "/platform", name: "platform", component: () => import("./views/PlatformView.vue") },
    { path: "/platform/nodes", name: "platform-nodes", component: () => import("./views/PlatformNodesView.vue") },
    {
      path: "/platform/workloads",
      name: "platform-workloads",
      component: () => import("./views/PlatformWorkloadsView.vue"),
    },
    { path: "/platform/edge", name: "platform-edge", component: () => import("./views/PlatformEdgeView.vue") },
    { path: "/platform/storage", name: "platform-storage", component: () => import("./views/PlatformStorageView.vue") },
    { path: "/platform/events", name: "platform-events", component: () => import("./views/PlatformEventsView.vue") },
    { path: "/builds/:name", name: "build", component: () => import("./views/BuildView.vue") },
    {
      path: "/environments/:name",
      name: "environment",
      component: () => import("./views/EnvironmentView.vue"),
    },
    { path: "/connections", name: "connections", component: () => import("./views/ConnectionsView.vue") },
    { path: "/settings", name: "settings", component: () => import("./views/SettingsView.vue") },
    { path: "/:pathMatch(.*)*", name: "not-found", component: () => import("./views/NotFoundView.vue") },
  ],
});

// Everything except the login round trip needs a signed-in session: the API
// answers 401 to anonymous callers, so there is nothing to render without one.
router.beforeEach((to) => {
  if (to.meta.public || isAuthenticated.value) return true;
  return { name: "login", query: to.fullPath === "/" ? {} : { returnTo: to.fullPath } };
});
