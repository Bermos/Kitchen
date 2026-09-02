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
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The golden signals for one environment: how much traffic it served, how much
// of it failed, how slow it was, and which routes that happened on. They cost
// the application nothing, because every request to every Kitchen application
// crosses the shared Gateway's proxy — an application nobody instrumented still
// has latency and error numbers, observed where they all pass anyway.
//
// What a request row cannot say is as load-bearing as what it can, and these
// endpoints promise neither:
//
//   - No build and no release. The edge routes to a Service, not to a pod, and
//     during a rollout both revisions answer under one route.
//   - No query strings. They are stripped at ingest and never stored, which is
//     privacy and path cardinality settled in one move.
//   - For gRPC, no application errors. A failed gRPC call is an HTTP 200 with a
//     `grpc-status` trailer the edge does not read, so its errors are
//     transport-level here.
//
// The three aggregate reads are answered from the rollups and the listing from
// the raw rows, which is why a year-wide summary is as cheap as an hour's and
// why the listing's window is the shorter one: raw rows are kept for seven days.

// requestQueryFrom reads the window, the route filter and the health-check
// decision the request reads share. An absent window is the store's own
// default — the hour ending now.
//
// The health check the platform makes of this application is not its traffic
// (see healthchecks.go), so it is left out unless `?health=include` asks for
// it. The view that comes back with the query is what the answer says about
// that, and every one of these endpoints carries it: a number that silently
// dropped rows is a number nobody can reconcile.
func (s *Server) requestQueryFrom(
	req *http.Request, env *kitchenv1alpha1.Environment,
) (clickhouse.RequestQuery, healthChecksView, error) {
	since, until, err := windowFrom(req)
	if err != nil {
		return clickhouse.RequestQuery{}, healthChecksView{}, err
	}
	include, err := includeHealth(req)
	if err != nil {
		return clickhouse.RequestQuery{}, healthChecksView{}, err
	}

	// Every request read is project-scoped, and the project is the
	// environment's own — the caller names an environment and the API
	// resolves what it belongs to, the way the log and metric endpoints do.
	project := &kitchenv1alpha1.Project{}
	if err := s.get(req.Context(), env.Spec.ProjectRef.Name, project); err != nil {
		return clickhouse.RequestQuery{}, healthChecksView{}, err
	}

	query := clickhouse.RequestQuery{
		Project:     project.Name,
		Environment: env.Name,
		Since:       since,
		Until:       until,
		Route:       strings.TrimSpace(req.URL.Query().Get("route")),
	}
	health := healthChecksView{Route: healthRouteOf(project).Route}
	// A caller filtering to one route has named what they want counted, and
	// the store ignores the exclusion in that case; saying `excluded` here
	// would be the screen claiming something the numbers do not do.
	if !include && health.Route != "" && query.Route == "" {
		query.ExcludeHealth = []clickhouse.HealthRoute{{Project: project.Name, Route: health.Route}}
		health.Excluded = true
	}
	return query, health, nil
}

// writeRequestQueryError answers the two ways resolving one of these reads can
// fail. A parameter somebody wrote badly is a 400 naming it; an object that
// could not be read is whatever that read's own failure was — the 404 of a
// project deleted between the environment's read and its own, rather than a
// 400 blaming the caller for it.
func (s *Server) writeRequestQueryError(w http.ResponseWriter, err error) {
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		s.writeError(w, err)
		return
	}
	badRequest(w, "%s", err.Error())
}

// edgeView is the honest degrade for a workload the golden signals do not fit.
//
// An environment with no HTTPRoute is not on the platform's edge at all, and
// four empty charts would then describe the platform rather than the
// application. "Nothing reaches this environment through the edge" and "nothing
// was asked of it in this window" are different answers, and the screen says
// different things about them, so the response distinguishes them: this says
// whether the environment is published, and the numbers beside it say whether
// anything happened.
type edgeView struct {
	// Routed is false only where the platform is sure nothing publishes this
	// environment on the shared Gateway. A route that could not be read leaves
	// it true with a Message saying so, because declaring an application off
	// the edge on the strength of a failed read is the loud way to be wrong.
	Routed bool `json:"routed"`
	// Message is what the screen says when Routed is false, and what went
	// unchecked when it is true without proof. Empty for the ordinary case.
	Message string `json:"message,omitempty"`
}

