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
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"unicode"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// Saved queries: a question about the logs with a name on it.
//
// The observability view already carries its whole selection in the URL, so
// any question is a link. What a link cannot be is *found* — "the one that
// shows the checkout 500s" lives in whoever's browser history, or nowhere.
// These are the same selection, named, and shared by everyone on the platform
// rather than by whoever has the link.
//
// A saved query is stored as a SavedQuery object because that is where the
// platform's state lives, and a saved query *without an alert* is the one
// write surface here with no reconciler behind it — deliberately. The rule
// that a write waits for its reconciler is about objects that do nothing until
// something acts on them; a saved query has its whole effect by existing.
//
// An alert is the exception, and PATCH is the route that adds one (#77). A
// standing question is not answered by existing: something has to ask it, on a
// schedule, which is SavedQueryReconciler — so the write surface has its
// reconciler after all, and the only thing that was ever true of a saved query
// with no alert stays true of one.

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=savedqueries,verbs=get;list;watch;create;update;patch;delete

// maxSavedQueries bounds the collection. It is a list to pick from, not a
// corpus: past a few dozen the sidebar is worse than the query bar.
const maxSavedQueries = 100

// savedQueryView is what the API answers with — the platform's vocabulary,
// and exactly the fields the observability view puts in its URL.
type savedQueryView struct {
	Name           string `json:"name"`
	Title          string `json:"title"`
	Description    string `json:"description,omitempty"`
	Query          string `json:"query,omitempty"`
	Where          string `json:"where,omitempty"`
	RangeMinutes   int32  `json:"rangeMinutes"`
	Limit          int32  `json:"limit,omitempty"`
	View           string `json:"view,omitempty"`
	IncludeCluster bool   `json:"includeCluster,omitempty"`
	SavedBy        string `json:"savedBy,omitempty"`
	CreatedAt      string `json:"createdAt"`

	// Alert is the standing question this one has become, absent when it is
	// only ever asked by a person opening it.
	Alert *savedQueryAlertView `json:"alert,omitempty"`
}

// savedQueryAlertView is the alert and what it has observed, in one object:
// the two are read together every time, and an alert whose last evaluation is
// somewhere else is an alert nobody can tell is working.
type savedQueryAlertView struct {
	WindowMinutes   int32  `json:"windowMinutes"`
	Threshold       int64  `json:"threshold"`
	Comparison      string `json:"comparison"`
	IntervalMinutes int32  `json:"intervalMinutes"`
	Suspended       bool   `json:"suspended"`

	Firing        bool   `json:"firing"`
	FiringSince   string `json:"firingSince,omitempty"`
	LastCount     int64  `json:"lastCount"`
	LastEvaluated string `json:"lastEvaluatedAt,omitempty"`
	Message       string `json:"message,omitempty"`
}

// newSavedQueryView renders a saved query for a reader.
//
// The alert's *definition* is everybody's — a window, a threshold and a
// comparison say nothing about anyone's lines. Its *observations* are not:
// `lastCount` is a number counted over the projects the query's author could
// see, and a reader who could not have counted it themselves is shown the
// alert without them (issue #421). That is the same rule the listing follows,
// applied to the one field that carries a count rather than to the object.
func newSavedQueryView(scope projectScope, query *kitchenv1alpha1.SavedQuery) savedQueryView {
	view := savedQueryView{
		Name:           query.Name,
		Title:          query.Spec.Title,
		Description:    query.Spec.Description,
		Query:          query.Spec.Query,
		Where:          query.Spec.Where,
		RangeMinutes:   query.Spec.RangeMinutes,
		Limit:          query.Spec.Limit,
		View:           query.Spec.View,
		IncludeCluster: query.Spec.IncludeCluster,
		SavedBy:        query.Spec.SavedBy,
		CreatedAt:      query.CreationTimestamp.UTC().Format("2006-01-02T15:04:05Z"),
	}
	if alert := query.Spec.Alert; alert != nil {
		comparison := alert.Comparison
		if comparison == "" {
			comparison = alertComparisonAbove
		}
		view.Alert = &savedQueryAlertView{
			WindowMinutes:   alert.WindowMinutes,
			Threshold:       alert.Threshold,
			Comparison:      comparison,
			IntervalMinutes: alert.Interval(),
			Suspended:       alert.Suspended,
		}
		if covers(scope, query.Spec.Scope) {
			view.Alert.Firing = query.Status.Firing
			view.Alert.LastCount = query.Status.LastCount
			view.Alert.Message = query.Status.Message
			if at := query.Status.FiringSince; at != nil {
				view.Alert.FiringSince = at.UTC().Format("2006-01-02T15:04:05Z")
			}
			if at := query.Status.LastEvaluationTime; at != nil {
				view.Alert.LastEvaluated = at.UTC().Format("2006-01-02T15:04:05Z")
			}
		}
	}
	return view
}

