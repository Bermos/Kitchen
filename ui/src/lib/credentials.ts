import { PLATFORM_SCOPES, POLICY, type PlatformScope, type Route } from "./policy.generated";

/**
 * Platform credentials, as the dashboard reasons about them (#349).
 *
 * The one thing worth writing down here is **where the vocabulary comes from**.
 * A screen that offers a list of scopes with nothing beside them is asking
 * somebody to pick from words they have no way to evaluate, and a hand-written
 * gloss for each one is a second description of the permission model that goes
 * stale the first time a route moves. So both halves are read out of the
 * generated policy table: `PLATFORM_SCOPES` is the vocabulary the API knows,
 * and the operations a scope reaches are the rows that name it — in the API's
 * own words, the ones its refusals are built from.
 */

/** What one scope reaches, in the words the API's own refusals use. */
export interface ScopeReach {
  scope: PlatformScope;
  /** Every operation the scope admits, as the policy table phrases it. */
  operations: string[];
}

/** The scopes a credential may be issued with, each with what it reaches. */
export function scopeReach(): ScopeReach[] {
  return PLATFORM_SCOPES.map((scope) => ({
    scope,
    operations: operationsOf(scope),
  }));
}

/** The operations one scope admits, sorted so the list does not move between renders. */
export function operationsOf(scope: PlatformScope): string[] {
  const operations = new Set<string>();
  for (const route of Object.keys(POLICY) as Route[]) {
    const requirement = POLICY[route];
    if (requirement.scope === scope && requirement.doing) {
      operations.add(requirement.doing);
    }
  }
  return [...operations].sort();
}

/**
 * A one-line summary of a scope, for a row that shows a scope rather than
 * explaining it. It counts rather than lists: a table cell with six operations
 * in it is a table cell nobody reads.
 */
export function scopeSummary(scope: string): string {
  const known = PLATFORM_SCOPES.find((candidate) => candidate === scope);
  if (!known) return "not a scope this platform knows";
  const count = operationsOf(known).length;
  return count === 1 ? "1 operation" : `${count} operations`;
}

/** The API's own name rule, checked here so a capital letter is a line under
 * the field rather than a round trip. The API still decides. */
const NAME = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?$/;
const NAME_MAX = 32;

export function credentialNameProblem(name: string): string {
  const trimmed = name.trim();
  if (!trimmed) return "A credential needs a name — it is how you revoke it later.";
  if (trimmed.length > NAME_MAX) {
    return `A credential's name is at most ${NAME_MAX} characters; this one is ${trimmed.length}.`;
  }
  if (!NAME.test(trimmed)) {
    return "A credential's name is lowercase letters, digits and dashes, starting and ending with a letter or digit — like nightly or backup-agent.";
  }
  return "";
}

/**
 * The lifetime a credential may be issued for. The API defaults to 30 days and
 * caps at 90; the options here are the same numbers, so the screen never
 * offers a value the API will refuse.
 */
export const LIFETIME_OPTIONS = [
  { label: "7 days", value: 7 },
  { label: "30 days (the default)", value: 30 },
  { label: "60 days", value: 60 },
  { label: "90 days — the longest a credential may last", value: 90 },
];

/**
 * Whether a credential holds nothing: it has lapsed, or its grant was removed
 * and it authenticates while being able to do nothing. Both are states a
 * listing has to show rather than hide, and both are answered the same way —
 * revoke it.
 */
export function holdsNothing(credential: { expired: boolean; scopes: string[] }): boolean {
  return credential.expired || credential.scopes.length === 0;
}
