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
	"context"
	"errors"
	"strings"
	"testing"
)

// What issue #421 was: a selection carried a second surface, `where`, whose
// text was evaluated as written, and the caller's projects were composed onto
// it with AND. A conjunct decides what a statement *returns* and nothing about
// what it may *read*, so a subquery inside the expression answered questions
// about every table the platform's database user can see — and `readonly=2`
// permits `url()`, which wants CREATE TEMPORARY TABLE, so the answers did not
// even have to come back through the response.
//
// These are the shapes that used to work. None of them is filtered out or
// escaped: they reach the statement as parameters or they are refused by the
// parser, because the compiler writes every piece of statement text itself.

// hostileQueries are read as `q`, which is now the only filter surface.
var hostileQueries = []struct {
	name  string
	query string
	// text is what must not appear in the statement whatever the parser made
	// of the query — the fragment that would mean the caller reached the SQL.
	text string
}{
	{
		name:  "a subquery over another project",
		query: `message:x') OR (SELECT count() FROM otel_logs WHERE project='billing`,
		text:  "billing",
	},
	{
		name:  "a UNION onto another table",
		query: `pod:shop UNION ALL SELECT * FROM system.tables`,
		text:  "system.tables",
	},
	{
		name:  "a comment ending the statement",
		query: `level:error' -- and the rest of the WHERE`,
		text:  "--",
	},
	{
		name:  "a block comment around the scope",
		query: `level:error /* project IN */ x`,
		text:  "/*",
	},
	{
		name:  "a second table named in a term",
		query: `message:"1 FROM otel_traces"`,
		text:  "otel_traces",
	},
	{
		name:  "a function outside what the compiler emits",
		query: `message:"url('http://elsewhere.example/?x=')"`,
		text:  "url(",
	},
	{
		name: "a project the caller cannot see",
		// The scope is `shop` below. This is the one that is *not* refused and
		// is not meant to be: it compiles, and matches nothing, because the
		// scope is ANDed around it structurally.
		query: `project:billing`,
		text:  "billing",
	},
}

func TestAHostileQueryReachesTheStatementOnlyAsParameters(t *testing.T) {
	for _, test := range hostileQueries {
		t.Run(test.name, func(t *testing.T) {
			store := newFakeLogStore(t)
			_, err := store.client(t).FilterLogs(context.Background(), LogFilter{
				LogSelection: LogSelection{
					Query: test.query,
					Scope: LogScope{Projects: []string{testProject}},
				},
				Limit: 10,
			})
			if err != nil {
				// Refused by the parser is a perfectly good answer, as long as
				// it is the caller's error rather than the platform's.
				queryErr := &LogQueryError{}
				if !errors.As(err, &queryErr) {
					t.Fatalf("want a refusal the caller can act on, got %v", err)
				}
				t.Logf("refused: %s", queryErr.Message)
				if store.query != "" {
					t.Fatalf("a refused query should reach the store as nothing:\n%s", store.query)
				}
				return
			}
			if strings.Contains(store.query, test.text) {
				t.Fatalf("%q reached the statement text:\n%s", test.text, store.query)
			}
			assertBoundedStatement(t, store.query)
			// The scope is still the outermost thing in the predicate,
			// whatever the query turned into.
			if !strings.Contains(store.query, "(project IN ({scope0:String}))") {
				t.Fatalf("the scope should bound every statement:\n%s", store.query)
			}
			if got := store.params.Get("param_scope0"); got != testProject {
				t.Fatalf("the scope should travel as a parameter, got %q", got)
			}
		})
	}
}

// assertBoundedStatement checks the shape a compiled selection always has: one
// SELECT over one table, no comment, no union.
func assertBoundedStatement(t *testing.T, statement string) {
	t.Helper()
	if got := strings.Count(statement, "SELECT"); got != 1 {
		t.Fatalf("a filter should compile to one SELECT, got %d:\n%s", got, statement)
	}
	if got := strings.Count(statement, "FROM"); got != 1 {
		t.Fatalf("a filter should read one table, got %d FROMs:\n%s", got, statement)
	}
	if !strings.Contains(statement, quoteIdentifier(LogsTable)) {
		t.Fatalf("the one table should be the log table:\n%s", statement)
	}
	for _, forbidden := range []string{"UNION", "--", "/*", "system.", "url(", "remote("} {
		if strings.Contains(statement, forbidden) {
			t.Fatalf("%q has no business in a compiled statement:\n%s", forbidden, statement)
		}
	}
}

// A selection that says nothing about what it may read reads nothing. The zero
// value is the dangerous one — it is what the saved-query alert evaluated with
// for as long as the alert existed — so it is the one this pins.
func TestASelectionWithNoScopeIsRefused(t *testing.T) {
	store := newFakeLogStore(t)
	client := store.client(t)

	if _, err := client.FilterLogs(context.Background(), LogFilter{Limit: 10}); err == nil {
		t.Fatal("a selection with no scope should read nothing at all")
	}
	if _, err := client.CountLogs(context.Background(), LogSelection{Query: "level:error"}); err == nil {
		t.Fatal("a count with no scope should be refused")
	}
	if store.query != "" {
		t.Fatalf("nothing should have been asked of the store:\n%s", store.query)
	}
}

// The scope is a boundary rather than a hint: two projects are two parameters,
// and a name that could have been written into the text is not.
func TestTheScopeBindsEveryProjectName(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).CountLogs(context.Background(), LogSelection{
		Scope: LogScope{Projects: []string{testProject, "blog"}},
	}); err != nil {
		t.Fatalf("CountLogs: %v", err)
	}
	if !strings.Contains(store.query, "project IN ({scope0:String}, {scope1:String})") {
		t.Fatalf("both names should be placeholders:\n%s", store.query)
	}
	for name, want := range map[string]string{"param_scope0": testProject, "param_scope1": "blog"} {
		if got := store.params.Get(name); got != want {
			t.Fatalf("%s: want %q, got %q", name, want, got)
		}
	}
	if strings.Contains(store.query, "'"+testProject+"'") {
		t.Fatalf("a project name should never be quoted into the text:\n%s", store.query)
	}
}

// The platform's own view is asked for rather than fallen into: it is the one
// scope with no `project` condition, and only the reads entitled to it say so.
func TestThePlatformScopeIsTheOneWithoutACondition(t *testing.T) {
	store := newFakeLogStore(t)
	if _, err := store.client(t).CountLogs(context.Background(), LogSelection{
		Scope: LogScope{Platform: true}, Query: "level:error",
	}); err != nil {
		t.Fatalf("CountLogs: %v", err)
	}
	if strings.Contains(store.query, "project IN") {
		t.Fatalf("the platform's own view narrows to no project:\n%s", store.query)
	}
	if !strings.Contains(store.query, "WHERE") {
		t.Fatalf("the query itself should still be compiled in:\n%s", store.query)
	}
}
