/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package clickhouse

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Kitchen's log query language: `level:error service:shop -stream:stdout`.
//
// The store is ClickHouse and the API does not hide it — the raw expression is
// still there, as `where` — but writing SQL should not be the price of asking
// "which of my errors are new". This compiles the small language the query bar
// speaks into the predicate the raw surface would have taken.
//
// Every value the user typed leaves as a `{name:Type}` parameter rather than as
// query text, so a quote or a backslash in a search term is a search term. The
// only thing this package writes into the statement is column names it chose
// itself and operators it recognised.
//
// The grammar, in the order it binds:
//
//	NOT term | -term | !term      negation
//	term term                     implicit AND
//	term AND term                 explicit, same thing
//	term OR term                  alternation
//	( … )                         grouping
//
// and a term is one of:
//
//	timeout                       case-insensitive substring of the message
//	"connection refused"          the same, as a phrase
//	level:error                   a column, matched exactly
//	level:error,fatal             either of them
//	pod:shop-*                    * and ? are wildcards
//	message:/GET \/works\?page=/  a ClickHouse regular expression
//	http.status:>=500             numeric comparison
//	trace_id:*                    the field is present and non-empty
//
// Anything that is not a known column is an attribute of the line —
// `http.status` reads `LogAttributes['http.status']`, which is where the
// collector put the line's own structured fields. That is deliberate (a user
// should not have to know which of the things they can see is a column) and it
// is the one place a typo goes quiet: `levl:error` asks for an attribute
// nothing writes and matches nothing, rather than being refused.

// logQueryColumns maps the names Kitchen's query language speaks to what they
// read in the OTel log table.
//
// The two vocabularies are deliberately not the same. Users type `level` and
// `message`; the table holds `SeverityText` and `Body`, because the collector
// writes it with a stock exporter and the exporter's schema is not ours to
// rename. The translation lives here rather than in `ALIAS` columns so that
// grepping for `SeverityText` finds every place Kitchen depends on it.
//
// `timestamp` is deliberately absent: the window is the window, set by the
// caller's since/until, and a query that could move it would make the histogram
// and the lines disagree about what they are showing.
var logQueryColumns = map[string]string{
	// Kitchen's own, materialized under the names the query language already
	// used, so there is nothing to translate. `stream` is one of these: the
	// schema lifts it out of the record's attributes precisely so that it stays
	// a column here and in the raw `where` surface.
	"source":      "source",
	"project":     "project",
	"environment": "environment",
	"build":       "build",
	// A Project's workers and scheduled jobs (#78). `run` is the Job one
	// firing of a schedule produced, which is what makes "show me what last
	// night's report job printed" one query rather than a hunt through an
	// environment's whole output.
	"process":   "process",
	"run":       "run",
	"namespace": "namespace",
	"pod":       "pod",
	"container": "container",
	"node":      "node",
	"stream":    "stream",

	// The exporter's, under its names.
	"traceId": "TraceId",
	"spanId":  "SpanId",
	"level":   logLevelColumn,
	"message": logMessageColumn,
}

// The message is a plain rename. The level is not, and the difference matters.
//
// `level` reads a *lower-cased* SeverityText. Kitchen's levels have always been
// lower case — the histogram counts `error` and `fatal`, the UI renders what it
// is given, and a facet's values are what a user clicks to build their next
// query. OTel leaves the spelling to whatever produced the line, so `ERROR`,
// `Error` and `error` all arrive; folding here is what keeps `level:error` one
// term instead of three, and keeps the facet from offering the same level twice.
//
// A line the collector parsed no severity out of has an empty SeverityText and
// so an empty level. The facet drops empty values, so such a window offers no
// level facet at all rather than one blank entry standing for everything — and
// the histogram counts the lines while reporting no errors, which is honest but
// is not the same as there being none. Filling it in is the collector's job:
// OTLP has a first-class severity field and a receiver-side parser is what sets
// it. The same is true of `TraceId` and `SpanId` above — they are populated
// from the record's own OTLP fields, not from a JSON line's `trace_id`
// attribute, which stays reachable only as `fields.trace_id`.
const (
	logLevelColumn   = "lower(SeverityText)"
	logMessageColumn = "Body"
)

// logFieldAliases are the names people reach for that are not what the column
// is called. `service` is the one that matters — every log UI has it, and in
// Kitchen the thing being served is the project.
//
// The trace keys are here as well as in logQueryColumns because a field name
// is matched case-insensitively and those two entries are the only ones that
// are not lower case. Their other spellings are the ones instrumentation
// libraries actually write, and a query should not depend on guessing which
// one this platform's collector settled on.
var logFieldAliases = map[string]string{
	"service":  "project",
	"app":      "project",
	"env":      "environment",
	"msg":      "message",
	"traceid":  "traceId",
	"trace_id": "traceId",
	"trace.id": "traceId",
	"trace":    "traceId",
	"spanid":   "spanId",
	"span_id":  "spanId",
	"span.id":  "spanId",
}

