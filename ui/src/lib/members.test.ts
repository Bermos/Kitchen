import { describe, expect, it } from "vitest";
import type { Member, ProjectKey } from "./api";
import {
  keyIsUngranted,
  keyNameProblem,
  memberDetail,
  memberKind,
  memberLabel,
  roleOptionsFor,
  KEY_NAME_MAX,
  KEY_ROLE_OPTIONS,
  MEMBER_ROLE_OPTIONS,
} from "./members";

// People and CI keys arrive in one list, and the only thing the dashboard has
// to do about that is render each as what it is. These are about that seam,
// and about the key-name rule — which is the API's, checked here so a mistyped
// name is a sentence under the field rather than a 400.

const person = (over: Partial<Member> = {}): Member => ({
  subject: "user_01J2Q",
  email: "anna@example.com",
  role: "developer",
  ...over,
});

const key = (over: Partial<Member> = {}): Member => ({
  subject: "user_01K4M",
  email: "shop.nightly@machines.kitchen.local",
  role: "developer",
  kind: "key",
  name: "nightly",
  ...over,
});

describe("what kind of member a grant is about", () => {
  it("reads a key as a key", () => {
    expect(memberKind(key())).toBe("key");
  });

  it("reads everything else as an account, including a grant from before keys existed", () => {
    expect(memberKind(person())).toBe("account");
    expect(memberKind(person({ kind: "account" }))).toBe("account");
    // A kind this build has never heard of is not a key, and the list has
    // always shown accounts.
    expect(memberKind(person({ kind: "robot" }))).toBe("account");
  });
});

describe("what a member reads as", () => {
  it("names a key by its own name, not by the address the issuer filed it under", () => {
    expect(memberLabel(key())).toBe("nightly");
    expect(memberDetail(key())).toBe("shop.nightly@machines.kitchen.local");
  });

  it("falls back to the address for a key whose name did not arrive", () => {
    expect(memberLabel(key({ name: undefined }))).toBe("shop.nightly@machines.kitchen.local");
  });

  it("names a person by the address they sign in with, with the subject underneath", () => {
    expect(memberLabel(person())).toBe("anna@example.com");
    expect(memberDetail(person())).toBe("user_01J2Q");
  });

  it("shows the subject once for a grant written by hand against one", () => {
    const handWritten = person({ email: undefined, subject: "anna@example.com" });
    expect(memberLabel(handWritten)).toBe("anna@example.com");
    // Already the label; repeating it underneath says nothing.
    expect(memberDetail(handWritten)).toBe("");
  });
});

describe("the roles a member's row offers", () => {
  it("offers a person every role", () => {
    expect(roleOptionsFor(person())).toEqual(MEMBER_ROLE_OPTIONS);
  });

  it("offers a key the two the API accepts, and not admin", () => {
    const values = roleOptionsFor(key()).map((option) => option.value);
    expect(values).toEqual(["developer", "viewer"]);
    expect(values).not.toContain("admin");
  });

  it("keeps a role the grant already holds, so no row shows an empty select", () => {
    const values = roleOptionsFor(key({ role: "admin" })).map((option) => option.value);
    expect(values).toContain("admin");
    expect(values.slice(0, 2)).toEqual(KEY_ROLE_OPTIONS.map((option) => option.value));
  });
});

describe("what is wrong with a key's name", () => {
  it("accepts a DNS label", () => {
    expect(keyNameProblem("nightly")).toBe("");
    expect(keyNameProblem("release-bot-2")).toBe("");
    expect(keyNameProblem("  nightly  ")).toBe("");
  });

  it("asks for one when there is none", () => {
    expect(keyNameProblem("")).toContain("needs a name");
    expect(keyNameProblem("   ")).toContain("needs a name");
  });

  it("refuses what a DNS label is not, and says what one is", () => {
    for (const name of ["Nightly", "night_ly", "-nightly", "nightly-", "night ly", "nightly.ci"]) {
      expect(keyNameProblem(name)).toContain("lowercase letters, digits and dashes");
    }
  });

  it("counts the characters and says how many there are", () => {
    const long = "n".repeat(KEY_NAME_MAX + 1);
    expect(keyNameProblem(long)).toContain(String(KEY_NAME_MAX + 1));
    expect(keyNameProblem("n".repeat(KEY_NAME_MAX))).toBe("");
  });
});

describe("a key whose grant has gone", () => {
  const listed = (over: Partial<ProjectKey> = {}): ProjectKey => ({
    name: "nightly",
    project: "shop",
    subject: "user_01K4M",
    prefix: "9f3a1c",
    created: "2026-08-19T09:12:44Z",
    role: "developer",
    ...over,
  });

  it("is the one with no role — it authenticates and can do nothing", () => {
    expect(keyIsUngranted(listed())).toBe(false);
    expect(keyIsUngranted(listed({ role: undefined }))).toBe(true);
    expect(keyIsUngranted(listed({ role: "" }))).toBe(true);
  });
});
