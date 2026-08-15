/**
 * The query bar's side of Kitchen's log query language.
 *
 * The parser lives in the operator, which is where it has to be — it is what
 * compiles a query into a ClickHouse predicate. What the browser needs is
 * smaller and different: to *edit* a query as text without destroying what the
 * user wrote. Clicking `error` in the level facet should add `level:error`,
 * clicking it again should take it away, and neither should disturb the rest of
 * the line.
 *
 * So this splits a query into its top-level terms and puts them back together,
 * and does not try to understand them. A term is a run of non-space characters
 * with quoted and `/…/` runs held together, exactly as the operator lexes it.
 * Anything with brackets in it is left alone rather than half-understood: the
 * facets stop offering to toggle and the user edits the text, which is honest
 * about what this knows.
 */

/** One clause of a query: a field, a value, and whether it is negated. */
export interface Clause {
  field: string;
  value: string;
  negated: boolean;
}

/** Split a query into its terms, keeping quoted and regex runs together. */
export function splitTerms(query: string): string[] {
  const terms: string[] = [];
  let current = "";
  let delimiter: string | null = null;

  for (let i = 0; i < query.length; i++) {
    const char = query[i];
    if (delimiter) {
      current += char;
      if (char === "\\" && i + 1 < query.length) current += query[++i];
      else if (char === delimiter) delimiter = null;
      continue;
    }
    if (char === '"' || (char === "/" && current.endsWith(":"))) {
      delimiter = char;
      current += char;
      continue;
    }
    if (/\s/.test(char)) {
      if (current) terms.push(current);
      current = "";
      continue;
    }
    current += char;
  }
  if (current) terms.push(current);
  return terms;
}

/**
 * Whether a query is simple enough to edit by clicking: a flat list of terms,
 * no brackets and no alternation. A query that is not stays the user's to edit
 * by hand.
 *
 * Brackets inside a quoted value are part of the value, not structure, so the
 * quoted runs come out before the question is asked.
 */
export function isEditable(query: string): boolean {
  return !splitTerms(query).some((term) => /[()]/.test(bareOf(term)) || term === "OR" || term === "||");
}

/** A term with its quoted and regex runs taken out, leaving only structure. */
function bareOf(term: string): string {
  return term.replace(/"(?:\\.|[^"\\])*"/g, "").replace(/:\/(?:\\.|[^/\\])*\//g, ":");
}

/**
 * Render a value, quoting it when it holds anything that would not survive
 * bare: whitespace and brackets would end the term, `*` and `?` would become
 * wildcards, a comma would become an alternation, and a leading `-` a negation.
 */
export function quoteValue(value: string): string {
  if (value !== "" && !/[\s"()*?,:\-\\/]/.test(value)) return value;
  return `"${value.replace(/(["\\])/g, "\\$1")}"`;
}

/** Render one clause the way the query bar spells it. */
export function renderClause(clause: Clause): string {
  return `${clause.negated ? "-" : ""}${clause.field}:${quoteValue(clause.value)}`;
}

/** Whether the query already carries this clause. */
export function hasClause(query: string, clause: Clause): boolean {
  return clausesOf(query).some(
    (existing) =>
      existing.field === clause.field &&
      existing.value === clause.value &&
      existing.negated === clause.negated,
  );
}

/**
 * Add a clause, or take it away if it is already there. This is what a facet
 * click does, and the reason it is a toggle is that a filter you cannot undo by
 * clicking the same thing again is a filter you have to edit text to escape.
 */
export function toggleClause(query: string, clause: Clause): string {
  if (hasClause(query, clause)) return removeClause(query, clause);
  return [...splitTerms(query), renderClause(clause)].join(" ");
}

/**
 * Take a clause away, however it was spelled — the user may have typed it
 * unquoted where a facet click would have quoted it.
 */
export function removeClause(query: string, clause: Clause): string {
  return splitTerms(query)
    .filter((term) => !isClause(term, clause))
    .join(" ");
}

function isClause(term: string, clause: Clause): boolean {
  const parsed = clausesOf(term);
  return (
    parsed.length === 1 &&
    parsed[0].field === clause.field &&
    parsed[0].value === clause.value &&
    parsed[0].negated === clause.negated
  );
}

/**
 * The clauses of a query that can be shown as removable chips. Terms this does
 * not recognise — bare search words, comparisons, regular expressions — are
 * left out rather than guessed at; they are still in the text.
 */
export function clausesOf(query: string): Clause[] {
  const clauses: Clause[] = [];
  for (const term of splitTerms(query)) {
    const negated = term.startsWith("-") || term.startsWith("!");
    const body = negated ? term.slice(1) : term;
    const colon = body.indexOf(":");
    if (colon <= 0) continue;
    const field = body.slice(0, colon);
    if (!/^[A-Za-z_][A-Za-z0-9_.-]*$/.test(field)) continue;
    const raw = body.slice(colon + 1);
    if (raw === "" || /^[<>]/.test(raw) || raw.startsWith("/")) continue;
    clauses.push({ field, value: unquote(raw), negated });
  }
  return clauses;
}

function unquote(value: string): string {
  if (value.length >= 2 && value.startsWith('"') && value.endsWith('"')) {
    return value.slice(1, -1).replace(/\\(.)/g, "$1");
  }
  return value;
}