// noEdgeMessage is the sentence the screen leads with for an environment the
// edge cannot see. It names what is real for such a workload instead, because
// a queue worker is not broken for having no HTTP traffic.
const noEdgeMessage = "no HTTP traffic reaches this environment through the platform's edge: " +
	"nothing publishes it on the shared Gateway, so there is nothing there to observe — " +
	"its logs, its resource usage against the release's limits and its restarts are what is real for it"

// edgeOf reports whether an HTTPRoute publishes this environment.
//
// The route object is read rather than the Environment's RouteProgrammed
// condition, because the object is the fact: a condition records what a
// reconcile decided, and an environment whose route was deleted out from under
// it still carries the condition that says one was applied.
func (s *Server) edgeOf(ctx context.Context, env *kitchenv1alpha1.Environment) edgeView {
	route := &gatewayv1.HTTPRoute{}
	key := types.NamespacedName{
		Namespace: controller.AppNamespace(env.Spec.ProjectRef.Name),
		Name:      env.Name,
	}
	err := s.Client.Get(ctx, key, route)
	switch {
	case err == nil:
		return edgeView{Routed: true}
	case apierrors.IsNotFound(err):
		return edgeView{Message: noEdgeMessage}
	default:
		// An installation whose Gateway API CRDs are a version behind, or a
		// ClusterRole that may not read routes, is not evidence about the
		// application. The numbers stay whatever the store has.
		return edgeView{
			Routed:  true,
			Message: "whether this environment is published on the shared Gateway could not be read: " + err.Error(),
		}
	}
}

// environmentOf resolves the Environment a path names, answering the request
// itself when there is none: a nil return means the response has been written.
// It is shaped like openLogStore, and for the same reason — every endpoint
// below starts by resolving the same object.
func (s *Server) environmentOf(w http.ResponseWriter, req *http.Request) *kitchenv1alpha1.Environment {
	env := &kitchenv1alpha1.Environment{}
	if err := s.get(req.Context(), req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return nil
	}
	return env
}

// requestSummaryBody is the golden-signal header. The store's summary is
// embedded whole, because the window it reports is the one it answered rather
// than the one that was asked for, and flattening it would lose that.
type requestSummaryBody struct {
	clickhouse.RequestSummary
	Environment  string           `json:"environment"`
	Edge         edgeView         `json:"edge"`
	HealthChecks healthChecksView `json:"healthChecks"`
}

