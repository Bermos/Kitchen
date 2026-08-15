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

// Package api serves the operator's REST API: the surface the Kitchen UI, the
// CLI and CI talk to. It reads and writes the same custom resources the
// controllers reconcile, so the API is a view onto the platform's state rather
// than a second copy of it.
//
// Every endpoint sits behind the platform's identity provider. There is no
// unauthenticated mode and no local-admin escape hatch: a request without a
// token the issuer signed gets a 401, including when the installation has no
// identity provider at all. The git webhook receiver is the one HTTP surface
// that stays outside this — it authenticates the provider's signature over the
// payload, which is a different question from who a caller is.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/chartrepo"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// maxRequestBody bounds the request bodies the API accepts. Everything it
// takes is a handful of fields; a megabyte is already generous.
const maxRequestBody = 1 << 20

// Server is the operator's REST API. It runs as a manager Runnable on every
// replica: reads are served from the manager's cache and writes go straight to
// the API server, so no replica has to be the leader to answer.
type Server struct {
	Client client.Client
	// APIReader reads straight from the API server, bypassing the manager's
	// cache. The introspection endpoints ask about pods and nodes, and
	// answering those from the cache would mean an informer over every pod in
	// the cluster kept warm for a question only an open dashboard asks. Nil
	// falls back to Client, which is what the tests do.
	APIReader client.Reader
	// Namespace is where the Kitchen custom resources live.
	Namespace string
	// BindAddr for the HTTP server, e.g. ":8082".
	BindAddr string
	// ExtraAudiences are token audiences accepted on top of the issuer and
	// the API's own external URL.
	ExtraAudiences []string
	// UI, when set, answers everything outside /api/: the dashboard's
	// static files, which are public — every request with state stays
	// behind the token check.
	UI http.Handler

	// Activity records the writes this API carries out — a project created,
	// a release promoted — into the platform's activity feed. May be nil.
	Activity *activity.Recorder

	// Version is the release this binary was built from, reported by the
	// updates endpoints as what the platform is currently running.
	Version string

	// SelfUpdate is the chart's self-update configuration, as the operator
	// received it. The API only reads it: whether the feature is on, and
	// whether upgrades across a minor version are allowed, so the dashboard
	// offers what the reconciler will actually accept.
	SelfUpdate controller.SelfUpdateConfig

	// charts lists the versions the platform's chart has been published at.
	// Nil when self-update is off or the chart reference cannot be read as an
	// OCI one, in which case the updates endpoint reports the running version
	// and no candidates. It is an interface for the same reason logStore is:
	// so the endpoint can be tested without a registry to talk to.
	charts chartVersions

	auth *authenticator

	// logStore builds the client the telemetry endpoints read through. It is
	// a field so tests can serve telemetry without a ClickHouse.
	logStore func(ctx context.Context) (logReader, error)
}

// chartVersions lists the versions the platform's Helm chart has been
// published at, newest last.
type chartVersions interface {
	Versions(ctx context.Context) ([]semver.Version, error)
}

// logReader is the slice of the telemetry store the API depends on: logs, the
// activity feed, traffic edges and the pre-aggregated metrics all live in the
// same ClickHouse.
type logReader interface {
	SearchLogs(ctx context.Context, query clickhouse.LogQuery) ([]clickhouse.LogLine, error)
	FilterLogs(ctx context.Context, filter clickhouse.LogFilter) ([]clickhouse.LogLine, error)
	QueryEvents(ctx context.Context, query clickhouse.EventQuery) ([]clickhouse.Event, error)
	TrafficEdges(ctx context.Context, query clickhouse.TrafficQuery) ([]clickhouse.TrafficEdge, error)
	MetricsOverview(ctx context.Context, query clickhouse.MetricsQuery) (clickhouse.MetricsOverview, error)
}

