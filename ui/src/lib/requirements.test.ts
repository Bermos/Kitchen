import { describe, expect, it } from "vitest";
import type { Me } from "./api";
import { bundleDigestProblem, formatParameters, mayOwn, parseOwners, parseParameters } from "./requirements";

// Ownership decides whether the requirements panel offers an edit at all, and
// it mirrors the server's own matching: subjects exactly, addresses
// case-insensitively, operators always. Getting this wrong in either
// direction is visible — a control that is not drawn for an owner, or one
// drawn for somebody the API will refuse.

const me = (over: Partial<Me> = {}): Me => ({
  subject: "user_01",
  email: "anna@example.com",
  platformRole: "member",
  ...over,
});

describe("mayOwn", () => {
  it("admits an operator whatever the list says", () => {
    expect(mayOwn([], null, true)).toBe(true);
    expect(mayOwn(undefined, me(), true)).toBe(true);
  });

  it("matches a subject exactly", () => {
    expect(mayOwn(["user_01"], me(), false)).toBe(true);
    expect(mayOwn(["user_02"], me(), false)).toBe(false);
    expect(mayOwn(["USER_01"], me(), false)).toBe(false);
  });

  it("matches an address case-insensitively", () => {
    expect(mayOwn(["Anna@Example.com"], me(), false)).toBe(true);
    expect(mayOwn(["other@example.com"], me(), false)).toBe(false);
  });

  it("satisfies nothing before /me has answered, and nothing on an empty list", () => {
    expect(mayOwn(["user_01"], null, false)).toBe(false);
    expect(mayOwn([], me(), false)).toBe(false);
    expect(mayOwn(undefined, me(), false)).toBe(false);
  });
});

describe("bundleDigestProblem", () => {
  it("admits the digest form and the empty removal", () => {
    expect(bundleDigestProblem("sha256:" + "ab".repeat(32))).toBeUndefined();
    expect(bundleDigestProblem("")).toBeUndefined();
    expect(bundleDigestProblem("   ")).toBeUndefined();
  });

  it("names anything else", () => {
    for (const digest of ["latest", "sha256:" + "a".repeat(63), "sha256:" + "A".repeat(64)]) {
      expect(bundleDigestProblem(digest)).toContain("sha256:<64 hex");
    }
  });
});

describe("parameters as lines", () => {
  it("round-trips through the form", () => {
    const text = formatParameters({ maxSeverity: "high", gate: "trivy" });
    expect(text).toBe("gate=trivy\nmaxSeverity=high");
    expect(parseParameters(text).parameters).toEqual({ gate: "trivy", maxSeverity: "high" });
  });

  it("skips blanks and keeps `=` in values", () => {
    expect(parseParameters("\n a = b=c \n").parameters).toEqual({ a: "b=c" });
  });

  it("names a line that is not name=value", () => {
    expect(parseParameters("justaname").problem).toContain("justaname");
    expect(parseParameters("=value").problem).toBeTruthy();
  });
});

describe("parseOwners", () => {
  it("is one owner per line, blanks skipped", () => {
    expect(parseOwners(" user_01 \n\nanna@example.com\n")).toEqual(["user_01", "anna@example.com"]);
    expect(parseOwners("")).toEqual([]);
  });
});
