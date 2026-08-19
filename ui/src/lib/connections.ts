import type { Connection } from "./api";

/**
 * Turning the connections a caller can see into the entries of a picker.
 *
 * `GET /connections` answers two shapes (docs/AUTH.md, and
 * `listConnections`): an operator gets the connection — provider, conditions,
 * the lot — and everybody else gets the picker's own shape, which is a name,
 * what it can back, and whether the platform has it working. This module is
 * the one place that reads both, so that a project's git source and registry
 * are chosen the same way whichever of the two arrived.
 *
 * **An entry that cannot be picked says why.** Omitting it leaves somebody
 * looking for a connection they were told exists, and the two reasons are
 * different problems with different owners: a connection that provides the
 * wrong capability is the wrong connection, and one the platform has not got
 * working is the operator's to fix. Neither is anything a member can do from
 * here, which is exactly why the sentence has to name what happened.
 */

/** One entry of a connection picker, as `USelect` takes it. */
export interface ConnectionChoice {
  /** The name, what it is, and — when there is one — the caveat. */
  label: string;
  value: string;
  /** Set only for an entry the API would refuse. */
  disabled?: boolean;
  /** The caveat on its own, for a line under the field. Empty when there is
   * nothing to say. */
  note: string;
}

/**
 * Whether the platform has this connection working: it reached the provider
 * and the provider accepted the credential.
 *
 * The picker's shape says so outright. The operator's does not — it carries
 * the conditions instead — so the same verdict is read off `CredentialsValid`,
 * which is the condition the choice view is built from. Undefined means
 * neither was there to read: an older API, or a connection nothing has
 * assessed, and it is not the same answer as "no".
 */
export function connectionReady(connection: Connection): boolean | undefined {
  if (typeof connection.ready === "boolean") return connection.ready;
  const condition = connection.conditions?.find((c) => c.type === "CredentialsValid");
  if (!condition) return undefined;
  return condition.status === "True";
}

/** Whether this connection can back the capability being chosen for. A
 * connection that has reported none has not been assessed — the API accepts
 * it (`requireConnection`), so the picker does too. */
export function connectionProvides(connection: Connection, capability: string): boolean {
  return !connection.capabilities?.length || connection.capabilities.includes(capability);
}

/**
 * The picker's entries for one capability, in the order the API answered.
 *
 * Nothing is filtered out. What the API would refuse is disabled — that is
 * the capability check, and it is a 400 from `requireConnection` rather than
 * an opinion held here. Everything else is selectable with a caveat, because
 * neither an unassessed connection nor one whose credential has not been
 * verified is refused: the project is created, and its own conditions say
 * whether it works.
 */
export function connectionChoices(connections: Connection[], capability: string): ConnectionChoice[] {
  return connections.map((connection) => {
    const what = connection.provider ? `${connection.name} · ${connection.provider}` : connection.name;
    if (!connectionProvides(connection, capability)) {
      const note = `does not provide ${capability}`;
      return { label: `${what} — ${note}`, value: connection.name, disabled: true, note };
    }
    if (!connection.capabilities?.length) {
      const note = "the platform has not assessed what it can back yet";
      return { label: `${what} — not assessed yet`, value: connection.name, note };
    }
    if (connectionReady(connection) === false) {
      const note = "the platform has not got this connection working — an operator has to fix it";
      return { label: `${what} — not working`, value: connection.name, note };
    }
    return { label: what, value: connection.name, note: "" };
  });
}

/** The entries a project could actually be created with. Zero of them is the
 * empty state — "no gitSource connection yet" — and it is a different message
 * from a list with entries that all carry caveats. */
export function selectableChoices(choices: ConnectionChoice[]): ConnectionChoice[] {
  return choices.filter((choice) => !choice.disabled);
}

/** The caveat on the entry currently chosen, for the line under the field. */
export function noteFor(choices: ConnectionChoice[], value: string | undefined): string {
  if (!value) return "";
  return choices.find((choice) => choice.value === value)?.note ?? "";
}
