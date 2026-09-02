/**
 * How a finding is read on screen.
 *
 * The operator's problems list and the environment page's diagnostics strip are
 * the same rows in two sizes — same catalogue, same fingerprints, same
 * `unreadable` list — so everything about how a finding renders lives here
 * rather than in either screen. Two screens disagreeing about what `unknown`
 * looks like would be two screens disagreeing about whether the platform is
 * healthy.
 *
 * The one rule that matters most: **an empty problems list is the strongest
 * claim this dashboard makes**, and it is only true when nothing went
 * unread. `unreadable` is never rendered as findings, never counted as
 * problems, and never omitted — see `unreadableSentence`.
 */

import type { Finding, FindingScope, InputFailure, Severity, SignalCounts, SignalsAnswer } from "./api";
import type { Tone } from "./status";

/**
 * The dot a severity gets.
 *
 * `unknown` is the interesting one: it means a rule could not be evaluated, so
 * it must be neither green nor red. Neutral is what the rest of this dashboard
 * already uses for "the operator has not looked yet" (`conditionsTone`), and
 * that is exactly what this is — with the label and the icon saying so in
 * words, because a grey dot alone would read as calm.
 */
export function severityTone(severity: Severity): Tone {
  switch (severity) {
    case "critical":
      return "error";
    case "warning":
      return "warning";
    case "info":
      return "info";
    default:
      return "neutral";
  }
}

/** What a severity is called where there is room to say it. */
export function severityLabel(severity: Severity): string {
  switch (severity) {
    case "critical":
      return "Critical";
    case "warning":
      return "Warning";
    case "info":
      return "Info";
    default:
      return "Could not evaluate";
  }
}

export function severityIcon(severity: Severity): string {
  switch (severity) {
    case "critical":
      return "i-lucide-octagon-alert";
    case "warning":
      return "i-lucide-triangle-alert";
    case "info":
      return "i-lucide-info";
    default:
      return "i-lucide-circle-help";
  }
}

/**
 * The order the problems list renders in, worst first. It mirrors the
 * operator's own ranking exactly, `unknown` included: "I could not tell you"
 * sits below a real warning and above info, because a rule that cannot see is
 * closer to a problem than to a note.
 */
export function severityRank(severity: Severity): number {
  switch (severity) {
    case "critical":
      return 3;
    case "warning":
      return 2;
    case "unknown":
      return 1;
    default:
      return 0;
  }
}

/**
 * Worst first, then by signal and fingerprint.
 *
 * The API already answers in this order; sorting again costs nothing and means
 * a dashboard talking to an operator a version behind still renders the list
 * the way the screen promises to.
 */
export function sortFindings(findings: Finding[] | null | undefined): Finding[] {
  return [...(findings ?? [])].sort((a, b) => {
    const rank = severityRank(b.severity) - severityRank(a.severity);
    if (rank !== 0) return rank;
    if (a.signal !== b.signal) return a.signal < b.signal ? -1 : 1;
    return a.fingerprint < b.fingerprint ? -1 : a.fingerprint > b.fingerprint ? 1 : 0;
  });
}

/**
 * The headline number out of a detail.
 *
 * The API's contract is that a detail's clauses are joined with `; ` and the
 * first one is the number the finding is about, so a strip can render
 * `title (first clause)` without knowing anything about the rule that produced
 * it. This is that split, and it is the only thing either screen assumes about
 * the string.
 */
export function firstClause(detail: string | undefined): string {
  const [first] = (detail ?? "").split(";");
  return first.trim();
}

/** `crash-looping (12 restarts in 30m)` — the strip's one line per problem. */
export function findingHeadline(finding: Finding): string {
  const clause = firstClause(finding.detail);
  return clause ? `${finding.title} (${clause})` : finding.title;
}

/**
 * A scope's identity, joined in the fixed order the operator joins it in — it
 * is the tail of every fingerprint, so the order is an interface rather than a
 * presentation choice.
 */
