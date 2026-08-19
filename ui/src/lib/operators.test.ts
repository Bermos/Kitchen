import { describe, expect, it } from "vitest";
import type { Condition, Operator } from "./api";
import {
  alreadyListed,
  describeOperator,
  isLastOperator,
  operatorList,
  operatorWrites,
  operatorsCondition,
  operatorsNote,
  operatorsState,
  wasSeeded,
  withOperator,
  withoutOperator,
} from "./operators";

// The operator list's three states are the whole reason this module exists:
// `null`, `[]` and a list are three different answers, and the payload
// distinguishes them on purpose — the field carries no omitempty. Reading `[]`
// as "not seeded yet" would tell somebody who deliberately narrowed the list
// to nobody that the reconciler is about to fill it back in.

const anna: Operator = { subject: "user_01H8X", email: "anna@example.com" };
const grace: Operator = { subject: "user_01J2Q", email: "grace@example.com" };

const condition = (reason: string): Condition => ({
  type: "OperatorsConfigured",
  status: "True",
  reason,
  message: `${reason} happened`,
  lastTransitionTime: "2026-08-19T09:12:44Z",
});

describe("which state the operator list is in", () => {
  it("reads null as nobody having ever named one", () => {
    expect(operatorsState(null)).toBe("unnamed");
  });

  it("reads an empty list as somebody having narrowed it to nobody", () => {
    expect(operatorsState([])).toBe("nobody");
  });

  it("reads a list as what it says", () => {
    expect(operatorsState([anna])).toBe("named");
  });

  it("reads an absent field as an API that does not serve the list, which is a fourth thing", () => {
    expect(operatorsState(undefined)).toBe("unserved");
  });

  it("hands every state a list to render, empty where there is none", () => {
    expect(operatorList(null)).toEqual([]);
    expect(operatorList(undefined)).toEqual([]);
    expect(operatorList([anna])).toEqual([anna]);
  });
});

describe("how the list got the way it is", () => {
  it("finds the reconciler's own account of it", () => {
    expect(operatorsCondition([condition("OperatorsSeeded")])?.reason).toBe("OperatorsSeeded");
    expect(operatorsCondition([{ ...condition("X"), type: "Ready" }])).toBeUndefined();
    expect(operatorsCondition(undefined)).toBeUndefined();
  });

  it("calls a seeded list seeded, and nothing else", () => {
    expect(wasSeeded(condition("OperatorsSeeded"))).toBe(true);
    expect(wasSeeded(condition("OperatorsNamed"))).toBe(false);
    expect(wasSeeded(condition("NobodyIsAnOperator"))).toBe(false);
    expect(wasSeeded(undefined)).toBe(false);
  });

  it("says what each reason means, and says nothing for one it has no words for", () => {
    expect(operatorsNote(condition("OperatorsSeeded"))).toContain("seeded from the accounts that existed");
    expect(operatorsNote(condition("AwaitingFirstAccount"))).toContain("bootstrap link");
    expect(operatorsNote(condition("SomethingNew"))).toBe("");
    expect(operatorsNote(undefined)).toBe("");
  });
});

describe("what a write carries", () => {
  it("names everybody who stays by the subject the list already resolved to", () => {
    expect(operatorWrites([anna, grace])).toEqual([{ subject: "user_01H8X" }, { subject: "user_01J2Q" }]);
  });

  it("adds an address for the platform to resolve, keeping everybody else by subject", () => {
    expect(withOperator([anna], "  grace@example.com ")).toEqual([
      { subject: "user_01H8X" },
      { email: "grace@example.com" },
    ]);
  });

  it("removes by subject and sends the rest back", () => {
    expect(withoutOperator([anna, grace], "user_01J2Q")).toEqual([{ subject: "user_01H8X" }]);
  });

  it("sends an empty list when the last one is removed — the API is what refuses that, not this", () => {
    expect(withoutOperator([anna], "user_01H8X")).toEqual([]);
  });

  it("leaves the list alone for a subject that is not on it", () => {
    expect(withoutOperator([anna], "user_nobody")).toEqual([{ subject: "user_01H8X" }]);
  });
});

describe("before the write is attempted", () => {
  it("spots an address already on the list, whatever its case", () => {
    expect(alreadyListed([anna], "anna@example.com")).toBe(true);
    expect(alreadyListed([anna], " ANNA@example.com ")).toBe(true);
    expect(alreadyListed([anna], "grace@example.com")).toBe(false);
    // An entry with no address matches nobody's — it is a subject grant.
    expect(alreadyListed([{ subject: "svc_ci" }], "svc_ci")).toBe(false);
  });

  it("knows when a removal would empty the list", () => {
    expect(isLastOperator([anna])).toBe(true);
    expect(isLastOperator([anna, grace])).toBe(false);
    expect(isLastOperator([])).toBe(false);
  });
});

describe("an operator as a person", () => {
  it("is the address where there is one and the subject where there is not", () => {
    expect(describeOperator(anna)).toBe("anna@example.com");
    expect(describeOperator({ subject: "svc_ci" })).toBe("svc_ci");
  });
});
