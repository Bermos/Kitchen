import { computed, ref } from "vue";
import type { Me } from "./api";
import { platformAtLeast, type Caller } from "./policy";

/**
 * Who is signed in, as the API describes them to themselves.
 *
 * `GET /me` is asked once, at start-up, and held here: the platform role
 * decides what the sidebar has in it and which routes exist at all, so every
 * screen would otherwise ask for it again and the answer would arrive after
 * the screen did. The router's guard is what loads it (`router.ts`), because
 * the guard is the first thing that needs it — a bookmarked `/platform/nodes`
 * has to be decided before the view is mounted, not after it has issued four
 * requests the API will refuse.
 *
 * It is a module-level ref rather than a store framework, which is how the
 * dashboard already holds the session (`auth.ts`) and the runtime config
 * (`config.ts`). The role changes when the account changes, and the account
 * changes by signing out — so `forgetMe()` belongs next to that and nowhere
 * else.
 *
 * **An answer that never came is not an operator.** A `/me` the network
 * refused leaves the role undefined, which satisfies nothing in
 * `policy.ts` — the same direction `Caller` documents, and the safe one: a
 * control that fails to render is a nuisance, one that renders for somebody
 * the API will refuse is a lie. The failure is kept so the shell can say so
 * rather than leaving a half-empty dashboard unexplained.
 */

const account = ref<Me | null>(null);
const failure = ref<string | null>(null);
let inFlight: Promise<void> | null = null;

/** The account behind the token, or null before `/me` has answered. */
export const me = computed<Me | null>(() => account.value);

/** `Me.platformRole`, and undefined until it is known. */
export const platformRole = computed<string | undefined>(() => account.value?.platformRole);

/** Whether this account owns the platform. The one question the navigation,
 * the route guards and the mode toggle all ask. */
export const isOperator = computed(() => platformAtLeast(platformRole.value, "operator"));

/** Why the dashboard does not know who it is talking to, when it does not.
 * Empty in the ordinary case, including before the first load finishes. */
export const meError = computed<string | null>(() => failure.value);

/**
 * Load `/me`, once. Concurrent callers wait on the same request, and a call
 * after it has answered is free — which is what lets the router guard call it
 * on every navigation without a request per navigation. A load that *failed*
 * is retried by the next navigation, on purpose: the dashboard is running
 * narrowed until it succeeds, so the cheapest moment to try again is the next
 * thing the person does.
 *
 * It resolves rather than rejects on failure: the caller is a route guard,
 * and a guard that throws strands the browser on a blank page. What went
 * wrong is in `meError`.
 *
 * The API client is reached for here rather than imported at the top, and
 * that is deliberate: the role is what decides which of the two dashboards is
 * rendered, so `mode.ts` reads it — and everything that reads it would
 * otherwise pull the whole client, its session and its browser storage into
 * its own import graph. Only the one function that makes a request needs it.
 */
export function loadMe(): Promise<void> {
  if (account.value) return Promise.resolve();
  inFlight ??= import("./api")
    .then(({ api }) => api.me())
    .then((answer) => {
      account.value = answer;
      failure.value = null;
    })
    .catch((err: unknown) => {
      failure.value = err instanceof Error ? err.message : String(err);
    })
    .finally(() => {
      inFlight = null;
    });
  return inFlight;
}

/** Drop the account. Signing out is the only thing that ends one, and the
 * next sign-in may be somebody else — a stale role would decide the first
 * screen they see. */
export function forgetMe(): void {
  account.value = null;
  failure.value = null;
  inFlight = null;
}

/**
 * The caller as `policy.ts` asks about them: the platform role always, and
 * the role on one project where the question is about a project.
 *
 * Every control passes the role that arrived on *that* project's payload —
 * which is why this takes it rather than reading it from anywhere. There is
 * no cache of project roles here on purpose: the payload the screen is
 * rendering is the freshest answer there is.
 */
export function callerFor(projectRole?: string, projectName?: string): Caller {
  return { platform: platformRole.value, project: projectRole, projectName };
}
