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
)

// The hostname the tests exclude: one the platform is not routing yet, which
// is what a custom domain is between being verified and being accepted.
const pendingHost = "app.example.com"

// Every request read takes the same exclusion, and every one of them has to
// apply it — the three aggregates share one scope, the listing builds its own
// predicate, and the series builds its scope from a resolved copy of the
// query, which is exactly where a new filter goes missing.
func TestEveryRequestReadDropsTheHostnamesItWasGiven(t *testing.T) {
	for _, read := range []struct {
		name   string
		prefix string
		run    func(*Client) error
	}{
		{"summary", "r.", func(c *Client) error {
			_, err := c.RequestSummary(context.Background(), RequestQuery{
				Project:      testProject,
				ExcludeHosts: []string{pendingHost},
			})
			return err
		}},
		{"series", "r.", func(c *Client) error {
			_, err := c.RequestSeries(context.Background(), RequestSeriesQuery{
				RequestQuery: RequestQuery{
					Project:      testProject,
					ExcludeHosts: []string{pendingHost},
				},
				Buckets: 60,
			})
			return err
		}},
		{"routes", "r.", func(c *Client) error {
			_, err := c.RequestRoutes(context.Background(), RequestRoutesQuery{
				RequestQuery: RequestQuery{
					Project:      testProject,
					ExcludeHosts: []string{pendingHost},
				},
			})
			return err
		}},
		{"listing", "", func(c *Client) error {
			_, err := c.QueryRequests(context.Background(), RequestListQuery{
				Project:      testProject,
				ExcludeHosts: []string{pendingHost},
			})
			return err
		}},
	} {
		t.Run(read.name, func(t *testing.T) {
			store := newFakeLogStore(t)
			if err := read.run(store.client(t)); err != nil {
				t.Fatalf("%s: %v", read.name, err)
			}
			want := read.prefix + "host NOT IN ({excludedHost0:String})"
			if !strings.Contains(store.query, want) {
				t.Fatalf("expected %q in the read's predicate:\n%s", want, store.query)
			}
			if got := store.params.Get("param_excludedHost0"); got != pendingHost {
				t.Errorf("the hostname parameter = %q, want %q", got, pendingHost)
			}
		})
	}
}

// A hostname comes from a Domain somebody attached, so it never reaches the
// statement text — the same rule the health route travels under.
func TestAnExcludedHostnameNeverReachesTheQueryText(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).QueryRequests(context.Background(), RequestListQuery{
		Project:      testProject,
		ExcludeHosts: []string{"app.example.com'; DROP TABLE http_requests; --"},
	}); err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	if strings.Contains(store.query, "DROP TABLE") {
		t.Fatalf("the hostname reached the query text:\n%s", store.query)
	}
	// Lower-cased on the way in, because that is how a DNS name is stored:
	// the injection attempt travels whole, as a parameter.
	if got := store.params.Get("param_excludedHost0"); !strings.Contains(got, "drop table") {
		t.Errorf("the hostname should travel as a parameter, got %q", got)
	}
}

// Naming a route does not cancel the hostname exclusion, which is the one way
// it differs from the health check: a template says nothing about the name it
// was asked on, so the route the edge answered on an unrouted hostname is not
// what a caller filtering to that template asked for.
func TestNamingARouteStillDropsTheUnroutedHostnames(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project:       testProject,
		Route:         testHealthRoute.Route,
		ExcludeHealth: []HealthRoute{testHealthRoute},
		ExcludeHosts:  []string{pendingHost},
	}); err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if strings.Contains(store.query, "NOT (") {
		t.Fatalf("a named route should not also exclude the health check:\n%s", store.query)
	}
	if !strings.Contains(store.query, "r.host NOT IN ({excludedHost0:String})") {
		t.Fatalf("the hostname exclusion should survive a route filter:\n%s", store.query)
	}
}

// An empty hostname is the unattributed bucket's own value, and a duplicate is
// what an environment with the same name attached twice would produce. Neither
// may become a predicate: the first would drop traffic nobody could attribute
// from a read that never had any, and the second is one exclusion written
// twice.
func TestHostExclusionIgnoresEmptyAndRepeatedHostnames(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).QueryRequests(context.Background(), RequestListQuery{
		Project:      testProject,
		ExcludeHosts: []string{"", "  ", pendingHost, "APP.example.com", "shop.example.com"},
	}); err != nil {
		t.Fatalf("QueryRequests: %v", err)
	}
	want := "host NOT IN ({excludedHost0:String}, {excludedHost1:String})"
	if !strings.Contains(store.query, want) {
		t.Fatalf("expected two hostnames and no more:\n%s", store.query)
	}
	if got := store.params.Get("param_excludedHost1"); got != "shop.example.com" {
		t.Errorf("the second hostname = %q, want the one that was not a repeat", got)
	}
}

// Nothing to exclude appends nothing: a tautology in the predicate is a query
// that reads as though something was dropped.
func TestNoHostnamesExcludeNothing(t *testing.T) {
	store := newFakeLogStore(t)

	if _, err := store.client(t).RequestSummary(context.Background(), RequestQuery{
		Project: testProject,
	}); err != nil {
		t.Fatalf("RequestSummary: %v", err)
	}
	if strings.Contains(store.query, "host NOT IN") {
		t.Fatalf("nothing was named, so nothing should be excluded:\n%s", store.query)
	}
}
