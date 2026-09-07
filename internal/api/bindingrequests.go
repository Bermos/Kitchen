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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// Asking to bind to an offering, and being answered (#495).
//
// An offering whose visibility is `request` admits nobody until the project
// that makes it says so, and this is the whole of how it says so. Three
// things follow from where the grant belongs, and they decide the shape of
// every route in this file:
//
//   - **The claim is the request.** There is no request object: a consumer
//     writes the ordinary service claim it would write anyway, and the claim
//     sits in phase PendingApproval with no Secret and no variable anywhere
//     until it is answered. That keeps approval a claim transition rather
//     than a second approval system beside Exception, which is for
//     suppressing a compliance finding under a named approver and would mean
//     something else entirely here.
//
//   - **The decision is the *providing* project's, so it is addressed under
//     the providing project.** `/projects/{name}/requests` is read by anyone
//     who may see that project and written by its admins — the same bar that
//     set the offering's visibility in the first place, because it is the
//     same grant, one consumer at a time. A route hung off the claim would
//     have been a route whose object belongs to one project and whose
//     authorization belongs to another, which is a thing nobody can read off
//     the table.
//
//   - **Asking again is the consumer's**, and it is the one verb on the
//     consumer's side: `POST /claims/{name}/request` puts a denied binding
//     back in front of the provider without the claim being deleted and
//     written afresh. Deleting it would work and would also drop everything
//     the record says about who asked for what and who refused it.
//
// A denial and a withdrawal are one state, `denied`, because they are one
// fact — this consumer is not admitted — and the reason says which it was.
// Withdrawing takes the address back: the claim's next reconcile removes
// every binding Secret it wrote, and the consumer's environments roll
// without the variables.

// bindingRequestView is one project's request to bind one offering, as the
// providing project's admins read it.
//
// It names the consumer project, the claim, and **the account that asked** —
// which is somebody else's person answered to this project. That is why the
// queue is `admin` and not a viewer's read: a decision made without knowing
// who is asking is not a decision, and the row is therefore answered to the
// people who have to make one and to nobody else. Nothing beyond those three
// crosses — not the consumer's repository, its members, its environments or
// its addresses.
type bindingRequestView struct {
	// Claim is the consumer's claim, which is what a decision addresses.
	Claim string `json:"claim"`
	// Project is the consumer: the project whose claim asked.
	Project string `json:"project"`
	// Offering is which of this project's offerings it asks for.
	Offering string `json:"offering"`
	// Visibility is that offering's visibility as it stands now, so a
	// reader can see why a request is waiting — or why one cannot be
	// decided, on an offering that is open to everybody.
	Visibility string `json:"visibility,omitempty"`
	// State is `requested`, `approved` or `denied`.
	State string `json:"state"`
	// Phase is the claim's own phase, which is what the request came to in
	// the end: an approved request whose offering has since moved names its
	// own trouble here rather than reading as bound.
	Phase string `json:"phase,omitempty"`

	RequestedBy string       `json:"requestedBy,omitempty"`
	RequestedAt *metav1.Time `json:"requestedAt,omitempty"`
	DecidedBy   string       `json:"decidedBy,omitempty"`
	DecidedAt   *metav1.Time `json:"decidedAt,omitempty"`
	// Reason is the decision's own words.
	Reason string `json:"reason,omitempty"`
	// Open marks a binding nobody approved because the offering was open to
	// every project when it bound. Closing the offering leaves these
	// binding, and denying one is how a consumer already through the door
	// is withdrawn.
	Open bool `json:"open,omitempty"`
}

// newBindingRequestView reads one claim's grant as a request row.
func newBindingRequestView(
	claim *kitchenv1alpha1.ResourceClaim,
	visibility kitchenv1alpha1.OfferingVisibility,
) bindingRequestView {
	cfg := claim.Service()
	grant := claim.ServiceGrant()
	if grant == nil {
		// Only reachable for a claim nobody has recorded a grant on, which
		// the list skips; the row still says what it is rather than
		// pretending to a state.
		grant = &kitchenv1alpha1.ClaimServiceGrant{}
	}
	view := bindingRequestView{
		Claim:      claim.Name,
		Project:    claim.Spec.ProjectRef.Name,
		Offering:   cfg.Offering,
		Visibility: string(visibility),
		State:      string(grant.State),
		Phase:      string(claim.Status.Phase),
		Open:       grant.Open,
		Reason:     grant.Reason,
	}
	if !grant.RequestedAt.IsZero() {
		at := grant.RequestedAt
		view.RequestedAt = &at
		view.RequestedBy = grant.RequestedBy
	}
	view.DecidedBy = grant.DecidedBy
	view.DecidedAt = grant.DecidedAt
	return view
}

