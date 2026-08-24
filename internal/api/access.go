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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The access surface: who holds what right now, and the recertification
// cycles somebody has reviewed that against.
//
// Every route here is the operator's. That is not caution — it is the subject
// matter: this is the whole installation's access in one document, and a
// member who could read it would learn every account on the platform and
// every project's membership. "Who has access to shop" is already answerable
// to shop's own admins, through `GET /projects/{name}/members`.
//
// # The reviewer may be the reviewed
//
// A decision a reviewer makes about their own grant is recorded as a
// self-review rather than refused, which is §8.4's answer for self-approval
// and is the same argument: an installation with one operator has exactly one
// person who can review that operator's grant, and refusing would either make
// the control unsatisfiable or push somebody into creating a second account
// to satisfy it — worse evidence, not better. What the platform will not do
// is let it pass unremarked.

// identityView is one grant in the live survey.
type identityView struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	// Grant is `platform` or a project name, and Role what is held there.
	Grant string `json:"grant"`
	Role  string `json:"role"`

	LastActive *time.Time `json:"lastActive,omitempty"`
	Inactive   bool       `json:"inactive,omitempty"`
	Unknown    bool       `json:"unknown,omitempty"`
	Orphaned   bool       `json:"orphaned,omitempty"`
}

// identitiesView is the whole survey, exportable as it is.
type identitiesView struct {
	GeneratedAt    time.Time `json:"generatedAt"`
	InactivityDays int32     `json:"inactivityDays"`
	// DirectoryConsulted says whether the identity provider answered. When it
	// is false nothing is reported as unknown and nothing as orphaned, and
	// `message` says why — "we could not ask" and "nobody is behind it" are
	// different sentences and only one of them is evidence.
	DirectoryConsulted bool           `json:"directoryConsulted"`
	Identities         []identityView `json:"identities"`
	Orphans            int            `json:"orphans"`
	Message            string         `json:"message,omitempty"`
}

// listIdentities handles GET /api/v1/access/identities: who holds what on
// this platform right now, with what the platform knows about whether anybody
// is still behind each grant.
//
// It is the same materializer a recertification cycle freezes — one function,
// two consumers — so the screen and the snapshot cannot disagree about what
// the platform's access is.
func (s *Server) listIdentities(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	kitchen := kitchenFrom(ctx)
	projects := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, projects, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}

	messages := []string{}
	activity, err := s.actorActivity(ctx)
	if err != nil {
		messages = append(messages,
			"the activity log could not be read, so dormancy is not judged: "+err.Error())
	}
	accounts, consulted, err := s.directoryAccounts(ctx)
	if err != nil {
		messages = append(messages,
			"the account directory could not be read, so no grant is reported as belonging to nobody: "+
				err.Error())
	}

	inactivity := int32(90)
	if kitchen != nil && kitchen.Spec.Compliance.Access.InactivityDays > 0 {
		inactivity = kitchen.Spec.Compliance.Access.InactivityDays
	}
	survey := access.Survey(access.SurveyInput{
		Kitchen:            kitchen,
		Projects:           projects.Items,
		Activity:           activity,
		Accounts:           accounts,
		DirectoryConsulted: consulted,
		InactivityDays:     inactivity,
		At:                 time.Now(),
		Message:            strings.Join(messages, "; "),
	})

	body := identitiesView{
		GeneratedAt:        survey.At,
		InactivityDays:     survey.InactivityDays,
		DirectoryConsulted: survey.DirectoryConsulted,
		Identities:         make([]identityView, 0, len(survey.Identities)),
		Orphans:            survey.Orphans,
		Message:            survey.Message,
	}
	for _, identity := range survey.Identities {
		body.Identities = append(body.Identities, identityView{
			Subject:    identity.Subject,
			Email:      identity.Email,
			Grant:      identity.Grant,
			Role:       identity.Role,
			LastActive: identity.LastActive,
			Inactive:   identity.Inactive,
			Unknown:    identity.Unknown,
			Orphaned:   identity.Orphaned,
		})
	}
	writeJSON(w, http.StatusOK, body)
}

