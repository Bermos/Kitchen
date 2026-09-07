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
	"strings"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/signals"
)

// The signal policy: what this installation counts as worth hearing.
//
// It is the operator's and installation-wide, and that is the whole
// distinction it exists to draw. `/alerts → Routing` is *who hears about it*,
// and a project edits its own; this is *what counts as worth hearing*, it is
// the floor the compliance posture reads, and nobody but an operator may move
// it.
//
// It is its own route rather than six more fields on `PATCH /settings` for the
// reason the retention model is: a set of numbers with three named presets
// behind them and a ladder to explain is a screen, not a form field — and this
// is the answer to a question somebody outside the platform asks, "what did
// you consider an incident at the time", which an answer with its own address
// can be fetched, exported and cited for.

// signalPolicyValues is one policy's numbers, and is shared by the current
// setting and by each preset offered beside it. The two must be the same shape
// or a screen cannot show what choosing a preset would do.
type signalPolicyValues struct {
	CorrelatedProjects       int  `json:"correlatedProjects"`
	CorrelationWindowMinutes int  `json:"correlationWindowMinutes"`
	EscalationWindowMinutes  int  `json:"escalationWindowMinutes"`
	UntendedMultiple         int  `json:"untendedMultiple"`
	MaxSilenceHours          int  `json:"maxSilenceHours"`
	Paging                   bool `json:"paging"`
}

func valuesOf(policy signals.Policy) signalPolicyValues {
	return signalPolicyValues{
		CorrelatedProjects:       policy.CorrelatedProjects,
		CorrelationWindowMinutes: int(policy.CorrelationWindow / time.Minute),
		EscalationWindowMinutes:  int(policy.EscalationWindow / time.Minute),
		UntendedMultiple:         policy.UntendedMultiple,
		MaxSilenceHours:          int(policy.MaxSilence / time.Hour),
		Paging:                   policy.Paging,
	}
}

// signalPolicyPreset is one named base as the screen offers it.
type signalPolicyPreset struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	signalPolicyValues
}

// signalPolicyView is the whole answer: what is in force, which preset it came
// from, whether it is still that preset unmodified, and the three to choose
// between.
type signalPolicyView struct {
	// Preset is the base in force, and Modified says whether somebody has
	// moved a number away from it. A screen needs both: "balanced" and
	// "balanced, with two numbers moved" are different sentences, and only
	// the second one warns that choosing the preset again would undo
	// something.
	Preset   string `json:"preset"`
	Modified bool   `json:"modified"`

	signalPolicyValues

	// UntendedAfterHours is the second clock spelled out, because it is the
	// product of two fields and a reader should not have to multiply. It is
	// how long a condition runs unacknowledged before it stops being an alert
	// and becomes a line on the compliance posture.
	UntendedAfterHours float64 `json:"untendedAfterHours"`

	// Provenance is the string every finding evaluated under this policy
	// carries, served so that a screen can show the reader the thing they
	// will later find on a finding rather than a paraphrase of it.
	Provenance string `json:"provenance"`

	// Presets are the three, in the order they are offered.
	Presets []signalPolicyPreset `json:"presets"`
}

func newSignalPolicyView(kitchen *kitchenv1alpha1.Kitchen) signalPolicyView {
	policy := signals.PolicyFrom(kitchen)
	view := signalPolicyView{
		Preset:             string(policy.Preset),
		Modified:           !policy.Matches(policy.Preset),
		signalPolicyValues: valuesOf(policy),
		UntendedAfterHours: policy.UntendedAfter().Hours(),
		Provenance:         policy.Provenance(),
	}
	for _, preset := range signals.Presets() {
		view.Presets = append(view.Presets, signalPolicyPreset{
			Name:               string(preset.Preset),
			Description:        signals.PresetDescriptions[preset.Preset],
			signalPolicyValues: valuesOf(preset),
		})
	}
	return view
}

func (s *Server) getSignalPolicy(w http.ResponseWriter, req *http.Request) {
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newSignalPolicyView(kitchen))
}

// patchSignalPolicyRequest changes the policy.
//
// Every number is a pointer, like the retention model's, so that a request
// which does not mention a field cannot disturb it: the dashboard sends the
// whole form and an operator's script sends one number, and both mean the same
// thing. `preset` is the exception in one direction only — naming a preset
// rebases every field the same request does not also set, which is what
// choosing one on the screen does.
type patchSignalPolicyRequest struct {
	Preset                   *string `json:"preset"`
	CorrelatedProjects       *int32  `json:"correlatedProjects"`
	CorrelationWindowMinutes *int32  `json:"correlationWindowMinutes"`
	EscalationWindowMinutes  *int32  `json:"escalationWindowMinutes"`
	UntendedMultiple         *int32  `json:"untendedMultiple"`
	MaxSilenceHours          *int32  `json:"maxSilenceHours"`
	Paging                   *bool   `json:"paging"`
}

// signalPolicyBound is one field's accepted range, spelled here as well as on
// the CRD for the reason the retention floor is: admission answers with a CEL
// rule's message, and a caller who asked for a correlation threshold of one
// should be told what to do about it by the thing they were talking to.
type signalPolicyBound struct {
	name     string
	value    *int32
	min, max int32
	// why is the sentence appended to a refusal — what the number means, so
	// that the bound reads as a judgement rather than as an arbitrary range.
	why string
}

