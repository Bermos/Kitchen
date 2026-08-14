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

	filter := clickhouse.LogFilter{Where: where, Since: since, Until: until, Limit: limit}
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

// wantsEventStream reports whether the caller asked to follow the logs live:
// the same endpoint, negotiated by Accept. A plain GET answers a bounded page
// exactly as before; `Accept: text/event-stream` keeps the response open.
func wantsEventStream(req *http.Request) bool {
	return strings.Contains(req.Header.Get("Accept"), "text/event-stream")
}

// How the follow loop paces itself: the store is polled at pollInterval —
// live enough for a human watching a build, far apart enough that a hundred
// open tails stay cheap — and a comment line goes out at heartbeatInterval so
// proxies do not reap the idle connection.
const (
	logPollInterval   = 2 * time.Second
	heartbeatInterval = 15 * time.Second
)

// streamLogs answers with Server-Sent Events: the query's current answer
// first, then whatever arrives after it, one `data:` event per line, until
// the client goes away.
//
// The follow reads are the same bounded queries the plain endpoint runs, with
// the window's start advanced to the newest line already sent. ClickHouse
// timestamps are milliseconds and a busy pod can write twice in one, so the
// boundary is re-read inclusively and the lines already sent at that exact
// timestamp are dropped by key.
func (s *Server) streamLogs(
	w http.ResponseWriter,
	req *http.Request,
	fetch func(ctx context.Context, since time.Time) ([]clickhouse.LogLine, error),
) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: "streaming is not supported on this connection"})
		return
	}

	ctx := req.Context()
	var boundary time.Time
	atBoundary := map[string]struct{}{}

	lineKey := func(line clickhouse.LogLine) string {
		return line.Pod + "\x00" + line.Container + "\x00" + line.Stream + "\x00" + line.Message
	}
	send := func(lines []clickhouse.LogLine) error {
		for _, line := range lines {
			if line.Timestamp.Before(boundary) {
				continue
			}
			key := lineKey(line)
			if line.Timestamp.Equal(boundary) {
				if _, sent := atBoundary[key]; sent {
					continue
				}
			} else {
				boundary = line.Timestamp
				atBoundary = map[string]struct{}{}
			}
			atBoundary[key] = struct{}{}

			payload, err := json.Marshal(line)
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
			lines, err := fetch(ctx, boundary)
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
			if err := send(lines); err != nil {
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
