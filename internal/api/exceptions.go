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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The break-glass surface: requesting an exception, reading the register, and
// resolving a grant that has served its purpose.
//
// The approval model is v1-honest and worth stating plainly. An API call has
// one caller, so creation carries both halves: the caller is the requester,
// and the body names the approver. The platform verifies two things about the
// approver — that they are somebody else (the CEL rule on the CRD refuses
// approver == requester too), and that they hold the role the escalation
// ladder demands for the requested duration. What the platform does NOT
// verify is that the approver actually said yes: that assertion is the
// requester's, made on a permanent, privileged record with both names on it,
// visible in the register and on every artifact the exception carries. A
// falsely named approver is a false statement in an audit log — which is the
// deterrent this model runs on until an approval flow of its own exists.

// createExceptionRequest asks for one break-glass exception.
type createExceptionRequest struct {
	Environment string `json:"environment"`
	// Release optionally narrows the grant to one release; empty covers the
	// whole environment.
	Release string `json:"release,omitempty"`
	// RuleIDs names the rules waived — specific rules, never a blanket.
	RuleIDs []string `json:"ruleIDs"`
	Reason  string   `json:"reason"`
	// ApprovedBy names the second human, by subject or verified address.
	ApprovedBy  string `json:"approvedBy"`
	IncidentRef string `json:"incidentRef,omitempty"`
	// ExpiresAt bounds the grant, RFC 3339.
	ExpiresAt    time.Time `json:"expiresAt"`
	AutoRollback bool      `json:"autoRollback,omitempty"`
}

// approverCaller reads an approver identity the way every access entry is
// read: a subject containing "@" is an email address, and it is treated as
// verified because the entry to match it against demands a verified one —
// what is being asked is "what role do the grants give this identity", not
// "is this token honest".
func approverCaller(identity string) access.Caller {
	if access.IsEmailSubject(identity) {
		return access.Caller{Email: identity, EmailVerified: true}
	}
	return access.Caller{Subject: identity}
}

// approverHolds answers whether the named approver holds the ladder's rung:
// developer and admin are project roles, operator is the platform's.
func approverHolds(
	required kitchenv1alpha1.ExceptionApproverRole,
	approver access.Caller,
	kitchen *kitchenv1alpha1.Kitchen,
	project *kitchenv1alpha1.Project,
) bool {
	switch required {
	case kitchenv1alpha1.ExceptionApproverOperator:
		return access.PlatformRoleFor(approver, kitchen).AtLeast(access.PlatformOperator)
	case kitchenv1alpha1.ExceptionApproverAdmin:
		return access.ProjectRoleFor(approver, kitchen, project).AtLeast(access.ProjectAdmin)
	default:
		return access.ProjectRoleFor(approver, kitchen, project).AtLeast(access.ProjectDeveloper)
	}
}

// exceptionTransition is the privileged audit record a granted exception
// appends before the object exists — built apart from the recording so a
// test can hold it up to the light without a store.
func exceptionTransition(
	exception *kitchenv1alpha1.Exception, required kitchenv1alpha1.ExceptionApproverRole,
) audit.Transition {
	details := map[string]any{
		"privileged":   true,
		"environment":  exception.Spec.EnvironmentRef.Name,
		"ruleIDs":      exception.Spec.RuleIDs,
		"reason":       exception.Spec.Reason,
		"requestedBy":  exception.Spec.RequestedBy,
		"approvedBy":   exception.Spec.ApprovedBy,
		"expiresAt":    exception.Spec.ExpiresAt.UTC().Format(time.RFC3339),
		"requiredRole": string(required),
		"autoRollback": exception.Spec.AutoRollback,
	}
	if exception.Spec.ReleaseRef != nil {
		details["release"] = exception.Spec.ReleaseRef.Name
	}
	if exception.Spec.IncidentRef != "" {
		details["incidentRef"] = exception.Spec.IncidentRef
	}
	return audit.Transition{
		Object:    exception,
		Kind:      audit.KindException,
		Operation: clickhouse.AuditCreate,
		To:        string(kitchenv1alpha1.ExceptionActive),
		Project:   exception.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf(
			"break-glass exception granted: %s waived for %s until %s, requested by %s, approved by %s",
			strings.Join(exception.Spec.RuleIDs, ", "), exception.Spec.EnvironmentRef.Name,
			exception.Spec.ExpiresAt.UTC().Format(time.RFC3339),
			exception.Spec.RequestedBy, exception.Spec.ApprovedBy),
		Details: details,
	}
}

