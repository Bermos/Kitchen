import type { FiredRule } from "./api";

/**
 * The decision register, as the screen reasons about it.
 *
 * A decision's verdict is one of exactly three words, and the middle one is
 * the honest emergency: rules fired, an exception waived every one of them,
 * and the record says both things at once. The helpers here keep that reading
 * in one tested place rather than re-derived per template.
 */

/** The colour a verdict reads in, in the app's own tone vocabulary. An
 * unknown verdict is toned, not invisible — a new word from a newer server
 * should look like something rather than nothing. */
export function verdictTone(verdict: string): string {
  switch (verdict) {
    case "allowed":
      return "text-success";
    case "allowed-with-exception":
      return "text-warning";
    case "blocked":
      return "text-error";
    default:
      return "text-toned";
  }
}

/** The rules that stand unmet: fired and not waived. These are what blocks a
 * promotion, and what an eligibility answer lists. */
export function unmetRules(fired: FiredRule[] | undefined): FiredRule[] {
  return (fired ?? []).filter((rule) => !rule.waived);
}

/** One line summarizing what fired: "2 rules fired, 1 waived", or "no rules
 * fired". The waived count is always said when present, because a verdict of
 * allowed-with-exception is only readable next to it. */
export function firedSummary(fired: FiredRule[] | undefined): string {
  const rules = fired ?? [];
  if (rules.length === 0) return "no rules fired";
  const waived = rules.filter((rule) => rule.waived).length;
  const counted = rules.length === 1 ? "1 rule fired" : `${rules.length} rules fired`;
  return waived > 0 ? `${counted}, ${waived} waived` : counted;
}

/** Enough of a digest to tell two apart on a screen; the whole thing rides in
 * the title attribute for anyone copying it out. */
export function shortDigest(digest: string | undefined): string {
  if (!digest) return "—";
  const hex = digest.startsWith("sha256:") ? digest.slice("sha256:".length) : digest;
  return hex.slice(0, 12);
}
