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

package accountsdb

import (
	"strings"
	"testing"
)

// The order is the whole of what makes a data-only restore work: every table
// has to arrive after the ones its foreign keys point at, or the very first
// COPY is refused and the transaction takes the rest of the restore with it.
func TestDependencyOrderPutsParentsFirst(t *testing.T) {
	// better-auth's shape: sessions, accounts and passkeys all point at the
	// user, and the OAuth consent points at both a user and a client.
	tables := []string{"session", "user", "account", "passkey", "oauthApplication", "oauthConsent"}
	references := map[string][]string{
		"session":      {"user"},
		"account":      {"user"},
		"passkey":      {"user"},
		"oauthConsent": {"user", "oauthApplication"},
	}

	ordered := dependencyOrder(tables, references)
	if len(ordered) != len(tables) {
		t.Fatalf("ordered %d of %d tables: %v", len(ordered), len(tables), ordered)
	}
	position := map[string]int{}
	for i, name := range ordered {
		position[name] = i
	}
	for child, parents := range references {
		for _, parent := range parents {
			if position[parent] > position[child] {
				t.Errorf("%s is restored after %s, which references it: %v", parent, child, ordered)
			}
		}
	}
}

// A cycle cannot be ordered, and the answer that matters is that every table
// still comes out: the restore is one transaction, so a foreign key it cannot
// satisfy fails loudly and names the table. A table silently left out would
// fail nothing and lose data.
func TestDependencyOrderKeepsEveryTableInACycle(t *testing.T) {
	ordered := dependencyOrder(
		[]string{"a", "b", "c"},
		map[string][]string{"a": {"b"}, "b": {"a"}, "c": {"a"}},
	)
	if len(ordered) != 3 {
		t.Fatalf("expected all three tables, got %v", ordered)
	}
}

// A table that references itself — a tree of organizations, say — is not a
// cycle between tables and must not be treated as one.
func TestDependencyOrderIgnoresSelfReferences(t *testing.T) {
	ordered := dependencyOrder([]string{"node"}, map[string][]string{"node": {"node"}})
	if len(ordered) != 1 || ordered[0] != "node" {
		t.Fatalf("expected [node], got %v", ordered)
	}
}

// "user" is a reserved word in SQL and one of better-auth's table names, so
// every identifier this package writes into a statement is quoted.
func TestQuotedIdentifiers(t *testing.T) {
	if got := qualified("user"); got != `"public"."user"` {
		t.Errorf("qualified(user) = %s", got)
	}
	if got := columnList([]string{"id", "createdAt"}); got != `"id", "createdAt"` {
		t.Errorf("columnList = %s", got)
	}
	// An embedded quote doubles, which is the one escape PostgreSQL has for
	// identifiers. Nothing here comes from a request, but a name that broke
	// out of its quotes would break out into a TRUNCATE.
	if got := quoted(`we"ird`); !strings.Contains(got, `we""ird`) {
		t.Errorf("quoted did not double the quote: %s", got)
	}
}