func (s *Server) patchSignalPolicy(w http.ResponseWriter, req *http.Request) {
	kitchen, err := s.getKitchen(req)
	if err != nil {
		s.writeError(w, err)
		return
	}

	body := patchSignalPolicyRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	base := kitchen.DeepCopy()
	was := signals.PolicyFrom(kitchen)

	if !applySignalPreset(w, kitchen, body) {
		return
	}
	if !applySignalPolicyNumbers(w, kitchen, body) {
		return
	}

	now := signals.PolicyFrom(kitchen)
	changed := signals.PolicyDifference(was, now)
	if len(changed) == 0 {
		// A write that changed nothing is recorded as nothing. The log is for
		// changes, and a no-op PATCH that appended a line would be one more
		// row between an auditor and the change they are looking for.
		writeJSON(w, http.StatusOK, newSignalPolicyView(kitchen))
		return
	}

	transition := audit.Transition{
		Object:    kitchen,
		Kind:      audit.KindSignalPolicy,
		Operation: clickhouse.AuditUpdate,
		Reason:    "what this installation counts as worth hearing was changed: " + strings.Join(changed, ", "),
		Details: map[string]any{
			"change":  "signal-policy",
			"fields":  changed,
			"was":     was.Provenance(),
			"becomes": now.Provenance(),
		},
	}
	if !s.recorded(w, req, transition) {
		return
	}

	if err := s.Client.Patch(req.Context(), kitchen, client.MergeFrom(base)); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(req.Context())
	s.log().Info("the signal policy was changed through the api",
		"fields", strings.Join(changed, ","), "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newSignalPolicyView(kitchen))
}

// applySignalPreset rebases on a named preset.
//
// Choosing a preset clears every override the installation had, which is what
// the word means on the screen: the numbers underneath move to the preset's,
// and a request that also sets one of them is choosing the preset and then
// moving that one. The alternative — keeping the overrides — would make
// picking `homelab` on an installation with three moved numbers produce a
// policy that is none of the three presets and looks like one.
func applySignalPreset(w http.ResponseWriter, kitchen *kitchenv1alpha1.Kitchen, body patchSignalPolicyRequest) bool {
	if body.Preset == nil {
		return true
	}
	name := signals.PresetName(strings.TrimSpace(*body.Preset))
	if _, known := signals.Preset(name); !known {
		badRequest(w, "`preset` must be one of %s: the catalogue is versioned code and only the "+
			"clock is policy, so there is no fourth kind of installation to name",
			strings.Join(presetNames(), ", "))
		return false
	}
	kitchen.Spec.Observability.Signals.Policy = kitchenv1alpha1.SignalPolicySpec{Preset: string(name)}
	return true
}

func presetNames() []string {
	names := make([]string, 0, 3)
	for _, preset := range signals.Presets() {
		names = append(names, "`"+string(preset.Preset)+"`")
	}
	return names
}

// applySignalPolicyNumbers writes the individual overrides, refusing anything
// outside the bound the CRD would also refuse.
func applySignalPolicyNumbers(
	w http.ResponseWriter,
	kitchen *kitchenv1alpha1.Kitchen,
	body patchSignalPolicyRequest,
) bool {
	spec := &kitchen.Spec.Observability.Signals.Policy
	for _, field := range []struct {
		signalPolicyBound
		into **int32
	}{
		{signalPolicyBound{"correlatedProjects", body.CorrelatedProjects, 2, 100,
			"two is the smallest number that can be a correlation at all, and a threshold above the " +
				"number of projects this platform runs is a detector that can never fire"},
			&spec.CorrelatedProjects},
		{signalPolicyBound{"correlationWindowMinutes", body.CorrelationWindowMinutes, 1, 1440,
			"it is how far apart two failures may have started and still be called the same moment"},
			&spec.CorrelationWindowMinutes},
		{signalPolicyBound{"escalationWindowMinutes", body.EscalationWindowMinutes, 5, 10080,
			"below five minutes it escalates rollouts, and above a week nothing is ever escalated"},
			&spec.EscalationWindowMinutes},
		{signalPolicyBound{"untendedMultiple", body.UntendedMultiple, 1, 100,
			"it multiplies the escalation window into how long a condition runs before it becomes a " +
				"line on the compliance posture"},
			&spec.UntendedMultiple},
		{signalPolicyBound{"maxSilenceHours", body.MaxSilenceHours, 1, 8760,
			"a silence nobody revisits is a decision whose reason has stopped being true without " +
				"anybody noticing"},
			&spec.MaxSilenceHours},
	} {
		if field.value == nil {
			continue
		}
		if *field.value < field.min || *field.value > field.max {
			badRequest(w, "%s must be between %d and %d (got %d): %s",
				field.name, field.min, field.max, *field.value, field.why)
			return false
		}
		value := *field.value
		*field.into = &value
	}
	if body.Paging != nil {
		paging := *body.Paging
		spec.Paging = &paging
	}
	return true
}
