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
	"strings"
)

// A health check is not traffic.
//
// Every request to a Kitchen application is observed at the shared Gateway,
// which is what makes the golden signals free — and it means that whatever is
// asking an application whether it is alive is counted beside the people using
// it. A quiet project answering one probe every thirty seconds reads as 2,880
// requests a day it never had, and the numbers that are supposed to say
// "nobody is here" say the opposite. The route table is where that shows first:
// a health path is usually the busiest row an idle environment has.
//
// So the reads that answer *a project's* traffic drop the rows whose route is
// the health check the platform itself declared for that project. Three things
// about that shape are deliberate:
//
//   - **It is the declared path and nothing else.** No list of paths that look
//     like health checks — an application is entitled to serve anything at
//     `/health`, and a platform that quietly decided which of somebody's routes
//     were not real would be wrong in a way nobody could see. What it may
//     discount is the check it was told to make.
//   - **The rows are kept.** This is a predicate on a read, not a filter at
//     ingest: the probe that started failing at 03:00 is still in the store,
//     still in the listing when the caller asks for it, and still joined to the
//     logs around a crash.
//   - **It is per project, as a pair.** Two projects may spell their health
//     check the same way and one of them may serve it as a real route; an
//     exclusion keyed on the path alone would take the second one's traffic
//     away because of the first one's configuration.
//
// What it cannot do is see backwards: the exclusion is the path a project
// declares *now*, so a health path that changed inside the window leaves the
// old one counted. The alternative is a column on every row deciding this at
// ingest, which is the same answer at ten times the cost — and one that could
// never be corrected after the fact.

// HealthRoute names one project's health check, as a route template rather
// than as the path it was declared with: the stored `route` column is what the
// follower templated the path to, so this is what a row is matched on. Use
// flows.RouteTemplate to derive it from a declared path, so that both sides of
// the comparison are templated by the same code.
type HealthRoute struct {
	Project string
	Route   string
}

// healthCondition renders the predicate that drops a set of projects' health
// checks, registering the parameters it names.
//
// It answers an empty string when there is nothing to exclude — a platform
// where no project declared a health path, or a read that was asked to keep
// them — and callers append nothing in that case rather than a tautology.
//
// prefix is how the read spells its columns: the rollup reads alias their
// table `r`, the raw listing does not alias at all. Every value travels as a
// query parameter, because a route template is derived from configuration a
// project's own repository can write.
func healthCondition(routes []HealthRoute, prefix string, params map[string]string) string {
	pairs := make([]string, 0, len(routes))
	seen := make(map[HealthRoute]struct{}, len(routes))
	for _, route := range routes {
		// A project that declared no health check excludes nothing. An entry
		// with no project would be worse than useless: the unrouted bucket's
		// project is the empty string, so it would silently exclude a route
		// from the traffic nobody could attribute.
		if route.Project == "" || route.Route == "" {
			continue
		}
		if _, done := seen[route]; done {
			continue
		}
		seen[route] = struct{}{}

		project := fmt.Sprintf("healthProject%d", len(pairs))
		template := fmt.Sprintf("healthRoute%d", len(pairs))
		params[project] = route.Project
		params[template] = route.Route
		pairs = append(pairs, fmt.Sprintf("(%sproject = {%s:String} AND %sroute = {%s:String})",
			prefix, project, prefix, template))
	}
	if len(pairs) == 0 {
		return ""
	}
	return "NOT (" + strings.Join(pairs, " OR ") + ")"
}