// LogQueryError is a query the parser refused: a missing bracket, a value where
// a number was wanted. It is the caller's to fix, so it travels separately from
// a store that could not be reached.
type LogQueryError struct {
	Message string
}

func (e *LogQueryError) Error() string { return e.Message }

func queryErrorf(format string, args ...any) *LogQueryError {
	return &LogQueryError{Message: fmt.Sprintf(format, args...)}
}

// LogQueryExpression is a compiled query: the ClickHouse predicate and the
// parameters its placeholders are filled from. An empty Expression is an empty
// query, which selects everything — the window and the limit are the bounds.
type LogQueryExpression struct {
	Expression string
	Params     map[string]string
}

// CompileLogQuery turns the query language into a ClickHouse predicate.
func CompileLogQuery(query string) (LogQueryExpression, error) {
	tokens, err := lexLogQuery(query)
	if err != nil {
		return LogQueryExpression{}, err
	}
	if len(tokens) == 0 {
		return LogQueryExpression{}, nil
	}

	parser := &logQueryParser{tokens: tokens, params: map[string]string{}}
	expression, err := parser.parseOr()
	if err != nil {
		return LogQueryExpression{}, err
	}
	if token, ok := parser.peek(); ok {
		return LogQueryExpression{}, queryErrorf("unexpected %q — a closing bracket has no opening one", token.text)
	}
	return LogQueryExpression{Expression: expression, Params: parser.params}, nil
}

type tokenKind int

const (
	tokenTerm tokenKind = iota
	tokenLeftParen
	tokenRightParen
)

type queryToken struct {
	kind tokenKind
	text string
}

// lexLogQuery splits a query into brackets and terms. Delimited runs are held
// together — `message:"connection refused"` is one term, and so is
// `message:/GET \/works/` — and the delimiters are kept, because whether a
// value was quoted decides whether its `*` is a wildcard or an asterisk.
func lexLogQuery(query string) ([]queryToken, error) {
	tokens := []queryToken{}
	runes := []rune(query)
	for i := 0; i < len(runes); {
		switch {
		case unicode.IsSpace(runes[i]):
			i++
		case runes[i] == '(':
			tokens = append(tokens, queryToken{kind: tokenLeftParen, text: "("})
			i++
		case runes[i] == ')':
			tokens = append(tokens, queryToken{kind: tokenRightParen, text: ")"})
			i++
		default:
			term := strings.Builder{}
			for i < len(runes) {
				current := runes[i]
				if unicode.IsSpace(current) || current == '(' || current == ')' {
					break
				}
				// A `/` only opens a regular expression where a value starts,
				// so a bare `GET /works` is still two ordinary terms.
				opensRegex := current == '/' && strings.HasSuffix(term.String(), ":")
				if current != '"' && !opensRegex {
					term.WriteRune(current)
					i++
					continue
				}
				consumed, ok := lexDelimited(runes[i:], current, &term)
				if !ok {
					return nil, queryErrorf("a quoted value is never closed: %s", query)
				}
				i += consumed
			}
			tokens = append(tokens, queryToken{kind: tokenTerm, text: term.String()})
		}
	}
	return tokens, nil
}

// lexDelimited copies a `"…"` or `/…/` run, escapes and all, and reports how
// much it read.
func lexDelimited(runes []rune, delimiter rune, into *strings.Builder) (int, bool) {
	into.WriteRune(runes[0])
	for i := 1; i < len(runes); {
		if runes[i] == '\\' && i+1 < len(runes) {
			into.WriteRune(runes[i])
			into.WriteRune(runes[i+1])
			i += 2
			continue
		}
		into.WriteRune(runes[i])
		i++
		if runes[i-1] == delimiter {
			return i, true
		}
	}
	return 0, false
}

type logQueryParser struct {
	tokens []queryToken
	pos    int
	params map[string]string
	next   int
	// prefix names this parser's parameters. A statement that compiles more
	// than one thing — the facet query resolves its field names through the
	// same resolver — gives each its own, so the two cannot collide.
	prefix string
}

// param binds a value the user typed and returns the placeholder that stands
// for it. Values never reach the statement text; this is the only way in.
func (p *logQueryParser) param(value string) string {
	prefix := p.prefix
	if prefix == "" {
		prefix = "q"
	}
	name := prefix + strconv.Itoa(p.next)
	p.next++
	p.params[name] = value
	return name
}

func (p *logQueryParser) peek() (queryToken, bool) {
	if p.pos >= len(p.tokens) {
		return queryToken{}, false
	}
	return p.tokens[p.pos], true
}

