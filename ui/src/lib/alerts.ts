/**
 * How a delivery is read on screen.
 *
 * `lib/signals.ts` is about a *finding* — what is wrong, and how bad. This is
 * about a **delivery**: the pair (fingerprint, audience), what its reader is
 * meant to do about it, and what anybody has already done. The two screens that
 * render deliveries — the fleet's Alerts and a project's — are the same rows
 * narrowed differently, so everything about how one reads lives here rather
 * than in either of them.
 *
 * ## The word the top tier does not get
 *
 * The API calls it `page`, because the model is the industry's and renaming it
 * there would make every comparison to that model a translation. The dashboard
 * calls it **Act now**, and that is deliberate: there is a delivery mechanism —
 * an outbound subscription with a tier filter — and *nothing in it wakes a
 * person*. A screen that said "PAGE" in red would be the platform claiming a
 * capability it does not have, and the first time somebody found out otherwise
 * would be the morning after an outage. The word is reserved until something
 * behind it actually pages.
 *
 * The other two are named for what they ask of the reader rather than for the
 * mechanism, for the same reason: **Needs a fix** is a ticket, and **For the
 * record** is a log — a row that appears on the list it belongs to and notifies
 * nobody, which is what makes it a tier rather than an absence.
 */

import type { Alert, Audience, Tier } from "./api";
import { timeAgo } from "./format";
import type { Tone } from "./status";

/** What a tier is called on screen. */
export function tierLabel(tier: Tier): string {
  switch (tier) {
    case "page":
      return "Act now";
    case "ticket":
      return "Needs a fix";
    default:
      return "For the record";
  }
}

/** One line of what the tier asks of whoever is reading it. */
export function tierMeaning(tier: Tier): string {
  switch (tier) {
    case "page":
      return "Urgent, and yours to act on now.";
    case "ticket":
      return "A fix is owed. Nobody needs waking.";
    default:
      return "A data point. It notifies nobody.";
  }
}

/** The colour a tier carries. It is the row's own tone, and deliberately not
 * the severity's: a critical condition somebody is already fixing is a ticket,
 * and reading as a five-alarm row would be the list lying about what is left
 * to do. */
export function tierTone(tier: Tier): Tone {
  switch (tier) {
    case "page":
      return "error";
    case "ticket":
      return "warning";
    default:
      return "neutral";
  }
}

export function tierIcon(tier: Tier): string {
  switch (tier) {
    case "page":
      return "i-lucide-siren";
    case "ticket":
      return "i-lucide-wrench";
    default:
      return "i-lucide-notebook-pen";
  }
}

/** The order the screens render in, highest first. */
export function tierRank(tier: Tier): number {
  switch (tier) {
    case "page":
      return 2;
    case "ticket":
      return 1;
    default:
      return 0;
  }
}

/** Worst first, then by fingerprint and audience — the API's own order,
 * applied again so a dashboard talking to an operator a version behind still
 * renders the list the way this screen promises to. */
export function sortAlerts(alerts: Alert[] | null | undefined): Alert[] {
  return [...(alerts ?? [])].sort((a, b) => {
    const rank = tierRank(b.tier) - tierRank(a.tier);
    if (rank !== 0) return rank;
    if (a.signal !== b.signal) return a.signal < b.signal ? -1 : 1;
    if (a.fingerprint !== b.fingerprint) return a.fingerprint < b.fingerprint ? -1 : 1;
    return a.audience < b.audience ? -1 : a.audience > b.audience ? 1 : 0;
  });
}

/** Who a delivery was made to, in a word. */
export function audienceLabel(audience: Audience): string {
  return audience === "operator" ? "Operator" : "Project";
}

/**
 * What is being done about a delivery, as one sentence.
 *
 * Empty means nothing is, which is the state the whole escalation clock is
 * about — so it is rendered as a sentence of its own by the screen rather than
 * as a blank cell here.
 */
export function mitigationSentence(alert: Alert): string {
  const state = alert.mitigation;
  if (!state) return "";
  if (state.silencedUntil && new Date(state.silencedUntil) > new Date()) {
    const reason = state.silenceReason ? ` — ${state.silenceReason}` : "";
    return `Silenced by ${state.silencedBy ?? "somebody"}${reason}`;
  }
  if (state.claimedBy) return `Claimed by ${state.claimedBy}`;
  if (state.acknowledged) {
    // "Rolled back" reads as what happened; "acknowledged" reads as a button
    // somebody pressed. The API says which, so the screen can say it too.
    const how = state.acknowledgedVia === "action" ? "Being worked on by" : "Acknowledged by";
    return `${how} ${state.acknowledgedBy ?? "somebody"}`;
  }
  return "";
}

/**
 * When the words on a row were read.
 *
 * It is not the question the age on the row answers, and that was the whole
 * bug: the age is the *condition's* — ten hours open — and it sat next to a
 * figure that was the reading's. A volume that opened at 85% and had since
 * filled to 89% went on saying 85% with nothing distinguishing "85% now" from
 * "85% when this opened", so this screen and the storage one read as
 * disagreeing (#532).
 *
 * The API now sends the reading it last took, and says which it is. `opened`
 * is not a failure: nothing holds a newer reading of a condition that has just
 * been handed over, or one that resolved between the platform's last look and
 * this one, and saying so is better than dating it as if it were current.
 */
export function readingSentence(alert: Alert): string {
  if (!alert.reading || !alert.readingAt) return "";
  if (alert.reading === "opened") {
    return `reading from when this opened, ${timeAgo(alert.readingAt)}`;
  }
  return `reading taken ${timeAgo(alert.readingAt)}`;
}

/** Whether a silence stands on this delivery right now. An expired silence is
 * not a silence: the expiry is the whole reason one is safe to grant. */
export function silenced(alert: Alert): boolean {
  const until = alert.mitigation?.silencedUntil;
  return Boolean(until && new Date(until) > new Date());
}

/** "2 need acting on now · 1 needs a fix", or nothing at all. */
export function alertsSentence(counts: Alert[] | null | undefined): string {
  const alerts = counts ?? [];
  const page = alerts.filter((alert) => alert.tier === "page").length;
  const ticket = alerts.filter((alert) => alert.tier === "ticket").length;
  const parts: string[] = [];
  if (page) parts.push(`${page} to act on now`);
  if (ticket) parts.push(`${ticket} needing a fix`);
  if (!parts.length) return "Nothing needs acting on.";
  return parts.join(" · ");
}

/** The default expiry a silence form offers: tomorrow morning, near enough.
 * A silence has to have an end, and the form must not make the reader invent
 * one before it will let them past. */
export function defaultSilenceUntil(now = new Date()): string {
  const until = new Date(now.getTime() + 24 * 60 * 60 * 1000);
  // `datetime-local` wants no zone and no seconds.
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${until.getFullYear()}-${pad(until.getMonth() + 1)}-${pad(until.getDate())}T${pad(until.getHours())}:${pad(until.getMinutes())}`;
}

/** The longest a silence may last, in days. It is the API's bound, restated
 * here so the form can refuse before the round trip does. */
export const MAX_SILENCE_DAYS = 30;
