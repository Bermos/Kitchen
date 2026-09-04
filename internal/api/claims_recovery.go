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
	"regexp"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// Point-in-time recovery for a claim, where the provider can actually do it
// (#247).
//
// Four routes for two operations, and the split between them is the design:
//
//   - **Recover** — `POST` and `DELETE` on `/claims/{name}/recoveries` — is
//     cheap and reversible. It makes a *sibling* database holding the claim's
//     data as it was at a moment, with a binding of its own, and touches
//     nothing the application is reading. Getting the timestamp wrong costs
//     another recovery, not the database, so it is the developer's.
//   - **Promote** — `POST /claims/{name}/recoveries/{id}/promote` — makes the
//     sibling the claim's binding, and every environment reading the claim
//     rolls onto it. That is a destructive write over a database that may be
//     production, so it takes the treatment the house rules prescribe: the
//     project `admin`'s, the same role `deletionPolicy: Delete` needs and the
//     same role that may delete the whole project; the dashboard confirms it
//     by typing the claim's name; the operator finishes it, so the route
//     answers `202`; and it lands in the audit log.
//
// The window is never accepted from the caller and never declared on the
// claim: it is read off the provider by the reconciler and reported on the
// status, and this surface refuses a timestamp outside it with the window in
// the refusal.

// recoveryName is the shape a recovery may be named: a DNS label, because the
// name reaches a Secret and a provider-side database.
var recoveryName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// maxRecoveryName keeps the derived Secret and branch names inside every
// budget they land in.
const maxRecoveryName = 40

// createRecoveryRequest asks for one recovery. The timestamp is the whole of
// it; the name is optional and derived from the timestamp when it is absent,
// because "the state at 14:05" is what somebody has in mind and not a name
// they have thought of.
type createRecoveryRequest struct {
	// At is the moment to recover to, RFC 3339. It must be inside the window
	// the claim reports.
	At string `json:"at"`
	// Name identifies the recovery on the claim. Absent derives one from the
	// timestamp.
	Name string `json:"name,omitempty"`
}

// recoveryWindowView is what the provider says it can reach back to. Absent
// where it cannot say — an absence, never a zero-length window standing in
// for one.
type recoveryWindowView struct {
	Earliest   time.Time `json:"earliest"`
	Latest     time.Time `json:"latest"`
	ObservedAt time.Time `json:"observedAt"`
}

// recoveryView is one recovered sibling database.
type recoveryView struct {
	Name string    `json:"name"`
	At   time.Time `json:"at"`
	// Phase is Pending, Ready or Failed; Message carries the provider's own
	// words for a Failed one.
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	// Secret is the recovery's own binding secret — a name, never its
	// contents, like every other binding this API answers with.
	Secret string `json:"secret,omitempty"`
	// Provenance and DataClass are what the recovery carries: a recovery of a
	// production database is production data at an earlier moment, and it
	// inherits the claim's classification because it is a new place the same
	// data lives.
	Provenance string     `json:"provenance,omitempty"`
	DataClass  string     `json:"dataClass,omitempty"`
	CreatedAt  time.Time  `json:"createdAt,omitempty"`
	PromotedAt *time.Time `json:"promotedAt,omitempty"`
	// Promoted says this recovery is the claim's binding now.
	Promoted bool `json:"promoted,omitempty"`
}

// retainedView is a database a promote displaced and did not destroy.
type retainedView struct {
	// Recovery is what was displaced, empty when it was the claim's original
	// database — which is still there under the claim's instanceName.
	Recovery    string    `json:"recovery,omitempty"`
	ID          string    `json:"id,omitempty"`
	DisplacedBy string    `json:"displacedBy"`
	At          time.Time `json:"at"`
}

// recoveriesView is the whole answer of GET /claims/{name}/recoveries: what
// the claim can be recovered to, and what it has been.
//
// It is one object rather than a bare list because the list is meaningless
// without the window — a recovery screen with no window is a date picker over
// a span nobody has confirmed, which is the thing this feature exists not to
// be.
type recoveriesView struct {
	Claim string `json:"claim"`
	// Available says whether this claim can be recovered at all, and Reason
	// says why not — the provider's own account, so that a claim through a
	// provider that cannot do this says which provider and why rather than
	// showing a disabled button.
	Available  bool                `json:"available"`
	Reason     string              `json:"reason,omitempty"`
	Window     *recoveryWindowView `json:"window,omitempty"`
	Recoveries []recoveryView      `json:"recoveries"`
	Retained   []retainedView      `json:"retained,omitempty"`
	// Promoted names the recovery the claim currently binds, empty for a
	// claim bound to its own database.
	Promoted string `json:"promoted,omitempty"`
}

