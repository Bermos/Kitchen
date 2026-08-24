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
	"sort"
	"strings"
	"time"

	"github.com/blang/semver/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/chartrepo"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The updates endpoints are the platform's own upgrades: what it is running,
// what has been published since, and the record of every attempt.
//
// Creating one is the only write in the API that changes the platform itself
// rather than something running on it, and it carries exactly one field. The
// job it eventually starts holds cluster-admin, so anything this endpoint
// accepted beyond a version number would be a way to reach that grant; the
// operator builds the helm invocation, and this only names the release to
// build it for.

// requestedByAnnotation records which authenticated caller asked for
// something the API created, so the object itself answers "who did this".
const requestedByAnnotation = "kitchen.bermos.dev/requested-by"

type updateView struct {
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Phase       string          `json:"phase,omitempty"`
	FromVersion string          `json:"fromVersion,omitempty"`
	Message     string          `json:"message,omitempty"`
	RequestedBy string          `json:"requestedBy,omitempty"`
	StartedAt   *metav1.Time    `json:"startedAt,omitempty"`
	CompletedAt *metav1.Time    `json:"completedAt,omitempty"`
	Conditions  []conditionView `json:"conditions,omitempty"`
}

// updatesView is the settings page's whole answer: whether this installation
// can update itself, what it would update to, and what it has done before.
type updatesView struct {
	// Enabled is whether the chart was installed with self-update on. When it
	// is false the rest is still worth reporting — the version it is running
	// is the first thing anyone asks — and Reason says how to turn it on.
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`

	CurrentVersion string `json:"currentVersion"`

	// LatestVersion is the newest published release, and Available whether it
	// is newer than the running one.
	LatestVersion string `json:"latestVersion,omitempty"`
	Available     bool   `json:"available"`

	// UpgradableTo is every published version this installation would accept
	// right now, newest first — so the dashboard offers what will work rather
	// than what will be refused.
	UpgradableTo []string `json:"upgradableTo,omitempty"`

	// AllowMinor says whether upgrades across a minor version are permitted.
	// Pre-1.0 that is where breaking changes land, so a dashboard that hides
	// the distinction would be hiding the only one that matters.
	AllowMinor bool `json:"allowMinor"`

	// CheckedAt is when the published versions were last read from the
	// registry. The listing is cached for an hour, so an answer with nothing
	// new in it means little without saying how old it is — and a refresh
	// that changes nothing else at least moves this.
	CheckedAt *metav1.Time `json:"checkedAt,omitempty"`

	// DiscoveryError explains an empty version list: no route to the
	// registry, most often. The endpoint still answers, because an
	// installation that cannot reach the registry can still be told which
	// version it is on and still accept a version typed in by hand.
	DiscoveryError string `json:"discoveryError,omitempty"`

	Items []updateView `json:"items"`
}

func newUpdateView(update *kitchenv1alpha1.PlatformUpdate) updateView {
	return updateView{
		Name:        update.Name,
		Version:     update.Spec.Version,
		Phase:       string(update.Status.Phase),
		FromVersion: update.Status.FromVersion,
		Message:     update.Status.Message,
		RequestedBy: update.Annotations[requestedByAnnotation],
		StartedAt:   update.Status.StartedAt,
		CompletedAt: update.Status.CompletedAt,
		Conditions:  conditionViews(update.Status.Conditions),
	}
}

func (s *Server) listUpdates(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	// `?refresh=true` is the dashboard's re-check control. It reaches the
	// registry rather than the cached listing, which is why it is a parameter
	// on the read and not a second endpoint: the answer is the same answer,
	// taken again.
	refresh, err := boolParam(req, "refresh")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	updates := &kitchenv1alpha1.PlatformUpdateList{}
	if err := s.Client.List(ctx, updates); err != nil {
		s.writeError(w, err)
		return
	}
	// Newest first: the last upgrade is the one being watched, and the rest
	// is history underneath it.
	sort.Slice(updates.Items, func(i, j int) bool {
		return updates.Items[j].CreationTimestamp.Before(&updates.Items[i].CreationTimestamp)
	})

	view := updatesView{
		Enabled:        s.SelfUpdate.Enabled(),
		CurrentVersion: s.Version,
		AllowMinor:     s.SelfUpdate.AllowMinor,
		Items:          make([]updateView, 0, len(updates.Items)),
	}
	if !view.Enabled {
		view.Reason = "this installation cannot update itself: upgrade the chart with " +
			"`--set selfUpdate.enabled=true`, which creates the update job's ServiceAccount. It is bound to " +
			"cluster-admin, because the upgrade applies the whole chart, so it is off by default."
	}
	for i := range updates.Items {
		view.Items = append(view.Items, newUpdateView(&updates.Items[i]))
	}

	s.describeAvailable(ctx, refresh, &view)
	writeJSON(w, http.StatusOK, view)
}

// describeAvailable fills in what the registry has published, leaving the rest
// of the answer intact when it cannot be reached. A refresh asks the registry
// again instead of reading the cached listing; both report when the answer
// they returned was taken.
func (s *Server) describeAvailable(ctx context.Context, refresh bool, view *updatesView) {
	if s.charts == nil {
		return
	}
	read := s.charts.Versions
	if refresh {
		read = s.charts.Refresh
	}
	listing, err := read(ctx)
	if !listing.CheckedAt.IsZero() {
		checked := metav1.NewTime(listing.CheckedAt)
		view.CheckedAt = &checked
	}
	if err != nil {
		view.DiscoveryError = err.Error()
		return
	}
	published := listing.Versions
	latest, hasLatest := chartrepo.Latest(published)
	if hasLatest {
		view.LatestVersion = latest.String()
	}

	current, err := semver.Parse(s.Version)
	if err != nil {
		// A development build has no place in the ordering, so nothing is
		// offered. The operator refuses these anyway, and says why.
		view.DiscoveryError = "this operator reports version " + s.Version +
			", so it was not built from a published release and cannot be upgraded from."
		return
	}
	view.Available = hasLatest && latest.GT(current)

	for i := len(published) - 1; i >= 0; i-- {
		candidate := published[i]
		if !candidate.GT(current) || len(candidate.Pre) > 0 {
			continue
		}
		if !s.SelfUpdate.AllowMinor && (candidate.Major != current.Major || candidate.Minor != current.Minor) {
			continue
		}
		view.UpgradableTo = append(view.UpgradableTo, candidate.String())
	}
}

func (s *Server) getUpdate(w http.ResponseWriter, req *http.Request) {
	update := &kitchenv1alpha1.PlatformUpdate{}
	key := types.NamespacedName{Name: req.PathValue("name")}
	if err := s.Client.Get(req.Context(), key, update); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newUpdateView(update))
}

// createUpdateRequest is the whole of what a caller may say about an upgrade.
type createUpdateRequest struct {
	Version string `json:"version"`
}

func (s *Server) createUpdate(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	if !s.SelfUpdate.Enabled() {
		writeJSON(w, http.StatusConflict, errorBody{Error: "this installation cannot update itself: " +
			"upgrade the chart with `--set selfUpdate.enabled=true` first. It grants the update job " +
			"cluster-admin, so it is off by default."})
		return
	}

	body := createUpdateRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	version := strings.TrimPrefix(strings.TrimSpace(body.Version), "v")
	if _, err := semver.Parse(version); err != nil {
		badRequest(w, "version must be a SemVer release like 0.2.1 (got %q)", body.Version)
		return
	}

	caller, _ := CallerFrom(ctx)
	update := &kitchenv1alpha1.PlatformUpdate{
		ObjectMeta: metav1.ObjectMeta{
			// Generated rather than derived from the version: an upgrade that
			// failed is retried as a second attempt, and both are worth
			// keeping in the history.
			GenerateName: "update-" + strings.ReplaceAll(version, ".", "-") + "-",
			Annotations:  map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.PlatformUpdateSpec{Version: version},
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    update,
		Kind:      audit.KindPlatformUpdate,
		Operation: clickhouse.AuditCreate,
		From:      s.Version,
		To:        version,
		Reason:    fmt.Sprintf("an upgrade of the platform to %s was requested", version),
		Details:   map[string]any{"fromVersion": s.Version, "toVersion": version},
	}) {
		return
	}
	if err := s.Client.Create(ctx, update); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("platform update requested",
		"caller", callerName(caller), "version", version, "update", update.Name)
	writeJSON(w, http.StatusCreated, newUpdateView(update))
}

// selfUpdateContainer is the container that runs helm in the update job —
// `internal/controller`'s createJob names it. Selecting on it keeps a sidecar
// or an init container somebody adds to that pod later out of the upgrade's
// output.
const selfUpdateContainer = "helm"

// updateLogSelection is the one upgrade's worth of lines, written in the log
// query language: the job's own pod, in the platform namespace, and only the
// container helm ran in.
//
// It is composed here rather than taken from the request. The route is the
// operator's, but it reads the platform's own namespace — where the operator,
// the API and the identity provider also write — so a caller-supplied `q` or
// `where` would be a way to ask this endpoint for something that is not an
// upgrade's output. What the caller may say is what narrows: a window, a
// limit, and a substring, all of which compose with this rather than replace
// it.
//
// The pod is `<jobName>-<suffix>`: a Job names its pods after itself, and `*`
// is the query language's wildcard.
func updateLogSelection(jobName, search string) string {
	terms := []string{
		"source:" + clickhouse.SourcePlatform,
		"namespace:" + controller.PlatformNamespace,
		"pod:" + jobName + "-*",
		"container:" + selfUpdateContainer,
	}
	if search != "" {
		// As a phrase, so that a search term with a space, a comma or a colon
		// in it stays one term and its `*` stays an asterisk.
		terms = append(terms, quotedLogTerm(search))
	}
	return strings.Join(terms, " ")
}

// quotedLogTerm writes a literal as the query language's quoted phrase.
func quotedLogTerm(text string) string {
	escaped := strings.ReplaceAll(text, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// updateLogs is helm's output for one upgrade, bounded or followed.
//
// It is the endpoint the self-update job's logs were always collected for: the
// job's TTL reclaims the pod an hour after it finishes, and the lines outlive
// it in ClickHouse, so this answers for an upgrade that ran last month as
// readily as for the one running now.
func (s *Server) updateLogs(w http.ResponseWriter, req *http.Request) {
	// The PlatformUpdate has to exist before its logs are worth looking for: a
	// typo should say "no such update", not "no lines".
	update := &kitchenv1alpha1.PlatformUpdate{}
	key := types.NamespacedName{Name: req.PathValue("name")}
	if err := s.Client.Get(req.Context(), key, update); err != nil {
		s.writeError(w, err)
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
	search := strings.TrimSpace(req.URL.Query().Get("search"))

	// An update that failed preflight, or that the reconciler has not reached
	// yet, has no job and therefore no pod to select over. That is an upgrade
	// with no output rather than a missing one — the record itself says what
	// happened, in its phase and its message — so it answers an empty page.
	// Selecting over an empty pod name would be worse than useless: it would
	// widen the query to the whole platform namespace.
	if update.Status.JobName == "" {
		writeList(w, []clickhouse.LogLine{})
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	filter := clickhouse.LogFilter{
		LogSelection: clickhouse.LogSelection{
			Query: updateLogSelection(update.Status.JobName, search),
			Since: since,
			Until: until,
		},
		Limit: limit,
	}
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
		// The selection is this endpoint's own, not the caller's, so a query
		// the store refuses is the platform's fault and is reported as one.
		s.writeError(w, err)
		return
	}
	writeList(w, lines)
}
