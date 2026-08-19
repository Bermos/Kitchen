/**
 * What the dashboard may offer, answered from the API's own enforcement table.
 *
 * The table itself is generated — `./policy.generated`, written by
 * `hack/gen-ui-policy` from `internal/api/policy.go` — so that the rules the
 * dashboard renders against and the rules the API enforces cannot be two
 * different opinions. This file is the other half of that split, and it is
 * hand-written on purpose: generated data plus a small helper that reasons
 * about it, rather than generated logic nobody can read.
 *
 * The question a screen asks is never "may I call `PATCH
 * /api/v1/environments/{name}`" — it is "may this person redeploy". So the
 * route is named once, at the control, and everything else comes from the
 * table: whether to render it, and the words to explain it when it is
 * disabled. Those words are the table's own `doing`, which is what the API's
 * 403 is built from, so a disabled button and the refusal behind it say the
 * same thing.
 *
 * **`may` answers admission, not completeness.** Two of the five requirement
 * kinds admit any valid token and narrow what comes back — the cross-project
 * reads to the caller's own projects, and `GET /status` to the caller's
 * platform role. There is nothing to hide for those, and no button to disable;
 * what the answer leaves out is the handler's business and cannot be read off
 * the table. `narrowsAnswer` marks them, and a screen showing one still has to
 * treat an absent field as "you may not know" rather than as a zero.
 */

import { PLATFORM_ROLES, POLICY, PROJECT_ROLES } from "./policy.generated";
import type { PlatformRole, ProjectRole, Requirement, Route } from "./policy.generated";

export { PLATFORM_ROLES, POLICY, PROJECT_ROLES, REQUIREMENT_KINDS } from "./policy.generated";
export type { PlatformRole, ProjectRole, Requirement, RequirementKind, Route } from "./policy.generated";

/**
 * Who is asking, in the two words the API tells the dashboard about them:
 * `Me.platformRole`, and the `role` that travels on every Project the caller
 * can see.
 *
 * Both are optional and both are plain strings, because both arrive from the
 * network. Anything that is not a role the table knows about — an older
 * operator's spelling, a payload that has not loaded yet — satisfies nothing
 * at all, which is `AtLeast`'s rule in `internal/access` and the safe
 * direction: a control that fails to render is a nuisance, one that renders
 * for somebody the API will refuse is a lie.
 */
export interface Caller {
  /** `Me.platformRole`: "operator" or "member". */
  platform?: string;
  /** `Project.role` on the project the control acts on: "admin", "developer" or "viewer". */
  project?: string;
  /** The project's name, used only to word a refusal the way the API words it. */
  projectName?: string;
}

/** Where a role sits in its ordering, and -1 for one this build has never
 * heard of — which is below every real role rather than equal to the weakest. */
function rank(roles: readonly string[], role: string | undefined): number {
  return role === undefined ? -1 : roles.indexOf(role);
}

/** Whether `held` carries at least the authority of `want`, comparing in the
 * order the API compares in (the arrays are generated from the same iota
 * blocks `internal/access` compares with). */
export function platformAtLeast(held: string | undefined, want: PlatformRole): boolean {
  const holds = rank(PLATFORM_ROLES, held);
  return holds >= 0 && holds >= rank(PLATFORM_ROLES, want);
}

/** The same, for a role on one project. */
export function projectAtLeast(held: string | undefined, want: ProjectRole): boolean {
  const holds = rank(PROJECT_ROLES, held);
  return holds >= 0 && holds >= rank(PROJECT_ROLES, want);
}

/**
 * The project role this caller effectively holds.
 *
 * An operator holds admin on every project, present and future — the rule
 * lives in `access.ProjectRoleFor` and is applied here for the same reason it
 * is applied there rather than in each caller. The API already says so on the
 * payload (`Project.role` reads "admin" for an operator, listed or not), so
 * this only matters where the dashboard is deciding about a project it has not
 * loaded.
 */
export function effectiveProjectRole(caller: Caller): string | undefined {
  if (platformAtLeast(caller.platform, "operator")) return "admin";
  return caller.project;
}

/** What a route asks of its caller. */
export function requirementFor(route: Route): Requirement {
  return POLICY[route];
}

/**
 * Whether a route admits this caller — the question a control asks before it
 * renders.
 *
 * It is the API's own decision, minus the part that needs the cluster: the
 * table says *which* role a route wants and the caller says which they hold,
 * but which project a request turns out to be about is resolved server-side
 * from the object it names. So the answer is only as right as the role handed
 * in; a screen acting on a project must pass that project's own `role`.
 */
export function may(route: Route, caller: Caller = {}): boolean {
  const requirement = POLICY[route];
  switch (requirement.kind) {
    case "authenticated":
    case "visibleProjects":
    case "roleShapedBody":
      // A valid token is the whole requirement. Whether the answer is narrowed
      // is a different question — see narrowsAnswer.
      return true;
    case "operator":
      return platformAtLeast(caller.platform, "operator");
    case "projectRole":
      return projectAtLeast(effectiveProjectRole(caller), requirement.role ?? "admin");
    default:
      return unknownKind(requirement.kind);
  }
}

/**
 * Why not, in the words the API refuses in — or undefined when there is
 * nothing to explain because the caller may.
 *
 * It is the same vocabulary rather than the same sentence: the API's 403 is
 * the authority on what happened, and this is what a disabled control says
 * before anybody calls it.
 */
export function refusal(route: Route, caller: Caller = {}): string | undefined {
  if (may(route, caller)) return undefined;
  const requirement = POLICY[route];
  const doing = requirement.doing ?? "this";

  if (requirement.kind === "operator") {
    const held = rank(PLATFORM_ROLES, caller.platform) >= 0 ? `; you are a ${caller.platform}` : "";
    return `${doing} needs the operator role${held}`;
  }

  const role = requirement.role ?? "admin";
  const held = effectiveProjectRole(caller);
  const on = caller.projectName ? ` on ${caller.projectName}` : "";
  if (rank(PROJECT_ROLES, held) < 0) {
    // No role at all. The API answers such a request with a not-found rather
    // than a refusal — a caller who may not see a project is not told it
    // exists — so the dashboard should not have offered the object either.
    return `${doing} needs ${role}; you have no role${on}`;
  }
  return `you have ${held}${on}; ${doing} needs ${role}`;
}

/**
 * Whether a route admits everybody and narrows what it answers with, rather
 * than admitting or refusing the call.
 *
 * A screen showing one of these is showing the caller's own slice of
 * something, and absent fields mean "you may not know" rather than zero. What
 * exactly is left out is the handler's, and is the one part of the policy this
 * table cannot state.
 */
export function narrowsAnswer(route: Route): boolean {
  const kind = POLICY[route].kind;
  return kind === "visibleProjects" || kind === "roleShapedBody";
}

/**
 * A requirement kind nothing above handles.
 *
 * The `never` parameter is the point: a kind added to the API's table and
 * regenerated into `RequirementKind` stops this file compiling, rather than
 * becoming a route the dashboard quietly treats as one anybody may call.
 */
function unknownKind(kind: never): never {
  throw new Error(`unknown requirement kind: ${String(kind)}`);
}
