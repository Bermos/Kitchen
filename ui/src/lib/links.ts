import type { RouteLocationRaw } from "vue-router";

/**
 * Where a build and an environment live now.
 *
 * Both are project-scoped addresses since #469 — `/projects/:name/deploys/:build`
 * and `/projects/:name/environments/:env` — and both used to be top-level ones
 * that named only the object. The old addresses still open the right screen and
 * correct themselves once the payload says which project they are in (see
 * `BuildView.vue` and `EnvironmentView.vue`), because the API emits them as a
 * finding's evidence and a link somebody pasted has to land.
 *
 * That correction is a fallback, not a route. Anything in the dashboard that
 * already knows the project links straight at the project-scoped address, so
 * the address bar does not visibly rewrite itself after every click; anything
 * that genuinely does not — a cluster Event naming an environment, an edge
 * ranking — links at the old one and lets it resolve.
 */
export function buildLink(build: string, project?: string): RouteLocationRaw {
  if (!project) return { name: "build", params: { name: build } };
  return { name: "project-build", params: { name: project, build } };
}

export function environmentLink(
  environment: string,
  project?: string,
  query?: Record<string, string>,
): RouteLocationRaw {
  if (!project) return { name: "environment", params: { name: environment }, query };
  return { name: "project-environment", params: { name: project, env: environment }, query };
}