// createException handles POST /api/v1/projects/{name}/exceptions.
func (s *Server) createException(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := createExceptionRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Environment = strings.TrimSpace(body.Environment)
	body.Release = strings.TrimSpace(body.Release)
	body.Reason = strings.TrimSpace(body.Reason)
	body.ApprovedBy = strings.TrimSpace(body.ApprovedBy)
	body.IncidentRef = strings.TrimSpace(body.IncidentRef)

	rules := make([]string, 0, len(body.RuleIDs))
	for _, rule := range body.RuleIDs {
		if trimmed := strings.TrimSpace(rule); trimmed != "" {
			rules = append(rules, trimmed)
		}
	}
	switch {
	case body.Environment == "":
		badRequest(w, "environment is required: which environment's rules this exception waives")
		return
	case len(rules) == 0:
		badRequest(w, "ruleIDs must name at least one rule: an exception waives specific rules, never everything")
		return
	case body.Reason == "":
		badRequest(w, "reason is required: an exception without one is a bypass, which is what this object exists to replace")
		return
	case body.ApprovedBy == "":
		badRequest(w, "approvedBy is required: an exception takes two people")
		return
	case body.ExpiresAt.IsZero():
		badRequest(w, "expiresAt is required (RFC 3339): there is no unbounded exception")
		return
	case !body.ExpiresAt.After(time.Now()):
		badRequest(w, "expiresAt %s is not in the future: an exception that is born expired grants nothing",
			body.ExpiresAt.UTC().Format(time.RFC3339))
		return
	}

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, body.Environment, env); err != nil {
		badRequest(w, "environment %q does not exist", body.Environment)
		return
	}
	if env.Spec.ProjectRef.Name != project.Name {
		badRequest(w, "environment %q belongs to project %q, not %q",
			env.Name, env.Spec.ProjectRef.Name, project.Name)
		return
	}
	var releaseRef *kitchenv1alpha1.LocalObjectReference
	if body.Release != "" {
		release := &kitchenv1alpha1.Release{}
		if err := s.get(ctx, body.Release, release); err != nil {
			badRequest(w, "release %q does not exist", body.Release)
			return
		}
		if release.Spec.ProjectRef.Name != project.Name {
			badRequest(w, "release %q belongs to project %q, not %q",
				release.Name, release.Spec.ProjectRef.Name, project.Name)
			return
		}
		releaseRef = &kitchenv1alpha1.LocalObjectReference{Name: release.Name}
	}

	// Two people, always. The check here is against the caller — subject and
	// verified address both — so naming your own other identity does not
	// slip past the CEL rule, which only compares the two recorded strings.
	caller, _ := CallerFrom(ctx)
	if access.SubjectMatches(body.ApprovedBy, caller.access()) {
		badRequest(w, "approvedBy names the requester: an exception takes two people, "+
			"and the approver must be somebody other than whoever asks")
		return
	}

	// The escalation ladder: the longer the grant, the higher the approval.
	kitchen := kitchenFrom(ctx)
	var policy *kitchenv1alpha1.ExceptionPolicySpec
	if kitchen != nil {
		policy = kitchen.Spec.Compliance.Exceptions
	}
	duration := time.Until(body.ExpiresAt)
	required := policy.RequiredApproverRole(duration)
	if !approverHolds(required, approverCaller(body.ApprovedBy), kitchen, project) {
		badRequest(w, "an exception of %s needs an approver holding %s%s, and %q does not: "+
			"name an approver with the role, or ask for a shorter duration",
			duration.Round(time.Minute), required, ladderScope(required, project.Name), body.ApprovedBy)
		return
	}

	exception := &kitchenv1alpha1.Exception{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: project.Name + "-exc-",
			Namespace:    s.Namespace,
			Labels:       map[string]string{"kitchen.bermos.dev/project": project.Name},
			Annotations:  map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.ExceptionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: env.Name},
			ReleaseRef:     releaseRef,
			RuleIDs:        rules,
			Reason:         body.Reason,
			RequestedBy:    callerName(caller),
			ApprovedBy:     body.ApprovedBy,
			IncidentRef:    body.IncidentRef,
			ExpiresAt:      metav1.NewTime(body.ExpiresAt.UTC()),
			AutoRollback:   body.AutoRollback,
		},
	}
	if !s.recorded(w, req, exceptionTransition(exception, required)) {
		return
	}
	if err := s.Client.Create(ctx, exception); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("break-glass exception granted through the api", "exception", exception.Name,
		"project", project.Name, "environment", env.Name, "rules", rules,
		"approvedBy", body.ApprovedBy, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventExceptionGranted,
		Project:     project.Name,
		Environment: env.Name,
		Message: fmt.Sprintf("break-glass exception %s waives %s until %s",
			exception.Name, strings.Join(rules, ", "), exception.Spec.ExpiresAt.UTC().Format(time.RFC3339)),
		Actor: callerName(caller),
	})
	writeJSON(w, http.StatusCreated, newExceptionView(exception))
}