func newRecoveriesView(claim *kitchenv1alpha1.ResourceClaim) recoveriesView {
	view := recoveriesView{
		Claim:      claim.Name,
		Recoveries: []recoveryView{},
		Promoted:   claim.Spec.PromotedRecovery,
	}
	status := claim.Status.Recovery
	if status == nil {
		// Nothing has looked yet. Unavailable with an honest reason beats an
		// empty object that reads like a refusal.
		view.Reason = "the platform has not yet asked this claim's provider what it can recover to"
		return view
	}
	view.Available = status.Available
	view.Reason = status.Reason
	if window := status.Window; window != nil {
		view.Window = &recoveryWindowView{
			Earliest:   window.Earliest.Time,
			Latest:     window.Latest.Time,
			ObservedAt: window.ObservedAt.Time,
		}
	}
	for _, recovery := range status.Recoveries {
		item := recoveryView{
			Name:       recovery.Name,
			At:         recovery.At.Time,
			Phase:      recovery.Phase,
			Message:    recovery.Message,
			Secret:     recovery.SecretName,
			Provenance: recovery.Provenance,
			DataClass:  string(recovery.DataClass),
			CreatedAt:  recovery.CreatedAt.Time,
		}
		if recovery.PromotedAt != nil {
			at := recovery.PromotedAt.Time
			item.PromotedAt = &at
			item.Promoted = true
		}
		view.Recoveries = append(view.Recoveries, item)
	}
	for _, retained := range status.Retained {
		view.Retained = append(view.Retained, retainedView{
			Recovery:    retained.Recovery,
			ID:          retained.ID,
			DisplacedBy: retained.DisplacedBy,
			At:          retained.At.Time,
		})
	}
	return view
}

func (s *Server) listClaimRecoveries(w http.ResponseWriter, req *http.Request) {
	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(req.Context(), req.PathValue("name"), claim); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newRecoveriesView(claim))
}

func (s *Server) createClaimRecovery(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(ctx, req.PathValue("name"), claim); err != nil {
		s.writeError(w, err)
		return
	}
	body := createRecoveryRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	at, ok := parseRecoveryMoment(w, body.At)
	if !ok {
		return
	}
	status := claim.Status.Recovery
	if status == nil || !status.Available {
		reason := "the platform has not yet asked this claim's provider what it can recover to"
		if status != nil && status.Reason != "" {
			reason = status.Reason
		}
		badRequest(w, "claim %s cannot be recovered to a point in time: %s", claim.Name, reason)
		return
	}
	if window := status.Window; window == nil ||
		at.Before(window.Earliest.Time) || at.After(window.Latest.Time) {
		badRequest(w, "%s is outside what this claim can be recovered to: %s",
			at.UTC().Format(time.RFC3339), describeWindow(status.Window))
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		// Derived from the moment, because that is what somebody has in mind:
		// "the state at 14:05" rather than a name they have to invent.
		name = "at-" + at.UTC().Format("2006-01-02t1504")
	}
	if len(name) > maxRecoveryName || !recoveryName.MatchString(name) {
		badRequest(w, "%q is not a recovery name: lowercase letters, digits and dashes, at most %d characters",
			name, maxRecoveryName)
		return
	}
	for _, existing := range claim.Spec.Recoveries {
		if existing.Name == name {
			writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
				"claim %s already has a recovery called %s, holding its data as at %s. Ask for another name, "+
					"or discard that one first", claim.Name, name,
				existing.At.Time.UTC().Format(time.RFC3339))})
			return
		}
	}

	caller, _ := CallerFrom(ctx)
	claim.Spec.Recoveries = append(claim.Spec.Recoveries, kitchenv1alpha1.ClaimRecoveryRequest{
		Name: name,
		At:   metav1.NewTime(at),
	})
	if !s.recorded(w, req, audit.Transition{
		Object:    claim,
		Kind:      audit.KindResourceClaim,
		Operation: clickhouse.AuditUpdate,
		Project:   claim.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf("recovery %s of claim %s requested, holding its data as at %s",
			name, claim.Name, at.UTC().Format(time.RFC3339)),
		Details: map[string]any{"recovery": name, "at": at.UTC().Format(time.RFC3339)},
	}) {
		return
	}
	if err := s.Client.Update(ctx, claim); err != nil {
		s.writeError(w, err)
		return
	}
	s.log().Info("claim recovery requested through the api",
		"claim", claim.Name, "recovery", name, "at", at, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventClaimCreated,
		Project: claim.Spec.ProjectRef.Name,
		Claim:   claim.Name,
		Message: fmt.Sprintf("recovery %s requested: %s as it was at %s", name, claim.Name,
			at.UTC().Format(time.RFC3339)),
		Actor: callerName(caller),
	})
	// 202: the operator makes the sibling database and writes its binding
	// after this response has gone out.
	writeJSON(w, http.StatusAccepted, newRecoveriesView(claim))
}

