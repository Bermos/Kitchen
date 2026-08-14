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
