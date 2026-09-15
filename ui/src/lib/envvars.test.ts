import { describe, expect, it } from "vitest";
import {
  envVarDrafts,
  envVarWrites,
  newEnvVarDraft,
  renamed,
  withoutShownValues,
  withShownValues,
} from "./envvars";

describe("env var drafts", () => {
  it("carries presence over from the API, and never a value", () => {
    const drafts = envVarDrafts([
      { name: "PUBLIC_URL", set: true, previewSet: true },
      { name: "LOG_LEVEL", set: false, previewSet: false },
      { name: "API_KEY", set: false, previewSet: false, fromSecret: { name: "shop-api-key", key: "key" } },
    ]);
    expect(drafts[0]).toMatchObject({ name: "PUBLIC_URL", set: true, previewSet: true });
    expect(drafts[0].value).toBeUndefined();
    expect(drafts[0].previewValue).toBeUndefined();
    expect(drafts[1].set).toBe(false);
    expect(drafts[2].fromSecret).toEqual({ name: "shop-api-key", key: "key" });
  });

  it("leaves a variable nobody touched out of the write, so its value survives", () => {
    const drafts = envVarDrafts([{ name: "PUBLIC_URL", set: true, previewSet: true }]);
    expect(envVarWrites(drafts)).toEqual([{ name: "PUBLIC_URL" }]);
  });

  it("sends what was typed, empty string included — that is how a value is cleared", () => {
    const drafts = envVarDrafts([{ name: "PUBLIC_URL", set: true, previewSet: true }]);
    drafts[0].value = "https://shop.example.com";
    expect(envVarWrites(drafts)).toEqual([{ name: "PUBLIC_URL", value: "https://shop.example.com" }]);
    drafts[0].previewValue = "";
    expect(envVarWrites(drafts)).toEqual([
      { name: "PUBLIC_URL", value: "https://shop.example.com", previewValue: "" },
    ]);
  });

  it("writes a reference as a reference, with no value beside it", () => {
    const drafts = envVarDrafts([
      { name: "DATABASE_URL", set: false, previewSet: false, fromClaim: { name: "shop-db", key: "url" } },
    ]);
    // Even if something typed into it, a claim-backed variable writes its
    // reference alone: naming two sources is a 400.
    drafts[0].value = "postgres://typed";
    expect(envVarWrites(drafts)).toEqual([{ name: "DATABASE_URL", fromClaim: { name: "shop-db", key: "url" } }]);
  });

  it("knows a renamed variable has left its value behind", () => {
    const drafts = envVarDrafts([{ name: "PUBLIC_URL", set: true, previewSet: false }]);
    expect(renamed(drafts[0])).toBe(false);
    drafts[0].name = "SITE_URL";
    // Values are kept by name, so the rename lands a variable with none —
    // which the form says out loud rather than saving quietly.
    expect(renamed(drafts[0])).toBe(true);
    expect(envVarWrites(drafts)).toEqual([{ name: "SITE_URL" }]);
    // A variable the form added is not a rename of anything.
    expect(renamed(newEnvVarDraft())).toBe(false);
  });

  it("shows a literal once it has been read, and never what a reference points at", () => {
    const drafts = envVarDrafts([
      { name: "LOG_LEVEL", set: true, previewSet: true },
      { name: "API_KEY", set: false, previewSet: false, fromSecret: { name: "kitchen-project-secrets", key: "api" } },
      { name: "DATABASE_URL", set: false, previewSet: false, fromClaim: { name: "shop-db", key: "url" } },
    ]);
    // What the values route answers: the literals, and the references
    // restated with no literal on them.
    const shown = withShownValues(drafts, [
      { name: "LOG_LEVEL", set: true, previewSet: true, value: "debug", previewValue: "trace" },
      { name: "API_KEY", set: false, previewSet: false, fromSecret: { name: "kitchen-project-secrets", key: "api" } },
      { name: "DATABASE_URL", set: false, previewSet: false, fromClaim: { name: "shop-db", key: "url" } },
    ]);
    expect(shown[0].shown).toEqual({ value: "debug", previewValue: "trace" });
    expect(shown[1].shown).toBeUndefined();
    expect(shown[2].shown).toBeUndefined();

    // Revealing is drawing, not editing: the write is unchanged, so a value
    // somebody looked at is still not one they sent back.
    expect(envVarWrites(shown)).toEqual([
      { name: "LOG_LEVEL" },
      { name: "API_KEY", fromSecret: { name: "kitchen-project-secrets", key: "api" } },
      { name: "DATABASE_URL", fromClaim: { name: "shop-db", key: "url" } },
    ]);

    expect(withoutShownValues(shown).every((draft) => draft.shown === undefined)).toBe(true);
  });

  it("does not type a revealed value over what somebody is writing", () => {
    const drafts = envVarDrafts([{ name: "LOG_LEVEL", set: true, previewSet: false }]);
    drafts[0].value = "warn";
    const shown = withShownValues(drafts, [
      { name: "LOG_LEVEL", set: true, previewSet: false, value: "debug" },
    ]);
    expect(shown[0].value).toBe("warn");
    expect(shown[0].shown).toEqual({ value: "debug", previewValue: undefined });
    expect(envVarWrites(shown)).toEqual([{ name: "LOG_LEVEL", value: "warn" }]);
  });

  it("opens a new variable's value field, and drops nameless rows", () => {
    const draft = newEnvVarDraft();
    expect(draft.value).toBe("");
    expect(draft.set).toBe(false);
    draft.name = " LOG_LEVEL ";
    draft.value = "debug";
    expect(envVarWrites([draft, newEnvVarDraft()])).toEqual([{ name: "LOG_LEVEL", value: "debug" }]);
  });
});