// hiddenFrom reports whether a saved query is one this caller must not be
// shown, because it names a project they cannot see.
//
// A saved query is shared by everyone on the platform and carries no project
// of its own: what it is about is inside its selection, as `project:billing`.
// Rather than parse the query language a second time here — which is how the
// parser and the guard end up disagreeing — this looks for the names of the
// projects the caller cannot see, as whole words anywhere in the selection or
// in the title and description that were written to describe them.
//
// The stored `where` is read too, though nothing may write one any more: a
// query saved before #421 still carries what it carried, and the reason to
// withhold it has not changed. That expression is also why this guard could
// once be walked around — `project = concat('bil','ling')` names nothing a
// word match can see — which is another thing that stops being true when the
// only selection is one the platform compiles.
//
// It errs towards hiding: a query whose title happens to contain a word that
// is also somebody else's project name is withheld from a caller who cannot
// see that project. That is the safe direction, and the query's results would
// have been filtered to nothing for them anyway.
func hiddenFrom(scope projectScope, query *kitchenv1alpha1.SavedQuery) bool {
	if scope.all || len(scope.hidden) == 0 {
		return false
	}
	for _, text := range []string{query.Spec.Query, query.Spec.Where, query.Spec.Title, query.Spec.Description} {
		if namesAny(text, scope.hidden) {
			return true
		}
	}
	return false
}

// covers reports whether a reader may be shown what an alert on this query
// counted: their own scope has to contain the scope the count was taken over.
//
// An operator is shown everything. A query whose scope is the platform's whole
// store is therefore an operator's to read the numbers of, and a query saved
// before scopes were recorded has none — nothing is counted over it, so there
// is nothing to withhold and nothing to show.
func covers(reader projectScope, recorded *kitchenv1alpha1.SavedQueryScope) bool {
	if reader.all {
		return true
	}
	if recorded == nil || recorded.Platform || len(recorded.Projects) == 0 {
		return false
	}
	for _, project := range recorded.Projects {
		if !reader.allows(project) {
			return false
		}
	}
	return true
}

// recordedScope is the scope a query is saved with: what its author could see
// at the moment they saved it, which is what an alert on it may ever count.
func recordedScope(scope projectScope) *kitchenv1alpha1.SavedQueryScope {
	if scope.all {
		return &kitchenv1alpha1.SavedQueryScope{Platform: true}
	}
	return &kitchenv1alpha1.SavedQueryScope{Projects: scope.names()}
}

// namesAny reports whether text contains any of these names as a whole word.
// The split is on everything a Kubernetes object name cannot contain, so
// `project:billing` and `project = 'billing'` both name `billing`, and
// `billing-api` does not.
func namesAny(text string, names []string) bool {
	if text == "" {
		return false
	}
	for _, word := range strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-'
	}) {
		for _, name := range names {
			if word == name {
				return true
			}
		}
	}
	return false
}

