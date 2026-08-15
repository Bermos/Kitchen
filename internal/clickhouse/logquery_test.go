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
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestTheQueryLanguageCompiles(t *testing.T) {
	for _, test := range []struct {
		name       string
		query      string
		expression string
		values     []string
	}{{
		name:       "a bare word searches the message",
		query:      "timeout",
		expression: "positionCaseInsensitive(`message`, {q0:String}) > 0",
		values:     []string{"timeout"},
	}, {
		name:       "a phrase is one term",
		query:      `"connection refused"`,
		expression: "positionCaseInsensitive(`message`, {q0:String}) > 0",
		values:     []string{"connection refused"},
	}, {
		name:       "a column is matched exactly",
		query:      "level:error",
		expression: "`level` = {q0:String}",
		values:     []string{"error"},
	}, {
		name:       "service is the project",
		query:      "service:shop",
		expression: "`project` = {q0:String}",
		values:     []string{"shop"},
	}, {
		name:       "adjacency is an AND",
		query:      "level:error service:shop",
		expression: "(`level` = {q0:String} AND `project` = {q1:String})",
		values:     []string{"error", "shop"},
	}, {
		name:       "a leading dash negates",
		query:      "-source:cluster",
		expression: "NOT (`source` = {q0:String})",
		values:     []string{"cluster"},
	}, {
		name:       "commas are alternatives",
		query:      "level:error,fatal",
		expression: "(`level` = {q0:String} OR `level` = {q1:String})",
		values:     []string{"error", "fatal"},
	}, {
		name:       "a wildcard becomes a LIKE pattern",
		query:      "pod:shop-*",
		expression: "`pod` LIKE {q0:String}",
		values:     []string{"shop-%"},
	}, {
		name:       "an unknown name is a structured field",
		query:      "http.status:500",
		expression: "fields[{q0:String}] = {q1:String}",
		values:     []string{"http.status", "500"},
	}, {
		name:       "a numeric comparison casts the field",
		query:      "http.status:>=500",
		expression: "toFloat64OrNull(fields[{q0:String}]) >= {q1:Float64}",
		values:     []string{"http.status", "500"},
	}, {
		name:       "a star asks whether the field is there at all",
		query:      "trace_id:*",
		expression: "fields[{q0:String}] != ''",
		values:     []string{"trace_id"},
	}, {
		name:       "a pod label is addressed under its own name",
		query:      "labels.tier:web",
		expression: "labels[{q0:String}] = {q1:String}",
		values:     []string{"tier", "web"},
	}, {
		name:       "slashes are a regular expression",
		query:      `message:/GET \/works/`,
		expression: "match(`message`, {q0:String})",
		values:     []string{`GET \/works`},
	}, {
		name:       "OR binds looser than adjacency",
		query:      "level:error service:shop OR level:fatal",
		expression: "((`level` = {q0:String} AND `project` = {q1:String}) OR `level` = {q2:String})",
		values:     []string{"error", "shop", "fatal"},
	}, {
		name:       "brackets regroup",
		query:      "(level:error OR level:fatal) service:shop",
		expression: "((`level` = {q0:String} OR `level` = {q1:String}) AND `project` = {q2:String})",
		values:     []string{"error", "fatal", "shop"},
	}} {
		t.Run(test.name, func(t *testing.T) {
			compiled, err := CompileLogQuery(test.query)
			if err != nil {
				t.Fatalf("CompileLogQuery(%q): %v", test.query, err)
			}
			if compiled.Expression != test.expression {
				t.Fatalf("want\n  %s\ngot\n  %s", test.expression, compiled.Expression)
			}
			for i, want := range test.values {
				name := "q" + strconv.Itoa(i)
				if got := compiled.Params[name]; got != want {
					t.Fatalf("%s should be %q, got %q (params %v)", name, want, got, compiled.Params)
				}
			}
			if len(compiled.Params) != len(test.values) {
				t.Fatalf("want %d parameters, got %v", len(test.values), compiled.Params)
			}
		})
	}
}

// An empty query is a legitimate question — everything in the window — and it
// compiles to no predicate at all rather than to a tautology.
func TestAnEmptyQueryCompilesToNothing(t *testing.T) {
	for _, query := range []string{"", "   ", "\t\n"} {
		compiled, err := CompileLogQuery(query)
		if err != nil {
			t.Fatalf("CompileLogQuery(%q): %v", query, err)
		}
		if compiled.Expression != "" {
			t.Fatalf("%q should compile to nothing, got %q", query, compiled.Expression)
		}
	}
}

// Every value the user typed leaves as a parameter. Nothing they can type
// reaches the statement text, so a quote is a quote and a semicolon is a
// semicolon.
func TestValuesNeverReachTheStatementText(t *testing.T) {
	hostile := `'; DROP TABLE logs; --`
	compiled, err := CompileLogQuery(`message:"` + hostile + `"`)
	if err != nil {
		t.Fatalf("CompileLogQuery: %v", err)
	}
	if strings.Contains(compiled.Expression, "DROP") {
		t.Fatalf("a value must not reach the statement: %s", compiled.Expression)
	}
	if !slices.Contains(paramValues(compiled), hostile) {
		t.Fatalf("the value should travel as a parameter, intact: %v", compiled.Params)
	}
}

func TestLikePatternsEscapeTheirOwnWildcards(t *testing.T) {
	compiled, err := CompileLogQuery("pod:100%-*")
	if err != nil {
		t.Fatalf("CompileLogQuery: %v", err)
	}
	if !slices.Contains(paramValues(compiled), `100\%-%`) {
		t.Fatalf("a literal %% should stay one and only * should become a wildcard: %v", compiled.Params)
	}
}

func TestTheQueryLanguageRefusesWhatItCannotRead(t *testing.T) {
	for _, test := range []struct{ query, says string }{
		{"(level:error", "bracket"},
		{"level:error)", "bracket"},
		{"level:error AND", "term"},
		{"OR level:error", "term"},
		{`message:"unclosed`, "quoted"},
		{"http.status:>=abc", "number"},
		{"timestamp:2026-08-13", "time range"},
		{"level:", "nothing to match"},
	} {
		_, err := CompileLogQuery(test.query)
		if err == nil {
			t.Fatalf("%q should be refused", test.query)
		}
		queryErr := &LogQueryError{}
		if !errors.As(err, &queryErr) {
			t.Fatalf("%q should be refused as the caller's error, got %T", test.query, err)
		}
		if !strings.Contains(queryErr.Message, test.says) {
			t.Fatalf("%q should be refused with a message mentioning %q, got %q",
				test.query, test.says, queryErr.Message)
		}
	}
}

func paramValues(compiled LogQueryExpression) []string {
	values := make([]string, 0, len(compiled.Params))
	for _, value := range compiled.Params {
		values = append(values, value)
	}
	return values
}
