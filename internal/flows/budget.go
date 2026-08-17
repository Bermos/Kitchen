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

import "container/list"

// The stateful half of §3.3: a budget of route templates per environment.
//
// Classification (paths.go) is a set of guesses about how identifiers are
// spelled, and a guess misses. What it misses does not merely mislabel one
// row — it mints a series per value, in a LowCardinality column, inside the
// ordering key of both rollups, for as long as the retention holds. The budget
// makes that failure bounded and, more importantly, visible: past the cap an
// environment's further templates are all recorded as the one overflow route,
// so a missed identifier scheme shows up as a suspiciously busy `/…` row
// instead of quietly poisoning the rollup.
//
// 300 is beyond any hand-written API, so hitting the cap is nearly always the
// classifier's fault rather than the application's — which is exactly the
// reading the overflow row is there to prompt.

const (
	// routeBudget is how many distinct templates one environment may mint.
	routeBudget = 300

	// budgetedEnvironments bounds what the budgets cost across a platform.
	// Previews come and go by the pull request, so without a bound the
	// follower would hold the route set of every environment that ever served
	// a request for as long as the operator ran. At both caps together the
	// worst case is a few megabytes of strings, and the environments that fall
	// out are the ones nothing has asked for in the longest time.
	budgetedEnvironments = 500
)

// routeBudgets is every environment's route set, itself bounded.
//
// It is not safe for concurrent use, and deliberately so: it sits on the path
// of every request the platform serves, the follower is a single goroutine,
// and a lock here would be taken millions of times to guard against a second
// caller that does not exist.
type routeBudgets struct {
	environments *lru[*lru[struct{}]]
}

func newRouteBudgets() *routeBudgets {
	return &routeBudgets{environments: newLRU[*lru[struct{}]](budgetedEnvironments)}
}

// route templates one path and charges it to the environment's budget,
// answering with the route the row should carry.
//
// Requests the platform could not attribute — a host it never published —
// share one budget between them, which is deliberate. That bucket is where
// scanners land, and a scanner walking invented hostnames should be able to
// cost the store 300 templates in total rather than 300 for every name it
// makes up.
func (b *routeBudgets) route(project, environment, path string) string {
	template := templatePath(path)
	routes := b.routesFor(project, environment)
	if _, known := routes.get(template); known {
		return template
	}

	// Past the budget the template is still remembered, because the set is an
	// LRU rather than the first 300 templates ever seen: a deploy that
	// genuinely replaced an application's routes has to be able to re-learn
	// them, and freezing the set would report the new API as overflow for
	// ever. This request is recorded as the overflow route all the same — a
	// template the environment has never charged to its budget is precisely
	// the series the cap exists not to mint.
	overflowed := routes.len() >= routeBudget
	routes.put(template, struct{}{})
	if overflowed {
		return overflowRoute
	}
	return template
}

// routesFor finds an environment's route set, starting one where there is
// none. The key joins the two names through a byte neither can contain, so
// that no pair of names can collide into one budget.
func (b *routeBudgets) routesFor(project, environment string) *lru[struct{}] {
	key := project + "\x00" + environment
	if routes, ok := b.environments.get(key); ok {
		return routes
	}
	routes := newLRU[struct{}](routeBudget)
	b.environments.put(key, routes)
	return routes
}

// lru is a bounded, most-recently-used map. Both halves of the budget need the
// same thing — the environments, and each environment's routes — and both want
// the same eviction: what has not been asked for in the longest time is what a
// bound should drop.
//
// It inherits routeBudgets' single-goroutine rule; nothing here locks.
type lru[V any] struct {
	capacity int
	// order holds *lruEntry[V], most recently used at the front.
	order   *list.List
	entries map[string]*list.Element
}

type lruEntry[V any] struct {
	key   string
	value V
}

func newLRU[V any](capacity int) *lru[V] {
	return &lru[V]{
		capacity: capacity,
		order:    list.New(),
		entries:  make(map[string]*list.Element, capacity),
	}
}

// get answers with the value under a key and marks it as the most recently
// used — a read is what "recently used" means here, since the whole point is
// to keep what traffic keeps asking for.
func (l *lru[V]) get(key string) (V, bool) {
	element, ok := l.entries[key]
	if !ok {
		var zero V
		return zero, false
	}
	l.order.MoveToFront(element)
	return element.Value.(*lruEntry[V]).value, true
}

// put inserts or refreshes a key, evicting from the back until the map is
// inside its capacity again.
func (l *lru[V]) put(key string, value V) {
	if element, ok := l.entries[key]; ok {
		l.order.MoveToFront(element)
		element.Value.(*lruEntry[V]).value = value
		return
	}
	l.entries[key] = l.order.PushFront(&lruEntry[V]{key: key, value: value})
	for l.order.Len() > l.capacity {
		oldest := l.order.Back()
		l.order.Remove(oldest)
		delete(l.entries, oldest.Value.(*lruEntry[V]).key)
	}
}

func (l *lru[V]) len() int { return l.order.Len() }
