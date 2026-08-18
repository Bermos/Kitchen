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
	"context"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/Bermos/Kitchen/internal/controller"
)

// Host attribution, the load-bearing half of §3.2.
//
// A request is attributed by the authority in `l7.http.url`, not by the flow's
// destination endpoint, and the difference is the whole point. For a protected
// preview the destination is the shared forward-auth gate; for an idling
// environment it is KEDA's interceptor. An attribution keyed on the
// destination therefore files every preview on the platform under the gate and
// every cold start under the autoscaler — which is exactly the misattribution
// the flows-only traffic view has today and this pipeline exists to fix.
//
// The Host header is the one thing every hop preserves. It has to be: it is
// what the interceptor routes on, so the platform's own scale-to-zero would
// not work if any hop rewrote it. That makes the authority a name for the
// application however many proxies the request passed through.
//
// What the authority is looked up in is the operator's own routing knowledge.
// Every hostname an application is served on was published by
// EnvironmentReconciler as an HTTPRoute — the generated URL and every verified
// custom domain, on one route, carrying the project and environment labels
// every child of an Environment carries. Listing HTTPRoutes cluster-wide is
// therefore exactly "every hostname the platform published, with its
// attribution attached", and needs no second source of truth to drift from.

const (
	// hostRefreshInterval is how often the table is rebuilt from the
	// platform's routes. It matches the collector's config-poll cadence
	// because it answers the same kind of question — what has the operator
	// changed since we last looked — and neither is worth a watch.
	hostRefreshInterval = 30 * time.Second

	// hostMissInterval floors how often a host the table does not know may
	// force a rebuild. Without a floor, traffic to hostnames nobody published
	// is traffic that lists the platform's routes once per request, and that
	// traffic is by definition the traffic a scanner sends.
	hostMissInterval = 5 * time.Second
)

// attribution is whose request this was. Both fields empty is the unattributed
// bucket, which is a finding rather than a defect — §7's `edge.unrouted-hosts`
// reads it to catch stale DNS entries and hosts nobody ever published, once it
// has subtracted the names the platform did publish (see PublishedHosts).
type attribution struct {
	project     string
	environment string
}

// hostTable is one reading of the host → environment map.
type hostTable struct {
	exact map[string]attribution
	// wildcards holds the entries a route published with a leading `*.`.
	// Kitchen's own generated hostnames are never wildcards and a custom
	// Domain is documented as fully qualified, but Gateway API permits one and
	// nothing refuses it at admission — so a wildcard route that silently
	// attributed all of its traffic to nobody would look exactly like a
	// platform-wide DNS fault on the Edge screen.
	wildcards []wildcardHost
}

type wildcardHost struct {
	// suffix keeps the dot, so `*.example.com` matches `shop.example.com` and
	// not `notexample.com`.
	suffix string
	owner  attribution
}

// lookup attributes one authority.
func (t hostTable) lookup(authority string) attribution {
	host := NormaliseHost(authority)
	if host == "" {
		return attribution{}
	}
	if owner, ok := t.exact[host]; ok {
		return owner
	}
	for _, candidate := range t.wildcards {
		if strings.HasSuffix(host, candidate.suffix) {
			return candidate.owner
		}
	}
	return attribution{}
}