// actorActivity reads when each identity was last recorded doing something.
// A store that is not there is not a failure of this endpoint: the survey
// still answers, with dormancy unjudged and a message saying so.
func (s *Server) actorActivity(ctx context.Context) (map[string]time.Time, error) {
	store, err := s.logStore(ctx)
	if err != nil {
		return nil, err
	}
	return store.ActorActivity(ctx)
}

// directoryAccounts reads the identity provider's account directory. The
// second result says whether it answered at all — a federated issuer serves
// none by design, and every grant on such an installation must not become an
// orphan because of it.
func (s *Server) directoryAccounts(ctx context.Context) (map[string]struct{}, bool, error) {
	directory, err := s.directory(ctx)
	if err != nil {
		return nil, false, err
	}
	found, err := directory.Accounts(ctx)
	if err != nil {
		return nil, false, err
	}
	accounts := make(map[string]struct{}, len(found)*2)
	for _, account := range found {
		if account.Subject != "" {
			accounts[account.Subject] = struct{}{}
		}
		if account.Email != "" {
			accounts[account.Email] = struct{}{}
		}
	}
	return accounts, true, nil
}

// accessReviewEntryView is one grant in a cycle, with its decision.
type accessReviewEntryView struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	Grant   string `json:"grant"`
	Role    string `json:"role"`

	LastActive *time.Time `json:"lastActive,omitempty"`
	Inactive   bool       `json:"inactive,omitempty"`
	Unknown    bool       `json:"unknown,omitempty"`
	Orphaned   bool       `json:"orphaned,omitempty"`

	Decision   string     `json:"decision,omitempty"`
	DecidedBy  string     `json:"decidedBy,omitempty"`
	DecidedAt  *time.Time `json:"decidedAt,omitempty"`
	Note       string     `json:"note,omitempty"`
	SelfReview bool       `json:"selfReview,omitempty"`

	Applied      bool   `json:"applied,omitempty"`
	ApplyMessage string `json:"applyMessage,omitempty"`
}

// accessReviewArtifactView points at the retained evidence a closed cycle
// produced. It is a pointer, never the envelope: the envelope is in the
// store, and a copy on the way out would be a second source for the one
// document that must have only one.
type accessReviewArtifactView struct {
	RecordID      string     `json:"recordID,omitempty"`
	Subject       string     `json:"subject,omitempty"`
	PredicateType string     `json:"predicateType,omitempty"`
	SignedAt      *time.Time `json:"signedAt,omitempty"`
	Message       string     `json:"message,omitempty"`
}

// accessReviewView is one cycle as the register serves it.
type accessReviewView struct {
	Name      string   `json:"name"`
	Scope     string   `json:"scope"`
	Project   string   `json:"project,omitempty"`
	Reviewers []string `json:"reviewers"`
	OpenedBy  string   `json:"openedBy"`
	Reason    string   `json:"reason,omitempty"`

	DueBy      time.Time  `json:"dueBy"`
	OpenedAt   *time.Time `json:"openedAt,omitempty"`
	SnapshotAt *time.Time `json:"snapshotAt,omitempty"`
	ClosedBy   string     `json:"closedBy,omitempty"`
	ClosedAt   *time.Time `json:"closedAt,omitempty"`

	// Phase is judged against the clock server-side, so Overdue here means
	// overdue now rather than "the reconciler has got round to it".
	Phase        string `json:"phase"`
	Pending      int32  `json:"pending"`
	Confirmed    int32  `json:"confirmed"`
	Revoked      int32  `json:"revoked"`
	SelfReviewed int32  `json:"selfReviewed"`
	Orphaned     int32  `json:"orphaned"`

	Entries  []accessReviewEntryView   `json:"entries"`
	Artifact *accessReviewArtifactView `json:"artifact,omitempty"`

	CreatedAt  time.Time       `json:"createdAt"`
	Conditions []conditionView `json:"conditions,omitempty"`
}

