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

package flows

import (
	"fmt"
	"testing"
)

// The environment every budget assertion is about, spelled once.
const (
	budgetProject     = "shop"
	budgetEnvironment = "production"
)

// unclassifiable spells a route segment nothing in paths.go templates away, so
// that a test about the budget is not accidentally a test about the
// classifier: each of these is a distinct template.
func unclassifiable(n int) string {
	return fmt.Sprintf("/api/route%s", string(rune('a'+n%26))+string(rune('a'+n/26%26))+string(rune('a'+n/676%26)))
}

func TestRouteBudgetTemplatesAndCharges(t *testing.T) {
	budgets := newRouteBudgets()

	// The template is what the row carries, so two requests that differ only
	// in an identifier cost the environment one route between them.
	first := budgets.route(budgetProject, budgetEnvironment, "/users/12345/orders")
	second := budgets.route(budgetProject, budgetEnvironment, "/users/98765/orders")
	if first != "/users/:id/orders" || second != first {
		t.Errorf("route = %q then %q, want both /users/:id/orders", first, second)
	}
}

func TestRouteBudgetOverflowsAtItsCap(t *testing.T) {
	budgets := newRouteBudgets()

	for i := range routeBudget {
		path := unclassifiable(i)
		if got := budgets.route(budgetProject, budgetEnvironment, path); got != path {
			t.Fatalf("route %d = %q, want %q", i, got, path)
		}
	}

	// The next distinct template is the one the cap exists to refuse.
	beyond := unclassifiable(routeBudget)
	if got := budgets.route(budgetProject, budgetEnvironment, beyond); got != overflowRoute {
		t.Errorf("route past the budget = %q, want %q", got, overflowRoute)
	}

	// A template still in the set keeps working; the cap refuses new series,
	// not traffic. The one asked for most recently is the one certain to have
	// survived the insert above.
	inside := unclassifiable(routeBudget - 1)
	if got := budgets.route(budgetProject, budgetEnvironment, inside); got != inside {
		t.Errorf("route inside the budget = %q, want %q", got, inside)
	}

	// The set is an LRU rather than the first 300 templates ever seen, and
	// that is what lets a route table that genuinely changed — a deploy with a
	// new API — be re-learned instead of reported as overflow for ever: the
	// overflowing template was remembered, and answers as itself from the
	// second request on, at the cost of the template used longest ago.
	if got := budgets.route(budgetProject, budgetEnvironment, beyond); got != beyond {
		t.Errorf("re-learned route = %q, want %q", got, beyond)
	}
	if got := budgets.route(budgetProject, budgetEnvironment, unclassifiable(0)); got != overflowRoute {
		t.Errorf("evicted route = %q, want %q", got, overflowRoute)
	}

	// One environment's budget is its own.
	other := budgets.route(budgetProject, "preview", beyond)
	if other != beyond {
		t.Errorf("another environment's route = %q, want %q", other, beyond)
	}
}

func TestRouteBudgetBoundsHowManyEnvironmentsItHolds(t *testing.T) {
	budgets := newRouteBudgets()
	for i := range budgetedEnvironments + 10 {
		budgets.route(budgetProject, fmt.Sprintf("pr-%d", i), "/")
	}
	if got := budgets.environments.len(); got != budgetedEnvironments {
		t.Errorf("holding %d environments, want %d", got, budgetedEnvironments)
	}
}

func TestLRUEvictsWhatWasUsedLongestAgo(t *testing.T) {
	cache := newLRU[int](2)
	cache.put("a", 1)
	cache.put("b", 2)

	// Reading is using: `a` is now the newer of the two, so `b` is what a
	// third entry displaces.
	if _, ok := cache.get("a"); !ok {
		t.Fatal("a should still be held")
	}
	cache.put("c", 3)

	if _, ok := cache.get("b"); ok {
		t.Error("b should have been evicted")
	}
	if value, ok := cache.get("a"); !ok || value != 1 {
		t.Errorf("a = %d %v, want 1 true", value, ok)
	}
	if cache.len() != 2 {
		t.Errorf("holding %d entries, want 2", cache.len())
	}
}