func isKeyword(token queryToken, word, symbol string) bool {
	return token.kind == tokenTerm && (token.text == word || token.text == symbol)
}

func (p *logQueryParser) parseOr() (string, error) {
	left, err := p.parseAnd()
	if err != nil {
		return "", err
	}
	for {
		token, ok := p.peek()
		if !ok || !isKeyword(token, "OR", "||") {
			return left, nil
		}
		p.pos++
		right, err := p.parseAnd()
		if err != nil {
			return "", err
		}
		left = "(" + left + " OR " + right + ")"
	}
}

func (p *logQueryParser) parseAnd() (string, error) {
	left, err := p.parseUnary()
	if err != nil {
		return "", err
	}
	for {
		token, ok := p.peek()
		if !ok || token.kind == tokenRightParen || isKeyword(token, "OR", "||") {
			return left, nil
		}
		// Adjacency is an AND; the word is allowed for the people who expect
		// to have to write it.
		if isKeyword(token, "AND", "&&") {
			p.pos++
		}
		right, err := p.parseUnary()
		if err != nil {
			return "", err
		}
		left = "(" + left + " AND " + right + ")"
	}
}

func (p *logQueryParser) parseUnary() (string, error) {
	token, ok := p.peek()
	if !ok {
		return "", queryErrorf("the query ends where a term was expected")
	}
	if isKeyword(token, "NOT", "!") {
		p.pos++
		inner, err := p.parseUnary()
		if err != nil {
			return "", err
		}
		return "NOT (" + inner + ")", nil
	}
	// `-level:debug` and `!level:debug` are the same negation written shorter.
	// A term that starts with one of them and is otherwise empty is not.
	if token.kind == tokenTerm && len(token.text) > 1 &&
		(strings.HasPrefix(token.text, "-") || strings.HasPrefix(token.text, "!")) {
		p.tokens[p.pos].text = token.text[1:]
		inner, err := p.parseUnary()
		if err != nil {
			return "", err
		}
		return "NOT (" + inner + ")", nil
	}
	return p.parsePrimary()
}

func (p *logQueryParser) parsePrimary() (string, error) {
	token, ok := p.peek()
	if !ok {
		return "", queryErrorf("the query ends where a term was expected")
	}
	p.pos++

	switch {
	case token.kind == tokenLeftParen:
		inner, err := p.parseOr()
		if err != nil {
			return "", err
		}
		closing, ok := p.peek()
		if !ok || closing.kind != tokenRightParen {
			return "", queryErrorf("a bracket is never closed")
		}
		p.pos++
		// No parentheses of our own: a compound sub-expression already carries
		// them, and an atom does not need them.
		return inner, nil
	case token.kind == tokenRightParen:
		return "", queryErrorf("a closing bracket has no opening one")
	case isKeyword(token, "AND", "&&") || isKeyword(token, "OR", "||"):
		return "", queryErrorf("%s joins two terms; one of them is missing", token.text)
	}
	return p.condition(token.text)
}

// condition compiles one term: a bare word searches the message, `field:value`
// matches whatever the field resolves to.
func (p *logQueryParser) condition(term string) (string, error) {
	field, value := splitFieldValue(term)
	if field == "" {
		return p.match(logMessageColumn, true, value)
	}
	if strings.TrimSpace(value) == "" {
		return "", queryErrorf("%s: has nothing to match; write %s:value, or %s:* for any value", field, field, field)
	}
	expression, isMessage, err := p.columnExpression(field)
	if err != nil {
		return "", err
	}
	return p.match(expression, isMessage, value)
}

// splitFieldValue finds the `field:` prefix of a term, if it has one. The
// field has to look like a field for the colon to count, so a bare `12:30` or
// a quoted term searches the message rather than naming a column.
func splitFieldValue(term string) (string, string) {
	for i, r := range term {
		if r == ':' {
			if i == 0 {
				return "", term
			}
			return term[:i], term[i+1:]
		}
		if !isFieldRune(r) {
			return "", term
		}
	}
	return "", term
}