func newAccessReviewView(review *kitchenv1alpha1.AccessReview) accessReviewView {
	view := accessReviewView{
		Name:         review.Name,
		Scope:        string(review.Spec.Scope),
		Reviewers:    make([]string, 0, len(review.Spec.Reviewers)),
		OpenedBy:     review.Spec.OpenedBy,
		Reason:       review.Spec.Reason,
		DueBy:        review.Spec.DueBy.Time,
		ClosedBy:     review.Status.ClosedBy,
		Phase:        string(review.EffectivePhase(time.Now())),
		Pending:      review.Status.Pending,
		Confirmed:    review.Status.Confirmed,
		Revoked:      review.Status.Revoked,
		SelfReviewed: review.Status.SelfReviewed,
		Orphaned:     review.Status.Orphaned,
		Entries:      make([]accessReviewEntryView, 0, len(review.Status.Entries)),
		CreatedAt:    review.CreationTimestamp.Time,
		Conditions:   conditionViews(review.Status.Conditions),
	}
	for _, reviewer := range review.Spec.Reviewers {
		name := reviewer.Email
		if name == "" {
			name = reviewer.Subject
		}
		view.Reviewers = append(view.Reviewers, name)
	}
	if review.Spec.ProjectRef != nil {
		view.Project = review.Spec.ProjectRef.Name
	}
	if at := review.Status.OpenedAt; at != nil {
		view.OpenedAt = &at.Time
	}
	if at := review.Status.SnapshotAt; at != nil {
		view.SnapshotAt = &at.Time
	}
	if at := review.Status.ClosedAt; at != nil {
		view.ClosedAt = &at.Time
	}
	for i := range review.Status.Entries {
		entry := &review.Status.Entries[i]
		row := accessReviewEntryView{
			Subject:      entry.Subject,
			Email:        entry.Email,
			Grant:        entry.Grant,
			Role:         entry.Role,
			Inactive:     entry.Inactive,
			Unknown:      entry.Unknown,
			Orphaned:     entry.Orphaned,
			Decision:     string(entry.Decision),
			DecidedBy:    entry.DecidedBy,
			Note:         entry.Note,
			SelfReview:   entry.SelfReview,
			Applied:      entry.Applied,
			ApplyMessage: entry.ApplyMessage,
		}
		if at := entry.LastActive; at != nil {
			row.LastActive = &at.Time
		}
		if at := entry.DecidedAt; at != nil {
			row.DecidedAt = &at.Time
		}
		view.Entries = append(view.Entries, row)
	}
	if artifact := review.Status.Artifact; artifact != nil {
		row := &accessReviewArtifactView{
			RecordID:      artifact.RecordID,
			Subject:       artifact.Subject,
			PredicateType: artifact.PredicateType,
			Message:       artifact.Message,
		}
		if at := artifact.SignedAt; at != nil {
			row.SignedAt = &at.Time
		}
		view.Artifact = row
	}
	return view
}