func (s *Server) listSavedQueries(w http.ResponseWriter, req *http.Request) {
	list := &kitchenv1alpha1.SavedQueryList{}
	if err := s.Client.List(req.Context(), list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	scope := scopeFrom(req.Context())
	views := make([]savedQueryView, 0, len(list.Items))
	for i := range list.Items {
		if hiddenFrom(scope, &list.Items[i]) {
			continue
		}
		views = append(views, newSavedQueryView(scope, &list.Items[i]))
	}
	// By title, because that is what the sidebar shows and creation order is
	// not an order anyone remembers.
	sort.Slice(views, func(i, j int) bool {
		return strings.ToLower(views[i].Title) < strings.ToLower(views[j].Title)
	})
	writeList(w, views)
}

// saveQueryRequest is what the dashboard sends when a question is worth
// keeping. Everything but the title is optional, because an empty selection
// over a window is itself a legitimate question.
type saveQueryRequest struct {
	Name           string `json:"name,omitempty"`
	Title          string `json:"title"`
	Description    string `json:"description,omitempty"`
	Query          string `json:"query,omitempty"`
	Where          string `json:"where,omitempty"`
	RangeMinutes   int32  `json:"rangeMinutes,omitempty"`
	Limit          int32  `json:"limit,omitempty"`
	View           string `json:"view,omitempty"`
	IncludeCluster bool   `json:"includeCluster,omitempty"`

	// Alert makes it a standing question at the moment it is saved, which is
	// usually when somebody knows they want one — they are looking at the
	// lines that made them want it.
	Alert *alertRequest `json:"alert,omitempty"`
}

// alertRequest is a threshold over a window, asked on a schedule. It is the
// same shape on create and on patch, and on patch it replaces the alert whole:
// an alert is five numbers, and a patch that merged them would let a request
// that says `{"threshold": 0}` mean two different things.
type alertRequest struct {
	WindowMinutes   int32  `json:"windowMinutes,omitempty"`
	Threshold       int64  `json:"threshold"`
	Comparison      string `json:"comparison,omitempty"`
	IntervalMinutes int32  `json:"intervalMinutes,omitempty"`
	Suspended       bool   `json:"suspended,omitempty"`
}

const (
	alertComparisonAbove = kitchenv1alpha1.AlertComparisonAbove
	alertComparisonBelow = kitchenv1alpha1.AlertComparisonBelow
	// maxAlertMinutes bounds both the window and the interval, and is a day:
	// past that the question is about a trend rather than about now, and the
	// answer is a chart somebody reads rather than a message somebody is sent.
	maxAlertMinutes = 1440
)

// alertSpec validates an alert request and turns it into the spec, refusing
// what the CRD would refuse later — at the moment somebody could still fix it.
func alertSpec(request *alertRequest) (*kitchenv1alpha1.SavedQueryAlert, error) {
	if request == nil {
		return nil, nil
	}
	if request.WindowMinutes < 1 || request.WindowMinutes > maxAlertMinutes {
		return nil, fmt.Errorf("alert.windowMinutes must be between 1 and %d — how far back each "+
			"evaluation counts (got %d)", maxAlertMinutes, request.WindowMinutes)
	}
	if request.Threshold < 0 {
		return nil, fmt.Errorf("alert.threshold must not be negative (got %d)", request.Threshold)
	}
	if request.IntervalMinutes < 0 || request.IntervalMinutes > maxAlertMinutes {
		return nil, fmt.Errorf("alert.intervalMinutes must be between 1 and %d; 0 means the "+
			"default of %d (got %d)", maxAlertMinutes, kitchenv1alpha1.DefaultAlertIntervalMinutes,
			request.IntervalMinutes)
	}
	comparison := request.Comparison
	switch comparison {
	case "":
		comparison = alertComparisonAbove
	case alertComparisonAbove, alertComparisonBelow:
	default:
		return nil, fmt.Errorf("alert.comparison must be %q — more matching lines than the "+
			"threshold — or %q, which is the heartbeat: a service that logs every minute and has "+
			"stopped (got %q)", alertComparisonAbove, alertComparisonBelow, request.Comparison)
	}
	return &kitchenv1alpha1.SavedQueryAlert{
		WindowMinutes:   request.WindowMinutes,
		Threshold:       request.Threshold,
		Comparison:      comparison,
		IntervalMinutes: request.IntervalMinutes,
		Suspended:       request.Suspended,
	}, nil
}

func (s *Server) createSavedQuery(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := saveQueryRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Title = strings.TrimSpace(body.Title)
	body.Query = strings.TrimSpace(body.Query)
	body.Name = strings.TrimSpace(body.Name)

	if body.Title == "" {
		badRequest(w, "title is required: what this query is called")
		return
	}
	// The same refusal the four query routes give, for the same reason: a
	// saved query is a selection, and a selection is written in the query
	// language. This one matters twice over — a stored `where` is evaluated
	// later, by a reconciler, on a schedule, with nobody looking at it.
	if strings.TrimSpace(body.Where) != "" {
		badRequest(w, "%s", errRawWhere.Error())
		return
	}
	// The query is compiled before it is stored. A saved query that cannot be
	// run is worse than no saved query — it is found later, by someone who
	// did not write it, at the moment they needed an answer.
	if body.Query != "" {
		if _, err := clickhouse.CompileLogQuery(body.Query); err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
	}
	switch body.View {
	case "", "lines", "patterns":
	default:
		badRequest(w, "view must be lines or patterns (got %q)", body.View)
		return
	}
	if body.Limit < 0 || body.Limit > clickhouse.MaxLogLimit {
		badRequest(w, "limit must be between 1 and %d (got %d)", clickhouse.MaxLogLimit, body.Limit)
		return
	}
	if body.RangeMinutes < 0 {
		badRequest(w, "rangeMinutes must not be negative; 0 means everything retained (got %d)", body.RangeMinutes)
		return
	}

	if body.Name == "" {
		body.Name = savedQueryName(body.Title)
	}
	if errs := validation.IsDNS1123Label(body.Name); len(errs) > 0 {
		badRequest(w, "name must work as a DNS label — lowercase letters, digits and '-', starting and ending alphanumeric (got %q)", body.Name)
		return
	}

	list := &kitchenv1alpha1.SavedQueryList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	if len(list.Items) >= maxSavedQueries {
		badRequest(w, "the platform already holds %d saved queries; delete one before saving another", maxSavedQueries)
		return
	}

	alert, err := alertSpec(body.Alert)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	caller, _ := CallerFrom(ctx)
	scope := scopeFrom(ctx)
	saved := &kitchenv1alpha1.SavedQuery{
		ObjectMeta: metav1.ObjectMeta{Name: body.Name, Namespace: s.Namespace},
		Spec: kitchenv1alpha1.SavedQuerySpec{
			Title:       body.Title,
			Description: strings.TrimSpace(body.Description),
			Query:       body.Query,
			// What an alert on this query may ever count: the projects this
			// caller can see, written down now, while the token that says so
			// has just been checked.
			Scope:          recordedScope(scope),
			RangeMinutes:   body.RangeMinutes,
			Limit:          body.Limit,
			View:           body.View,
			IncludeCluster: body.IncludeCluster,
			SavedBy:        callerName(caller),
			Alert:          alert,
		},
	}
	if err := s.Client.Create(ctx, saved); err != nil {
		// The name was derived from the title, so the API server's own
		// conflict — which talks about savedqueries.kitchen.bermos.dev — would
		// name something the caller never typed.
		if apierrors.IsAlreadyExists(err) {
			writeJSON(w, http.StatusConflict, errorBody{Error: savedQueryConflict(body.Name)})
			return
		}
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newSavedQueryView(scope, saved))
}

// patchSavedQueryRequest is the alert and nothing else.
//
// The selection itself is not patchable, and that is not an omission: a saved
// query is a link with a name on it, so changing what it asks is saving a
// different question. Editing one in place would move it under everybody who
// had already found it — including, once it has an alert, whoever is being
// woken by it.
//
// `alert` is a raw message so that the three cases stay three: absent changes
// nothing, `null` removes the alert, and an object replaces it. (A pointer
// would not: encoding/json reads a `null` into one by setting it to nil, which
// is the same as absent.)
//
// The selection's own fields are named here only so that sending one is
// answered with the reason rather than with the decoder's "unknown field".
type patchSavedQueryRequest struct {
	Alert json.RawMessage `json:"alert,omitempty"`

	Title        json.RawMessage `json:"title,omitempty"`
	Query        json.RawMessage `json:"query,omitempty"`
	Where        json.RawMessage `json:"where,omitempty"`
	RangeMinutes json.RawMessage `json:"rangeMinutes,omitempty"`
}

// patchSavedQuery sets, changes or removes the standing alert on a saved
// query — the second trigger onto the notification path (#77).
//
// It is the same requirement as deleting one, for the same reason: a saved
// query belongs to the platform rather than to whoever saved it, and this
// route can only make it ask itself on a schedule. What that crossing *does*
// is a notification subscription's, and writing one of those is an admin's
// (see docs/api/notifications.md).
func (s *Server) patchSavedQuery(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	saved := &kitchenv1alpha1.SavedQuery{}
	if err := s.get(ctx, req.PathValue("name"), saved); err != nil {
		s.writeError(w, err)
		return
	}
	if hiddenFrom(scopeFrom(ctx), saved) {
		s.writeError(w, apierrors.NewNotFound(
			schema.GroupResource{Group: kitchenv1alpha1.GroupVersion.Group, Resource: "savedqueries"},
			saved.Name))
		return
	}

	body := patchSavedQueryRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.Title != nil || body.Query != nil || body.Where != nil || body.RangeMinutes != nil {
		badRequest(w, "a saved query's selection is not editable — save the question you meant "+
			"instead, so that a link somebody already has keeps asking what it asked. This route "+
			"changes the alert and nothing else")
		return
	}
	if body.Alert == nil {
		badRequest(w, "nothing to change: send alert to set one, or alert: null to remove it")
		return
	}

	if string(body.Alert) == "null" {
		if saved.Spec.Alert == nil {
			badRequest(w, "this query has no alert")
			return
		}
		saved.Spec.Alert = nil
	} else {
		request := alertRequest{}
		if err := json.Unmarshal(body.Alert, &request); err != nil {
			badRequest(w, "alert is not an object: %s", err.Error())
			return
		}
		alert, err := alertSpec(&request)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		saved.Spec.Alert = alert
	}

	if err := s.Client.Update(ctx, saved); err != nil {
		s.writeError(w, err)
		return
	}
	// The status is the last evaluation's and stays as it was until the next
	// one: an alert that has just been widened has not been asked yet, and
	// saying it is firing on the old threshold's answer would be a lie the
	// reconciler corrects a minute later.
	writeJSON(w, http.StatusOK, newSavedQueryView(scopeFrom(ctx), saved))
}

func (s *Server) deleteSavedQuery(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	saved := &kitchenv1alpha1.SavedQuery{}
	if err := s.get(ctx, req.PathValue("name"), saved); err != nil {
		s.writeError(w, err)
		return
	}
	if hiddenFrom(scopeFrom(ctx), saved) {
		// A query the caller was never shown is one they are told does not
		// exist. A 403 here would say it does, which is the whole of what was
		// being kept from them.
		s.writeError(w, apierrors.NewNotFound(
			schema.GroupResource{Group: kitchenv1alpha1.GroupVersion.Group, Resource: "savedqueries"},
			saved.Name))
		return
	}
	// Nothing owns a saved query and nothing cleans up after it, so the delete
	// is complete when the API server accepts it — 200 rather than 202.
	if err := s.Client.Delete(ctx, saved); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newSavedQueryView(scopeFrom(ctx), saved))
}

// savedQueryName turns a title into the object name a caller does not have to
// invent: "Checkout 500s" becomes "checkout-500s".
//
// A title that survives none of that — an emoji, a name in a script with no
// ASCII in it — leaves nothing to build a DNS label from, so it falls back to
// a fixed prefix and the caller is told the name it got.
func savedQueryName(title string) string {
	slug := strings.Builder{}
	previousDash := false
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', unicode.IsDigit(r):
			slug.WriteRune(r)
			previousDash = false
		case slug.Len() > 0 && !previousDash:
			slug.WriteRune('-')
			previousDash = true
		}
	}
	name := strings.Trim(slug.String(), "-")
	if len(name) > 50 {
		name = strings.Trim(name[:50], "-")
	}
	if name == "" {
		return "query"
	}
	return name
}

// savedQueryConflict is the message a duplicate name gets. It is separate from
// the API server's own, which talks about "savedqueries.kitchen.bermos.dev".
func savedQueryConflict(name string) string {
	return fmt.Sprintf("a saved query called %q already exists; give this one a different title or delete that one", name)
}
