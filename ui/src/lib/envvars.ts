import type { EnvVar, EnvVarWrite, KeyRef } from "./api";

// The settings form's side of an environment variable. The API reports whether
// a variable has a value, never what it is, so the form cannot prefill one:
// a stored value reads as "•••• set" and replacing it means typing the new one,
// the same shape rotating a connection's credential takes.

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
