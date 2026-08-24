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
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/retention"
)

// The retention surface: how long the platform keeps each class of what it
// holds, and how far back each class actually goes.
//
// It is the operator's, like every other platform setting, and it is its own
// route rather than four more fields on `PATCH /settings` for two reasons.
// One is size: nine classes with a floor, an override and a per-class
// measurement is a screen, not a form field. The other is that this is the
// answer to a question somebody outside the platform asks — "what is your
// retention policy, and can you show me it is enforced" — and an answer with
// its own address can be fetched, exported and cited.
//
// The old `logRetentionDays` on `/settings` still works and still means what
// it meant: it writes spec.observability.clickhouse.retentionDays, which is
// the default every telemetry class inherits. Setting a class here overrides
// it for that class, and the view says which of the two each number came from.

// retentionClassView is one class as the dashboard and an export read it.
type retentionClassView struct {
	Class string `json:"class"`
	Label string `json:"label"`
	// Description is what the class holds, in one line.
	Description string `json:"description"`
	Days        int32  `json:"days"`
	// Source is where the number came from: `retention` when somebody set
	// this class, or the legacy field it inherits.
	Source string `json:"source"`
	// Enforced is whether the store is holding the retention, and the rest is
	// the last sweep's measurement — absent until one has run.
	Enforced bool       `json:"enforced"`
	Rows     int64      `json:"rows,omitempty"`
	Oldest   *time.Time `json:"oldest,omitempty"`
	Expired  int64      `json:"expired,omitempty"`
	Removed  int64      `json:"removed,omitempty"`
	Message  string     `json:"message,omitempty"`
}

// retentionOverrideView is the written decision behind an audit retention
// under the floor. It is not a credential and is read back in full — the whole
// value of the field is that somebody outside the platform can see who signed
// off on keeping less evidence.
type retentionOverrideView struct {
	Reason     string `json:"reason"`
	ApprovedBy string `json:"approvedBy"`
}

// retentionView is the whole model, plus the floor it is judged against.
type retentionView struct {
	Classes []retentionClassView `json:"classes"`

	// AuditFloorDays is the documented minimum for the audit class. It is
	// served rather than assumed by the client: a dashboard that hard-coded
	// 90 would be a second copy of a number this API is the source of.
	AuditFloorDays int32 `json:"auditFloorDays"`

	// AuditFloorOverridden and AuditFloorOverride are the state of the
	// escape hatch.
	AuditFloorOverridden bool                   `json:"auditFloorOverridden"`
	AuditFloorOverride   *retentionOverrideView `json:"auditFloorOverride,omitempty"`

	// LastSweep is when the retention sweep last measured all of this.
	LastSweep *time.Time `json:"lastSweep,omitempty"`

	// Message explains a model that is configured and not being enforced.
	Message string `json:"message,omitempty"`
}

// newRetentionView reads the singleton into the answer. The configured half
// comes from the spec through the same resolver the operator uses — one model,
// not a second reading of it — and the measured half from the status the sweep
// publishes.
func newRetentionView(kitchen *kitchenv1alpha1.Kitchen) retentionView {
	model := retention.Resolve(kitchen)

	measured := map[string]kitchenv1alpha1.RetentionClassStatus{}
	view := retentionView{
		AuditFloorDays:       retention.AuditFloorDays,
		AuditFloorOverridden: model.AuditBelowFloor(),
	}
	if status := kitchen.Status.Retention; status != nil {
		view.Message = status.Message
		if status.LastSweep != nil {
			swept := status.LastSweep.Time
			view.LastSweep = &swept
		}
		for _, entry := range status.Classes {
			measured[entry.Class] = entry
		}
	}
	if override := kitchen.Spec.Retention.AuditFloorOverride; override != nil {
		view.AuditFloorOverride = &retentionOverrideView{
			Reason:     override.Reason,
			ApprovedBy: override.ApprovedBy,
		}
	}

	for _, definition := range retention.Definitions() {
		setting, ok := model.Setting(definition.Class)
		if !ok {
			continue
		}
		entry := retentionClassView{
			Class:       string(definition.Class),
			Label:       definition.Label,
			Description: definition.Description,
			Days:        setting.Days,
			Source:      setting.Source,
		}
		if seen, found := measured[string(definition.Class)]; found {
			entry.Enforced = seen.Enforced
			entry.Rows = seen.Rows
			entry.Expired = seen.Expired
			entry.Removed = seen.Removed
			entry.Message = seen.Message
			if seen.Oldest != nil {
				oldest := seen.Oldest.Time
				entry.Oldest = &oldest
			}
		}
		view.Classes = append(view.Classes, entry)
	}
	return view
}

