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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/policy"
)

// Environment ownership is segregation of duties written as a schema: the
// project role says who may deploy into an environment, spec.owners says who
// may decide what it demands, and neither confers the other. These tests pin
// the property from the outside — a developer who can redeploy all day long
// must be refused the requirements write, and the refusal must say who holds
// it instead.

const testBundleDigest = "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// requirementsPath is the write these tests are about, on the fixtures'
// production environment.
const requirementsPath = "/api/v1/environments/" + testEnvironment + "/requirements"

// environment reads the fixtures' production environment back, which is what
// every assertion about "was the spec written / left alone" reads.
func (h *harness) environment(t *testing.T) *kitchenv1alpha1.Environment {
	t.Helper()
	env := &kitchenv1alpha1.Environment{}
	key := types.NamespacedName{Namespace: testNamespace, Name: testEnvironment}
	if err := h.server.Client.Get(context.Background(), key, env); err != nil {
		t.Fatal(err)
	}
	return env
}

func (h *harness) updateEnvironment(t *testing.T, edit func(*kitchenv1alpha1.Environment)) {
	t.Helper()
	env := h.environment(t)
	edit(env)
	if err := h.server.Client.Update(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

func TestADeveloperWhoIsNotAnOwnerMayNotChangeRequirements(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)

	// An environment naming no owners is locked to the operators, and the
	// refusal says exactly that rather than pointing at a project role the
	// caller could go and ask for.
	recorder := h.do(t, http.MethodPatch, requirementsPath,
		`{"bundleDigest":"`+testBundleDigest+`"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	want := "environment " + testEnvironment + " names no owners, " +
		"so only a platform operator may change its requirements"
	if got := errorOf(t, recorder.Body.String()); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}

	// With owners named, the refusal names them — the answer to "who may" is
	// written on the environment, and the 403 is where somebody reads it.
	h.updateEnvironment(t, func(env *kitchenv1alpha1.Environment) {
		env.Spec.Owners = []string{"risk-officer@example.com"}
	})
	recorder = h.do(t, http.MethodPatch, requirementsPath,
		`{"bundleDigest":"`+testBundleDigest+`"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	got := errorOf(t, recorder.Body.String())
	if !strings.Contains(got, "risk-officer@example.com") || !strings.Contains(got, "platform operator") {
		t.Fatalf("the refusal must say who may: %q", got)
	}

	if env := h.environment(t); env.Spec.Requirements != nil {
		t.Fatalf("a refused write must change nothing, got %+v", env.Spec.Requirements)
	}
}

func TestAnOwnerChangesRequirementsOnAViewersProjectRole(t *testing.T) {
	// The owner holds viewer — enough to see the environment, nothing that
	// could deploy into it. Ownership is what admits the write, not the role.
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	h.updateEnvironment(t, func(env *kitchenv1alpha1.Environment) {
		env.Spec.Owners = []string{testCaller}
	})

	recorder := h.do(t, http.MethodPatch, requirementsPath,
		`{"bundleDigest":"`+testBundleDigest+`","parameters":{"maxSeverity":"high"}}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	env := h.environment(t)
	if env.Spec.Requirements == nil || env.Spec.Requirements.BundleDigest != testBundleDigest {
		t.Fatalf("the requirements were not written: %+v", env.Spec.Requirements)
	}
	if env.Spec.Requirements.Parameters["maxSeverity"] != "high" {
		t.Fatalf("the parameters were not written: %+v", env.Spec.Requirements.Parameters)
	}

	view := decode[environmentView](t, recorder)
	if view.Requirements == nil || view.Requirements.BundleDigest != testBundleDigest {
		t.Fatalf("the answer must echo the requirements, got %+v", view.Requirements)
	}
}

func TestAnOperatorChangesRequirementsWithoutBeingAnOwner(t *testing.T) {
	// The default harness caller is an operator; the environment names owners
	// that are somebody else. Operators always may — an environment must not
	// be able to lock the platform's own people out of governing it.
	h := newHarness(t, nil, fixtures()...)
	h.updateEnvironment(t, func(env *kitchenv1alpha1.Environment) {
		env.Spec.Owners = []string{"risk-officer@example.com"}
	})

	recorder := h.do(t, http.MethodPatch, requirementsPath,
		`{"bundleDigest":"`+testBundleDigest+`","owners":["risk-officer@example.com","`+testCaller+`"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	env := h.environment(t)
	if len(env.Spec.Owners) != 2 {
		t.Fatalf("the owners were not replaced: %v", env.Spec.Owners)
	}
}

func TestAMalformedBundleDigestIsRefusedWithTheForm(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, requirementsPath,
		`{"bundleDigest":"latest"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "sha256:<64 hex") {
		t.Fatalf("the refusal must say the form, got %q", got)
	}
}

func TestARequirementsChangeTheLogCannotRecordIsNotMade(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.withUnreachableAuditLog(t)

	recorder := h.do(t, http.MethodPatch, requirementsPath,
		`{"bundleDigest":"`+testBundleDigest+`"}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if env := h.environment(t); env.Spec.Requirements != nil {
		t.Fatalf("an unrecorded change must not be made, got %+v", env.Spec.Requirements)
	}
}

// The record of a requirements change is what makes it reversible on paper:
// the previous digest is in it, the parameters that moved are named — never
// valued — and it is marked privileged. The transition is built apart from
// the recording exactly so this can be asserted without a store.
func TestTheRequirementsTransitionCarriesTheChangeByNameAndNotByValue(t *testing.T) {
	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment, Namespace: testNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: feedProject},
		},
	}
	previous := "sha256:" + strings.Repeat("a", 64)
	owners := []string{"risk-officer@example.com"}
	changed := changedParameterNames(
		map[string]string{"maxSeverity": "critical", "gate": "trivy"},
		map[string]string{"maxSeverity": "high", "gate": "trivy", "vexMaxAgeDays": "30"},
	)

	transition := requirementsTransition(env, previous, testBundleDigest, changed, &owners,
		&dataClassChange{previous: "", next: inventoryClassConfidential}, nil)
	if transition.From != previous || transition.To != testBundleDigest {
		t.Fatalf("the transition must run from the previous digest to the next: %q -> %q",
			transition.From, transition.To)
	}
	if transition.Details["privileged"] != true {
		t.Fatalf("a requirements change is privileged, got %v", transition.Details["privileged"])
	}
	if transition.Details["previousBundleDigest"] != previous {
		t.Fatalf("the previous digest must be recorded, got %v", transition.Details["previousBundleDigest"])
	}
	if transition.Details["bundleDigest"] != testBundleDigest {
		t.Fatalf("the new digest must be recorded, got %v", transition.Details["bundleDigest"])
	}

	names, ok := transition.Details["changedParameters"].([]string)
	if !ok {
		t.Fatalf("the changed parameters must be recorded, got %v", transition.Details["changedParameters"])
	}
	if got, want := strings.Join(names, ","), "maxSeverity,vexMaxAgeDays"; got != want {
		t.Fatalf("want the changed names %q, got %q", want, got)
	}
	if transition.Details["previousDataClass"] != "" || transition.Details["dataClass"] != inventoryClassConfidential {
		t.Fatalf("a class change must carry its previous value, got %v -> %v",
			transition.Details["previousDataClass"], transition.Details["dataClass"])
	}

	// Names, never values: the whole record must not carry what any parameter
	// was set to.
	raw, err := json.Marshal(transition.Details)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"critical", "high", "30"} {
		if strings.Contains(string(raw), `"`+value+`"`) {
			t.Fatalf("the record carries a parameter value %q: %s", value, raw)
		}
	}
}

func TestEligibilityWithoutRequirementsSaysEveryReleaseIsEligible(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/"+testEnvironment+"/eligibility", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[eligibilityBody](t, recorder)
	if body.Eligible == nil || !*body.Eligible || !body.Evaluated {
		t.Fatalf("no requirements means eligible and evaluated, got %s", recorder.Body.String())
	}
	if body.Requirements != nil {
		t.Fatalf("no requirements must answer null, got %+v", body.Requirements)
	}
	if body.Release != testRelease {
		t.Fatalf("the release must default to the one the environment runs, got %q", body.Release)
	}
	if !strings.Contains(body.Message, "declares no requirements") {
		t.Fatalf("the answer must say why, got %q", body.Message)
	}
}

func TestEligibilityWithAnUnresolvableBundleStaysUnevaluated(t *testing.T) {
	// The pinned digest resolves to nothing — no such bundle exists. The
	// answer stays three-valued: eligible null, evaluated false, and the
	// message says which bundle could not be found rather than guessing in
	// either direction.
	h, registry, digest := gateHarness(t)
	h.updateEnvironment(t, func(env *kitchenv1alpha1.Environment) {
		env.Spec.Requirements = &kitchenv1alpha1.EnvironmentRequirements{
			BundleDigest: testBundleDigest,
			Parameters:   map[string]string{"maxSeverity": "high"},
		}
	})

	// The gate harness's build produced the artifact; give it an evidence
	// index and a release, and let the stub registry verify the provenance
	// half of it.
	build := &kitchenv1alpha1.Build{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "shop-bld-9"}, build); err != nil {
		t.Fatal(err)
	}
	build.Status.Artifact.Evidence = []kitchenv1alpha1.ArtifactEvidence{
		{PredicateType: attestation.PredicateBuildRecord, Manifest: digest, Source: "platform"},
		{PredicateType: attestation.PredicateSLSAProvenanceV1, Manifest: digest, Source: "builder"},
	}
	if err := h.server.Client.Status().Update(context.Background(), build); err != nil {
		t.Fatal(err)
	}
	release := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-9", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "shop-bld-9"},
			Image:      "registry.example.com/kitchen/shop@" + digest,
		},
	}
	if err := h.server.Client.Create(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	registry.set = attestation.EvidenceSet{
		Subject:  "registry.example.com/kitchen/shop@" + digest,
		Verified: true,
		Attestations: []attestation.Evidence{
			{PredicateType: attestation.PredicateBuildRecord, Verified: true},
			{PredicateType: attestation.PredicateSLSAProvenanceV1, Verified: false},
		},
	}

	recorder := h.do(t, http.MethodGet,
		"/api/v1/environments/"+testEnvironment+"/eligibility?release=shop-rel-9", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[eligibilityBody](t, recorder)

	// The bar is echoed, and nothing has judged the pair: eligible is null,
	// not a guess in either direction, and the rules list is empty rather
	// than generic.
	if body.Requirements == nil || body.Requirements.BundleDigest != testBundleDigest {
		t.Fatalf("the requirements must be echoed, got %+v", body.Requirements)
	}
	if body.Eligible != nil || body.Evaluated {
		t.Fatalf("nothing has evaluated, got %s", recorder.Body.String())
	}
	if body.UnmetRules == nil || len(body.UnmetRules) != 0 {
		t.Fatalf("unmetRules must be an empty list until rules fire, got %s", recorder.Body.String())
	}
	if !strings.Contains(body.Message, "no policy bundle") {
		t.Fatalf("the answer must say the bundle is unavailable, got %q", body.Message)
	}

	// The evidence summary is the screen's half: what the artifact carries,
	// whose claim each piece is, and what verified against the platform key.
	verified := map[string]bool{}
	source := map[string]string{}
	for _, evidence := range body.Evidence {
		verified[evidence.PredicateType] = evidence.Verified
		source[evidence.PredicateType] = evidence.Source
	}
	if !verified[attestation.PredicateBuildRecord] || verified[attestation.PredicateSLSAProvenanceV1] {
		t.Fatalf("the verified flags must come from the registry read, got %s", recorder.Body.String())
	}
	if source[attestation.PredicateBuildRecord] != "platform" ||
		source[attestation.PredicateSLSAProvenanceV1] != "builder" {
		t.Fatalf("the sources must come from the build's index, got %s", recorder.Body.String())
	}
}

func TestEligibilityEvaluatesForRealWhenTheBundleResolves(t *testing.T) {
	// The built-in bundle, pinned by its real digest, with two rules turned
	// on: provenance the artifact carries, a gate it does not. The answer is
	// an actual evaluation — the same engine a promotion decision will come
	// from — naming exactly the rule that fired. It stores nothing: it is a
	// read, and the decision register stays empty.
	h, registry, digest := gateHarness(t)
	bundleDigest := policy.Digest(policy.DefaultBundle())
	h.updateEnvironment(t, func(env *kitchenv1alpha1.Environment) {
		env.Spec.Requirements = &kitchenv1alpha1.EnvironmentRequirements{
			BundleDigest: bundleDigest,
			Parameters: map[string]string{
				"require-provenance": "true",
				"requiredGates":      "trivy",
			},
		}
	})

	release := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-9", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: feedProject},
			BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "shop-bld-9"},
			Image:      "registry.example.com/kitchen/shop@" + digest,
		},
	}
	if err := h.server.Client.Create(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	registry.set = attestation.EvidenceSet{
		Subject:  "registry.example.com/kitchen/shop@" + digest,
		Verified: true,
		Attestations: []attestation.Evidence{
			{PredicateType: attestation.PredicateSLSAProvenanceV1, Verified: true},
		},
	}

	recorder := h.do(t, http.MethodGet,
		"/api/v1/environments/"+testEnvironment+"/eligibility?release=shop-rel-9", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[eligibilityBody](t, recorder)
	if body.Eligible == nil || *body.Eligible || !body.Evaluated {
		t.Fatalf("a real evaluation with a missing gate answers eligible=false, got %s", recorder.Body.String())
	}
	if len(body.UnmetRules) != 1 || body.UnmetRules[0] != "require-gate" {
		t.Fatalf("the unmet rules name what fired, got %v", body.UnmetRules)
	}
	if len(h.logs.insertedDecisions) != 0 {
		t.Fatalf("an eligibility read must store no decision, got %+v", h.logs.insertedDecisions)
	}

	// With the gate's evidence attached, the same bar clears.
	registry.set.Attestations = append(registry.set.Attestations, attestation.Evidence{
		PredicateType: attestation.PredicateQualityGate,
		Verified:      true,
		Statement: attestation.Statement{
			Predicate: json.RawMessage(`{"gate":"trivy","findings":[]}`),
		},
	})
	recorder = h.do(t, http.MethodGet,
		"/api/v1/environments/"+testEnvironment+"/eligibility?release=shop-rel-9", "")
	body = decode[eligibilityBody](t, recorder)
	if body.Eligible == nil || !*body.Eligible || !body.Evaluated {
		t.Fatalf("with the evidence in place the release is eligible, got %s", recorder.Body.String())
	}
	if len(body.UnmetRules) != 0 {
		t.Fatalf("nothing should fire, got %v", body.UnmetRules)
	}
}

func TestEligibilityOfAStrangersReleaseIsRefused(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), blogFixtures()...)...)

	recorder := h.do(t, http.MethodGet,
		"/api/v1/environments/"+testEnvironment+"/eligibility?release=blog-rel-0", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}
