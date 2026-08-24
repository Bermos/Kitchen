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

package policy

import (
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What VEXFrom has to get right: it is a faithful projection of the evidence,
// it applies expiry against the evaluation's clock and nothing else, and it is
// ordered, because the registry is not.

const openVEXPredicateType = "https://openvex.dev/ns/v0.2.0"

func vexEvidence(verified bool, predicate string) Evidence {
	return Evidence{
		PredicateType: openVEXPredicateType,
		Verified:      verified,
		Predicate:     json.RawMessage(predicate),
	}
}

func TestVEXFromFlattensADocumentFaithfully(t *testing.T) {
	evidence := []Evidence{vexEvidence(true, `{
		"@context": "https://openvex.dev/ns/v0.2.0",
		"author": "security@shop.example",
		"timestamp": "2026-08-01T00:00:00Z",
		"statements": [
			{"vulnerability": {"name": "CVE-2026-2"}, "status": "not_affected",
			 "justification": "component_not_present",
			 "products": [{"@id": "pkg:oci/shop@sha256:abc"}]},
			{"vulnerability": "CVE-2026-1", "status": "affected",
			 "supplier": "vendor@upstream.example"}
		]}`)}

	statements := VEXFrom(evidence, evaluationTime)
	if len(statements) != 2 {
		t.Fatalf("want two statements, got %+v", statements)
	}
	// Ordered by vulnerability: the registry lists attestations in whatever
	// order it pleases, and an input digest that depended on that would make
	// two evaluations of the same facts two different decisions.
	if statements[0].Vulnerability != "CVE-2026-1" || statements[1].Vulnerability != "CVE-2026-2" {
		t.Fatalf("statements must be ordered, got %+v", statements)
	}
	if statements[0].Author != "vendor@upstream.example" || statements[1].Author != "security@shop.example" {
		t.Errorf("authorship must be per statement, got %+v", statements)
	}
	if !statements[0].Verified || !statements[1].Verified {
		t.Error("the envelope's verification state must travel with every statement it carries")
	}
	if statements[1].Justification != "component_not_present" {
		t.Errorf("the justification must be carried verbatim, got %q", statements[1].Justification)
	}
	if len(statements[1].Products) != 1 || statements[1].Products[0] != "pkg:oci/shop@sha256:abc" {
		t.Errorf("products must be carried, got %v", statements[1].Products)
	}
	if statements[0].Timestamp != "2026-08-01T00:00:00Z" {
		t.Errorf("an undated statement takes the document's date, got %q", statements[0].Timestamp)
	}
}

func TestVEXFromCarriesWhatItDoesNotBelieve(t *testing.T) {
	// The materializer judges nothing: an unjustified not_affected and an
	// unverified envelope both reach the input, and the bundle is what
	// refuses them. That is what "visible, never silently applied" means at
	// this level — a statement dropped here would be a suppression decision
	// taken where nobody could see it.
	statements := VEXFrom([]Evidence{vexEvidence(false, `{
		"@context": "https://openvex.dev/ns/v0.2.0",
		"author": "someone@example",
		"statements": [{"vulnerability": "CVE-2026-9", "status": "not_affected",
			"impact_statement": "trust me"}]}`)}, evaluationTime)

	if len(statements) != 1 {
		t.Fatalf("an unjustified statement must still be materialized, got %+v", statements)
	}
	if statements[0].Justification != "" || statements[0].Verified {
		t.Errorf("it must be carried as it is, got %+v", statements[0])
	}
}

func TestVEXExpiryIsJudgedAgainstTheEvaluationsClock(t *testing.T) {
	evidence := []Evidence{vexEvidence(true, `{
		"@context": "https://openvex.dev/ns/v0.2.0",
		"author": "security@shop.example",
		"statements": [
			{"vulnerability": "CVE-2026-1", "status": "not_affected",
			 "justification": "component_not_present", "expires": "2026-08-19T00:00:00Z"},
			{"vulnerability": "CVE-2026-2", "status": "not_affected",
			 "justification": "component_not_present", "expires": "2026-09-19T00:00:00Z"}
		]}`)}

	// evaluationTime is 2026-08-20: the first has run out, the second has not.
	statements := VEXFrom(evidence, evaluationTime)
	if len(statements) != 1 || statements[0].Vulnerability != "CVE-2026-2" {
		t.Fatalf("an expired statement must not be in the listing, got %+v", statements)
	}

	// Replaying the same decision at the moment it was taken suppresses
	// exactly what it suppressed — the clock is the input's, never the
	// reader's, which is the same rule ApplyExceptions keeps.
	earlier := VEXFrom(evidence, time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC))
	if len(earlier) != 2 {
		t.Fatalf("both statements were current then, got %+v", earlier)
	}
}

func TestVEXFromIgnoresEverythingThatIsNotAReadableDocument(t *testing.T) {
	statements := VEXFrom([]Evidence{
		{PredicateType: "https://spdx.dev/Document", Verified: true, Predicate: json.RawMessage(`{}`)},
		vexEvidence(true, `not json at all`),
		vexEvidence(true, `{"@context":"https://openvex.dev/ns/v0.2.0","author":"a","statements":[
			{"vulnerability":"","status":"fixed"},{"vulnerability":"CVE-3","status":""}]}`),
	}, evaluationTime)

	if len(statements) != 0 {
		t.Fatalf("nothing here asserts anything the engine can act on, got %+v", statements)
	}
}

func TestMaterializeInputCarriesVEXOnEveryPath(t *testing.T) {
	// The point of materializing VEX here rather than at each caller: a
	// promotion, a rescan, a replay and the eligibility preview all reach the
	// engine through this one function, so none of them can be the one that
	// forgot.
	evidence := []Evidence{vexEvidence(true, `{
		"@context": "https://openvex.dev/ns/v0.2.0", "author": "security@shop.example",
		"statements": [{"vulnerability": "CVE-2026-1", "status": "not_affected",
			"justification": "component_not_present"}]}`)}

	for _, kind := range []string{KindPromotion, KindRescan, KindReplay, KindEligibility} {
		input := MaterializeInput(kind, evaluationTime, nil, vexEnvironment(), vexRelease(), nil, evidence, nil)
		if len(input.VEX) != 1 || input.VEX[0].Vulnerability != "CVE-2026-1" {
			t.Errorf("kind %s reached the engine without its VEX statements: %+v", kind, input.VEX)
		}
	}
}

func vexEnvironment() *kitchenv1alpha1.Environment {
	return &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-production"},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.EnvironmentProduction,
		},
	}
}

func vexRelease() *kitchenv1alpha1.Release {
	return &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-1"},
		Spec:       kitchenv1alpha1.ReleaseSpec{Image: "registry.example.com/shop@sha256:1111"},
	}
}
