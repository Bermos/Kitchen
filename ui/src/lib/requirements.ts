import type { Me } from "./api";

/**
 * An environment's requirements, as the panel that renders and edits them
 * needs them.
 *
 * **Ownership is not a project role.** Who may change an environment's bar is
 * written on the environment itself — `owners`, in the same vocabulary as
 * every access entry — and the API enforces it in the handler, past anything
 * `may()` can read off the enforcement table. So the screen asks two
 * questions before offering the edit: `may()` for the route, and `mayOwn`
 * for the list. Both are display rules; the API stays the authority, and a
 * refusal there is worded by the handler.
 *
 * The matching mirrors `internal/access.SubjectMatches`: an entry containing
 * `@` is an email address, compared case-insensitively; anything else is the
 * issuer's `sub`, compared exactly. The server additionally honours an
 * address only for a verified token — the dashboard's own token came from the
 * issuer, so checking the address here is the honest approximation for
 * whether to *draw* a control, never for whether it works.
 */

/** Whether this account is named in an owners list — or is an operator, who
 * always may. `me` not yet loaded satisfies nothing, the safe direction. */
export function mayOwn(owners: string[] | undefined, me: Me | null, operator: boolean): boolean {
  if (operator) return true;
  if (!me) return false;
  return (owners ?? []).some((owner) => ownerMatches(owner, me));
}

function ownerMatches(owner: string, me: Me): boolean {
  if (!owner) return false;
  if (owner.includes("@")) {
    return !!me.email && owner.toLowerCase() === me.email.toLowerCase();
  }
  return !!me.subject && owner === me.subject;
}

/** The bundle digest form the CRD admits: sha256 plus 64 hex characters.
 * Checked here so a paste that will be refused is a sentence under the field
 * rather than a round trip. An empty value is fine — it removes the bar. */
export function bundleDigestProblem(digest: string): string | undefined {
  const trimmed = digest.trim();
  if (trimmed === "" || /^sha256:[a-f0-9]{64}$/.test(trimmed)) return undefined;
  return "A bundle digest has the form sha256:<64 hex characters>.";
}

/**
 * Parameters as the edit form holds them: one `name=value` per line. Blank
 * lines are skipped; a line without `=` is the name of the problem, returned
 * instead of a guess at what was meant.
 */
export function parseParameters(text: string): { parameters?: Record<string, string>; problem?: string } {
  const parameters: Record<string, string> = {};
  for (const line of text.split("\n")) {
    const trimmed = line.trim();
    if (trimmed === "") continue;
    const at = trimmed.indexOf("=");
    if (at <= 0) {
      return { problem: `Each line is name=value; ${JSON.stringify(trimmed)} is not.` };
    }
    parameters[trimmed.slice(0, at).trim()] = trimmed.slice(at + 1).trim();
  }
  return { parameters };
}

/** The inverse, for seeding the form: stable order, one per line. */
export function formatParameters(parameters: Record<string, string> | undefined): string {
  return Object.entries(parameters ?? {})
    .sort(([a], [b]) => a.localeCompare(b))
    .map(([name, value]) => `${name}=${value}`)
    .join("\n");
}

/** Owners as the edit form holds them: one per line, blanks skipped. */
export function parseOwners(text: string): string[] {
  return text
    .split("\n")
    .map((line) => line.trim())
    .filter((line) => line !== "");
}