func isFieldRune(r rune) bool {
	return r == '_' || r == '.' || r == '-' ||
		(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
}

// columnExpression resolves a field name to what it reads in ClickHouse, and
// reports whether it is the message — which is matched as a substring rather
// than for equality, because that is what typing a word into a log search
// means.
func (p *logQueryParser) columnExpression(field string) (string, bool, error) {
	name := strings.ToLower(field)
	if alias, ok := logFieldAliases[name]; ok {
		name = alias
	}
	if name == "timestamp" || name == "time" {
		return "", false, queryErrorf("timestamp is not part of the query; the time range sets the window")
	}
	if column, ok := logQueryColumns[name]; ok {
		return column, name == "message", nil
	}
	// The pod's Kubernetes labels ride in the resource attributes, under the
	// name they have on the pod.
	if key, ok := strings.CutPrefix(field, "labels."); ok {
		return fmt.Sprintf("ResourceAttributes[{%s:String}]", p.param(key)), false, nil
	}
	key := field
	if trimmed, ok := strings.CutPrefix(field, "fields."); ok {
		key = trimmed
	}
	return fmt.Sprintf("LogAttributes[{%s:String}]", p.param(key)), false, nil
}

// comparisons are the numeric operators, longest first so `>=` is seen before
// `>`.
var comparisons = []string{">=", "<=", ">", "<"}

// match compiles the right-hand side of a term against an already-resolved
// column expression.
func (p *logQueryParser) match(expression string, isMessage bool, value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", queryErrorf("a term with nothing to match")
	}

	// `field:*` — present and non-empty. A missing key of a Map reads as the
	// empty string, so this is one test for both columns and fields.
	if value == "*" {
		return expression + " != ''", nil
	}

	for _, operator := range comparisons {
		rest, ok := strings.CutPrefix(value, operator)
		if !ok {
			continue
		}
		number := strings.TrimSpace(unquoteValue(rest))
		if _, err := strconv.ParseFloat(number, 64); err != nil {
			return "", queryErrorf("%s wants a number, got %q", operator, number)
		}
		return fmt.Sprintf("toFloat64OrNull(%s) %s {%s:Float64}",
			expression, operator, p.param(number)), nil
	}

	// `/…/` is a ClickHouse regular expression, evaluated by ClickHouse. It
	// still travels as a parameter, so it can only ever be a pattern.
	if len(value) > 1 && strings.HasPrefix(value, "/") && strings.HasSuffix(value, "/") {
		return fmt.Sprintf("match(%s, {%s:String})", expression, p.param(value[1:len(value)-1])), nil
	}

	alternatives := splitValues(value)
	matches := make([]string, 0, len(alternatives))
	for _, alternative := range alternatives {
		if strings.TrimSpace(alternative) == "" {
			return "", queryErrorf("%q has an empty alternative; commas separate values", value)
		}
		matches = append(matches, p.matchOne(expression, isMessage, alternative))
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	return "(" + strings.Join(matches, " OR ") + ")", nil
}

func (p *logQueryParser) matchOne(expression string, isMessage bool, value string) string {
	quoted := isQuoted(value)
	literal := unquoteValue(value)

	if !quoted && strings.ContainsAny(literal, "*?") {
		operator := "LIKE"
		if isMessage {
			operator = "ILIKE"
		}
		return fmt.Sprintf("%s %s {%s:String}", expression, operator, p.param(likePattern(literal)))
	}
	if isMessage {
		return fmt.Sprintf("positionCaseInsensitive(%s, {%s:String}) > 0", expression, p.param(literal))
	}
	return fmt.Sprintf("%s = {%s:String}", expression, p.param(literal))
}

// splitValues breaks `error,fatal` into its alternatives, leaving commas inside
// quotes alone.
func splitValues(value string) []string {
	values := []string{}
	current := strings.Builder{}
	inQuotes := false
	for i := 0; i < len(value); i++ {
		switch {
		case value[i] == '\\' && i+1 < len(value):
			current.WriteByte(value[i])
			current.WriteByte(value[i+1])
			i++
		case value[i] == '"':
			inQuotes = !inQuotes
			current.WriteByte(value[i])
		case value[i] == ',' && !inQuotes:
			values = append(values, current.String())
			current.Reset()
		default:
			current.WriteByte(value[i])
		}
	}
	values = append(values, current.String())
	return values
}

func isQuoted(value string) bool {
	return len(value) >= 2 && strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`)
}

// unquoteValue strips the quotes a value was written with and undoes the
// escapes inside them.
func unquoteValue(value string) string {
	if !isQuoted(value) {
		return value
	}
	inner := value[1 : len(value)-1]
	unescaped := strings.Builder{}
	for i := 0; i < len(inner); i++ {
		if inner[i] == '\\' && i+1 < len(inner) {
			i++
		}
		unescaped.WriteByte(inner[i])
	}
	return unescaped.String()
}

// likePattern turns the query language's wildcards into LIKE's, and escapes
// LIKE's own so that a literal `%` in a pod name stays one.
func likePattern(value string) string {
	pattern := strings.Builder{}
	for _, r := range value {
		switch r {
		case '*':
			pattern.WriteRune('%')
		case '?':
			pattern.WriteRune('_')
		case '%', '_', '\\':
			pattern.WriteRune('\\')
			pattern.WriteRune(r)
		default:
			pattern.WriteRune(r)
		}
	}
	return pattern.String()
}