// Start implements manager.Runnable.
func (s *Server) Start(ctx context.Context) error {
	server := &http.Server{
		Addr:              s.BindAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	s.log().Info("starting api server", "addr", s.BindAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// NeedLeaderElection implements manager.LeaderElectionRunnable: the API is
// served by every replica.
func (s *Server) NeedLeaderElection() bool { return false }

func (s *Server) log() logr.Logger { return logf.Log.WithName("api") }

// reader is where the uncached reads go; see APIReader.
func (s *Server) reader() client.Reader {
	if s.APIReader != nil {
		return s.APIReader
	}
	return s.Client
}

// Handler builds the routed, authenticated handler. It is exported so the
// routing table can be exercised without binding a port.
func (s *Server) Handler() http.Handler {
	if s.auth == nil {
		s.auth = &authenticator{extraAudiences: s.ExtraAudiences}
	}
	if s.logStore == nil {
		s.logStore = s.telemetryStore
	}
	if s.charts == nil && s.SelfUpdate.Chart != "" {
		charts, err := chartrepo.New(s.SelfUpdate.Chart)
		if err != nil {
			// Not fatal, and not silent: the platform serves its dashboard
			// either way, and the settings page says the versions could not
			// be listed rather than showing none and implying there are none.
			s.log().Error(err, "cannot list the chart's published versions", "chart", s.SelfUpdate.Chart)
		}
		s.charts = charts
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/v1/projects", s.listProjects)
	mux.HandleFunc("POST /api/v1/projects", s.createProject)
	mux.HandleFunc("GET /api/v1/projects/{name}", s.getProject)
	mux.HandleFunc("GET /api/v1/projects/{name}/builds", s.listProjectBuilds)
	mux.HandleFunc("POST /api/v1/projects/{name}/builds", s.createBuild)
	mux.HandleFunc("GET /api/v1/projects/{name}/releases", s.listProjectReleases)
	mux.HandleFunc("GET /api/v1/projects/{name}/environments", s.listProjectEnvironments)

	mux.HandleFunc("GET /api/v1/builds", s.listBuilds)
	mux.HandleFunc("GET /api/v1/builds/{name}", s.getBuild)
	mux.HandleFunc("GET /api/v1/builds/{name}/logs", s.buildLogs)

	mux.HandleFunc("GET /api/v1/releases", s.listReleases)
	mux.HandleFunc("GET /api/v1/releases/{name}", s.getRelease)

	mux.HandleFunc("GET /api/v1/environments", s.listEnvironments)
	mux.HandleFunc("GET /api/v1/environments/{name}", s.getEnvironment)
	mux.HandleFunc("PATCH /api/v1/environments/{name}", s.patchEnvironment)
	mux.HandleFunc("GET /api/v1/environments/{name}/logs", s.environmentLogs)
	mux.HandleFunc("GET /api/v1/environments/{name}/workload", s.environmentWorkload)
	mux.HandleFunc("GET /api/v1/environments/{name}/objects", s.environmentObjects)

	mux.HandleFunc("GET /api/v1/logs", s.queryLogs)

	mux.HandleFunc("GET /api/v1/events", s.listEvents)
	mux.HandleFunc("GET /api/v1/metrics/overview", s.metricsOverview)
	mux.HandleFunc("GET /api/v1/traffic", s.traffic)

	mux.HandleFunc("GET /api/v1/status", s.getStatus)

	mux.HandleFunc("GET /api/v1/settings", s.getSettings)
	mux.HandleFunc("PATCH /api/v1/settings", s.patchSettings)

	mux.HandleFunc("GET /api/v1/updates", s.listUpdates)
	mux.HandleFunc("POST /api/v1/updates", s.createUpdate)
	mux.HandleFunc("GET /api/v1/updates/{name}", s.getUpdate)

	mux.HandleFunc("GET /api/v1/connections", s.listConnections)
	mux.HandleFunc("GET /api/v1/connections/{name}", s.getConnection)

	mux.HandleFunc("GET /api/v1/domains", s.listDomains)
	mux.HandleFunc("GET /api/v1/domains/{name}", s.getDomain)

	mux.HandleFunc("GET /api/v1/claims", s.listClaims)
	mux.HandleFunc("GET /api/v1/claims/{name}", s.getClaim)

	// Anything else under the API prefix is a 404 rather than a fall-through,
	// and it is still a 404 only after the caller has been identified: an
	// anonymous request should not be able to map the API's shape.
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, http.StatusNotFound, errorBody{
			Error: fmt.Sprintf("no such endpoint: %s %s", req.Method, req.URL.Path),
		})
	})

	authenticated := s.authenticated(mux)
	if s.UI == nil {
		return authenticated
	}

	// The dashboard rides on the same server: /api/ keeps its token check
	// (including the authenticated 404 above), everything else serves the
	// SPA and its assets, which are public and stateless.
	root := http.NewServeMux()
	root.Handle("/api/", authenticated)
	root.Handle("/", s.UI)
	return root
}

// telemetryStore resolves the connection to the telemetry store the way the
// Kitchen reconciler does: off the singleton's secret reference, which the
// chart writes whether it runs ClickHouse itself or points at an external one.
func (s *Server) telemetryStore(ctx context.Context) (logReader, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return nil, err
	}
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return nil, errNoLogStore
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: controller.PlatformNamespace, Name: ref.Name}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return nil, err
	}
	return clickhouse.New(cfg), nil
}

