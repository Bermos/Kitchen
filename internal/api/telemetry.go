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
	"sync"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The activity feed, the traffic map and the dashboard's metrics are all
// reads of the same telemetry store the logs live in. Like the log endpoints,
// they answer 503 on an installation that runs without one.
//
// Every query here is written by the operator, so a store that refuses one is
// reporting a fault in Kitchen and not a mistake the caller made: they go
// through writeStoreError, which names the read and leaves ClickHouse's own
// text in the operator's log. GET /logs is the exception — there the
// expression is the caller's, and its diagnostic is the answer.

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
		s.writeStoreError(w, err, "the activity query")
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

// overviewHours is the window the traffic numbers cover, in whole hours ending
// with the one in progress — the same 24 the store's own hourly series are
// drawn over, so the sparklines beside each other are the same shape.
const overviewHours = 24

// overviewConcurrency bounds the hourly platform reads below. Four is the
// signals gatherer's own bound, and for the same reason: enough that a day of
// hours does not serialise, few enough that one screen refresh is not a load
// test of a single-node ClickHouse.
const overviewConcurrency = 4

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
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	overview, err := store.MetricsOverview(ctx, query)
	if err != nil {
		s.writeStoreError(w, err, "the metrics query")
		return
	}

	// The window is the current hour and the 23 before it, which is what makes
	// every sparkline on this screen 24 entries long and its last one the hour
	// in progress.
	hourStart := time.Now().UTC().Truncate(time.Hour).Add(-(overviewHours - 1) * time.Hour)

	body := metricsOverviewBody{MetricsOverview: overview}
	if what, err := fillOverviewTraffic(ctx, store, &body, query.Project, hourStart); err != nil {
		s.writeStoreError(w, err, what)
		return
	}
	if query.Project == "" {
		traffic, err := store.ProjectTraffic(ctx, clickhouse.ProjectTrafficQuery{
			Since:     hourStart,
			Sparkline: true,
		})
		if err != nil {
			s.writeStoreError(w, err, "the per-project traffic query")
			return
		}
		body.Projects, err = s.joinProjectTraffic(ctx, traffic)
		if err != nil {
			s.writeError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// fillOverviewTraffic replaces the flow pipeline's traffic numbers with the
// request pipeline's, and is the last part of this endpoint to move.
//
// The correction is the one joinProjectTraffic documents, applied to the
// totals: flows are attributed by the *destination* endpoint, so a protected
// preview's traffic was credited to the forward-auth gate and an idling
// environment's to the KEDA interceptor. Both live in the platform's own
// namespace, so on the totals they were not lost but double-counted as
// platform traffic — and on any project-scoped view of them, missing entirely.
//
// It cannot be arithmetic over the per-project rows this endpoint already
// reads. Counts would add up; a p95 does not. The mean of twenty projects'
// p95s is not the platform's p95, and neither is the largest of them, so the
// percentile has to be merged from the states — which means reading the rollup
// across projects rather than adding up reads of it within them.
func fillOverviewTraffic(
	ctx context.Context,
	store logReader,
	body *metricsOverviewBody,
	project string,
	hourStart time.Time,
) (string, error) {
	// The three series are replaced whole rather than written into, so that no
	// bucket of the flow pipeline's answer survives in one the request
	// pipeline did not fill.
	body.RequestsPerHour = make([]uint64, overviewHours)
	body.ErrorsPerHour = make([]uint64, overviewHours)
	body.P95MsPerHour = make([]float64, overviewHours)

	if project != "" {
		return fillProjectOverviewTraffic(ctx, store, body, project, hourStart)
	}

	total, err := store.PlatformRequests(ctx, clickhouse.PlatformRequestsQuery{Since: hourStart})
	if err != nil {
		return whatPlatformTraffic, err
	}
	body.Requests24h, body.ErrorRate24h, body.P95Ms24h = total.Requests, total.ErrorRate, total.P95Ms

	// One read per hour, because the series carries a percentile and a
	// percentile is only mergeable inside the window it is merged over: there
	// is no cross-project series read to ask for 24 buckets of, and inventing
	// one out of the day-wide answer would be drawing a straight line. They run
	// together rather than in sequence; each is a small merge over one hour of
	// the minute rollup, and the whole set reads exactly the data the day-wide
	// query above already read.
	hours := make([]clickhouse.PlatformRequests, overviewHours)
	if err := inParallel(overviewHours, overviewConcurrency, func(hour int) error {
		start := hourStart.Add(time.Duration(hour) * time.Hour)
		bucket, err := store.PlatformRequests(ctx, clickhouse.PlatformRequestsQuery{
			Since: start,
			Until: start.Add(time.Hour),
		})
		if err != nil {
			return err
		}
		hours[hour] = bucket
		return nil
	}); err != nil {
		return whatPlatformTraffic, err
	}
	for hour, bucket := range hours {
		body.RequestsPerHour[hour] = bucket.Requests
		body.ErrorsPerHour[hour] = bucket.Errors
		body.P95MsPerHour[hour] = bucket.P95Ms
	}
	return "", nil
}

// fillProjectOverviewTraffic is the same correction for `?project=`, where the
// store has a read shaped exactly right: one summary and one series, both
// scoped to the project, both off the same rollups.
func fillProjectOverviewTraffic(
	ctx context.Context,
	store logReader,
	body *metricsOverviewBody,
	project string,
	hourStart time.Time,
) (string, error) {
	scope := clickhouse.RequestQuery{Project: project, Since: hourStart}
	summary, err := store.RequestSummary(ctx, scope)
	if err != nil {
		return "the project traffic query", err
	}
	body.Requests24h, body.ErrorRate24h, body.P95Ms24h = summary.Requests, summary.ErrorRate, summary.P95Ms

	series, err := store.RequestSeries(ctx, clickhouse.RequestSeriesQuery{
		RequestQuery: scope,
		Buckets:      overviewHours,
	})
	if err != nil {
		return "the project traffic series query", err
	}
	// The series reports the width it chose rather than the one that was asked
	// for, and the sparkline is 24 fixed hours: a bucket that does not land on
	// one of them is dropped rather than stretched, which keeps the chart's
	// x-axis the same on every screen that draws it.
	for _, point := range series.Points {
		hour := int(point.Start.Sub(hourStart) / time.Hour)
		if hour < 0 || hour >= overviewHours {
			continue
		}
		body.RequestsPerHour[hour] = point.Requests
		body.ErrorsPerHour[hour] = point.Errors
		body.P95MsPerHour[hour] = point.P95Ms
	}
	return "", nil
}

// inParallel runs `count` numbered reads, at most `concurrency` at a time, and
// answers the first failure. It exists so that the hourly reads above are one
// loop rather than a goroutine pattern written out in the middle of a handler.
func inParallel(count, concurrency int, run func(int) error) error {
	if concurrency < 1 {
		concurrency = 1
	}
	var wait sync.WaitGroup
	var once sync.Once
	var failure error
	slots := make(chan struct{}, concurrency)

	for i := range count {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			if err := run(i); err != nil {
				once.Do(func() { failure = err })
			}
		}(i)
	}
	wait.Wait()
	return failure
}

// joinProjectTraffic puts the request pipeline's per-project numbers onto the
// projects that exist. Every project gets a row — a project nobody visited
// still belongs in the table, at zero — and traffic for a project the platform
// no longer has falls away.
//
// This reads the request rows rather than the flows the rest of the overview
// still comes from, and that is a correction rather than a refactor. Flows are
// attributed by the *destination* endpoint, so a protected preview's traffic
// was credited to the forward-auth gate and an idling environment's to the KEDA
// interceptor — both of which live in the platform's own namespace, so both
// simply vanished from the project that served them. A request row is
// attributed by the Host header, which is the one thing every hop preserves and
// the only thing the interceptor routes on.
func (s *Server) joinProjectTraffic(ctx context.Context, traffic []clickhouse.ProjectTraffic) ([]projectTraffic, error) {
	list := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}

	byProject := make(map[string]clickhouse.ProjectTraffic, len(traffic))
	for _, entry := range traffic {
		byProject[entry.Project] = entry
	}

	projects := make([]projectTraffic, 0, len(list.Items))
	for _, project := range list.Items {
		entry := byProject[project.Name]
		row := projectTraffic{
			Project:         project.Name,
			Requests24h:     entry.Requests,
			Errors5xx24h:    entry.Errors,
			P95Ms:           entry.P95Ms,
			RequestsPerHour: entry.RequestsPerHour,
		}
		if row.RequestsPerHour == nil {
			row.RequestsPerHour = make([]uint64, overviewHours)
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
		s.writeStoreError(w, err, "the traffic query")
		return
	}
	writeList(w, edges)
}
