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
	"net/http"
	"sort"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The activity feed, the traffic map and the dashboard's metrics are all
// reads of the same telemetry store the logs live in. Like the log endpoints,
// they answer 503 on an installation that runs without one.

// listEvents serves the platform's recent activity, newest first.
func (s *Server) listEvents(w http.ResponseWriter, req *http.Request) {
	limit, err := intParam(req, "limit", clickhouse.DefaultEventLimit)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	since, err := timeParam(req, "since")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	events, err := store.QueryEvents(req.Context(), clickhouse.EventQuery{
		Project: strings.TrimSpace(req.URL.Query().Get("project")),
		Since:   since,
		Limit:   limit,
	})
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeList(w, events)
}

// metricsOverviewBody is the metrics answer: the store's aggregates plus a
// per-project join, which speaks the API's vocabulary (projects) rather than
// the store's (namespaces).
type metricsOverviewBody struct {
	clickhouse.MetricsOverview
	Projects []projectTraffic `json:"projects,omitempty"`
}

type projectTraffic struct {
	Project         string   `json:"project"`
	Requests24h     uint64   `json:"requests24h"`
	Errors5xx24h    uint64   `json:"errors5xx24h"`
	P95Ms           float64  `json:"p95Ms"`
	RequestsPerHour []uint64 `json:"requestsPerHour"`
}

// metricsOverview serves the dashboard's numbers, pre-aggregated. `?project=`
// narrows everything to one project.
func (s *Server) metricsOverview(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	query := clickhouse.MetricsQuery{}
	if project := strings.TrimSpace(req.URL.Query().Get("project")); project != "" {
		// The project has to exist before its numbers are worth computing: a
		// typo should say "no such project", not answer zeroes.
		obj := &kitchenv1alpha1.Project{}
		if err := s.get(ctx, project, obj); err != nil {
			s.writeError(w, err)
			return
		}
		query.Project = project
		query.Namespace = controller.AppNamespace(project)
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	overview, err := store.MetricsOverview(ctx, query)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := metricsOverviewBody{MetricsOverview: overview}
	if query.Project == "" {
		body.Projects, err = s.joinNamespaceTraffic(ctx, overview.Namespaces)
		if err != nil {
			s.writeError(w, err)
			return
		}
	}
	// The namespace rows were only raw material for the join; the answer
	// speaks in projects.
	body.MetricsOverview.Namespaces = nil
	writeJSON(w, http.StatusOK, body)
}

// joinNamespaceTraffic maps per-namespace traffic onto the projects whose app
// namespaces they are. Every project gets a row — a project nobody visited
// still belongs in the table, at zero — while namespaces that are no
// project's (the platform's own, kube-system) fall away.
func (s *Server) joinNamespaceTraffic(ctx context.Context, namespaces []clickhouse.NamespaceTraffic) ([]projectTraffic, error) {
	list := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}

	byNamespace := make(map[string]clickhouse.NamespaceTraffic, len(namespaces))
	for _, entry := range namespaces {
		byNamespace[entry.Namespace] = entry
	}

	projects := make([]projectTraffic, 0, len(list.Items))
	for _, project := range list.Items {
		entry := byNamespace[controller.AppNamespace(project.Name)]
		row := projectTraffic{
			Project:         project.Name,
			Requests24h:     entry.Requests24h,
			Errors5xx24h:    entry.Errors5xx24h,
			P95Ms:           entry.P95Ms,
			RequestsPerHour: entry.RequestsPerHour,
		}
		if row.RequestsPerHour == nil {
			row.RequestsPerHour = make([]uint64, 24)
		}
		projects = append(projects, row)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].Project < projects[j].Project })
	return projects, nil
}

// traffic serves the aggregated service map for a window: one edge per
// (source workload, destination workload) pair seen by the flow collector.
func (s *Server) traffic(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	since, err := timeParam(req, "since")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	until, err := timeParam(req, "until")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	query := clickhouse.TrafficQuery{Since: since, Until: until}
	if project := strings.TrimSpace(req.URL.Query().Get("project")); project != "" {
		obj := &kitchenv1alpha1.Project{}
		if err := s.get(ctx, project, obj); err != nil {
			s.writeError(w, err)
			return
		}
		query.Namespace = controller.AppNamespace(project)
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	edges, err := store.TrafficEdges(ctx, query)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeList(w, edges)
}
