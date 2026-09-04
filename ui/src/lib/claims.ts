import { effectiveProjectRole, projectAtLeast, type Caller } from "./policy";

/**
 * The one thing about a claim that is not the developer's (#320).
 *
 * A claim's `deletionPolicy` is `Retain` or `Delete`, and the difference is
 * the whole of what deleting the claim later means: `Retain` withdraws the
 * platform's binding and leaves the database, the bucket or the disk where it
 * is, while `Delete` destroys it and everything on it. Asking for a resource
 * and taking one away are the day job — the API's route table asks for
 * `developer` on both — but destroying the data is not, and the API refuses
 * `Delete` to anybody below `admin` at both ends.
 *
 * That escalation cannot be read off `policy.generated.ts`: the table's unit
 * is a whole route, and this depends on a field of the request going out and
 * on the stored claim coming back. So it is stated here, once, in the same
 * role ordering `policy.ts` compares in — and the screens ask this rather
 * than each spelling out its own `=== "admin"`.
 *
 * The rule is the same on both sides of the line the dashboard draws: a
 * control the API would refuse is not offered, and the reason is said in the
 * words the refusal uses.
 */

/** The policy that takes the provisioned resource and its data with the
 * claim. The other is `Retain`, which is the default everywhere. */
export const DESTRUCTIVE_POLICY = "Delete";

/** Whether deleting this claim destroys what it provisioned. A claim with no
 * policy at all — an `oidcClient`, or one created before it was asked for —
 * retains, which is the CRD's default and the safe reading. */
export function destroysData(claim: { deletionPolicy?: string }): boolean {
  return claim.deletionPolicy === DESTRUCTIVE_POLICY;
}

/** Whether this caller may ask for a `Delete` policy, or delete a claim that
 * already carries one. Admin on the claim's project — which an operator holds
 * on every project, as `effectiveProjectRole` says. */
export function mayDestroyData(caller: Caller): boolean {
  return projectAtLeast(effectiveProjectRole(caller), "admin");
}

/**
 * Why not, in the vocabulary the API refuses in — or undefined when there is
 * nothing to explain because the caller may.
 *
 * `doing` completes the sentence the way the API's own 403 completes it:
 * "asking for a claim that destroys its database", "deleting a claim that
 * destroys its bucket".
 */
export function destroysDataRefusal(caller: Caller, doing: string): string | undefined {
  if (mayDestroyData(caller)) return undefined;
  const held = effectiveProjectRole(caller);
  const on = caller.projectName ? ` on ${caller.projectName}` : "";
  const have = held ? `you have ${held}${on}` : `you have no role${on}`;
  return `${have}; ${doing} needs admin: deletionPolicy Delete destroys the provisioned resource and the data on it, and there is no undo`;
}

/**
 * Whether the delete confirmation is gated on typing the claim's name.
 *
 * A `Retain` claim's confirmation is a sentence and a click: the resource
 * survives it, and the worst case is a binding to put back. A `Delete`
 * claim's is the same act project deletion is — data that does not come back
 * — so it takes the same gate, and a click cannot be the whole of it.
 */
export function deletionGatedByName(claim: { deletionPolicy?: string } | null | undefined): boolean {
  return Boolean(claim && destroysData(claim));
}

/**
 * The second thing about a claim that is not the developer's (#247).
 *
 * A claim's data can be recovered to a point in time where the provider can
 * actually do it, and the operation is deliberately two: **recover** makes a
 * sibling database holding the data as it was at a moment and touches nothing
 * the application is reading, and **promote** makes that sibling the claim's
 * binding. The first is cheap and reversible and is the developer's; the
 * second replaces the database every environment of the project reads, so the
 * API refuses it below `admin` — the same role `deletionPolicy: Delete` needs
 * and the same role that may delete the project.
 *
 * Like that one, it cannot be read off `policy.generated.ts`: the route's row
 * is the floor, and the escalation is the handler's. So it is stated here in
 * the same role ordering, and the screen asks this rather than spelling out
 * its own comparison.
 */

/** Whether this caller may promote a recovery over the claim's own database. */
export function mayPromoteRecovery(caller: Caller): boolean {
  return projectAtLeast(effectiveProjectRole(caller), "admin");
}

/** Why not, in the vocabulary the API refuses in — or undefined when the
 * caller may. */
export function promoteRefusal(caller: Caller): string | undefined {
  if (mayPromoteRecovery(caller)) return undefined;
  const held = effectiveProjectRole(caller);
  const on = caller.projectName ? ` on ${caller.projectName}` : "";
  const have = held ? `you have ${held}${on}` : `you have no role${on}`;
  return `${have}; promoting a recovery needs admin: it replaces the database every environment of this project reads, and the one it displaces is kept but no longer bound`;
}
