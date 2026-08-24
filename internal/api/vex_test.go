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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/controller"
)

// Ingesting an exploitability assertion, and showing it beside what it
// modifies.
//
// Two things are being tested and neither is that bytes arrive. The document
// must reach the registry **verbatim** — a predicate re-encoded on the way
// through is a different claim from the one somebody made — and it must be
// attributed twice over: to the author it names and to the identity that sent
// it, which are different facts and only the second is the platform's own
// observation.

const vexAuthor = "security@shop.example"

// vexDocument is one well formed OpenVEX document about two vulnerabilities.
const vexDocument = `{
	"@context": "https://openvex.dev/ns/v0.2.0",
	"@id": "https://shop.example/vex/2026-08-24",
	"author": "security@shop.example",
	"timestamp": "2026-08-24T09:00:00Z",
	"statements": [
		{"vulnerability": {"name": "CVE-2026-1"}, "status": "not_affected",
		 "justification": "vulnerable_code_not_in_execute_path",
		 "impact_statement": "the parser is never reached from our entry points"},
		{"vulnerability": "CVE-2026-2", "status": "affected", "action_statement": "upgrade to 2.4.1"}
	]
}`

// submitVEXDocument posts a document exactly as written — concatenated rather
// than marshalled, so that the test's own encoder cannot be what normalizes
// it on the way in.
func submitVEXDocument(t *testing.T, h *harness, document string) *httptest.ResponseRecorder {
	t.Helper()
	return h.do(t, http.MethodPost, "/api/v1/builds/shop-bld-9/vex", `{"document":`+document+`}`)
}

// compacted is the document with its insignificant whitespace removed, which
// is the one thing that does not survive the trip: every key, every value and
// their order do. That is the property that matters — a predicate rebuilt
// from a decoded map would reorder keys and renumber numbers, and the
// platform's signature would then be over a claim nobody made.
func compacted(t *testing.T, document string) string {
	t.Helper()
	out := &bytes.Buffer{}
	if err := json.Compact(out, []byte(document)); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestASubmittedVEXDocumentIsSignedVerbatimAndAttributed(t *testing.T) {
	h, registry, digest := gateHarness(t)

	recorder := submitVEXDocument(t, h, vexDocument)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(registry.attached) != 1 {
		t.Fatalf("attached %d envelopes, want 1", len(registry.attached))
	}
	// OpenVEX has its own vocabulary and its own URI. Minting a Kitchen
	// predicate type for the same claim would produce evidence only Kitchen
	// could read, which is the opposite of why this layer is standards.
	if registry.predicate != "https://openvex.dev/ns/v0.2.0" {
		t.Errorf("attached under %s", registry.predicate)
	}

	statement, err := registry.attached[0].Statement()
	if err != nil {
		t.Fatal(err)
	}
	if !statement.Describes(digest) {
		t.Error("the document is not about the artifact")
	}
	// Verbatim: the signed predicate is the submitted document, byte for
	// byte. Anything else makes the platform the author of a claim it merely
	// received.
	if string(statement.Predicate) != compacted(t, vexDocument) {
		t.Errorf("the document was re-encoded on the way through:\n%s", statement.Predicate)
	}

	stored := &kitchenv1alpha1.Build{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "shop-bld-9"}, stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Status.VEX) != 1 {
		t.Fatalf("the build indexes %d documents, want 1", len(stored.Status.VEX))
	}
	ingested := stored.Status.VEX[0]
	// Two different facts, kept apart: what the document claims about its own
	// authorship, and who the platform watched hand it over.
	if ingested.Author != vexAuthor {
		t.Errorf("the document's author is recorded as %q", ingested.Author)
	}
	if ingested.SubmittedBy != testCaller {
		t.Errorf("the submitter is recorded as %q, want the caller %q", ingested.SubmittedBy, testCaller)
	}
	if ingested.Statements != 2 || len(ingested.Vulnerabilities) != 2 {
		t.Errorf("the index does not say what the document covers: %+v", ingested)
	}
	if ingested.IngestedAt == nil {
		t.Error("nothing records when the platform received it")
	}

	// And the artifact's evidence index names it, so a reader knows to go and
	// fetch it without listing the registry.
	found := false
	for _, evidence := range stored.Status.Artifact.Evidence {
		if evidence.PredicateType == attestation.PredicateOpenVEX {
			found = true
			if evidence.Source != "platform" {
				t.Errorf("the evidence index credits it to %q", evidence.Source)
			}
		}
	}
	if !found {
		t.Error("the artifact's evidence index does not name the VEX document")
	}

	// A corrected document restating the same assertions is one register
	// entry, not two.
	if recorder := submitVEXDocument(t, h, vexDocument); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201 on re-submission, got %d: %s", recorder.Code, recorder.Body.String())
	}
	again := &kitchenv1alpha1.Build{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "shop-bld-9"}, again); err != nil {
		t.Fatal(err)
	}
	if len(again.Status.VEX) != 1 {
		t.Errorf("re-submitting the same document grew the index to %d rows", len(again.Status.VEX))
	}
}

