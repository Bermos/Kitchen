import { createRouter, createWebHistory } from "vue-router";
import { isAuthenticated } from "./lib/auth";
import { callerFor, forgetMe, loadMe } from "./lib/me";
import { may } from "./lib/policy";
import { resetScreenFreshness } from "./lib/freshness";
import { movedProjectSection, routes } from "./routes";

// The addresses themselves are `routes.ts`, as data. This file is what a
// browser adds to them: the history, and the two questions asked before a
// navigation is allowed to happen.
export const router = createRouter({
  history: createWebHistory(),
  routes,
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
  // A `?section=` that named part of the old project mega-page. It cannot be a
  // row of the route table — the path is the same one the Overview answers at
  // — so it is a redirect here, and `routes.ts` holds the map.
  const moved = movedProjectSection(to);
  if (moved) return moved;
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

// Freshness is per screen, and this is what makes it so: a pause held while
// somebody read one screen is not a pause they asked for on the next.
// `lib/freshness.ts` says why that is the right scope, and why the screen's
// sources are left to leave with their own component rather than cleared here.
router.afterEach(() => {
  resetScreenFreshness();
});
