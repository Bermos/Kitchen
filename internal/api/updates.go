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
	"sort"
	"strings"

	"github.com/blang/semver/v4"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/chartrepo"
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

	s.describeAvailable(req, &view)
	writeJSON(w, http.StatusOK, view)
}

// describeAvailable fills in what the registry has published, leaving the rest
// of the answer intact when it cannot be reached.
func (s *Server) describeAvailable(req *http.Request, view *updatesView) {
	if s.charts == nil {
		return
	}
	published, err := s.charts.Versions(req.Context())
	if err != nil {
		view.DiscoveryError = err.Error()
		return
	}
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
	if err := s.Client.Create(ctx, update); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("platform update requested",
		"caller", callerName(caller), "version", version, "update", update.Name)
	writeJSON(w, http.StatusCreated, newUpdateView(update))
}
