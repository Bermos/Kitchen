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

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/flows"
)

// Which of an environment's requests the environment did not answer.
//
// A request row is attributed by the authority the client asked for, and the
// authorities are the hostnames the environment's route carries — its
// generated one and every custom domain attached to it. A domain joins that
// route as soon as it is verified and stays on it while the gateway works
// through accepting it, which is a window that can last as long as a
// certificate takes to issue. Traffic arriving in that window is attributed to
// the environment and answered by the platform's edge, which has no route for
// the name yet — whatever the edge answers with, the application never saw it.
//
// That is not a rare corner. It is exactly what happens while a domain is
// coming up, because issuing its certificate is what sends traffic at it —
// Let's Encrypt retries `/.well-known/acme-challenge/<token>` for as long as
// validation is outstanding. Left in the numbers it is a route the application
// never served, a request count it never had, and an error rate belonging to
// the edge; and it lands on the screen of exactly the person trying to work
// out why the domain will not come up (#574).
//
// So the environment's request reads leave it out by default and say that they
// did, the way they do for the platform's own health checks — same shape, same
// one parameter to ask for it back. Two things this deliberately is not:
//
//   - **It is not a judgement about a row.** Nothing stored says whether the
//     edge or the application answered a request: the row carries the host,
//     the path, the status and the latency, and no upstream, backend or
//     response flag. What is known is which hostnames the platform is routing,
//     so the exclusion is by hostname and never by guessing at a request.
//   - **It is not retrospective.** The hostnames are the ones the platform is
//     not serving *now*, so a domain that came up inside the window leaves the
//     traffic it took before it did counted. The answer names the hostnames it
//     excluded for that reason: a number that silently dropped rows is a
//     number nobody can reconcile.

// pendingHostsOf is every hostname of this environment's whose traffic the
// store filed under the environment and the platform's edge is what answered,
// lower-cased the way the stored rows spell it.
//
// Two things have to hold for a hostname to belong here, and each rules out a
// different way of saying something untrue on a Project-scope screen:
//
//   - **The environment's route carries it.** Attribution is not a property of
//     the Domain: flows.hostsFromRoutes builds the host -> environment map out
//     of exactly the hostnames the HTTPRoutes publish, so a name that is not
//     on this environment's route is a name whose requests were filed under
//     nobody, and there is nothing here to subtract. Every reason the domain
//     reconciler writes before it has looked at the route says so — a domain
//     nobody verified, an environment of an `internal`-exposure project with
//     no route at all — and naming one of those would put a permanent
//     sentence about traffic that cannot exist on the reader's screen.
//   - **The gateway is not serving it.** That is the Domain's own
//     RouteProgrammed condition, rather than a second reading of which
//     listener the hostname rides: DomainReconciler recomputes it from this
//     same route and the gateway's acceptance of the listener this hostname's
//     traffic actually uses, and it watches routes so that acceptance reaches
//     it promptly. Re-deriving it would mean a second copy of the platform's
//     TLS mode and the domain's own — one answer, arrived at twice, is how
//     the two come to disagree.
func (s *Server) pendingHostsOf(
	ctx context.Context, env *kitchenv1alpha1.Environment,
) ([]string, error) {
	carried := s.routedHostnamesOf(ctx, env)
	if len(carried) == 0 {
		return nil, nil
	}
	list := &kitchenv1alpha1.DomainList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(list.Items))
	for i := range list.Items {
		domain := &list.Items[i]
		if domain.Spec.EnvironmentRef.Name != env.Name {
			continue
		}
		if meta.IsStatusConditionTrue(domain.Status.Conditions, controller.ConditionRouteProgrammed) {
			continue
		}
		// NormaliseHost is the follower's own, so the name compared here is
		// spelled exactly as the `host` column spells it — and exactly as
		// routedHostnamesOf spelled the route's.
		host := flows.NormaliseHost(domain.Spec.Hostname)
		if _, published := carried[host]; !published {
			continue
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// routedHostnamesOf is the set of hostnames this environment's own HTTPRoute
// carries, spelled the way the stored rows spell them.
//
// The route object is read rather than a condition, for edgeOf's reason: the
// object is the fact. It is also the exact field the attribution pipeline
// reads, which is what makes this set "the names whose requests were filed
// under this environment" rather than an approximation of it.
//
// An environment with no route, and a route that could not be read, both
// answer the empty set. Neither is evidence that the platform is answering for
// any name, and an exclusion nothing justifies is worse than numbers that
// include everything — a failed read is already reported beside them, by
// edgeOf.
//
// A wildcard hostname is left out. Gateway API permits one and nothing refuses
// it at admission — internal/flows/hosts.go carries a wildcards list for that
// reason, and does attribute the traffic — but the stored `host` values are
// concrete names, so hostExclusion's `host NOT IN (...)` can never match one.
// Naming it would be the answer claiming a drop that did not happen; leaving
// its traffic counted is at least the number the read had before.
func (s *Server) routedHostnamesOf(
	ctx context.Context, env *kitchenv1alpha1.Environment,
) map[string]struct{} {
	route := &gatewayv1.HTTPRoute{}
	key := types.NamespacedName{
		Namespace: controller.AppNamespace(env.Spec.ProjectRef.Name),
		Name:      env.Name,
	}
	if err := s.Client.Get(ctx, key, route); err != nil {
		return nil
	}
	hosts := make(map[string]struct{}, len(route.Spec.Hostnames))
	for _, hostname := range route.Spec.Hostnames {
		host := flows.NormaliseHost(string(hostname))
		if host == "" || strings.HasPrefix(host, "*.") {
			continue
		}
		hosts[host] = struct{}{}
	}
	return hosts
}

// pendingDomainsView is what a request answer says about the hostnames it left
// out. `hostnames` are the names attached to this environment that the
// platform is not serving — present whether or not this read excluded them, so
// the screen can offer to put them back — and `excluded` is whether these
// numbers left them out.
type pendingDomainsView struct {
	Hostnames []string `json:"hostnames,omitempty"`
	Excluded  bool     `json:"excluded"`
}

// The values `?pending=` takes, spelled like `?health=` because they are the
// same question asked of a different set of rows.
const (
	pendingExclude = "exclude"
	pendingInclude = "include"
)

// includePending reads `?pending=`. An unknown value is refused rather than
// guessed at, for the reason includeHealth gives.
func includePending(req *http.Request) (bool, error) {
	switch value := strings.TrimSpace(req.URL.Query().Get("pending")); value {
	case "", pendingExclude:
		return false, nil
	case pendingInclude:
		return true, nil
	default:
		return false, fmt.Errorf("pending must be %s or %s (got %q)", pendingExclude, pendingInclude, value)
	}
}
