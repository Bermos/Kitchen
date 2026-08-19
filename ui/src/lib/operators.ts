import type { Condition, Operator, OperatorWrite } from "./api";

/**
 * Who holds the operator role, and what a screen showing that list has to be
 * careful about.
 *
 * The list is `spec.access.operators` on the `Kitchen` singleton — the one
 * every `operator` requirement in the policy table is resolved against — and
 * it is served on `GET /settings` and written by `PATCH /settings`. It is
 * worth a panel because of what happens on an upgrade: an installation that
 * had never named an operator has the list **seeded from every account the
 * identity provider holds**, since before enforcement every one of those
 * accounts really could call every route. Reviewing what that seeded is
 * something somebody should do once, and it should be one click rather than an
 * archaeology exercise — so the panel has to make "this was seeded, nobody
 * chose it" readable, which is what `wasSeeded` and `OPERATORS_CONTEXT` are
 * for.
 *
 * Everything here is about the *list*. Whether the panel offers a write at all
 * is `may("PATCH /api/v1/settings", …)` in `./policy`, like every other
 * control.
 */

/**
 * The states the list is in, and there are four — three of them the API's own
 * distinction and one of them the dashboard's problem.
 *
 * - `unnamed` — `null`: nobody has ever said who the operators are, and the
 *   reconciler will seed the list from the accounts that exist.
 * - `nobody` — `[]`: somebody narrowed it to nobody, deliberately. It is left
 *   exactly as it is; nothing seeds over a decision.
 * - `named` — a list, meaning what it says.
 * - `unserved` — the field did not arrive at all, which is an API too old to
 *   carry it rather than any of the three above. Saying "nobody has ever named
 *   an operator" there would be inventing an answer.
 */
export type OperatorsState = "unnamed" | "nobody" | "named" | "unserved";

/** Which of the four this payload is in. The distinction between `null` and
 * `[]` is the whole point, so it is read from the value rather than from its
 * length. */
export function operatorsState(operators: Operator[] | null | undefined): OperatorsState {
  if (operators === undefined) return "unserved";
  if (operators === null) return "unnamed";
  return operators.length ? "named" : "nobody";
}

/** The list itself, and an empty one for every state that has no list. */
export function operatorList(operators: Operator[] | null | undefined): Operator[] {
  return operators ?? [];
}

/** What the state means, in the words the decision was made in. */
export const OPERATORS_STATE_NOTE: Record<OperatorsState, string> = {
  unnamed:
    "Nobody has ever named the platform's operators, so there is no list yet. The reconciler seeds one from the accounts that exist — and until it has, this is not a platform with no operators.",
  nobody:
    "The list is empty on purpose: somebody narrowed it to nobody, and no account holds the platform surface. Nothing seeds over that.",
  named: "These accounts hold the platform surface, and admin on every project, present and future.",
  unserved:
    "This installation's API does not serve the operator list. It is still enforced against — it is on the Kitchen singleton — but nothing here can read or change it.",
};

/**
 * What the reconciler's own account of the list says it is about the platform.
 *
 * The `OperatorsConfigured` condition is still worth showing next to the list:
 * the list says who, and the condition says how they got there. This is the
 * second half — the reason in the words the decision was made in, rather than
 * the message, which says what happened.
 */
export const OPERATORS_CONTEXT: Record<string, string> = {
  OperatorsNamed: "Somebody wrote this list down. It is the platform's, and nothing seeds over it.",
  OperatorsSeeded:
    "Nobody had ever named an operator, so the list was seeded from the accounts that existed — every one of which could already call every route. Narrowing it is a deliberate edit, and this screen is where it is made.",
  NobodyIsAnOperator:
    "The list is empty on purpose, so no account holds the platform surface. It is left exactly as it is.",
  AwaitingFirstAccount: "No account exists yet. The first one the bootstrap link creates becomes the first operator.",
};

/** The `OperatorsConfigured` condition, which is where the reconciler records
 * who the operators are and how they got there. */
export function operatorsCondition(conditions: Condition[] | undefined): Condition | undefined {
  return conditions?.find((c) => c.type === "OperatorsConfigured");
}

/** The context sentence for a condition, or an empty string for a reason
 * nothing here has words for. */
export function operatorsNote(condition: Condition | undefined): string {
  return OPERATORS_CONTEXT[condition?.reason ?? ""] ?? "";
}

/**
 * Whether this list was seeded rather than chosen.
 *
 * It is the reading the review exists for: a list nobody decided on, holding
 * whoever happened to have an account when the platform was upgraded. The
 * reconciler says so in the condition's reason, which is the only place that
 * fact is recorded — the list itself looks the same either way.
 */
export function wasSeeded(condition: Condition | undefined): boolean {
  return condition?.reason === "OperatorsSeeded";
}

/** An operator as a person: the address where there is one, and the opaque
 * subject where there is not. */
export function describeOperator(operator: Operator): string {
  return operator.email || operator.subject;
}

/**
 * The list as `PATCH /settings` takes it.
 *
 * Every write replaces the list wholesale, so everybody who is to stay has to
 * be in the body — and each of them is named by `subject`, which is the
 * identifier the list already resolved to. Sending the addresses back would
 * ask the identity provider to resolve them again, which is a lookup that can
 * fail for somebody who is already an operator.
 */
export function operatorWrites(operators: Operator[]): OperatorWrite[] {
  return operators.map((operator) => ({ subject: operator.subject }));
}

/** Whether this address is already on the list. The API answers a duplicate
 * with a `400`; asking first turns that into a sentence under the field. */
export function alreadyListed(operators: Operator[], email: string): boolean {
  const wanted = email.trim().toLowerCase();
  return operators.some((operator) => (operator.email ?? "").toLowerCase() === wanted);
}

/** The list with one more address on it. The new entry is an `email` for the
 * platform to resolve at the identity provider — the address is what a person
 * can type, and the `sub` is what a token will carry. */
export function withOperator(operators: Operator[], email: string): OperatorWrite[] {
  return [...operatorWrites(operators), { email: email.trim() }];
}

/** The list with one subject off it. */
export function withoutOperator(operators: Operator[], subject: string): OperatorWrite[] {
  return operatorWrites(operators.filter((operator) => operator.subject !== subject));
}

/**
 * Whether removing this operator would empty the list.
 *
 * The API refuses that with a `409` explaining itself, and that sentence is
 * rendered rather than swallowed — but the removal is also worth naming before
 * it is attempted, because the reason it is refused is the same reason nobody
 * wants to attempt it: a platform with no operator has nobody left who can
 * appoint one, and the only way back is editing the `Kitchen` object with
 * kubectl.
 */
export function isLastOperator(operators: Operator[]): boolean {
  return operators.length === 1;
}
