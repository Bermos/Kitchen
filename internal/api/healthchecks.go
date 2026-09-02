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

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/flows"
)

// Which requests are the platform's own checks rather than somebody's visit.
//
// The store's half of this is in clickhouse/healthchecks.go, and says why a
// project's traffic numbers leave them out. This half is the only thing that
// knows the answer: a health check is the path `spec.runtime.health.path` names
// on the project, which is the path the platform asks the application for and
// the one thing here it is entitled to call not-traffic. Everything else on a
// project's edge is the project's, whoever sent it.
//
// It is read off the Project rather than off the Release the environment is
// running, although the release carries a snapshot of the same runtime. The
// question is "what does the platform ask this application for", not "what did
// it ask it for at 04:00 last Tuesday", and a window usually spans several
// releases: reading the current declaration gives every environment of a
// project the same answer, and gives the whole platform one answer per project
// rather than one per environment per release.
//
// A worker's health check (`spec.processes[].health`) is deliberately not here.
// Nothing publishes a worker on the shared Gateway, so its probes never become
// request rows in the first place.

// healthRouteOf is one project's health check as the store matches it: the
// route template the declared path belongs to, derived by the same code the
// follower templates a request with. A project that declared no HTTP health
// check — the TCP-connect default — has no route and excludes nothing.
func healthRouteOf(project *kitchenv1alpha1.Project) clickhouse.HealthRoute {
	path := strings.TrimSpace(project.Spec.Runtime.Health.HTTPPath())
	if path == "" {
		return clickhouse.HealthRoute{}
	}
	return clickhouse.HealthRoute{Project: project.Name, Route: flows.RouteTemplate(path)}
}

// healthRoutes is the health check of every project, for the reads that answer
// across projects. The pairs are what keeps that safe: one project's route
// never subtracts from another's traffic, however the two spell their checks.
//
// Projects that declare no HTTP check are left out entirely rather than carried
// as empty pairs, so a platform where nobody declared one asks for no exclusion
// at all.
func (s *Server) healthRoutes(ctx context.Context) ([]clickhouse.HealthRoute, error) {
	list := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	routes := make([]clickhouse.HealthRoute, 0, len(list.Items))
	for i := range list.Items {
		if route := healthRouteOf(&list.Items[i]); route.Route != "" {
			routes = append(routes, route)
		}
	}
	return routes, nil
}

// healthChecksView is what a request answer says about the checks it left out,
// and it is not optional decoration: a number that silently excludes something
// is a number nobody can reconcile against the store. `route` is what the
// platform asks for — present whether or not this read excluded it, so the
// screen can offer to put it back — and `excluded` is whether these numbers
// left it out.
type healthChecksView struct {
	Route    string `json:"route,omitempty"`
	Excluded bool   `json:"excluded"`
}

// The values `?health=` takes. The default is the whole point of the feature,
// and `include` is how somebody asks the older question back.
const (
	healthExclude = "exclude"
	healthInclude = "include"
)

// includeHealth reads `?health=`. An unknown value is refused rather than
// guessed at, because guessing means quietly answering a different question
// than the one that was asked.
func includeHealth(req *http.Request) (bool, error) {
	switch value := strings.TrimSpace(req.URL.Query().Get("health")); value {
	case "", healthExclude:
		return false, nil
	case healthInclude:
		return true, nil
	default:
		return false, fmt.Errorf("health must be %s or %s (got %q)", healthExclude, healthInclude, value)
	}
}
