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
	"net/http"
	"strings"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// An Environment's workload endpoint answers what is running right now, off
// the API server. This answers what has been running: the same environment's
// CPU, memory, replica count and restarts as a series out of the telemetry
// store.
//
// They are deliberately two endpoints rather than one. The instant is always
// available and always exact; the history exists only where the platform has a
// telemetry store and has been sampling, and a single endpoint would have to
// answer half of itself with a 503.

// environmentMetrics serves one environment's resource history.
func (s *Server) environmentMetrics(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}

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
	points, err := intParam(req, "points", clickhouse.DefaultResourceBuckets)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	// A window is what this endpoint is for, so it has one whether or not the
	// caller supplied it. An hour is what the environment page opens with.
	if since.IsZero() {
		since = time.Now().Add(-time.Hour)
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	series, err := store.ResourceSeries(ctx, clickhouse.ResourceSeriesQuery{
		Project:     env.Spec.ProjectRef.Name,
		Environment: env.Name,
		Since:       since,
		Until:       until,
		Buckets:     points,
	})
	if err != nil {
		s.writeStoreError(w, err, "the resource history query")
		return
	}
	writeJSON(w, http.StatusOK, series)
}

// listTraces serves the trace list for a window: one entry per trace, newest
// first. `project`, `environment` and `service` narrow it; `errors=1` keeps
// the traces something failed in and `minDuration` the slow ones, which are
// the two reasons anyone opens this.
func (s *Server) listTraces(w http.ResponseWriter, req *http.Request) {
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
	limit, err := intParam(req, "limit", clickhouse.DefaultTraceLimit)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	minDuration, err := floatParam(req, "minDuration")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	query := clickhouse.TraceQuery{
		Service:       strings.TrimSpace(req.URL.Query().Get("service")),
		Environment:   strings.TrimSpace(req.URL.Query().Get("environment")),
		Since:         since,
		Until:         until,
		OnlyErrors:    req.URL.Query().Get("errors") == "1",
		MinDurationMs: minDuration,
		Limit:         limit,
	}
	if project := strings.TrimSpace(req.URL.Query().Get("project")); project != "" {
		if !s.visibleProject(w, req, project) {
			return
		}
		// The project has to exist before its traces are worth looking for, the
		// same way the metrics overview insists on it: a typo should say "no
		// such project" rather than "no traces".
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
	traces, err := store.Traces(ctx, query)
	if err != nil {
		s.writeStoreError(w, err, "the trace query")
		return
	}
	// A trace carries the project it ran in, which the platform puts into
	// every application's resource attributes. One that names no project came
	// from the platform's own components, and is the operator's.
	writeList(w, visibleTo(scopeFrom(ctx), traces,
		func(trace *clickhouse.Trace) string { return trace.Project }))
}

// getTrace serves one trace's spans, oldest first.
//
// It takes no window. A trace id arrives from a log line or from the list, and
// requiring the caller to also know when it happened would break the one link
// that makes traces worth collecting.
func (s *Server) getTrace(w http.ResponseWriter, req *http.Request) {
	id := strings.TrimSpace(req.PathValue("traceId"))
	if id == "" {
		badRequest(w, "a trace id is required")
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	spans, err := store.Trace(req.Context(), id)
	if err != nil {
		s.writeStoreError(w, err, "the trace query")
		return
	}
	// A trace with nothing of the caller's in it is a trace they do not have:
	// the same answer as one the store never kept, rather than a refusal that
	// would confirm the id names something.
	spans = visibleTo(scopeFrom(req.Context()), spans,
		func(span *clickhouse.Span) string { return span.Project })
	if len(spans) == 0 {
		// A trace nothing was kept for is a 404 rather than an empty list:
		// the id was a name, and the platform does not have it. Retention is
		// the usual reason, so the answer says so.
		writeJSON(w, http.StatusNotFound, errorBody{
			Error: "no spans for trace " + id + "; it may have fallen outside the store's retention",
		})
		return
	}
	writeJSON(w, http.StatusOK, traceView{TraceID: id, Spans: spans})
}

// traceView is one whole trace. The spans carry the waterfall; the summary is
// what a header says about it, computed here rather than in a second query.
type traceView struct {
	TraceID string `json:"traceId"`
	// Spans are in start order, which is the order a waterfall is drawn in.
	Spans []clickhouse.Span `json:"spans"`
}