// environmentRequestSummary answers the four tiles the environment page leads
// with: traffic, errors, latency, and the window they are true of.
func (s *Server) environmentRequestSummary(w http.ResponseWriter, req *http.Request) {
	env := s.environmentOf(w, req)
	if env == nil {
		return
	}
	query, health, err := s.requestQueryFrom(req, env)
	if err != nil {
		s.writeRequestQueryError(w, err)
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	summary, err := store.RequestSummary(req.Context(), query)
	if err != nil {
		s.writeStoreError(w, err, "the request summary query")
		return
	}
	writeJSON(w, http.StatusOK, requestSummaryBody{
		RequestSummary: summary,
		Environment:    env.Name,
		Edge:           s.edgeOf(req.Context(), env),
		HealthChecks:   health,
	})
}

// requestSeriesBody is the charts' answer, the series embedded for the same
// reason the summary is: it reports its own bucket width and the rollup that
// answered it.
type requestSeriesBody struct {
	clickhouse.RequestSeries
	Environment  string           `json:"environment"`
	Edge         edgeView         `json:"edge"`
	HealthChecks healthChecksView `json:"healthChecks"`
}

// environmentRequestSeries answers traffic, error rate and the latency
// percentiles over time — the three charts of the requests section.
func (s *Server) environmentRequestSeries(w http.ResponseWriter, req *http.Request) {
	env := s.environmentOf(w, req)
	if env == nil {
		return
	}
	query, health, err := s.requestQueryFrom(req, env)
	if err != nil {
		s.writeRequestQueryError(w, err)
		return
	}
	buckets, err := intParam(req, "buckets", clickhouse.DefaultRequestBuckets)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	series, err := store.RequestSeries(req.Context(),
		clickhouse.RequestSeriesQuery{RequestQuery: query, Buckets: buckets})
	if err != nil {
		s.writeStoreError(w, err, "the request series query")
		return
	}
	writeJSON(w, http.StatusOK, requestSeriesBody{
		RequestSeries: series,
		Environment:   env.Name,
		Edge:          s.edgeOf(req.Context(), env),
		HealthChecks:  health,
	})
}

// routeSorts is what `?sort=` may be, in the store's own vocabulary. The
// membership check lives here so that a typo is a 400 naming the choices, and
// not a read the caller is told failed.
var routeSorts = []string{
	clickhouse.RouteSortRequests,
	clickhouse.RouteSortErrors,
	clickhouse.RouteSortErrorRate,
	clickhouse.RouteSortLatency,
}

// requestRoutesBody is the route table. It carries no window of its own: the
// rows are aggregates over the same snapped window the summary reports, and
// echoing a second copy of it invites the two to disagree.
type requestRoutesBody struct {
	Items        []clickhouse.RequestRoute `json:"items"`
	Environment  string                    `json:"environment"`
	Edge         edgeView                  `json:"edge"`
	HealthChecks healthChecksView          `json:"healthChecks"`
}

// environmentRequestRoutes answers one row per route template.
//
// The sort is a query rather than a presentation detail, because it decides
// which rows survive the limit: the ten slowest routes and the ten busiest are
// not the same ten.
func (s *Server) environmentRequestRoutes(w http.ResponseWriter, req *http.Request) {
	env := s.environmentOf(w, req)
	if env == nil {
		return
	}
	query, health, err := s.requestQueryFrom(req, env)
	if err != nil {
		s.writeRequestQueryError(w, err)
		return
	}
	limit, err := intParam(req, "limit", clickhouse.DefaultRequestGroupLimit)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	sortBy := strings.TrimSpace(req.URL.Query().Get("sort"))
	if sortBy != "" && !slices.Contains(routeSorts, sortBy) {
		badRequest(w, "cannot sort routes by %q; the sorts are %s", sortBy, strings.Join(routeSorts, ", "))
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	routes, err := store.RequestRoutes(req.Context(),
		clickhouse.RequestRoutesQuery{RequestQuery: query, SortBy: sortBy, Limit: limit})
	if err != nil {
		s.writeStoreError(w, err, "the route breakdown query")
		return
	}
	writeJSON(w, http.StatusOK, requestRoutesBody{
		Items:        itemsOf(routes),
		Environment:  env.Name,
		Edge:         s.edgeOf(req.Context(), env),
		HealthChecks: health,
	})
}

// requestListBody is the raw request list. It is an object with an `items`
// array rather than the bare collection the other listings answer with, because
// the edge's answer belongs beside the rows: an empty list means one thing for
// an environment on the edge and another for one that is not.
type requestListBody struct {
	Items        []clickhouse.Request `json:"items"`
	Environment  string               `json:"environment"`
	Edge         edgeView             `json:"edge"`
	HealthChecks healthChecksView     `json:"healthChecks"`
}

// environmentRequests answers the raw rows, newest first, and follows them live
// when asked to — the same Accept negotiation the log endpoints use, over the
// same loop.
func (s *Server) environmentRequests(w http.ResponseWriter, req *http.Request) {
	env := s.environmentOf(w, req)
	if env == nil {
		return
	}
	query, health, err := s.requestListFrom(req, env)
	if err != nil {
		s.writeRequestQueryError(w, err)
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	if wantsEventStream(req) {
		streamRows(s, w, req,
			func(ctx context.Context, since time.Time) ([]clickhouse.Request, error) {
				follow := query
				if !since.IsZero() {
					follow.Since = since
					follow.Limit = clickhouse.MaxRequestLimit
				}
				rows, err := store.QueryRequests(ctx, follow)
				if err != nil {
					return nil, err
				}
				// A listing reads newest first, which is the order a request
				// list is shown in. The follow loop walks a boundary forwards
				// through time and needs the other order, so the tail sends the
				// page oldest first and the client prepends.
				slices.Reverse(rows)
				return rows, nil
			},
			func(row clickhouse.Request) time.Time { return row.Timestamp },
			requestKey)
		return
	}

	rows, err := store.QueryRequests(req.Context(), query)
	if err != nil {
		s.writeStoreError(w, err, "the request query")
		return
	}
	writeJSON(w, http.StatusOK, requestListBody{
		Items:        itemsOf(rows),
		Environment:  env.Name,
		Edge:         s.edgeOf(req.Context(), env),
		HealthChecks: health,
	})
}

// requestListFrom reads what the raw listing is filtered by on top of the
// window: a route template, a verb, a class of answer, and the failures alone.
func (s *Server) requestListFrom(
	req *http.Request, env *kitchenv1alpha1.Environment,
) (clickhouse.RequestListQuery, healthChecksView, error) {
	query, health, err := s.requestQueryFrom(req, env)
	if err != nil {
		return clickhouse.RequestListQuery{}, healthChecksView{}, err
	}
	limit, err := intParam(req, "limit", clickhouse.DefaultRequestLimit)
	if err != nil {
		return clickhouse.RequestListQuery{}, healthChecksView{}, err
	}
	statusClass, err := statusClassParam(req)
	if err != nil {
		return clickhouse.RequestListQuery{}, healthChecksView{}, err
	}
	return clickhouse.RequestListQuery{
		Project:     query.Project,
		Environment: query.Environment,
		Since:       query.Since,
		Until:       query.Until,
		Route:       query.Route,
		// The rows under a set of numbers are the traffic those numbers are
		// of, so the listing drops what the aggregates dropped.
		ExcludeHealth: query.ExcludeHealth,
		// The follower canonicalises the verb before it stores it, so the
		// filter matches the stored spelling rather than the typed one.
		Method:      strings.ToUpper(strings.TrimSpace(req.URL.Query().Get("method"))),
		StatusClass: statusClass,
		OnlyErrors:  req.URL.Query().Get("errors") == "1",
		Limit:       limit,
	}, health, nil
}

// statusClassParam reads `?status=`, which selects a class of answer rather
// than one code: `5xx` — or plainly `5` — is every 5xx. One exact code is
// deliberately not offered; the question a request list is opened with is "show
// me the failures", and the route table is where a window is broken down.
func statusClassParam(req *http.Request) (int, error) {
	raw := strings.TrimSpace(req.URL.Query().Get("status"))
	if raw == "" {
		return 0, nil
	}
	class, err := strconv.Atoi(strings.TrimSuffix(strings.ToLower(raw), "xx"))
	if err != nil || class < 1 || class > 5 {
		return 0, fmt.Errorf("status must be a class of answer — 2xx, 4xx, 5xx (got %q)", raw)
	}
	return class, nil
}

// requestKey identifies a row inside one instant, so that a live tail
// re-reading its boundary inclusively does not send the same request twice.
// The store's timestamps carry microseconds, so a collision is two identical
// requests answered identically within the same microsecond.
func requestKey(row clickhouse.Request) string {
	return row.Method + "\x00" + row.Path + "\x00" + row.Host + "\x00" +
		strconv.FormatUint(uint64(row.Status), 10) + "\x00" +
		strconv.FormatFloat(row.DurationMs, 'f', -1, 64)
}

// itemsOf keeps an empty collection an empty array rather than a JSON null, the
// way writeList does for the endpoints whose whole body is the collection.
func itemsOf[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}
