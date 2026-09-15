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
// **None of this is gated to `lg`, and it cannot be.** Declaring a
// `scrollBehavior` at all switches `history.scrollRestoration` to `manual`, at
// every width — so the document's own restoration is ours from that moment on,
// on a phone as much as on a desktop, and a new screen opening at the top is a
// change there rather than a fix. The CSS half of the frame is `lg:`-only; this
// half is everywhere, and the shell's comment says so too. The other reason it
// has to be here is that vue-router's own `savedPosition` is computed from
// `window.scrollY`, which on a large screen is now always zero.
type Place = { main: number; page: number };

/**
 * Where each address was last left, most recently left last.
 *
 * It is keyed by the address rather than by the history entry, so two entries
 * at one address share a place — invisible in practice, and the alternative is
 * reading vue-router's own history state. It is bounded because it is the whole
 * of this feature's state and nothing else would ever drop an entry: a reader
 * who has been to more than `PLACES` distinct addresses in one page load has
 * lost the oldest, which is the same answer a browser gives.
 */
const places = new Map<string, Place>();
const PLACES = 50;

function remember(path: string, place: Place): void {
  // Re-inserting makes insertion order recency order, so the first key is the
  // least recently left.
  places.delete(path);
  places.set(path, place);
  while (places.size > PLACES) places.delete(places.keys().next().value as string);
}

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

/** A navigation to another screen, rather than the same one asked a different
 * question. A pane and a filter are both a query on one path and only the
 * screen can tell them apart, which is #609. */
function isNewScreen(to: { path: string }, from: { path: string }): boolean {
  return to.path !== from.path;
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
    //
    // The place is **aimed at rather than reached**, and the guide says so in
    // those words. This runs one `nextTick` after the route is confirmed, when
    // the new screen has mounted and fetched nothing — every view's `useAsync`
    // starts at `null` with no cache — so the scrollport holds a loading state
    // and the number clamps to the little there is to scroll. The browser's own
    // `auto` restoration clamped identically here before the shell was a frame,
    // so this is nobody's regression; #608 is where making it land lives.
    //
    // Read once and dropped, which is what vue-router does with its own saved
    // positions: leaving the address writes it again.
    if (savedPosition) {
      const place = places.get(to.fullPath);
      places.delete(to.fullPath);
      goTo(place ?? { main: 0, page: 0 });
      return false;
    }
    // The same screen asked a different question — a filter, a settings pane,
    // the picker's own query — is not a new screen, and where the reader had
    // got to in it is still theirs.
    if (!isNewScreen(to, from)) return false;
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
  if (from.name) remember(from.fullPath, placeNow());
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
router.afterEach((to, from) => {
  resetScreenFreshness();
  // A new screen takes focus to the landmark it is rendered in.
  //
  // At `lg` the document is no longer a scrollport, and a browser sends the
  // keys that scroll a page — space, page down, the arrows — through the scroll
  // chain of whatever has focus. A page load and every navigation leave that on
  // `<body>`, so without this the space bar does nothing on a build's log until
  // something inside the page is clicked or tabbed to. Chrome's promotion of
  // keyboard-focusable scrollers does not cover it: that is for scrollers with
  // no focusable children, and every screen here is full of links.
  //
  // It is the same move that tells a screen reader a new page has arrived, and
  // it is a *new screen's* to make — a filter or a settings pane changing would
  // otherwise take focus off the control that changed it.
  //
  // `preventScroll` because focusing an element otherwise brings it into view,
  // and below `lg` — where the document is still the scroller — that would move
  // the page. Note this runs *before* `scrollBehavior` rather than after it:
  // `finalizeNavigation` sets the route, schedules the scroll on a `nextTick`
  // and returns, and `triggerAfterEach` is called synchronously after that. So
  // the flag is belt to the braces of the scroll decision landing later and
  // overwriting anything the focus moved.
  //
  // It also runs before Vue's flush, which is what leaves a view free to focus
  // its own input. That the eleven `autofocus` inputs in this dashboard are
  // Nuxt UI's *prop* — an `onMounted` timeout, never the HTML attribute — is
  // what makes that safe rather than lucky: a raw `<input autofocus>` would be
  // **abandoned here**, because the algorithm that flushes autofocus candidates
  // bails when the document's focused area is not the document itself, which is
  // exactly the state this call creates. Anything new that wants focus on mount
  // takes the prop, or asks for it itself.
  if (isNewScreen(to, from)) scroller()?.focus({ preventScroll: true });
});