func (s *Server) getRetention(w http.ResponseWriter, req *http.Request) {
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newRetentionView(kitchen))
}

// patchRetentionRequest changes one or more classes. Every field is a pointer
// so that a request which does not mention a class cannot disturb it — the
// dashboard sends the whole form, an operator's script sends one number, and
// both mean the same thing.
type patchRetentionRequest struct {
	ContainerLogs *int32 `json:"containerLogs"`
	BuildLogs     *int32 `json:"buildLogs"`
	Flows         *int32 `json:"flows"`
	Metrics       *int32 `json:"metrics"`
	Traces        *int32 `json:"traces"`
	Requests      *int32 `json:"requests"`
	ClusterEvents *int32 `json:"clusterEvents"`
	Activity      *int32 `json:"activity"`
	Audit         *int32 `json:"audit"`

	// AuditFloorOverride is the written decision to keep audit records for
	// less than the floor. A `null` that is present clears it, which is why
	// this is a pointer to a pointer in effect: `auditFloorOverride: null`
	// removes the override, and an absent key leaves it as it is.
	//
	// Clearing it while the audit retention is still under the floor is
	// refused, for the same reason setting the retention low without one is:
	// the object would be in a state admission will not accept, and answering
	// 400 here says which field to fix rather than handing back a webhook
	// message about a CEL rule.
	AuditFloorOverride *retentionOverrideRequest `json:"auditFloorOverride"`

	// ClearAuditFloorOverride removes the override explicitly. It exists
	// because JSON cannot distinguish "the key was absent" from "the key was
	// null" through a single pointer, and the difference matters on the one
	// field here whose removal is itself a decision.
	ClearAuditFloorOverride bool `json:"clearAuditFloorOverride"`
}

type retentionOverrideRequest struct {
	Reason     string `json:"reason"`
	ApprovedBy string `json:"approvedBy"`
}

