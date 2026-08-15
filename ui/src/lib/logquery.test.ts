import { describe, expect, it } from "vitest";
import { clausesOf, hasClause, isEditable, quoteValue, renderClause, splitTerms, toggleClause } from "./logquery";

describe("splitTerms", () => {
  it("splits on whitespace", () => {
    expect(splitTerms("level:error service:shop")).toEqual(["level:error", "service:shop"]);
  });

  it("holds a quoted phrase together", () => {
    expect(splitTerms('message:"connection refused" level:error')).toEqual([
      'message:"connection refused"',
      "level:error",
    ]);
  });

  it("holds a regular expression together", () => {
    expect(splitTerms("message:/GET \\/works/ level:error")).toEqual(["message:/GET \\/works/", "level:error"]);
  });

  it("has nothing to split in an empty query", () => {
    expect(splitTerms("   ")).toEqual([]);
  });
});

describe("quoteValue", () => {
  it("leaves an ordinary value bare", () => {
    expect(quoteValue("error")).toBe("error");
  });

  it("quotes what would otherwise change the query's meaning", () => {
    expect(quoteValue("shop production")).toBe('"shop production"');
    expect(quoteValue("a,b")).toBe('"a,b"');
    expect(quoteValue("-v")).toBe('"-v"');
    expect(quoteValue("*")).toBe('"*"');
  });

  it("escapes the quotes it adds around", () => {
    expect(quoteValue('say "hi"')).toBe('"say \\"hi\\""');
  });
});

describe("toggleClause", () => {
  const clause = { field: "level", value: "error", negated: false };

  it("adds a clause to an empty query", () => {
    expect(toggleClause("", clause)).toBe("level:error");
  });

  it("appends without disturbing what is already there", () => {
    expect(toggleClause('message:"connection refused"', clause)).toBe('message:"connection refused" level:error');
  });

  it("takes the clause away when it is already there", () => {
    expect(toggleClause("service:shop level:error", clause)).toBe("service:shop");
  });

  // A facet click quotes what it adds; a person typing does not. Clicking the
  // same value twice has to undo itself either way.
  it("takes away a clause the user spelled differently", () => {
    expect(toggleClause('service:shop level:"error"', clause)).toBe("service:shop");
  });

  it("keeps a negation distinct from its positive", () => {
    expect(toggleClause("-level:error", clause)).toBe("-level:error level:error");
  });
});

describe("clausesOf", () => {
  it("reads the field, the value and the negation", () => {
    expect(clausesOf('-level:error service:"shop front"')).toEqual([
      { field: "level", value: "error", negated: true },
      { field: "service", value: "shop front", negated: false },
    ]);
  });

  // What it cannot render back as a chip it leaves out rather than guessing at.
  // The text still holds it.
  it("passes over what it does not recognise", () => {
    expect(clausesOf("timeout http.status:>=500 message:/GET/")).toEqual([]);
  });
});

describe("hasClause", () => {
  it("finds a clause whatever quoting it was written with", () => {
    expect(hasClause('level:"error"', { field: "level", value: "error", negated: false })).toBe(true);
    expect(hasClause("level:error", { field: "level", value: "error", negated: true })).toBe(false);
  });
});

describe("isEditable", () => {
  it("recognises a flat list of terms", () => {
    expect(isEditable("level:error service:shop")).toBe(true);
  });

  // Clicking a facet on a query with alternation in it would silently change
  // what it means, so the facets stop offering and the user edits the text.
  it("declines a query it would have to understand", () => {
    expect(isEditable("(level:error OR level:fatal)")).toBe(false);
    expect(isEditable("level:error OR level:fatal")).toBe(false);
  });

  it("is not fooled by brackets inside a quoted value", () => {
    expect(isEditable('message:"(deprecated)"')).toBe(true);
  });
});

describe("renderClause", () => {
  it("spells a negation with a leading dash", () => {
    expect(renderClause({ field: "source", value: "cluster", negated: true })).toBe("-source:cluster");
  });
});