func TestANotAffectedWithoutAnEnumeratedJustificationIsRefusedAtIngest(t *testing.T) {
	h, registry, _ := gateHarness(t)

	document := `{
		"@context": "https://openvex.dev/ns/v0.2.0",
		"author": "security@shop.example",
		"statements": [{"vulnerability": "CVE-2026-1", "status": "not_affected",
			"impact_statement": "we looked at it and it is fine"}]
	}`
	recorder := submitVEXDocument(t, h, document)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	// The refusal has to be actionable rather than merely correct.
	for _, want := range []string{"not_affected", "vulnerable_code_not_present", "impact_statement"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Errorf("the refusal must mention %q: %s", want, recorder.Body.String())
		}
	}
	if len(registry.attached) != 0 {
		t.Error("an unjustified suppression was attached to the artifact")
	}
}

func TestWhoMayAssertExploitabilityIsThePlatformsWord(t *testing.T) {
	h, registry, _ := gateHarness(t)
	setVEXSpec(t, h, &kitchenv1alpha1.VEXSpec{
		Enabled: true, TrustedAuthors: []string{"vendor@upstream.example"},
	})

	recorder := submitVEXDocument(t, h, vexDocument)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403 for an author the platform does not admit, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "vendor@upstream.example") {
		t.Errorf("the refusal must name the list: %s", recorder.Body.String())
	}
	if len(registry.attached) != 0 {
		t.Error("a document from an author nobody admitted was attached")
	}

	// The same document from an admitted author goes through.
	setVEXSpec(t, h, &kitchenv1alpha1.VEXSpec{
		Enabled: true, TrustedAuthors: []string{"vendor@upstream.example", vexAuthor},
	})
	if recorder := submitVEXDocument(t, h, vexDocument); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201 for an admitted author, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAnInstallationCanRefuseVEXAltogether(t *testing.T) {
	h, registry, _ := gateHarness(t)
	setVEXSpec(t, h, &kitchenv1alpha1.VEXSpec{Enabled: false})

	recorder := submitVEXDocument(t, h, vexDocument)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(registry.attached) != 0 {
		t.Error("a document was attached on a platform that admits none")
	}
}

