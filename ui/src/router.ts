import { createRouter, createWebHistory } from "vue-router";
import { isAuthenticated } from "./lib/auth";
import { callerFor, forgetMe, loadMe } from "./lib/me";
import { may } from "./lib/policy";
import { resetScreenFreshness } from "./lib/freshness";
import { movedProjectSection, routes } from "./routes";

// Where a navigation leaves the reader, which is this file's because nothing
// else can answer it.
//
// `AppShell.vue` is a fixed-height frame at `lg` and above, so the element that
// scrolls there is the shell's one `<main>` and not the document; below `lg`
// the shell is still a growing column and the document is. Which of the two
// moved is not worth asking — the other one is at zero, and reading or writing
// a zero costs nothing — so both are remembered and both are put back.
//
// Both halves have to be here. Declaring a `scrollBehavior` at all switches
// `history.scrollRestoration` to `manual`, so the document's own restoration is
// ours from that moment on; and vue-router's `savedPosition` is computed from
// `window.scrollY`, which on a large screen is now always zero.
type Place = { main: number; page: number };

const places = new Map<string, Place>();

function scroller(): HTMLElement | null {
  return document.querySelector<HTMLElement>("main");
}

function placeNow(): Place {
  return { main: scroller()?.scrollTop ?? 0, page: window.scrollY };
}

function goTo(place: Place): void {
  const main = scroller();
  if (main) main.scrollTop = place.main;
  window.scrollTo(0, place.page);
}

// The addresses themselves are `routes.ts`, as data. This file is what a
// browser adds to them: the history, the two questions asked before a
// navigation is allowed to happen, and where the screen it opens starts.
export const router = createRouter({
  history: createWebHistory(),
  routes,
  scrollBehavior(to, from, savedPosition) {
    // A back or a forward, which is the only thing `savedPosition` is read
    // for: its numbers are the document's, but its presence says this address
    // is one the reader has already been at and expects to find as they left
    // it. A first navigation arrives here too, with nothing remembered for it
    // — which is the top, where a page load starts anyway.
    if (savedPosition) {
      goTo(places.get(to.fullPath) ?? { main: 0, page: 0 });
      return false;
    }
    // The same screen asked a different question — a filter, a settings pane,
    // the picker's own query — is not a new screen, and where the reader had
    // got to in it is still theirs.
    if (to.path === from.path) return false;
    // A new screen starts at the top. A screen that scrolls to a place inside
    // itself — the `?section=` a finding's evidence link carries — does that
    // once its own data has arrived, which is long after this runs.
    goTo({ main: 0, page: 0 });
    return false;
  },
});

// Where each address was left, recorded on the way out: by the time
// `scrollBehavior` runs the new screen has rendered and the number is gone.
router.beforeEach((_to, from) => {
  if (from.name) places.set(from.fullPath, placeNow());
  return true;
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
