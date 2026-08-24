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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

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
		// `process` and `run` narrow an environment's logs to one of its
		// workers or scheduled jobs, and to one firing of a schedule. They are
		// read here rather than on the environment handler alone because they
		// cost nothing on a build's logs, which never carry either.
		Process:   strings.TrimSpace(req.URL.Query().Get("process")),
		Run:       strings.TrimSpace(req.URL.Query().Get("run")),
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

	if wantsEventStream(req) {
		s.streamLogs(w, req, func(ctx context.Context, since time.Time) ([]clickhouse.LogLine, error) {
			follow := query
			if !since.IsZero() {
				follow.Since = since
				follow.Limit = clickhouse.MaxLogLimit
			}
			return store.SearchLogs(ctx, follow)
		})
		return
	}

	lines, err := store.SearchLogs(req.Context(), query)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeList(w, lines)
}

// logSelectionFrom reads what every observability endpoint is asked over: the
// query, the escape hatch, and the window.
//
// Both query surfaces are optional. An empty selection asks for everything in
// the window, which is a legitimate question and is spelled by asking nothing —
// there is no sentinel expression to type.
func logSelectionFrom(req *http.Request) (clickhouse.LogSelection, error) {
	since, err := timeParam(req, "since")
	if err != nil {
		return clickhouse.LogSelection{}, err
	}
	until, err := timeParam(req, "until")
	if err != nil {
		return clickhouse.LogSelection{}, err
	}
	return clickhouse.LogSelection{
		Query: strings.TrimSpace(req.URL.Query().Get("q")),
		Where: strings.TrimSpace(req.URL.Query().Get("where")),
		Since: since,
		Until: until,
	}, nil
}

// scopedSelection narrows a cross-project selection to the projects the caller
// can see. An operator's is returned untouched.
//
// The narrowing goes into the query rather than over the answer, because these
// reads are bounded: filtering afterwards would spend the caller's limit on
// lines they are not shown, and a page could come back empty while the store
// held plenty of theirs. `project` is a real column on the log table — the
// query language's `project:` compiles to the same one — and the names are
// Kubernetes object names, so the only thing composed into the statement is a
// list of DNS labels, quoted anyway.
//
// A line belonging to no project at all is the platform's own (the cluster
// source), and stays out: the condition names projects, and an empty `project`
// is not one of them.
func scopedSelection(scope projectScope, selection clickhouse.LogSelection) clickhouse.LogSelection {
	if scope.all {
		return selection
	}
	names := scope.names()
	quoted := make([]string, 0, len(names))
	for _, name := range names {
		quoted = append(quoted, "'"+strings.ReplaceAll(name, "'", "''")+"'")
	}
	mine := "project IN (" + strings.Join(quoted, ", ") + ")"
	if selection.Where == "" {
		selection.Where = mine
		return selection
	}
	selection.Where = "(" + selection.Where + ") AND " + mine
	return selection
}

// writeQueryError answers the two ways a caller's own query can be wrong —
// Kitchen's parser refusing it, and ClickHouse refusing it — with the
// diagnostic that says how to fix it. Anything else is the platform's fault
// and is reported as one.
func (s *Server) writeQueryError(w http.ResponseWriter, err error) {
	syntaxErr := &clickhouse.LogQueryError{}
	if errors.As(err, &syntaxErr) {
		badRequest(w, "%s", syntaxErr.Message)
		return
	}
	queryErr := &clickhouse.QueryError{}
	if errors.As(err, &queryErr) {
		badRequest(w, "%s", queryErr.Message)
		return
	}
	s.writeError(w, err)
}