// promoteClaimRecovery cuts the claim's binding over to a recovery. It is the
// destructive half, and the whole of what it is answerable for is in
// mayDestroyData's sentence: admin, because a promote replaces the database
// every environment of the project is reading.
func (s *Server) promoteClaimRecovery(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(ctx, req.PathValue("name"), claim); err != nil {
		s.writeError(w, err)
		return
	}
	name := req.PathValue("recovery")
	recovery, ok := findRecovery(claim, name)
	if !ok {
		s.recoveryNotFound(w, claim, name)
		return
	}
	if claim.Spec.PromotedRecovery == name {
		writeJSON(w, http.StatusOK, newRecoveriesView(claim))
		return
	}
	if recovery.Phase == kitchenv1alpha1.ClaimRecoveryFailed {
		badRequest(w, "recovery %s of claim %s could not be made, so there is nothing to promote: %s",
			name, claim.Name, recovery.Message)
		return
	}
	if recovery.Phase != kitchenv1alpha1.ClaimRecoveryReady {
		badRequest(w, "recovery %s of claim %s is still being made; promote it once it is ready",
			name, claim.Name)
		return
	}

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, claim.Spec.ProjectRef.Name, project); err != nil {
		s.writeError(w, err)
		return
	}
	if !s.mayPromoteRecovery(ctx, w, project, claim) {
		return
	}

	caller, _ := CallerFrom(ctx)
	displaced := "the database this claim was provisioned with"
	if claim.Spec.PromotedRecovery != "" {
		displaced = "recovery " + claim.Spec.PromotedRecovery
	}
	claim.Spec.PromotedRecovery = name
	if !s.recorded(w, req, audit.Transition{
		Object:    claim,
		Kind:      audit.KindResourceClaim,
		Operation: clickhouse.AuditUpdate,
		Project:   claim.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf("recovery %s promoted over %s: claim %s now binds its data as at %s, and what "+
			"it displaced is retained", name, displaced, claim.Name,
			recovery.At.Time.UTC().Format(time.RFC3339)),
		Details: map[string]any{
			"recovery":  name,
			"at":        recovery.At.Time.UTC().Format(time.RFC3339),
			"displaced": displaced,
		},
	}) {
		return
	}
	if err := s.Client.Update(ctx, claim); err != nil {
		s.writeError(w, err)
		return
	}
	s.log().Info("claim recovery promoted through the api",
		"claim", claim.Name, "recovery", name, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventClaimBound,
		Project: claim.Spec.ProjectRef.Name,
		Claim:   claim.Name,
		Message: fmt.Sprintf("recovery %s promoted: %s now binds its data as it was at %s", name,
			claim.Name, recovery.At.Time.UTC().Format(time.RFC3339)),
		Actor: callerName(caller),
	})
	// 202: the operator rewrites the binding and the environments reading it
	// roll after this response has gone out.
	writeJSON(w, http.StatusAccepted, newRecoveriesView(claim))
}

func (s *Server) deleteClaimRecovery(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(ctx, req.PathValue("name"), claim); err != nil {
		s.writeError(w, err)
		return
	}
	name := req.PathValue("recovery")
	if _, ok := findRequestedRecovery(claim, name); !ok {
		s.recoveryNotFound(w, claim, name)
		return
	}
	if claim.Spec.PromotedRecovery == name {
		// Discarding the database the application is reading is not a
		// discard, and there is no unpromote: promote something else first,
		// which is itself the admin's.
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"recovery %s is what claim %s binds now, so discarding it would take the application's database "+
				"away. Promote another recovery first", name, claim.Name)})
		return
	}

	caller, _ := CallerFrom(ctx)
	kept := make([]kitchenv1alpha1.ClaimRecoveryRequest, 0, len(claim.Spec.Recoveries))
	for _, request := range claim.Spec.Recoveries {
		if request.Name != name {
			kept = append(kept, request)
		}
	}
	claim.Spec.Recoveries = kept
	if !s.recorded(w, req, audit.Transition{
		Object:    claim,
		Kind:      audit.KindResourceClaim,
		Operation: clickhouse.AuditUpdate,
		Project:   claim.Spec.ProjectRef.Name,
		Reason:    fmt.Sprintf("recovery %s of claim %s discarded", name, claim.Name),
		Details:   map[string]any{"recovery": name},
	}) {
		return
	}
	if err := s.Client.Update(ctx, claim); err != nil {
		s.writeError(w, err)
		return
	}
	s.log().Info("claim recovery discarded through the api",
		"claim", claim.Name, "recovery", name, "caller", callerName(caller))
	// 202: the operator removes the recovered database and its binding after
	// this response has gone out.
	writeJSON(w, http.StatusAccepted, newRecoveriesView(claim))
}