// listBindingRequests handles GET /api/v1/projects/{name}/requests: every
// binding another project has asked this one for, decided or not.
//
// The decided ones stay in the list rather than dropping out of it, because
// the list is also where a grant is withdrawn: an approval nobody can find
// again is an approval nobody can take back. `?state=` narrows it to the
// requests waiting for somebody, which is what the pane asks for.
//
// It is `admin` on this project, like the decision beside it — see the row
// in policy.go for why reading a queue that carries somebody's name is not a
// viewer's read.
func (s *Server) listBindingRequests(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	provider := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), provider); err != nil {
		s.writeError(w, err)
		return
	}
	state := strings.TrimSpace(req.URL.Query().Get("state"))
	if state != "" && !knownGrantState(state) {
		badRequest(w, "state is one of %s (got %q)", strings.Join(grantStateNames(), ", "), state)
		return
	}

	claims, err := s.serviceClaims(ctx)
	if err != nil {
		s.writeError(w, err)
		return
	}
	views := []bindingRequestView{}
	for i := range claims {
		claim := &claims[i]
		cfg := claim.Service()
		if cfg.Project != provider.Name || claim.Spec.ProjectRef.Name == provider.Name {
			continue
		}
		if claim.ServiceGrant() == nil {
			// A claim the reconciler has not answered for yet, and every
			// claim on an offering that has always been open: neither has
			// ever been put to anybody, so neither is a request.
			continue
		}
		// An offering since withdrawn carries no visibility rather than the
		// default one a zero value would read as: it is not offered at all,
		// and the row should not claim it admits anybody by request.
		var visibility kitchenv1alpha1.OfferingVisibility
		if offering, offered := provider.Offering(cfg.Offering); offered {
			visibility = offering.Visibility()
		}
		view := newBindingRequestView(claim, visibility)
		if state != "" && view.State != state {
			continue
		}
		views = append(views, view)
	}
	// Oldest request first: the queue is answered from the top, and a
	// request that has been waiting longest is the one somebody should
	// answer next.
	sort.Slice(views, func(i, j int) bool {
		left, right := views[i], views[j]
		if left.RequestedAt != nil && right.RequestedAt != nil && !left.RequestedAt.Equal(right.RequestedAt) {
			return left.RequestedAt.Before(right.RequestedAt)
		}
		return left.Claim < right.Claim
	})
	writeList(w, views)
}

// decideBindingRequestRequest is the decision body: approve or deny, and the
// words that go on the record.
type decideBindingRequestRequest struct {
	// Decision is `approved` or `denied`.
	Decision string `json:"decision"`
	// Reason is required of a denial and optional of an approval. A refusal
	// the consumer cannot account for is a support ticket rather than a
	// decision, and the consumer reads this on its own claim.
	Reason string `json:"reason,omitempty"`
}