// queryLogs is the observability surface's lines. `q` is Kitchen's log query
// language and the front door; `where` is a real ClickHouse expression, kept
// because it is genuinely more powerful. Given both, they compose with AND.
func (s *Server) queryLogs(w http.ResponseWriter, req *http.Request) {
	selection, err := logSelectionFrom(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	limit, err := intParam(req, "limit", clickhouse.DefaultLogLimit)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	scope := scopeFrom(req.Context())
	if scope.empty() {
		writeList(w, []clickhouse.LogLine{})
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	filter := clickhouse.LogFilter{LogSelection: scopedSelection(scope, selection), Limit: limit}
	if wantsEventStream(req) {
		s.streamLogs(w, req, func(ctx context.Context, followSince time.Time) ([]clickhouse.LogLine, error) {
			follow := filter
			if !followSince.IsZero() {
				follow.Since = followSince
				follow.Limit = clickhouse.MaxLogLimit
			}
			return store.FilterLogs(ctx, follow)
		})
		return
	}

	lines, err := store.FilterLogs(req.Context(), filter)
	if err != nil {
		s.writeQueryError(w, err)
		return
	}
	writeList(w, lines)
}

// logHistogram is when the selection's lines happened: counts per bucket over
// the window, so a spike is seen rather than scrolled to, and so the chart can
// be dragged to narrow the window it is drawn over.
func (s *Server) logHistogram(w http.ResponseWriter, req *http.Request) {
	selection, err := logSelectionFrom(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	buckets, err := intParam(req, "buckets", clickhouse.DefaultHistogramBuckets)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	scope := scopeFrom(req.Context())
	if scope.empty() {
		writeJSON(w, http.StatusOK, clickhouse.LogHistogram{})
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	histogram, err := store.LogHistogram(req.Context(),
		clickhouse.LogHistogramQuery{LogSelection: scopedSelection(scope, selection), Buckets: buckets})
	if err != nil {
		s.writeQueryError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, histogram)
}

// logFacets is what else is in the selection: each field's distinct values with
// their counts, over the whole window rather than over the page of lines that
// came back. It is what lets someone narrow a search without knowing which
// columns the table has.
func (s *Server) logFacets(w http.ResponseWriter, req *http.Request) {
	selection, err := logSelectionFrom(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	limit, err := intParam(req, "limit", clickhouse.MaxFacetValues)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	var fields []string
	for _, field := range strings.Split(req.URL.Query().Get("fields"), ",") {
		if field = strings.TrimSpace(field); field != "" {
			fields = append(fields, field)
		}
	}

	scope := scopeFrom(req.Context())
	if scope.empty() {
		writeList(w, []clickhouse.LogFacet{})
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	facets, err := store.LogFacets(req.Context(),
		clickhouse.LogFacetQuery{LogSelection: scopedSelection(scope, selection), Fields: fields, Limit: limit})
	if err != nil {
		s.writeQueryError(w, err)
		return
	}
	writeList(w, facets)
}

// logPatterns is what the selection's lines are actually saying: the messages
// collapsed into templates, commonest first, so a spike of 14,021 lines reads
// as the handful of shapes it is.
func (s *Server) logPatterns(w http.ResponseWriter, req *http.Request) {
	selection, err := logSelectionFrom(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	limit, err := intParam(req, "limit", clickhouse.DefaultPatternRows)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	scan, err := intParam(req, "scan", clickhouse.DefaultPatternScan)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	scope := scopeFrom(req.Context())
	if scope.empty() {
		writeList(w, []clickhouse.LogPattern{})
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	patterns, err := store.LogPatterns(req.Context(),
		clickhouse.LogPatternQuery{LogSelection: scopedSelection(scope, selection), Limit: limit, Scan: scan})
	if err != nil {
		s.writeQueryError(w, err)
		return
	}
	writeList(w, patterns)
}

// wantsEventStream reports whether the caller asked to follow the logs live:
// the same endpoint, negotiated by Accept. A plain GET answers a bounded page
// exactly as before; `Accept: text/event-stream` keeps the response open.
func wantsEventStream(req *http.Request) bool {
	return strings.Contains(req.Header.Get("Accept"), "text/event-stream")
}

// How the follow loop paces itself: the store is polled at pollInterval —
// live enough for a human watching a build or a request tail, far apart enough
// that a hundred open tails stay cheap — and a comment line goes out at
// heartbeatInterval so proxies do not reap the idle connection.
const (
	logPollInterval   = 2 * time.Second
	heartbeatInterval = 15 * time.Second
)

// streamLogs answers with Server-Sent Events: the query's current answer
// first, then whatever arrives after it, one `data:` event per line, until
// the client goes away.
//
// A log line is identified within one instant by where it was written and what
// it said, which is what keeps a re-read boundary from sending it twice.
func (s *Server) streamLogs(
	w http.ResponseWriter,
	req *http.Request,
	fetch func(ctx context.Context, since time.Time) ([]clickhouse.LogLine, error),
) {
	streamRows(s, w, req, fetch,
		func(line clickhouse.LogLine) time.Time { return line.Timestamp },
		func(line clickhouse.LogLine) string {
			return line.Pod + "\x00" + line.Container + "\x00" + line.Stream + "\x00" + line.Message
		})
}

// streamRows is the follow loop both live tails are built from — log lines and
// the edge's requests, which differ only in what a row is.
//
// The follow reads are the same bounded queries the plain endpoint runs, with
// the window's start advanced to the newest row already sent. ClickHouse
// timestamps are sub-second and a busy pod can write twice inside one tick, so
// the boundary is re-read inclusively and the rows already sent at that exact
// timestamp are dropped by key. Rows must arrive in time order, oldest first:
// the boundary only ever moves forwards.
//
// It is a function rather than a method because Go has no generic methods, and
// the alternative — a second copy of this loop for the second row type — is how
// two tails end up disagreeing about deduplication.
func streamRows[T any](
	s *Server,
	w http.ResponseWriter,
	req *http.Request,
	fetch func(ctx context.Context, since time.Time) ([]T, error),
	at func(T) time.Time,
	key func(T) string,
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "streaming is not supported on this connection"})
		return
	}

	ctx := req.Context()
	var boundary time.Time
	atBoundary := map[string]struct{}{}

	send := func(rows []T) error {
		for _, row := range rows {
			timestamp := at(row)
			if timestamp.Before(boundary) {
				continue
			}
			rowKey := key(row)
			if timestamp.Equal(boundary) {
				if _, sent := atBoundary[rowKey]; sent {
					continue
				}
			} else {
				boundary = timestamp
				atBoundary = map[string]struct{}{}
			}
			atBoundary[rowKey] = struct{}{}

			payload, err := json.Marshal(row)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
				return err
			}
		}
		flusher.Flush()
		return nil
	}

	initial, err := fetch(ctx, time.Time{})
	if err != nil {
		s.writeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// Tell intermediaries not to buffer: an event that sits in a proxy is a
	// tail that is not live.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := send(initial); err != nil {
		return
	}

	poll := time.NewTicker(logPollInterval)
	defer poll.Stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case <-poll.C:
			rows, err := fetch(ctx, boundary)
			if err != nil {
				// The response is already streaming, so an error cannot
				// become a status code any more; it becomes an event the
				// client can show, and the stream ends so the client's
				// reconnect (or fallback to polling) takes over.
				payload, marshalErr := json.Marshal(errorBody{Error: err.Error()})
				if marshalErr == nil {
					_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", payload)
					flusher.Flush()
				}
				return
			}
			if err := send(rows); err != nil {
				return
			}
		}
	}
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
