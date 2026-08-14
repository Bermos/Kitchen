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
	"errors"
	"net/http"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// Logs are read out of the telemetry store rather than off the pods that
// wrote them: the collector has been shipping every container's output into
// ClickHouse since the log pipeline landed, so a build's output survives the
// build pod that produced it, and a preview's logs outlive the preview.

// logQueryFrom reads the window, the limit and the search term shared by the
// log endpoints.
func logQueryFrom(req *http.Request) (clickhouse.LogQuery, error) {
	limit, err := intParam(req, "limit", clickhouse.DefaultLogLimit)
	if err != nil {
		return clickhouse.LogQuery{}, err
	}
	since, err := timeParam(req, "since")
	if err != nil {
		return clickhouse.LogQuery{}, err
	}
	until, err := timeParam(req, "until")
	if err != nil {
		return clickhouse.LogQuery{}, err
	}
	return clickhouse.LogQuery{
		Container: strings.TrimSpace(req.URL.Query().Get("container")),
		Search:    strings.TrimSpace(req.URL.Query().Get("search")),
		Since:     since,
		Until:     until,
		Limit:     limit,
	}, nil
}

func (s *Server) buildLogs(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	// The Build has to exist before its logs are worth looking for: a typo
	// should say "no such build", not "no lines".
	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, req.PathValue("name"), build); err != nil {
		s.writeError(w, err)
		return
	}

	query, err := logQueryFrom(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	query.Source = clickhouse.SourceBuild
	query.Build = build.Name
	query.Project = build.Spec.ProjectRef.Name

	s.writeLogs(w, req, query)
}

func (s *Server) environmentLogs(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}

	query, err := logQueryFrom(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	query.Source = clickhouse.SourceRuntime
	query.Environment = env.Name
	query.Project = env.Spec.ProjectRef.Name

	s.writeLogs(w, req, query)
}

func (s *Server) writeLogs(w http.ResponseWriter, req *http.Request, query clickhouse.LogQuery) {
	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	lines, err := store.SearchLogs(req.Context(), query)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeList(w, lines)
}

// queryLogs is the observability surface: a caller-written ClickHouse
// expression over the whole logs table. The platform stores logs in ClickHouse
// and does not hide it — `where` is real ClickHouse syntax, run read-only.
func (s *Server) queryLogs(w http.ResponseWriter, req *http.Request) {
	where := strings.TrimSpace(req.URL.Query().Get("where"))
	if where == "" {
		badRequest(w, "where is required: a ClickHouse expression over the logs table, e.g. "+
			"where=project = 'shop' AND stream = 'stderr'. `1 = 1` selects everything.")
		return
	}
	limit, err := intParam(req, "limit", clickhouse.DefaultLogLimit)
	if err != nil {
		badRequest(w, "%s", err.Error())
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

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	lines, err := store.FilterLogs(req.Context(), clickhouse.LogFilter{
		Where: where,
		Since: since,
		Until: until,
		Limit: limit,
	})
	if err != nil {
		// ClickHouse judging the expression is the caller's problem to fix,
		// and its message is the diagnostic they need to fix it.
		queryErr := &clickhouse.QueryError{}
		if errors.As(err, &queryErr) {
			badRequest(w, "%s", queryErr.Message)
			return
		}
		s.writeError(w, err)
		return
	}
	writeList(w, lines)
}

// openLogStore resolves the telemetry store, answering the request itself when
// there is none: a nil return means the response has been written.
func (s *Server) openLogStore(w http.ResponseWriter, req *http.Request) logReader {
	store, err := s.logStore(req.Context())
	if err != nil {
		if errors.Is(err, errNoLogStore) {
			// The installation chose to run without telemetry. That is a
			// missing capability, not a broken request.
			writeJSON(w, http.StatusServiceUnavailable, errorBody{Error: err.Error()})
			return nil
		}
		s.writeError(w, err)
		return nil
	}
	return store
}