// errNoLogStore is what the log endpoints report on an installation that was
// deliberately brought up without a telemetry store.
var errNoLogStore = errors.New("this installation has no telemetry store, so there are no logs to read")

// errorBody is the shape of every error the API returns.
type errorBody struct {
	Error string `json:"error"`
}

// listBody wraps collections, so that a list can grow fields (paging, totals)
// without turning a JSON array into an object under the client's feet.
type listBody[T any] struct {
	Items []T `json:"items"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeList[T any](w http.ResponseWriter, items []T) {
	if items == nil {
		items = []T{}
	}
	writeJSON(w, http.StatusOK, listBody[T]{Items: items})
}

// writeError maps the errors handlers produce onto status codes. Everything
// the API does is one Kubernetes call away, so the API server's own opinion of
// an error is usually the right one to pass on.
func (s *Server) writeError(w http.ResponseWriter, err error) {
	switch {
	case apierrors.IsNotFound(err):
		writeJSON(w, http.StatusNotFound, errorBody{Error: err.Error()})
	case apierrors.IsAlreadyExists(err), apierrors.IsConflict(err):
		writeJSON(w, http.StatusConflict, errorBody{Error: err.Error()})
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
	case apierrors.IsForbidden(err):
		writeJSON(w, http.StatusForbidden, errorBody{Error: err.Error()})
	default:
		s.log().Error(err, "request failed")
		writeJSON(w, http.StatusInternalServerError, errorBody{Error: err.Error()})
	}
}

// writeStoreError answers a telemetry read that the store refused or could not
// complete. The query was the operator's own, not the caller's, so ClickHouse's
// diagnostic — a nested-aggregate complaint, a version string, the offending
// SQL — is a fault report for whoever maintains Kitchen and not something the
// dashboard's user can act on. They get the name of the read that failed; the
// store's text stays in the operator's log.
//
// what names the read in the dashboard's words: "the traffic query".
func (s *Server) writeStoreError(w http.ResponseWriter, err error, what string) {
	s.log().Error(err, what+" failed")
	writeJSON(w, http.StatusInternalServerError, errorBody{
		Error: what + " failed; the operator's log has the store's diagnostic",
	})
}

func badRequest(w http.ResponseWriter, format string, args ...any) {
	writeJSON(w, http.StatusBadRequest, errorBody{Error: fmt.Sprintf(format, args...)})
}

// decodeBody reads a JSON request body, refusing fields the endpoint does not
// know: a typo in a field name should be an error, not a silently ignored
// instruction.
func decodeBody(req *http.Request, into any) error {
	decoder := json.NewDecoder(io.LimitReader(req.Body, maxRequestBody))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("a JSON body is required")
		}
		return fmt.Errorf("unreadable JSON body: %w", err)
	}
	return nil
}

// intParam reads a bounded integer query parameter.
func intParam(req *http.Request, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(req.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 {
		return 0, fmt.Errorf("%s must be a positive integer (got %q)", name, raw)
	}
	return value, nil
}

// timeParam reads an RFC 3339 timestamp query parameter.
func timeParam(req *http.Request, name string) (time.Time, error) {
	raw := strings.TrimSpace(req.URL.Query().Get(name))
	if raw == "" {
		return time.Time{}, nil
	}
	value, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC 3339 timestamp (got %q)", name, raw)
	}
	return value, nil
}