// decideBindingRequest handles PATCH /api/v1/projects/{name}/requests/{claim}:
// the providing project's admins answering one request.
//
// Approving writes the grant and nothing else — the claim's own reconcile
// resolves the address, exactly as it would have on an open offering, so
// there is one bind path and not two. Denying writes the grant and the
// reconcile takes back whatever the binding had: an approval withdrawn is a
// denial with a Secret to remove.
func (s *Server) decideBindingRequest(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	provider := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), provider); err != nil {
		s.writeError(w, err)
		return
	}
	body := decideBindingRequestRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	decision := kitchenv1alpha1.ServiceGrantState(strings.TrimSpace(body.Decision))
	if decision != kitchenv1alpha1.ServiceGrantApproved && decision != kitchenv1alpha1.ServiceGrantDenied {
		badRequest(w, `decision is "%s" or "%s" (got %q)`,
			kitchenv1alpha1.ServiceGrantApproved, kitchenv1alpha1.ServiceGrantDenied, body.Decision)
		return
	}
	reason := strings.TrimSpace(body.Reason)
	if decision == kitchenv1alpha1.ServiceGrantDenied && reason == "" {
		badRequest(w, "reason is required to refuse a binding: the consumer reads it on its own claim, and a "+
			"refusal nobody can account for is a support ticket rather than a decision")
		return
	}

	claim, offering, ok := s.requestedBinding(ctx, w, provider, req.PathValue("claim"))
	if !ok {
		return
	}
	// An open offering admits every project on the platform by its own
	// terms, so there is nothing here for a decision to settle. Saying so is
	// better than writing a grant that changes nothing: the way to refuse
	// one consumer of an open offering is to close the offering first.
	if offering.Visibility() == kitchenv1alpha1.OfferingOpen {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"offering %s/%s is open to every project on this platform, so %s binds it whatever is decided "+
				"here. Set its visibility to %s on the project first — the projects already bound stay "+
				"bound, and each one is then withdrawn on its own",
			provider.Name, offering.Name, claim.Spec.ProjectRef.Name, kitchenv1alpha1.OfferingRequest)})
		return
	}
	grant := claim.ServiceGrant()
	if grant != nil && grant.State == decision && !grant.Open {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"%s's binding to %s/%s is already %s", claim.Spec.ProjectRef.Name, provider.Name, offering.Name,
			decision)})
		return
	}

	caller, _ := CallerFrom(ctx)
	from := grantStateOf(grant)
	if !s.recorded(w, req, bindingDecisionTransition(claim, provider, offering, from, decision, reason)) {
		return
	}

	now := metav1.Time{Time: time.Now().UTC()}
	if claim.Status.Service == nil {
		claim.Status.Service = &kitchenv1alpha1.ClaimServiceStatus{}
	}
	if grant == nil {
		// Decided before the reconciler wrote the request down, which is a
		// race a fast admin can win. The request is still this claim, so
		// the record is completed rather than refused.
		grant = &kitchenv1alpha1.ClaimServiceGrant{RequestedAt: now}
		grant.RequestedBy = claim.Annotations[audit.RequestedByAnnotation]
	}
	grant.State = decision
	grant.DecidedBy = callerName(caller)
	grant.DecidedAt = &now
	grant.Reason = reason
	// Whatever this was, it is a decision now: an open offering's admission
	// that somebody has since closed and answered by hand stops reading as
	// one nobody made.
	grant.Open = false
	claim.Status.Service.Grant = grant
	if err := s.Client.Status().Update(ctx, claim); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("binding request decided", "claim", claim.Name, "consumer", claim.Spec.ProjectRef.Name,
		"offering", provider.Name+"/"+offering.Name, "decision", string(decision), "caller", callerName(caller))
	// The *consumer's* feed, and the claim's: the people waiting for this
	// answer are on the other side of it, and this is the entry that tells
	// them their application is about to gain an address or lose one.
	event := clickhouse.EventBindingApproved
	message := fmt.Sprintf("binding to offering %s/%s approved by %s",
		provider.Name, offering.Name, callerName(caller))
	if decision == kitchenv1alpha1.ServiceGrantDenied {
		event = clickhouse.EventBindingDenied
		message = fmt.Sprintf("binding to offering %s/%s refused by %s: %s",
			provider.Name, offering.Name, callerName(caller), reason)
	}
	s.Activity.Record(ctx, clickhouse.Event{
		Type:    event,
		Project: claim.Spec.ProjectRef.Name,
		Claim:   claim.Name,
		Message: message,
		Actor:   callerName(caller),
	})
	writeJSON(w, http.StatusOK, newBindingRequestView(claim, offering.Visibility()))
}

// requestBinding handles POST /api/v1/claims/{name}/request: the consumer
// asking again for a binding that was refused.
//
// It is the consumer's own developers' write, the same bar that created the
// claim, and it changes nothing but whose turn it is. The claim is not
// deleted and written afresh — which would work, and would also drop who
// asked for what and who refused it.
func (s *Server) requestBinding(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(ctx, req.PathValue("name"), claim); err != nil {
		s.writeError(w, err)
		return
	}
	if claim.Spec.Type != kitchenv1alpha1.ClaimTypeService {
		badRequest(w, "claim %s is a %s claim, and asks nobody for anything: a binding request is how a "+
			"service claim reaches an offering that admits consumers by request", claim.Name, claim.Spec.Type)
		return
	}
	cfg := claim.Service()
	provider := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, cfg.Project, provider); err != nil {
		s.writeError(w, err)
		return
	}
	offering, ok := provider.Offering(cfg.Offering)
	if !ok {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"project %s no longer offers %q, so there is nobody to ask", provider.Name, cfg.Offering)})
		return
	}
	if offering.Visibility() == kitchenv1alpha1.OfferingOpen {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"offering %s/%s is open to every project on this platform: there is nothing to ask for, and the "+
				"claim binds on its own", provider.Name, offering.Name)})
		return
	}
	grant := claim.ServiceGrant()
	if grant != nil && grant.State != kitchenv1alpha1.ServiceGrantDenied {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"this binding is already %s; asking again is for one that was refused", grant.State)})
		return
	}

	caller, _ := CallerFrom(ctx)
	if !s.recorded(w, req, audit.Transition{
		Object:    claim,
		Kind:      audit.KindResourceClaim,
		Operation: clickhouse.AuditUpdate,
		From:      grantStateOf(grant),
		To:        string(kitchenv1alpha1.ServiceGrantRequested),
		Project:   claim.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf("binding of project %s to offering %s/%s asked for again",
			claim.Spec.ProjectRef.Name, provider.Name, offering.Name),
		Details: map[string]any{
			"offeringProject": provider.Name,
			"offering":        offering.Name,
			"claim":           claim.Name,
		},
	}) {
		return
	}

	if claim.Status.Service == nil {
		claim.Status.Service = &kitchenv1alpha1.ClaimServiceStatus{}
	}
	claim.Status.Service.Grant = &kitchenv1alpha1.ClaimServiceGrant{
		State:       kitchenv1alpha1.ServiceGrantRequested,
		RequestedBy: callerName(caller),
		RequestedAt: metav1.Time{Time: time.Now().UTC()},
	}
	if err := s.Client.Status().Update(ctx, claim); err != nil {
		s.writeError(w, err)
		return
	}
	s.log().Info("binding requested again", "claim", claim.Name, "project", claim.Spec.ProjectRef.Name,
		"offering", provider.Name+"/"+offering.Name, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newBindingRequestView(claim, offering.Visibility()))
}