// ladderScope words where a required role is held, for the refusal.
func ladderScope(role kitchenv1alpha1.ExceptionApproverRole, project string) string {
	if role == kitchenv1alpha1.ExceptionApproverOperator {
		return " on the platform"
	}
	return " on " + project
}

// listExceptions handles GET /api/v1/exceptions — the register. Active
// grants by default; ?historical=true adds the expired and the resolved,
// because the register's history is the point of retaining them. ?project=
// and ?environment= narrow; visibility follows the caller's projects.
func (s *Server) listExceptions(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	historical, err := boolParam(req, "historical")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	project := strings.TrimSpace(req.URL.Query().Get("project"))
	if !s.visibleProject(w, req, project) {
		return
	}
	environment := strings.TrimSpace(req.URL.Query().Get("environment"))

	list := &kitchenv1alpha1.ExceptionList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}

	scope := scopeFrom(ctx)
	now := time.Now()
	views := []exceptionView{}
	for i := range list.Items {
		exception := &list.Items[i]
		if !scope.allows(exception.Spec.ProjectRef.Name) {
			continue
		}
		if project != "" && exception.Spec.ProjectRef.Name != project {
			continue
		}
		if environment != "" && exception.Spec.EnvironmentRef.Name != environment {
			continue
		}
		if !historical && exception.EffectivePhase(now) != kitchenv1alpha1.ExceptionActive {
			continue
		}
		views = append(views, newExceptionView(exception))
	}
	// Soonest to expire first: the register's job is to say what is standing
	// open, and the one about to lapse is the one to look at.
	sort.Slice(views, func(i, j int) bool {
		if !views[i].ExpiresAt.Equal(views[j].ExpiresAt) {
			return views[i].ExpiresAt.Before(views[j].ExpiresAt)
		}
		return views[i].Name < views[j].Name
	})
	writeList(w, views)
}

// getException handles GET /api/v1/exceptions/{name}.
func (s *Server) getException(w http.ResponseWriter, req *http.Request) {
	exception := &kitchenv1alpha1.Exception{}
	if err := s.get(req.Context(), req.PathValue("name"), exception); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newExceptionView(exception))
}

// resolveExceptionRequest ends a grant: the finding was fixed, the incident
// closed. Resolution is an act with a reason, and both go on the record.
type resolveExceptionRequest struct {
	Resolved bool   `json:"resolved"`
	Reason   string `json:"reason"`
}

// resolveException handles PATCH /api/v1/exceptions/{name}. Admin on the
// project (which an operator holds everywhere): granting needed two people,
// but ending a waiver only ever narrows what is allowed.
func (s *Server) resolveException(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	exception := &kitchenv1alpha1.Exception{}
	if err := s.get(ctx, req.PathValue("name"), exception); err != nil {
		s.writeError(w, err)
		return
	}

	body := resolveExceptionRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if !body.Resolved {
		badRequest(w, `the one change this endpoint makes is resolution: {"resolved": true, "reason": "..."}`)
		return
	}
	if body.Reason == "" {
		badRequest(w, "reason is required: a resolution is an auditable act, and the record says why")
		return
	}
	if exception.Status.Phase == kitchenv1alpha1.ExceptionResolved {
		writeJSON(w, http.StatusConflict, errorBody{
			Error: fmt.Sprintf("exception %s is already resolved", exception.Name)})
		return
	}

	caller, _ := CallerFrom(ctx)
	from := string(exception.EffectivePhase(time.Now()))
	if !s.recorded(w, req, audit.Transition{
		Object:    exception,
		Kind:      audit.KindException,
		Operation: clickhouse.AuditUpdate,
		From:      from,
		To:        string(kitchenv1alpha1.ExceptionResolved),
		Project:   exception.Spec.ProjectRef.Name,
		Reason:    fmt.Sprintf("exception %s resolved: %s", exception.Name, body.Reason),
		Details: map[string]any{
			"privileged":  true,
			"reason":      body.Reason,
			"environment": exception.Spec.EnvironmentRef.Name,
			"ruleIDs":     exception.Spec.RuleIDs,
			"usedBy":      exception.Status.UsedBy,
		},
	}) {
		return
	}

	exception.Status.Phase = kitchenv1alpha1.ExceptionResolved
	exception.Status.ResolvedBy = callerName(caller)
	exception.Status.ResolvedAt = &metav1.Time{Time: time.Now().UTC()}
	if err := s.Client.Status().Update(ctx, exception); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("exception resolved through the api", "exception", exception.Name,
		"project", exception.Spec.ProjectRef.Name, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventExceptionResolved,
		Project:     exception.Spec.ProjectRef.Name,
		Environment: exception.Spec.EnvironmentRef.Name,
		Message:     fmt.Sprintf("break-glass exception %s resolved: %s", exception.Name, body.Reason),
		Actor:       callerName(caller),
	})
	writeJSON(w, http.StatusOK, newExceptionView(exception))
}
