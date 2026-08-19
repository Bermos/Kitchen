import type { Member, ProjectKey } from "./api";

/**
 * A project's members, as the panel that renders them needs them.
 *
 * **People and CI keys are one list.** A key is a member of exactly one
 * project (docs/AUTH.md, "Machine accounts"): it is owned by a machine account
 * created for it, and that account's subject is what the project grants a role
 * to. So a key's grant appears in `GET /projects/{name}/members` like anybody
 * else's, carrying `kind: "key"` and the key's own name — and the only thing
 * the dashboard has to do about it is render it as a key rather than as a
 * person with an address nobody recognises.
 *
 * Nothing here decides what anybody may do. `kind` is a display rule and the
 * API says so; roles are resolved from the subject alone, and whether a
 * control exists is `may(...)` in `./policy`. What is here is only what a
 * member *reads* as, plus the two format rules a key's name has to satisfy —
 * which are the API's, checked here so a mistyped name is a sentence under the
 * field rather than a round trip that answers `400`.
 */

/** The two kinds of member. Anything else that ever arrives reads as an
 * account, which is what the list has always shown. */
export type MemberKind = "account" | "key";

/** What kind of member this is, defaulting to the one the list held before
 * keys existed. */
export function memberKind(member: Member): MemberKind {
  return member.kind === "key" ? "key" : "account";
}

/**
 * A member as a name rather than an identifier.
 *
 * A key is its own name — `nightly`, not the machine address the issuer filed
 * it under, which is generated and tells nobody anything. A person is the
 * address they sign in with, and the subject where there is not one: a grant
 * written by hand against an address, or an issuer that serves no directory.
 */
export function memberLabel(member: Member): string {
  if (memberKind(member) === "key" && member.name) return member.name;
  return member.email || member.subject;
}

/**
 * The line under the name: what the label left out.
 *
 * For a key that is what it is — a CI key, and the machine account it belongs
 * to is the thing the grant actually names. For a person it is the subject,
 * which is what every membership write addresses them by, folded away only
 * when it is the only name there is and already shown above.
 */
export function memberDetail(member: Member): string {
  if (memberKind(member) === "key") return member.email || member.subject;
  return member.email ? member.subject : "";
}

/** The project roles a *person* may be given, weakest first, each with the
 * sentence docs/AUTH.md describes it in — the field is a decision about what
 * somebody may do, not a word to pick off a list. */
export const MEMBER_ROLE_OPTIONS = [
  { label: "viewer — reads status, builds, logs; may open a protected preview", value: "viewer" },
  { label: "developer — builds, redeploys, rollbacks, env vars, domains", value: "developer" },
  { label: "admin — everything a developer may, plus membership and settings", value: "admin" },
];

/**
 * The roles a *key* may be given.
 *
 * `admin` is missing because the API refuses it, not because this list is an
 * opinion: admin is the role that issues keys, and a credential in a build
 * pipeline that can mint its own successors is one nobody can account for.
 * `developer` is the default and comes first for that reason.
 */
export const KEY_ROLE_OPTIONS = [
  { label: "developer — builds, redeploys, rollbacks (the default)", value: "developer" },
  { label: "viewer — reads builds, releases and logs", value: "viewer" },
];

/**
 * The roles this member's row may be moved between.
 *
 * A key gets the key's list, for the reason the key's list is short. A role a
 * grant already holds is added to whichever list it is missing from, so a row
 * never shows a select with nothing selected — the dashboard does not get to
 * decide that a grant which exists is not one.
 */
export function roleOptionsFor(member: Member): { label: string; value: string }[] {
  const base = memberKind(member) === "key" ? KEY_ROLE_OPTIONS : MEMBER_ROLE_OPTIONS;
  if (member.role && !base.some((option) => option.value === member.role)) {
    return [...base, { label: `${member.role} — the role this grant already holds`, value: member.role }];
  }
  return base;
}

/** One line per role, for a row that shows the role rather than offering it. */
export const ROLE_SUMMARY: Record<string, string> = {
  viewer: "reads, no writes",
  developer: "the day job",
  admin: "membership and settings too",
};

/** At most this many characters in a key's name — it is half of the machine
 * account's own address at the issuer. */
export const KEY_NAME_MAX = 32;

const KEY_NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

/**
 * What is wrong with a key's name, in a sentence that says how to fix it — or
 * an empty string when there is nothing wrong.
 *
 * The rule is the API's: a DNS label, because the name addresses the key in a
 * path and is half of an address at the identity provider. It is checked here
 * so that a capital letter is a line under the field rather than a `400`, and
 * the API is still the one that decides.
 */
export function keyNameProblem(name: string): string {
  const trimmed = name.trim();
  if (!trimmed) return "A key needs a name — it is how you revoke it later.";
  if (trimmed.length > KEY_NAME_MAX) {
    return `A key's name is at most ${KEY_NAME_MAX} characters; this one is ${trimmed.length}.`;
  }
  if (!KEY_NAME.test(trimmed)) {
    return "A key's name is lowercase letters, digits and dashes, starting and ending with a letter or digit — like nightly or release-bot.";
  }
  return "";
}

/**
 * Whether this key can authenticate and do nothing.
 *
 * A listed key's role is read from the project's grant, not from anything
 * stored on the key, so a key with no role is one whose grant has been
 * removed. The API lists it rather than hiding it, and so does the dashboard:
 * it is a live credential that no longer works, which is worth seeing.
 */
export function keyIsUngranted(key: ProjectKey): boolean {
  return !key.role;
}