// bindingDecisionTransition is the audit record of one decision, built apart
// from the recording so a test can hold it up to the light without a store.
//
// It is the **providing** project's record: this is that project's admins
// exercising that project's grant, one consumer at a time, and it is their
// audit pack the decision has to be answerable from. Which consumer it was
// about is in the details, and the consumer learns of it on its own claim
// and in its own feed. It is classified `access` for the reason every other
// grant is: it is who may do what.
func bindingDecisionTransition(
	claim *kitchenv1alpha1.ResourceClaim,
	provider *kitchenv1alpha1.Project,
	offering kitchenv1alpha1.ServiceOffering,
	from string,
	decision kitchenv1alpha1.ServiceGrantState,
	reason string,
) audit.Transition {
	return audit.Transition{
		Object:     claim,
		Kind:       audit.KindResourceClaim,
		Operation:  clickhouse.AuditUpdate,
		Privileged: audit.PrivilegeAccess,
		From:       from,
		To:         string(decision),
		Project:    provider.Name,
		Reason: fmt.Sprintf("binding of project %s to offering %s/%s %s: %s",
			claim.Spec.ProjectRef.Name, provider.Name, offering.Name, decision, decisionWords(reason)),
		Details: map[string]any{
			"consumer": claim.Spec.ProjectRef.Name,
			"claim":    claim.Name,
			"offering": offering.Name,
			"decision": string(decision),
			"reason":   reason,
		},
	}
}

// requestedBinding is the claim a decision addresses, and the offering it
// asks for.
//
// A claim that is not a request of *this* project's offerings is a 404 and
// not a 403: the caller is an admin of the project they named, and what they
// asked for does not exist there.
func (s *Server) requestedBinding(
	ctx context.Context,
	w http.ResponseWriter,
	provider *kitchenv1alpha1.Project,
	name string,
) (*kitchenv1alpha1.ResourceClaim, kitchenv1alpha1.ServiceOffering, bool) {
	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(ctx, strings.TrimSpace(name), claim); err != nil {
		s.writeError(w, err)
		return nil, kitchenv1alpha1.ServiceOffering{}, false
	}
	cfg := claim.Service()
	if claim.Spec.Type != kitchenv1alpha1.ClaimTypeService || cfg.Project != provider.Name {
		writeJSON(w, http.StatusNotFound, errorBody{Error: fmt.Sprintf(
			"no binding request named %q was made of project %s's offerings", claim.Name, provider.Name)})
		return nil, kitchenv1alpha1.ServiceOffering{}, false
	}
	if claim.Spec.ProjectRef.Name == provider.Name {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"claim %s is project %s binding its own offering, which needs nobody's approval",
			claim.Name, provider.Name)})
		return nil, kitchenv1alpha1.ServiceOffering{}, false
	}
	offering, ok := provider.Offering(cfg.Offering)
	if !ok {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"project %s no longer offers %q, so there is nothing to admit %s to",
			provider.Name, cfg.Offering, claim.Spec.ProjectRef.Name)})
		return nil, kitchenv1alpha1.ServiceOffering{}, false
	}
	return claim, offering, true
}

// grantStateNames is the vocabulary a `?state=` filter takes.
func grantStateNames() []string {
	return []string{
		string(kitchenv1alpha1.ServiceGrantRequested),
		string(kitchenv1alpha1.ServiceGrantApproved),
		string(kitchenv1alpha1.ServiceGrantDenied),
	}
}

func knownGrantState(state string) bool {
	for _, known := range grantStateNames() {
		if state == known {
			return true
		}
	}
	return false
}

// grantStateOf is a grant's state, and "unrecorded" for a claim nobody has
// written one on — which is what an audit record's `from` says.
func grantStateOf(grant *kitchenv1alpha1.ClaimServiceGrant) string {
	if grant == nil {
		return unrecordedGrant
	}
	return string(grant.State)
}

// unrecordedGrant is what a claim with no grant on it reads as, in a record
// that has to say what the decision moved from.
const unrecordedGrant = "unrecorded"

// decisionWords is the reason a decision carries, and a sentence in its place
// for an approval that gave none.
func decisionWords(reason string) string {
	if reason == "" {
		return "no reason given"
	}
	return reason
}
