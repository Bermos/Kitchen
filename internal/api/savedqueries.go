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
// platform's state lives, and it is the one write surface here with no
// reconciler behind it — deliberately. The rule that a write waits for its
// reconciler is about objects that do nothing until something acts on them; a
// saved query has its whole effect by existing.

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
}

func newSavedQueryView(query *kitchenv1alpha1.SavedQuery) savedQueryView {
	return savedQueryView{
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
}

// hiddenFrom reports whether a saved query is one this caller must not be
// shown, because it names a project they cannot see.
//
// A saved query is shared by everyone on the platform and carries no project
// of its own: what it is about is inside its selection, as `project:billing`
// or as a `where` naming the column. Rather than parse the query language a
// second time here — which is how the parser and the guard end up disagreeing
// — this looks for the names of the projects the caller cannot see, as whole
// words in either half of the selection or in the title and description that
// were written to describe them.
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
		views = append(views, newSavedQueryView(&list.Items[i]))
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
	body.Where = strings.TrimSpace(body.Where)
	body.Name = strings.TrimSpace(body.Name)

	if body.Title == "" {
		badRequest(w, "title is required: what this query is called")
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

	caller, _ := CallerFrom(ctx)
	saved := &kitchenv1alpha1.SavedQuery{
		ObjectMeta: metav1.ObjectMeta{Name: body.Name, Namespace: s.Namespace},
		Spec: kitchenv1alpha1.SavedQuerySpec{
			Title:          body.Title,
			Description:    strings.TrimSpace(body.Description),
			Query:          body.Query,
			Where:          body.Where,
			RangeMinutes:   body.RangeMinutes,
			Limit:          body.Limit,
			View:           body.View,
			IncludeCluster: body.IncludeCluster,
			SavedBy:        callerName(caller),
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
	writeJSON(w, http.StatusCreated, newSavedQueryView(saved))
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
	writeJSON(w, http.StatusOK, newSavedQueryView(saved))
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
