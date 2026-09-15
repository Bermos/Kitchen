import type { EnvVar, EnvVarWrite, KeyRef } from "./api";

// The settings form's side of an environment variable.
//
// The project read reports whether a variable has a value, never what it is,
// so the form does not prefill one: a stored value reads as "•••• set" and
// replacing it means typing the new one, the same shape rotating a
// connection's credential takes. Revealing a value is a second, deliberate
// read of a route of its own (#598) — `shown` is what it fills in, and it is
// not a draft, because nothing the form sends is derived from it.

/** One variable as the form holds it. `value` and `previewValue` are the
 * replacements being typed — `undefined` is "leave the stored one alone",
 * which is what the PATCH says by leaving the field out. */
export interface EnvVarDraft {
  name: string;
  set: boolean;
  previewSet: boolean;
  fromSecret?: KeyRef;
  fromClaim?: KeyRef;
  value?: string;
  previewValue?: string;
  /** The name the stored value is filed under, for a variable read back from
   * the API. Absent on one the form has just added. */
  storedAs?: string;
  /** What the platform holds, once somebody has asked to see it. It is drawn
   * and never sent: a value the form was shown is still not a value it edits,
   * so replacing one means typing the new one, exactly as before. */
  shown?: { value?: string; previewValue?: string };
}

/** The project's variables as drafts: presence carried over, nothing typed. */
export function envVarDrafts(env: EnvVar[] | undefined): EnvVarDraft[] {
  return (env ?? []).map((v) => ({
    name: v.name,
    set: Boolean(v.set),
    previewSet: Boolean(v.previewSet),
    fromSecret: v.fromSecret,
    fromClaim: v.fromClaim,
    value: undefined,
    previewValue: undefined,
    storedAs: v.name,
  }));
}

/** Put the values route's answer onto the drafts already on screen, by name.
 *
 * It is an overlay rather than a reload: somebody may be mid-edit, and a
 * revealed value must not type over what they were writing. A variable the
 * answer does not mention keeps what it had — a reference-backed one is
 * answered with no literal, which is the platform declining to resolve it and
 * not a value of empty. */
export function withShownValues(drafts: EnvVarDraft[], env: EnvVar[]): EnvVarDraft[] {
  const answered = new Map(env.map((v) => [v.name, v]));
  return drafts.map((draft) => {
    const found = draft.storedAs === undefined ? undefined : answered.get(draft.storedAs);
    if (!found || found.fromSecret || found.fromClaim) return draft;
    return { ...draft, shown: { value: found.value, previewValue: found.previewValue } };
  });
}

/** Take every revealed value back off the screen. */
export function withoutShownValues(drafts: EnvVarDraft[]): EnvVarDraft[] {
  return drafts.map(({ shown: _shown, ...draft }) => draft);
}

/** Whether a variable has been renamed away from the name its value is stored
 * under. Values are kept by name, so a rename does not carry one along — and
 * since the form cannot copy a value it was never shown, the only honest thing
 * it can do is say so and ask for it again. */
export function renamed(draft: EnvVarDraft): boolean {
  return draft.storedAs !== undefined && draft.name.trim() !== draft.storedAs;
}

/** A variable the "Add variable" button just made: it replaces nothing, so its
 * value field is open from the start. */
export function newEnvVarDraft(): EnvVarDraft {
  return { name: "", set: false, previewSet: false, value: "", previewValue: undefined };
}

/** What the PATCH carries: every variable by name, its reference if it has
 * one, and only the values someone actually typed. A value left out keeps the
 * stored one; an empty one clears it. */
export function envVarWrites(drafts: EnvVarDraft[]): EnvVarWrite[] {
  return drafts
    .filter((draft) => draft.name.trim() !== "")
    .map((draft) => {
      const write: EnvVarWrite = { name: draft.name.trim() };
      if (draft.fromSecret) {
        write.fromSecret = draft.fromSecret;
        return write;
      }
      if (draft.fromClaim) {
        write.fromClaim = draft.fromClaim;
        return write;
      }
      if (draft.value !== undefined) write.value = draft.value;
      if (draft.previewValue !== undefined) write.previewValue = draft.previewValue;
      return write;
    });
}
