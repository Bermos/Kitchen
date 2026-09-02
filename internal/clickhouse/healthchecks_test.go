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
	"strings"
	"testing"
	"time"
)

// The health route the fixtures exclude, and the project it belongs to.
var testHealthRoute = HealthRoute{Project: testProject, Route: "/api/health"}

// Every aggregate read shares one scope, so proving the predicate is on the
// summary is proving it is on all three — but the route table is where the
// number is read most literally, so both are checked.
func TestTheGoldenSignalsDropTheProjectsHealthCheck(t *testing.T) {
	for _, read := range []struct {
		name string
		run  func(*Client) error
	}{
		{"summary", func(c *Client) error {
			_, err := c.RequestSummary(context.Background(), RequestQuery{
				Project:       testProject,
				ExcludeHealth: []HealthRoute{testHealthRoute},
			})
			return err
		}},
		{"series", func(c *Client) error {
			_, err := c.RequestSeries(context.Background(), RequestSeriesQuery{
				RequestQuery: RequestQuery{
					Project:       testProject,
					ExcludeHealth: []HealthRoute{testHealthRoute},
				},
				Buckets: 60,
			})
			return err
		}},
		{"routes", func(c *Client) error {
			_, err := c.RequestRoutes(context.Background(), RequestRoutesQuery{
				RequestQuery: RequestQuery{
					Project:       testProject,
					ExcludeHealth: []HealthRoute{testHealthRoute},
				},
			})
			return err
		}},
		{"listing", func(c *Client) error {
			_, err := c.QueryRequests(context.Background(), RequestListQuery{
				Project:       testProject,
				ExcludeHealth: []HealthRoute{testHealthRoute},
			})
			return err
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			store := newFakeLogStore(t)
			if err := read.run(store.client(t)); err != nil {
				t.Fatalf("%s: %v", read.name, err)
			}
			if !strings.Contains(store.query, "NOT (") {
				t.Fatalf("expected the health check to be excluded:\n%s", store.query)
			}
			if !strings.Contains(store.query, "{healthRoute0:String}") ||
				!strings.Contains(store.query, "{healthProject0:String}") {
				t.Errorf("expected the pair to travel as parameters:\n%s", store.query)
			}
			if got := store.params.Get("param_healthRoute0"); got != testHealthRoute.Route {
				t.Errorf("health route parameter = %q, want %q", got, testHealthRoute.Route)
			}
			if got := store.params.Get("param_healthProject0"); got != testProject {
				t.Errorf("health project parameter = %q, want %q", got, testProject)
			}
		})
	}
}

// The exclusion is a pair and never a route on its own: two projects may spell
// their check the same way and one of them may serve it as a real route.
func TestAHealthExclusionIsPairedWithItsProject(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).ProjectTraffic(context.Background(), ProjectTrafficQuery{
		ExcludeHealth: []HealthRoute{
			{Project: "shop", Route: "/api/health"},
			{Project: "blog", Route: "/healthz"},
		},
	}); err != nil {
		t.Fatalf("ProjectTraffic: %v", err)
	}

	want := "NOT ((r.project = {healthProject0:String} AND r.route = {healthRoute0:String}) OR " +
		"(r.project = {healthProject1:String} AND r.route = {healthRoute1:String}))"
	if !strings.Contains(store.query, want) {
		t.Fatalf("expected each project's own pair:\n%s", store.query)
	}
	if store.params.Get("param_healthProject1") != "blog" ||
		store.params.Get("param_healthRoute1") != "/healthz" {
		t.Errorf("the second pair did not travel: %v", store.params)
	}
}

// A caller who named the health route asked for it. Excluding it there would
// answer zero to a question whose answer is on the screen beside it.
func TestNamingTheHealthRouteBeatsExcludingIt(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project:       testProject,
		Route:         testHealthRoute.Route,
		ExcludeHealth: []HealthRoute{testHealthRoute},
	}); err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if strings.Contains(store.query, "NOT (") {
		t.Fatalf("a named route should not also be excluded:\n%s", store.query)
	}
	if !strings.Contains(store.query, "r.route = {route:String}") {
		t.Errorf("expected the route filter:\n%s", store.query)
	}
}

// A pair with an empty project would exclude the route from the traffic the
// follower could not attribute, whose project column is the empty string; a
// pair with an empty route would exclude every row of a project that declared
// no check at all. Both are dropped rather than rendered.
func TestAnIncompleteHealthPairExcludesNothing(t *testing.T) {
	for _, routes := range [][]HealthRoute{
		nil,
		{{Project: "", Route: "/api/health"}},
		{{Project: testProject, Route: ""}},
	} {
		store := newFakeLogStore(t)
		if _, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
			Project:       testProject,
			ExcludeHealth: routes,
		}); err != nil {
			t.Fatalf("RequestSummary: %v", err)
		}
		if strings.Contains(store.query, "NOT (") {
			t.Errorf("%v should exclude nothing:\n%s", routes, store.query)
		}
	}
}

// The same pairs are named by the same parameters however many times a read
// repeats one, so a duplicate cannot leave a parameter the statement names
// unset — or name one the statement never uses.
func TestARepeatedHealthRouteIsExcludedOnce(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).PlatformRequests(context.Background(), PlatformRequestsQuery{
		Since:         time.Date(2026, 8, 16, 11, 0, 0, 0, time.UTC),
		Until:         time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC),
		ExcludeHealth: []HealthRoute{testHealthRoute, testHealthRoute},
	}); err != nil {
		t.Fatalf("PlatformRequests: %v", err)
	}
	if strings.Contains(store.query, "{healthRoute1:String}") {
		t.Fatalf("the duplicate minted a second pair:\n%s", store.query)
	}
	if store.params.Get("param_healthRoute1") != "" {
		t.Errorf("a parameter the statement does not name was sent: %v", store.params)
	}
}

// The route is derived from a project's own configuration, so it travels as a
// parameter like every other value that came from outside this package.
func TestAHealthRouteNeverReachesTheQueryText(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project: testProject,
		ExcludeHealth: []HealthRoute{
			{Project: testProject, Route: "/health'; DROP TABLE http_requests_1m; --"},
		},
	}); err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if strings.Contains(store.query, "DROP TABLE") {
		t.Fatalf("the health route reached the query text:\n%s", store.query)
	}
}
