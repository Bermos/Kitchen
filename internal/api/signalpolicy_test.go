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
	"testing"

	"github.com/Bermos/Kitchen/internal/signals"
)

// The signal policy surface, against what #472 asks of it:
//
//   - the numbers are the operator's, installation-wide, and are the floor;
//   - three presets, with today's constants as `balanced`, so an installation
//     that has never opened the screen sees exactly what it saw before;
//   - choosing a preset rebases, and a number moved off it is still visible as
//     a move rather than as a fourth preset;
//   - a bound is refused with the reason rather than with a range.

func policyOf(t *testing.T, h *harness) signalPolicyView {
	t.Helper()
	recorder := h.do(t, http.MethodGet, "/api/v1/platform/policy", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /platform/policy: %d %s", recorder.Code, recorder.Body.String())
	}
	return decode[signalPolicyView](t, recorder)
}

func TestSignalPolicyStartsAtBalancedAndOffersThree(t *testing.T) {
	h := newHarness(t, nil)
	answer := policyOf(t, h)

	if answer.Preset != string(signals.PresetBalanced) {
		t.Errorf("an unconfigured installation reads as %q, want balanced", answer.Preset)
	}
	if answer.Modified {
		t.Error("an installation that has configured nothing is reported as modified")
	}
	if answer.CorrelatedProjects != signals.CorrelatedProjects {
		t.Errorf("correlatedProjects = %d, want the constant %d",
			answer.CorrelatedProjects, signals.CorrelatedProjects)
	}
	if len(answer.Presets) != 3 {
		t.Fatalf("the route offers %d presets, want three", len(answer.Presets))
	}
	for _, preset := range answer.Presets {
		if preset.Description == "" {
			t.Errorf("the %s preset is served with no description, so a screen would have to "+
				"write its own copy of a decision this API owns", preset.Name)
		}
	}
	if !strings.Contains(answer.Provenance, "preset=balanced") {
		t.Errorf("the provenance a finding will carry is served as %q", answer.Provenance)
	}
	if answer.UntendedAfterHours <= 0 {
		t.Error("the second clock is served as zero, so a reader has to multiply two fields")
	}
}

func TestChoosingAPresetRebasesEveryNumber(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, "/api/v1/platform/policy", `{"correlatedProjects": 7}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("moving one number answered %d: %s", recorder.Code, recorder.Body.String())
	}
	moved := policyOf(t, h)
	if moved.CorrelatedProjects != 7 || !moved.Modified {
		t.Fatalf("after moving one number: correlatedProjects=%d modified=%v",
			moved.CorrelatedProjects, moved.Modified)
	}

	// Choosing a preset clears the override, which is what the word means on
	// the screen: picking `homelab` on an installation with a moved number
	// must not produce a policy that is none of the three and looks like one.
	recorder = h.do(t, http.MethodPatch, "/api/v1/platform/policy", `{"preset": "homelab"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("choosing a preset answered %d: %s", recorder.Code, recorder.Body.String())
	}
	homelab := policyOf(t, h)
	if homelab.Preset != string(signals.PresetHomelab) || homelab.Modified {
		t.Fatalf("after choosing homelab: preset=%q modified=%v", homelab.Preset, homelab.Modified)
	}
	if homelab.Paging {
		t.Error("the homelab preset pages, which is not what it is for")
	}
	preset, _ := signals.Preset(signals.PresetHomelab)
	if homelab.CorrelatedProjects != preset.CorrelatedProjects {
		t.Errorf("correlatedProjects = %d after rebasing, want the preset's %d",
			homelab.CorrelatedProjects, preset.CorrelatedProjects)
	}
}

// A request that does not mention a field cannot disturb it: the dashboard
// sends the whole form and a script sends one number, and both mean the same.
func TestPatchingOneFieldLeavesTheRest(t *testing.T) {
	h := newHarness(t, nil)
	before := policyOf(t, h)

	recorder := h.do(t, http.MethodPatch, "/api/v1/platform/policy", `{"maxSilenceHours": 48}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("PATCH answered %d: %s", recorder.Code, recorder.Body.String())
	}
	after := policyOf(t, h)
	if after.MaxSilenceHours != 48 {
		t.Errorf("maxSilenceHours = %d, want 48", after.MaxSilenceHours)
	}
	if after.EscalationWindowMinutes != before.EscalationWindowMinutes {
		t.Errorf("the escalation window moved to %d without being asked",
			after.EscalationWindowMinutes)
	}
}

// A refusal names what the number means. A bound whose message is only a range
// is a bound somebody patches out of the code.
func TestARefusedNumberSaysWhatItIsFor(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, http.MethodPatch, "/api/v1/platform/policy", `{"correlatedProjects": 1}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("a correlation threshold of one answered %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, want := range []string{"correlatedProjects", "smallest number"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not mention %q: %s", want, body)
		}
	}

	recorder = h.do(t, http.MethodPatch, "/api/v1/platform/policy", `{"preset": "paranoid"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("an unknown preset answered %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "balanced") {
		t.Errorf("the refusal does not name the presets that exist: %s", recorder.Body.String())
	}
}