// mayPromoteRecovery is the escalation on this surface, and it is the same
// shape mayDestroyData is: the route table's row is the floor — recovering is
// the developer's day job — and promoting is the ceiling, because it replaces
// the database every environment of the project reads.
//
// It is a handler condition rather than a route row for the opposite reason
// to deletionPolicy's: this one *is* a whole route, but the route table
// cannot express "admin on the claim's project" for a route whose row already
// resolves the project, and the two would then disagree about which of them
// was in charge. Stating it here keeps one answer, and the refusal names the
// role it wants like every other on this API.
func (s *Server) mayPromoteRecovery(
	ctx context.Context,
	w http.ResponseWriter,
	project *kitchenv1alpha1.Project,
	claim *kitchenv1alpha1.ResourceClaim,
) bool {
	role := s.roleOn(ctx, project)
	if role.AtLeast(access.ProjectAdmin) {
		return true
	}
	held := role.String()
	if held == "" {
		held = "no role"
	}
	forbidden(w, fmt.Sprintf("you have %s on %s; promoting a recovery needs admin: it replaces the database "+
		"every environment of this project reads, and the one it displaces is kept but no longer bound",
		held, claim.Spec.ProjectRef.Name))
	return false
}

// recoveryNotFound answers a recovery this claim does not have, naming the
// ones it does — the refusal that is useful at three in the morning.
func (s *Server) recoveryNotFound(w http.ResponseWriter, claim *kitchenv1alpha1.ResourceClaim, name string) {
	names := make([]string, 0, len(claim.Spec.Recoveries))
	for _, request := range claim.Spec.Recoveries {
		names = append(names, request.Name)
	}
	has := "it has none"
	if len(names) > 0 {
		has = "it has " + strings.Join(names, ", ")
	}
	writeJSON(w, http.StatusNotFound, errorBody{Error: fmt.Sprintf(
		"claim %s has no recovery called %s: %s", claim.Name, name, has)})
}

// findRecovery is the recovery as the reconciler recorded it — what it holds,
// and whether it is there yet.
func findRecovery(claim *kitchenv1alpha1.ResourceClaim, name string) (kitchenv1alpha1.ClaimRecovery, bool) {
	if claim.Status.Recovery == nil {
		return kitchenv1alpha1.ClaimRecovery{}, false
	}
	for _, recovery := range claim.Status.Recovery.Recoveries {
		if recovery.Name == name {
			return recovery, true
		}
	}
	return kitchenv1alpha1.ClaimRecovery{}, false
}

// findRequestedRecovery is the recovery as the claim asks for it, which is
// what a discard removes — a recovery asked for and not yet made is still one
// somebody may change their mind about.
func findRequestedRecovery(
	claim *kitchenv1alpha1.ResourceClaim,
	name string,
) (kitchenv1alpha1.ClaimRecoveryRequest, bool) {
	for _, request := range claim.Spec.Recoveries {
		if request.Name == name {
			return request, true
		}
	}
	return kitchenv1alpha1.ClaimRecoveryRequest{}, false
}

// parseRecoveryMoment reads the one field a recovery is: when. It is RFC 3339
// and nothing else — a timestamp the API guessed the format of is a recovery
// to the wrong moment.
func parseRecoveryMoment(w http.ResponseWriter, raw string) (time.Time, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		badRequest(w, "at is required: the moment to recover to, as RFC 3339 (2026-08-30T14:05:00Z)")
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		badRequest(w, "at must be an RFC 3339 timestamp (2026-08-30T14:05:00Z), got %q", raw)
		return time.Time{}, false
	}
	return at, true
}

// describeWindow says what the window is, in the sentence a refusal ends
// with.
func describeWindow(window *kitchenv1alpha1.ClaimRecoveryWindow) string {
	if window == nil {
		return "this claim's provider reports no window at all"
	}
	return fmt.Sprintf("its provider can reach back to %s, and no further forward than %s",
		window.Earliest.Time.UTC().Format(time.RFC3339), window.Latest.Time.UTC().Format(time.RFC3339))
}