func TestVEXStatementsAreShownBesideTheFindingsTheyModify(t *testing.T) {
	h, registry, _ := gateHarness(t)

	// One scan with three findings, and one document that speaks to two of
	// them — one suppression the platform would let a policy act on, and one
	// that ran out yesterday.
	expired := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339)
	document := `{
		"@context": "https://openvex.dev/ns/v0.2.0",
		"@id": "https://shop.example/vex/1",
		"author": "security@shop.example",
		"timestamp": "2026-01-01T00:00:00Z",
		"statements": [
			{"vulnerability": "CVE-2026-1", "status": "not_affected",
			 "justification": "component_not_present"},
			{"vulnerability": "CVE-2026-2", "status": "not_affected",
			 "justification": "component_not_present", "expires": "` + expired + `"}
		]
	}`
	registry.set = attestation.EvidenceSet{
		Verified: true,
		Attestations: []attestation.Evidence{
			{
				PredicateType: attestation.PredicateVulnerabilityScan,
				Verified:      true,
				Statement: attestation.Statement{Predicate: json.RawMessage(`{"findings":[
					{"vulnerability":"CVE-2026-1","severity":"critical","package":"libfoo"},
					{"vulnerability":"CVE-2026-2","severity":"high","package":"libbar"},
					{"vulnerability":"CVE-2026-3","severity":"low","package":"libbaz"}]}`)},
			},
			{
				PredicateType: attestation.PredicateOpenVEX,
				Verified:      true,
				Statement:     attestation.Statement{Predicate: json.RawMessage(document)},
			},
		},
	}
	// The Build's index is what knows who submitted the document; the
	// registry only knows who it says wrote it.
	stored := &kitchenv1alpha1.Build{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "shop-bld-9"}, stored); err != nil {
		t.Fatal(err)
	}
	stored.Status.VEX = []kitchenv1alpha1.VEXStatus{{
		DocumentID: "https://shop.example/vex/1", Author: vexAuthor, SubmittedBy: testCaller,
	}}
	if err := h.server.Client.Status().Update(context.Background(), stored); err != nil {
		t.Fatal(err)
	}

	recorder := h.do(t, http.MethodGet, "/api/v1/builds/shop-bld-9/vex", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := vexBody{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}

	if len(body.Statements) != 2 {
		t.Fatalf("want two statements, got %+v", body.Statements)
	}
	for _, statement := range body.Statements {
		if statement.Author != vexAuthor || statement.SubmittedBy != testCaller {
			t.Errorf("a statement must carry both attributions: %+v", statement)
		}
	}
	// An expired statement is shown rather than dropped: "why has this come
	// back?" is the question the screen exists to answer, and the assertion
	// that ran out is the answer.
	if body.Statements[0].Expired || !body.Statements[1].Expired {
		t.Errorf("expiry is not reported per statement: %+v", body.Statements)
	}

	if len(body.Findings) != 3 {
		t.Fatalf("want three findings, got %+v", body.Findings)
	}
	byName := map[string]vexFindingView{}
	for _, finding := range body.Findings {
		byName[finding.Vulnerability] = finding
	}
	// Suppressed, and still visible with the statement and the author that
	// suppressed it — never silently applied.
	suppressed := byName["CVE-2026-1"]
	if suppressed.VEX == nil || !suppressed.VEX.effective() || suppressed.VEX.Author != vexAuthor {
		t.Errorf("the suppressed finding does not carry the statement suppressing it: %+v", suppressed)
	}
	if suppressed.Severity != "critical" || suppressed.Package != "libfoo" {
		t.Errorf("a suppressed finding is still a finding: %+v", suppressed)
	}
	if lapsed := byName["CVE-2026-2"]; lapsed.VEX == nil || lapsed.VEX.effective() {
		t.Errorf("an expired statement must be shown and must not read as effective: %+v", lapsed)
	}
	if uncovered := byName["CVE-2026-3"]; uncovered.VEX != nil {
		t.Errorf("a finding nobody asserted anything about must carry no statement: %+v", uncovered)
	}
}

func TestAnUnverifiedVEXDocumentIsListedAndSaidToBeUnverified(t *testing.T) {
	h, registry, _ := gateHarness(t)

	// An evidence set gathered with no key: a listing, not a verification. A
	// reader that could not tell the two apart would treat one as the other,
	// and the default bundle honours none of it.
	registry.set = attestation.EvidenceSet{
		Verified: false,
		Attestations: []attestation.Evidence{{
			PredicateType: attestation.PredicateOpenVEX,
			Verified:      false,
			Statement:     attestation.Statement{Predicate: json.RawMessage(vexDocument)},
		}},
	}

	recorder := h.do(t, http.MethodGet, "/api/v1/builds/shop-bld-9/vex", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := vexBody{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Verification != "listed" || body.Caveat == "" {
		t.Errorf("an unverified set must say so: %+v", body)
	}
	for _, statement := range body.Statements {
		if statement.Verified || statement.effective() {
			t.Errorf("an unverified statement must not read as one a policy would act on: %+v", statement)
		}
	}
}

func TestAVEXSubmissionIsRecordedBeforeItIsAttached(t *testing.T) {
	h, registry, _ := gateHarness(t)

	// A log that cannot append refuses the write it was asked to record.
	// Attribution is the whole of what the platform adds to somebody else'"'"'s
	// assertion, so an assertion attached with nothing said about who filed it
	// is the one outcome this endpoint must not have — over-recording is the
	// acceptable direction, and this is which direction it fails in.
	h.withUnreachableAuditLog(t)

	recorder := submitVEXDocument(t, h, vexDocument)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503 when the log cannot append, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(registry.attached) != 0 {
		t.Error("a document nothing recorded was attached to the artifact anyway")
	}

	// And what is foreseeable is still decided in the caller'"'"'s own words: a
	// document the platform would refuse outright is refused as such rather
	// than as a broken log.
	unjustified := `{"@context":"https://openvex.dev/ns/v0.2.0","author":"security@shop.example",
		"statements":[{"vulnerability":"CVE-2026-1","status":"not_affected"}]}`
	if recorder := submitVEXDocument(t, h, unjustified); recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 about the document rather than %d about the log: %s",
			recorder.Code, recorder.Body.String())
	}
}

// setVEXSpec rewrites what the platform admits.
func setVEXSpec(t *testing.T, h *harness, spec *kitchenv1alpha1.VEXSpec) {
	t.Helper()
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Compliance.VEX = spec
	if err := h.server.Client.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}
}