// patchRetention changes the retention model.
//
// The floor is enforced here as well as by the CRD's own CEL rule, and that is
// not belt and braces: admission answers with a rule's message, and this
// answers with the field, the number, the floor and the way past it. A caller
// who set the audit retention to 30 should be told what to do about it by the
// thing they were talking to.
func (s *Server) patchRetention(w http.ResponseWriter, req *http.Request) {
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := patchRetentionRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	base := kitchen.DeepCopy()
	was := retention.Resolve(kitchen)

	if !s.applyRetentionDays(w, kitchen, body) {
		return
	}
	if !s.applyAuditOverride(w, kitchen, body) {
		return
	}

	now := retention.Resolve(kitchen)
	if refusal := retention.ValidateAudit(now.Days(retention.ClassAudit),
		kitchen.Spec.Retention.AuditFloorOverride); refusal != "" {
		badRequest(w, "%s", refusal)
		return
	}

	changed := changedRetentionClasses(was, now, base, kitchen)
	if len(changed) == 0 {
		writeJSON(w, http.StatusOK, newRetentionView(kitchen))
		return
	}

	transition := audit.Transition{
		Object:    kitchen,
		Kind:      audit.KindRetention,
		Operation: clickhouse.AuditUpdate,
		Reason:    "the platform's retention was changed: " + strings.Join(changed, ", "),
		Details: map[string]any{
			"change":  "retention",
			"classes": changed,
		},
	}
	// A write that puts the audit retention under the documented floor is
	// recorded as its own kind of change, loudly and with the reason and the
	// approver in the record. That is the whole meaning of "the override is
	// itself an audit record": the field cannot be set quietly.
	if override := kitchen.Spec.Retention.AuditFloorOverride; override != nil && now.AuditBelowFloor() {
		transition.Details["change"] = audit.ChangeAuditFloorOverride
		transition.Details["auditFloorOverride"] = map[string]any{
			"days":       now.Days(retention.ClassAudit),
			"floor":      retention.AuditFloorDays,
			"reason":     override.Reason,
			"approvedBy": override.ApprovedBy,
		}
		transition.Reason = fmt.Sprintf(
			"audit records will be kept for %d days, below the platform's %d-day floor, on %s's authority: %s",
			now.Days(retention.ClassAudit), retention.AuditFloorDays, override.ApprovedBy, override.Reason)
	}
	if !s.recorded(w, req, transition) {
		return
	}

	if err := s.Client.Patch(req.Context(), kitchen, client.MergeFrom(base)); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(req.Context())
	s.log().Info("the platform's retention was changed through the api",
		"classes", strings.Join(changed, ","), "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newRetentionView(kitchen))
}

// applyRetentionDays writes the per-class numbers, refusing anything under a
// day. Zero is not "keep nothing" and not "inherit" — it is a value nobody
// means, so it is refused rather than interpreted.
func (s *Server) applyRetentionDays(
	w http.ResponseWriter,
	kitchen *kitchenv1alpha1.Kitchen,
	body patchRetentionRequest,
) bool {
	spec := &kitchen.Spec.Retention
	for _, field := range []struct {
		name  string
		value *int32
		into  **int32
	}{
		{"containerLogs", body.ContainerLogs, &spec.ContainerLogs},
		{"buildLogs", body.BuildLogs, &spec.BuildLogs},
		{"flows", body.Flows, &spec.Flows},
		{"metrics", body.Metrics, &spec.Metrics},
		{"traces", body.Traces, &spec.Traces},
		{"requests", body.Requests, &spec.Requests},
		{"clusterEvents", body.ClusterEvents, &spec.ClusterEvents},
		{"activity", body.Activity, &spec.Activity},
		{"audit", body.Audit, &spec.Audit},
	} {
		if field.value == nil {
			continue
		}
		if *field.value < 1 {
			badRequest(w, "%s must be at least 1 day (got %d); there is no value meaning "+
				"\"keep nothing\", and omitting the field is how a class goes back to inheriting",
				field.name, *field.value)
			return false
		}
		days := *field.value
		*field.into = &days
	}
	return true
}

// applyAuditOverride writes or clears the floor override.
func (s *Server) applyAuditOverride(
	w http.ResponseWriter,
	kitchen *kitchenv1alpha1.Kitchen,
	body patchRetentionRequest,
) bool {
	if body.ClearAuditFloorOverride {
		kitchen.Spec.Retention.AuditFloorOverride = nil
		return true
	}
	if body.AuditFloorOverride == nil {
		return true
	}

	reason := strings.TrimSpace(body.AuditFloorOverride.Reason)
	approvedBy := strings.TrimSpace(body.AuditFloorOverride.ApprovedBy)
	if len(reason) < auditOverrideReasonMinimum {
		badRequest(w, "auditFloorOverride.reason must be at least %d characters: it is the answer somebody "+
			"gets when they ask why this installation keeps less evidence than the platform's floor, "+
			"and \"n/a\" is not an answer", auditOverrideReasonMinimum)
		return false
	}
	if approvedBy == "" {
		badRequest(w, "auditFloorOverride.approvedBy must name whoever decided it: an override with no "+
			"name against it is a decision nobody made")
		return false
	}
	kitchen.Spec.Retention.AuditFloorOverride = &kitchenv1alpha1.RetentionOverrideSpec{
		Reason:     reason,
		ApprovedBy: approvedBy,
	}
	return true
}

// auditOverrideReasonMinimum matches the CRD's MinLength, so the API refuses
// what admission would refuse and says so more usefully.
const auditOverrideReasonMinimum = 20

// changedRetentionClasses names what actually moved, for the audit record. A
// PATCH that sets a class to the number it already had changes nothing and is
// recorded as nothing — the log is for changes, and a no-op write that
// appended a record would be one more line between an auditor and the change
// they are looking for.
func changedRetentionClasses(
	was, now retention.Model,
	base, kitchen *kitchenv1alpha1.Kitchen,
) []string {
	changed := []string{}
	for _, class := range retention.Sorted(now.Classes()) {
		if was.Days(class) != now.Days(class) {
			changed = append(changed, fmt.Sprintf("%s %d→%d days", class, was.Days(class), now.Days(class)))
		}
	}
	if describeOverride(base.Spec.Retention.AuditFloorOverride) !=
		describeOverride(kitchen.Spec.Retention.AuditFloorOverride) {
		changed = append(changed, "auditFloorOverride")
	}
	return changed
}

func describeOverride(override *kitchenv1alpha1.RetentionOverrideSpec) string {
	if override == nil {
		return ""
	}
	return override.ApprovedBy + ": " + override.Reason
}