// NormaliseHost reduces an authority to the name a route publishes: without
// the userinfo Hubble redacts into the URL, without the port Envoy records
// when the client named one, without the trailing dot a client is allowed to
// write on a fully qualified name, and lower-cased, because DNS names are
// case-insensitive and a `Host` header is whatever the client felt like
// sending. Every one of those would otherwise be a second value for one host,
// both in the map and in the store's host facet.
func NormaliseHost(authority string) string {
	host := strings.TrimSpace(authority)
	if at := strings.LastIndexByte(host, '@'); at >= 0 {
		host = host[at+1:]
	}
	if strings.HasPrefix(host, "[") {
		// An IPv6 literal: the colons inside the brackets are the address, not
		// a port, so the port can only be after the closing bracket.
		if end := strings.IndexByte(host, ']'); end >= 0 {
			host = host[:end+1]
		}
	} else if colon := strings.LastIndexByte(host, ':'); colon >= 0 {
		host = host[:colon]
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// hostsFromRoutes builds the table from the routes the platform published.
//
// A route carrying neither label is one of the platform's own surfaces — the
// dashboard, the API, the git webhook receiver — and is deliberately left out.
// Those requests belong to no project, and inventing one for them would put
// platform traffic on a project's charts. They land in the same unattributed
// bucket a host nobody published lands in, which is a distinction a request
// row has no column for and the `edge.unrouted-hosts` signal does not need one
// for: it evaluates against the operator's informer caches, so it subtracts
// the hostnames the platform did publish — PublishedHosts, below — before
// calling anything unrouted.
func hostsFromRoutes(routes []gatewayv1.HTTPRoute) hostTable {
	table := hostTable{exact: make(map[string]attribution, len(routes))}
	for i := range routes {
		labels := routes[i].GetLabels()
		owner := attribution{
			project:     labels[controller.LabelProject],
			environment: labels[controller.LabelEnvironment],
		}
		if owner.project == "" || owner.environment == "" {
			continue
		}
		for _, hostname := range routes[i].Spec.Hostnames {
			host := NormaliseHost(string(hostname))
			switch {
			case host == "":
			case strings.HasPrefix(host, "*."):
				table.wildcards = append(table.wildcards, wildcardHost{
					suffix: host[len("*"):],
					owner:  owner,
				})
			default:
				table.exact[host] = owner
			}
		}
	}
	return table
}

// HostSet is a set of hostnames the platform published, wildcards included.
type HostSet struct {
	exact     map[string]struct{}
	wildcards []string
}

// Covers reports whether an authority is one of the names in the set.
func (s HostSet) Covers(authority string) bool {
	host := NormaliseHost(authority)
	if host == "" {
		return false
	}
	if _, ok := s.exact[host]; ok {
		return true
	}
	for _, suffix := range s.wildcards {
		if strings.HasSuffix(host, suffix) {
			return true
		}
	}
	return false
}

// Len is how many names the set holds, which is what a reader over-fetching a
// bounded list needs: at most this many of its rows can be published ones.
func (s HostSet) Len() int {
	return len(s.exact) + len(s.wildcards)
}

// PublishedHosts is every hostname the platform published, whoever published
// it: the applications' routes and the platform's own surfaces alike.
//
// It is deliberately not hostsFromRoutes. That map answers "whose traffic is
// this", so a route carrying no project labels is no answer and is skipped —
// the dashboard's requests belong to no project and inventing one for them
// would put platform traffic on a project's charts. This set answers the other
// question, "did the platform publish this name at all", and there the
// dashboard's own hostname is as published as any application's. It is what
// `edge.unrouted-hosts` subtracts before calling a host unrouted, so that the
// platform's own URL is not reported as traffic nobody published.
//
// A route with no hostnames publishes no name of its own — it serves whatever
// its listener does — and is skipped rather than read as publishing
// everything. The operator writes exactly one of those, the HTTPS redirect on
// port 80, and treating it as a wildcard would silence the signal on every
// acme installation.
func PublishedHosts(routes []gatewayv1.HTTPRoute) HostSet {
	set := HostSet{exact: make(map[string]struct{}, len(routes))}
	for i := range routes {
		for _, hostname := range routes[i].Spec.Hostnames {
			host := NormaliseHost(string(hostname))
			switch {
			case host == "":
			case strings.HasPrefix(host, "*."):
				set.wildcards = append(set.wildcards, host[len("*"):])
			default:
				set.exact[host] = struct{}{}
			}
		}
	}
	return set
}

// hostIndex is the attribution table plus the policy that keeps it current. It
// belongs to the follower's goroutine and inherits the budget's no-locking
// rule; the counters another package reads live in loss.go instead.
type hostIndex struct {
	client    client.Client
	log       logr.Logger
	table     hostTable
	refreshed time.Time
	missed    time.Time
}

func newHostIndex(ctx context.Context, reader client.Client, log logr.Logger) *hostIndex {
	index := &hostIndex{client: reader, log: log}
	index.rebuild(ctx)
	return index
}

// lookup attributes one authority, treating a miss as evidence that the table
// is behind the platform.
//
// A hostname the operator published seconds ago is indistinguishable from one
// nobody ever published until the routes are read again, and the first
// requests to a new preview are precisely the ones somebody is watching for.
// The rebuild is floored by hostMissInterval so that traffic to hostnames that
// really are unrouted cannot turn every request into a listing.
func (i *hostIndex) lookup(ctx context.Context, authority string) attribution {
	owner := i.table.lookup(authority)
	// A request that named no host at all is not evidence of anything — no
	// listing will ever produce a hostname for it — so it must not be able to
	// keep the rebuild armed.
	if owner.project != "" || authority == "" || time.Since(i.missed) < hostMissInterval {
		return owner
	}
	i.missed = time.Now()
	i.rebuild(ctx)
	return i.table.lookup(authority)
}

// tick rebuilds the table once it is old enough, and is called often enough
// that "old enough" is the only thing deciding when.
func (i *hostIndex) tick(ctx context.Context) {
	if time.Since(i.refreshed) < hostRefreshInterval {
		return
	}
	i.rebuild(ctx)
}

// rebuild reads the platform's routes and replaces the table with them.
//
// The listing goes through the manager's cache, so the cost of asking often is
// a map walk rather than a request to the API server. A listing that fails
// leaves the previous table in place: a stale attribution puts a project's
// traffic on the right charts a little late, where an emptied one puts every
// project's traffic in the unrouted bucket and fires the operator's
// unrouted-hosts signal for the whole platform at once.
func (i *hostIndex) rebuild(ctx context.Context) {
	routes := &gatewayv1.HTTPRouteList{}
	if err := i.client.List(ctx, routes); err != nil {
		i.log.V(1).Info("route listing failed, keeping the previous attribution", "reason", err.Error())
		return
	}
	i.table = hostsFromRoutes(routes.Items)
	i.refreshed = time.Now()
}