export function scopePath(scope: FindingScope | undefined): string {
  if (!scope) return "";
  return [scope.project, scope.environment, scope.namespace, scope.node, scope.name].filter(Boolean).join("/");
}

/** What a finding is about, in a phrase: `node node-b`, `environment
 * shop/shop-production/app`, `platform`. */
export function scopeLabel(scope: FindingScope | undefined): string {
  if (!scope) return "";
  const path = scopePath(scope);
  return path ? `${scope.kind} ${path}` : scope.kind;
}

/** Where a finding sends the reader, as a router location. */
export interface EvidenceLocation {
  path: string;
  query: Record<string, string>;
}

/**
 * A finding's evidence link, parsed.
 *
 * Evidence is a dashboard path, relative because the dashboard and the API
 * answering with it are one origin. Anything that is not — an absolute URL, a
 * protocol-relative one — is refused rather than rendered: a link out of the
 * dashboard is not evidence, and a finding is a string the operator will click.
 */
export function evidenceLocation(evidence: string | undefined): EvidenceLocation | null {
  const link = (evidence ?? "").trim();
  if (!link.startsWith("/") || link.startsWith("//")) return null;
  // The base is never used — `link` is already absolute-path — and only exists
  // because URL parsing needs one.
  const url = new URL(link, "http://dashboard.invalid");
  return { path: url.pathname, query: Object.fromEntries(url.searchParams) };
}

/** The screen a piece of evidence points at, named. A link that says `Nodes`
 * is a link somebody follows; one that says `/platform/nodes?node=node-b` is a
 * path somebody reads. */
export function evidenceLabel(evidence: string | undefined): string {
  const location = evidenceLocation(evidence);
  if (!location) return "";
  const named: Record<string, string> = {
    "/platform": "Platform",
    "/platform/nodes": "Nodes",
    "/platform/workloads": "Workloads",
    "/platform/edge": "Edge",
    "/platform/addons": "Addons",
    "/platform/storage": "Storage",
    "/platform/events": "Events",
    "/builds": "Builds",
  };
  if (named[location.path]) return named[location.path];
  const [, section] = location.path.split("/");
  switch (section) {
    case "environments":
      return "Environment";
    case "projects":
      return "Project";
    case "builds":
      return "Build";
    default:
      return "Evidence";
  }
}

/** How many problems are firing. `unknown` is not among them: a rule that could
 * not be evaluated is in `unreadable`, and counting it here would turn a store
 * outage into a problems count. */
export function problemCount(counts: SignalCounts | undefined): number {
  if (!counts) return 0;
  return counts.critical + counts.warning + counts.info;
}

/** "2 problems", "1 problem", "no problems". */
export function problemsSentence(counts: SignalCounts | undefined): string {
  const total = problemCount(counts);
  if (total === 0) return "no problems";
  return `${total} problem${total === 1 ? "" : "s"}`;
}

/**
 * The sentence an unreadable list gets.
 *
 * This is the whole reason `unreadable` is rendered apart from the findings: a
 * platform reporting no problems because it could not check anything is the
 * exact failure the design exists to prevent, and the difference has to be a
 * sentence rather than an absence.
 */
export function unreadableSentence(unreadable: InputFailure[] | undefined, findings = 0): string {
  const count = unreadable?.length ?? 0;
  if (count === 0) return "";
  const inputs = `${count} input${count === 1 ? "" : "s"}`;
  return findings === 0
    ? `Nothing is reported wrong — but ${inputs} could not be read, so parts of this platform were not checked at all.`
    : `${inputs} could not be read, so the list below is incomplete: the rules that depend on them were not evaluated.`;
}

/** Whether an answer has anything at all to say — findings, or inputs it could
 * not read. An environment strip renders only when this is true. */
export function hasSomethingToSay(answer: SignalsAnswer | null | undefined): boolean {
  if (!answer) return false;
  return (answer.items?.length ?? 0) > 0 || (answer.unreadable?.length ?? 0) > 0;
}