// openAccessReviewRequest opens a cycle out of cadence: an audit is coming,
// somebody left, a project changed hands.
type openAccessReviewRequest struct {
	// Scope is `platform`, `project` or `all`; empty means `all`.
	Scope string `json:"scope,omitempty"`
	// Project narrows a project-scoped cycle.
	Project string `json:"project,omitempty"`
	// Reviewers name who is expected to decide, by subject or verified
	// address. Empty means the platform's operators.
	Reviewers []string `json:"reviewers,omitempty"`
	// DueBy bounds the cycle, RFC 3339. Empty uses the configured window.
	DueBy  time.Time `json:"dueBy,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

// openAccessReview handles POST /api/v1/access/reviews.
func (s *Server) openAccessReview(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	kitchen := kitchenFrom(ctx)

	body := openAccessReviewRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Scope = strings.TrimSpace(body.Scope)
	body.Project = strings.TrimSpace(body.Project)
	body.Reason = strings.TrimSpace(body.Reason)

	scope := kitchenv1alpha1.AccessReviewAll
	if body.Scope != "" {
		scope = kitchenv1alpha1.AccessReviewScope(body.Scope)
	}
	switch scope {
	case kitchenv1alpha1.AccessReviewAll, kitchenv1alpha1.AccessReviewPlatform:
		if body.Project != "" {
			badRequest(w, "project belongs to a project-scoped review only: "+
				"a %s cycle already covers every project", scope)
			return
		}
	case kitchenv1alpha1.AccessReviewProject:
		if body.Project == "" {
			badRequest(w, "project is required for a project-scoped review: name the project it is about")
			return
		}
		if err := s.get(ctx, body.Project, &kitchenv1alpha1.Project{}); err != nil {
			badRequest(w, "project %q does not exist", body.Project)
			return
		}
	default:
		badRequest(w, "scope %q is not one of platform, project or all", body.Scope)
		return
	}

	// One cycle at a time over the same grants. Two open cycles would be two
	// reviewers deciding the same question, and a close that applied one set
	// of revocations while the other still showed the grant.
	open, err := s.openCycles(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}
	for i := range open {
		if !overlaps(&open[i], scope, body.Project) {
			continue
		}
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"access review %s is already open over these grants: close it before opening another, "+
				"or two reviewers will be deciding the same question", open[i].Name)})
		return
	}

	reviewers, err := s.resolveReviewers(body.Reviewers, kitchen)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	dueBy := body.DueBy
	if dueBy.IsZero() {
		days := int32(14)
		if kitchen != nil && kitchen.Spec.Compliance.Access.DueDays > 0 {
			days = kitchen.Spec.Compliance.Access.DueDays
		}
		dueBy = time.Now().Add(time.Duration(days) * 24 * time.Hour)
	}
	if !dueBy.After(time.Now()) {
		badRequest(w, "dueBy %s is not in the future: a cycle that is born overdue asks nobody anything",
			dueBy.UTC().Format(time.RFC3339))
		return
	}

	caller, _ := CallerFrom(ctx)
	reason := body.Reason
	if reason == "" {
		reason = "opened out of cadence through the API"
	}
	review := &kitchenv1alpha1.AccessReview{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "access-review-",
			Namespace:    s.Namespace,
			Annotations:  map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.AccessReviewSpec{
			Scope:     scope,
			Reviewers: reviewers,
			OpenedBy:  callerName(caller),
			DueBy:     metav1.NewTime(dueBy.UTC()),
			Reason:    reason,
		},
	}
	if scope == kitchenv1alpha1.AccessReviewProject {
		review.Spec.ProjectRef = &kitchenv1alpha1.LocalObjectReference{Name: body.Project}
	}

	if !s.recorded(w, req, audit.Transition{
		Object:     review,
		Kind:       audit.KindAccessReview,
		Operation:  clickhouse.AuditCreate,
		Privileged: audit.PrivilegeAccess,
		To:         string(kitchenv1alpha1.AccessReviewOpen),
		Reason: fmt.Sprintf("access recertification opened over %s, due %s",
			describeScope(scope, body.Project), dueBy.UTC().Format(time.RFC3339)),
		Details: map[string]any{
			"scope":     string(scope),
			"dueBy":     dueBy.UTC().Format(time.RFC3339),
			"reviewers": subjectsOf(reviewers),
			"reason":    reason,
		},
	}) {
		return
	}
	if err := s.Client.Create(ctx, review); err != nil {
		s.writeError(w, err)
		return
	}

	// The snapshot is taken here rather than by the reconciler, and the
	// reason is the one the whole object exists for: a review is of what was
	// true at an instant, and the instant that matters is the one the person
	// asked at — not whenever a reconcile next ran.
	if err := s.snapshot(ctx, review); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("access recertification opened through the api", "review", review.Name,
		"scope", scope, "grants", len(review.Status.Entries), "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, newAccessReviewView(review))
}

// snapshot freezes who holds what onto a newly opened cycle.
func (s *Server) snapshot(ctx context.Context, review *kitchenv1alpha1.AccessReview) error {
	kitchen := kitchenFrom(ctx)
	projects := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, projects, client.InNamespace(s.Namespace)); err != nil {
		return err
	}

	activity, _ := s.actorActivity(ctx)
	accounts, consulted, _ := s.directoryAccounts(ctx)

	inactivity := int32(90)
	if kitchen != nil && kitchen.Spec.Compliance.Access.InactivityDays > 0 {
		inactivity = kitchen.Spec.Compliance.Access.InactivityDays
	}
	survey := access.Survey(access.SurveyInput{
		Kitchen:            kitchen,
		Projects:           projects.Items,
		Activity:           activity,
		Accounts:           accounts,
		DirectoryConsulted: consulted,
		InactivityDays:     inactivity,
		At:                 time.Now(),
	})

	now := metav1.Now()
	entries := []kitchenv1alpha1.AccessReviewEntry{}
	orphans := int32(0)
	for _, identity := range survey.Identities {
		if !review.Covers(identity.Grant) {
			continue
		}
		entry := kitchenv1alpha1.AccessReviewEntry{
			AccessSubject: kitchenv1alpha1.AccessSubject{
				Subject: identity.Subject,
				Email:   identity.Email,
			},
			Grant:    identity.Grant,
			Role:     identity.Role,
			Inactive: identity.Inactive,
			Unknown:  identity.Unknown,
			Orphaned: identity.Orphaned,
		}
		if identity.LastActive != nil {
			entry.LastActive = &metav1.Time{Time: *identity.LastActive}
		}
		if entry.Orphaned {
			orphans++
		}
		entries = append(entries, entry)
	}

	review.Status.Phase = kitchenv1alpha1.AccessReviewOpen
	review.Status.OpenedAt = &now
	review.Status.SnapshotAt = &metav1.Time{Time: survey.At}
	review.Status.Entries = entries
	review.Status.Pending = int32(len(entries)) //nolint:gosec // a grant count is not a security boundary
	review.Status.Orphaned = orphans
	return s.Client.Status().Update(ctx, review)
}

// openCycles lists the cycles that are not closed.
func (s *Server) openCycles(ctx context.Context) ([]kitchenv1alpha1.AccessReview, error) {
	list := &kitchenv1alpha1.AccessReviewList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	now := time.Now()
	open := []kitchenv1alpha1.AccessReview{}
	for i := range list.Items {
		if list.Items[i].EffectivePhase(now) != kitchenv1alpha1.AccessReviewClosed {
			open = append(open, list.Items[i])
		}
	}
	return open, nil
}

// overlaps reports whether an existing cycle covers any of the grants a new
// one would. Two `all` cycles overlap; so do an `all` and anything; two
// project cycles over different projects do not.
func overlaps(
	existing *kitchenv1alpha1.AccessReview,
	scope kitchenv1alpha1.AccessReviewScope,
	project string,
) bool {
	if existing.Spec.Scope == kitchenv1alpha1.AccessReviewAll || scope == kitchenv1alpha1.AccessReviewAll {
		return true
	}
	if existing.Spec.Scope != scope {
		return false
	}
	if scope == kitchenv1alpha1.AccessReviewProject {
		return existing.Spec.ProjectRef != nil && existing.Spec.ProjectRef.Name == project
	}
	return true
}

func describeScope(scope kitchenv1alpha1.AccessReviewScope, project string) string {
	switch scope {
	case kitchenv1alpha1.AccessReviewPlatform:
		return "the platform's operators"
	case kitchenv1alpha1.AccessReviewProject:
		return "project " + project
	default:
		return "every grant on the installation"
	}
}

// resolveReviewers turns the request's names into access subjects. Empty
// means the platform's operators, which is who the cadence asks and the only
// accounts that can see every grant in the first place.
func (s *Server) resolveReviewers(
	names []string, kitchen *kitchenv1alpha1.Kitchen,
) ([]kitchenv1alpha1.AccessSubject, error) {
	trimmed := make([]string, 0, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			trimmed = append(trimmed, name)
		}
	}
	if len(trimmed) == 0 {
		if kitchen == nil || len(kitchen.Spec.Access.Operators) == 0 {
			return nil, fmt.Errorf("this platform names no operators, so there is nobody to review it: " +
				"name reviewers explicitly, or set spec.access.operators")
		}
		return kitchen.Spec.Access.Operators, nil
	}
	reviewers := make([]kitchenv1alpha1.AccessSubject, 0, len(trimmed))
	for _, name := range trimmed {
		reviewer := kitchenv1alpha1.AccessSubject{Subject: name}
		if access.IsEmailSubject(name) {
			reviewer.Email = name
		}
		reviewers = append(reviewers, reviewer)
	}
	return reviewers, nil
}

// listAccessReviews handles GET /api/v1/access/reviews — the register. Open
// cycles by default; ?historical=true adds the closed ones, because the
// register's history is the point of retaining them.
func (s *Server) listAccessReviews(w http.ResponseWriter, req *http.Request) {
	historical, err := boolParam(req, "historical")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	list := &kitchenv1alpha1.AccessReviewList{}
	if err := s.Client.List(req.Context(), list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	now := time.Now()
	views := []accessReviewView{}
	for i := range list.Items {
		review := &list.Items[i]
		if !historical && review.EffectivePhase(now) == kitchenv1alpha1.AccessReviewClosed {
			continue
		}
		views = append(views, newAccessReviewView(review))
	}
	// Newest first: the register's job is to answer "where does this stand",
	// and the cycle that is open — or was, last — is the one to look at.
	sort.Slice(views, func(i, j int) bool {
		if !views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].CreatedAt.After(views[j].CreatedAt)
		}
		return views[i].Name < views[j].Name
	})
	writeList(w, views)
}

// getAccessReview handles GET /api/v1/access/reviews/{name}.
func (s *Server) getAccessReview(w http.ResponseWriter, req *http.Request) {
	review := &kitchenv1alpha1.AccessReview{}
	if err := s.get(req.Context(), req.PathValue("name"), review); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newAccessReviewView(review))
}

// accessDecisionRequest is one decision about one grant.
type accessDecisionRequest struct {
	Subject string `json:"subject"`
	// Grant is `platform` or the project's name — the pair identifies the
	// entry, because one account holding a role on four projects is four
	// decisions.
	Grant    string `json:"grant"`
	Decision string `json:"decision"`
	Note     string `json:"note,omitempty"`
}

// reviewAccessRequest records decisions and, optionally, closes the cycle.
type reviewAccessRequest struct {
	Decisions []accessDecisionRequest `json:"decisions,omitempty"`
	// Close ends the cycle: the revocations are carried out and the artefact
	// is minted, both by the reconciler.
	Close bool `json:"close,omitempty"`
}

// reviewAccess handles PATCH /api/v1/access/reviews/{name}: record decisions,
// close the cycle, or both in one request.
//
// Both in one request is the ordinary case and the reason this is one route
// rather than two: a reviewer works through the list and closes it, and a
// close that raced the last decision would produce an artefact missing it.
func (s *Server) reviewAccess(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	review := &kitchenv1alpha1.AccessReview{}
	if err := s.get(ctx, req.PathValue("name"), review); err != nil {
		s.writeError(w, err)
		return
	}

	body := reviewAccessRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if len(body.Decisions) == 0 && !body.Close {
		badRequest(w, `this endpoint records decisions and closes a cycle: `+
			`{"decisions": [{"subject": "…", "grant": "…", "decision": "confirm|revoke"}], "close": true}`)
		return
	}
	if review.Status.Phase == kitchenv1alpha1.AccessReviewClosed {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"access review %s is already closed: its artefact is minted and its decisions stand. "+
				"Open a new cycle rather than reopening this one", review.Name)})
		return
	}

	caller, _ := CallerFrom(ctx)
	decider := callerName(caller)
	entries := map[string]*kitchenv1alpha1.AccessReviewEntry{}
	for i := range review.Status.Entries {
		entry := &review.Status.Entries[i]
		entries[entry.Key()] = entry
	}

	now := metav1.Now()
	selfReviews := []string{}
	for _, decision := range body.Decisions {
		subject := strings.TrimSpace(decision.Subject)
		grant := strings.TrimSpace(decision.Grant)
		entry, ok := entries[kitchenv1alpha1.EntryKey(subject, grant)]
		if !ok {
			badRequest(w, "no grant of %q on %q is in this cycle's snapshot: a review decides about what "+
				"was true when it opened, and a grant made since is in the next cycle", subject, grant)
			return
		}
		verdict := kitchenv1alpha1.AccessDecision(strings.TrimSpace(decision.Decision))
		if verdict != kitchenv1alpha1.AccessConfirm && verdict != kitchenv1alpha1.AccessRevoke {
			badRequest(w, "decision %q is not confirm or revoke", decision.Decision)
			return
		}
		entry.Decision = verdict
		entry.DecidedBy = decider
		entry.DecidedAt = &now
		entry.Note = strings.TrimSpace(decision.Note)
		// The reviewer may be the reviewed, and it is recorded rather than
		// refused — see the file comment and docs/COMPLIANCE.md §11.3.
		entry.SelfReview = access.SubjectMatches(entry.Subject, caller.access())
		if entry.SelfReview {
			selfReviews = append(selfReviews, entry.Grant+"/"+entry.Subject)
		}
	}
	retally(review)

	if !s.recorded(w, req, accessDecisionTransition(review, body, decider, selfReviews)) {
		return
	}

	if body.Close {
		review.Status.Phase = kitchenv1alpha1.AccessReviewClosed
		review.Status.ClosedBy = decider
		review.Status.ClosedAt = &now
	}
	if err := s.Client.Status().Update(ctx, review); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("access review decided through the api", "review", review.Name,
		"decisions", len(body.Decisions), "selfReviews", len(selfReviews),
		"closed", body.Close, "caller", decider)
	writeJSON(w, http.StatusOK, newAccessReviewView(review))
}

// retally recounts the cycle's status from its entries, so the numbers are
// derived rather than incremented — an increment that ran twice would be a
// tally that disagreed with the list it summarizes.
func retally(review *kitchenv1alpha1.AccessReview) {
	var pending, confirmed, revoked, selfReviewed, orphaned int32
	for i := range review.Status.Entries {
		entry := &review.Status.Entries[i]
		switch entry.Decision {
		case kitchenv1alpha1.AccessConfirm:
			confirmed++
		case kitchenv1alpha1.AccessRevoke:
			revoked++
		default:
			pending++
		}
		if entry.SelfReview {
			selfReviewed++
		}
		if entry.Orphaned {
			orphaned++
		}
	}
	review.Status.Pending = pending
	review.Status.Confirmed = confirmed
	review.Status.Revoked = revoked
	review.Status.SelfReviewed = selfReviewed
	review.Status.Orphaned = orphaned
}

// accessDecisionTransition is the privileged record a batch of decisions
// appends before they are made — built apart from the recording so a test can
// hold it up to the light without a store.
//
// Every decision is in the details rather than a count of them, because "who
// confirmed grace's operator role in the March review" is the question this
// record exists to answer, and a tally cannot.
func accessDecisionTransition(
	review *kitchenv1alpha1.AccessReview,
	body reviewAccessRequest,
	decider string,
	selfReviews []string,
) audit.Transition {
	decisions := make([]map[string]any, 0, len(body.Decisions))
	for _, decision := range body.Decisions {
		decisions = append(decisions, map[string]any{
			"subject":  strings.TrimSpace(decision.Subject),
			"grant":    strings.TrimSpace(decision.Grant),
			"decision": strings.TrimSpace(decision.Decision),
			"note":     strings.TrimSpace(decision.Note),
		})
	}
	details := map[string]any{
		"review":    review.Name,
		"scope":     string(review.Spec.Scope),
		"decisions": decisions,
		"closed":    body.Close,
		"pending":   review.Status.Pending,
		"confirmed": review.Status.Confirmed,
		"revoked":   review.Status.Revoked,
	}
	if len(selfReviews) > 0 {
		// Recorded, not refused: the record is where a self-review is
		// answered for.
		details["selfReviewed"] = selfReviews
	}

	reason := fmt.Sprintf("access review %s: %d decision(s) recorded by %s",
		review.Name, len(body.Decisions), decider)
	to := string(kitchenv1alpha1.AccessReviewOpen)
	if body.Close {
		reason = fmt.Sprintf(
			"access review %s closed by %s: %d confirmed, %d revoked, %d left undecided",
			review.Name, decider, review.Status.Confirmed, review.Status.Revoked, review.Status.Pending)
		to = string(kitchenv1alpha1.AccessReviewClosed)
	}
	return audit.Transition{
		Object:      review,
		Kind:        audit.KindAccessReview,
		Operation:   clickhouse.AuditUpdate,
		Privileged:  audit.PrivilegeAccess,
		Correlation: review.Name,
		From:        string(review.Status.Phase),
		To:          to,
		Reason:      reason,
		Details:     details,
	}
}
