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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/controller"
)

// Ingesting a gate result somebody else produced.
//
// The thing being tested is not that bytes arrive. It is that the result is
// attributed: a scan an application's own CI ran and submitted must not end up
// indistinguishable from one the platform ran itself, because the difference is
// exactly what a policy that trusts only the platform needs in order to say so.

// stubEvidence stands in for the registry.
type stubEvidence struct {
	attached  []attestation.Envelope
	predicate string
	subject   string
	err       error
}

func (s *stubEvidence) Evidence(
	context.Context, string, ...attestation.Verifier,
) (attestation.EvidenceSet, error) {
	return attestation.EvidenceSet{}, nil
}

func (s *stubEvidence) Attach(
	_ context.Context, imageRef string, envelope attestation.Envelope, predicateType string,
) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.attached = append(s.attached, envelope)
	s.predicate = predicateType
	s.subject = imageRef
	return "sha256:" + strings.Repeat("f", 64), nil
}

// gateHarness is a project whose latest build produced an attestable artifact,
// and a platform holding a signing key.
func gateHarness(t *testing.T) (*harness, *stubEvidence, string) {
	t.Helper()
	digest := "sha256:" + strings.Repeat("a", 64)

	_, privatePEM, publicPEM, err := attestation.GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	key := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: controller.SigningKeySecretName, Namespace: testNamespace,
		},
		Data: map[string][]byte{
			attestation.SecretKeyPrivate: privatePEM,
			attestation.SecretKeyPublic:  publicPEM,
		},
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-9", Namespace: testNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git:        kitchenv1alpha1.GitRevision{SHA: "abc123def456", Branch: "main"},
		},
		Status: kitchenv1alpha1.BuildStatus{
			Phase: kitchenv1alpha1.BuildSucceeded,
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "registry.example.com/kitchen/shop",
				Digest:     digest,
			},
		},
	}

	credentials := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-credentials", Namespace: testNamespace},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(
				`{"auths":{"registry.example.com":{"username":"robot","password":"hunter2"}}}`),
		},
	}
	objects := append([]runtime.Object{key, build, credentials}, fixtures()...)
	// The harness builds the platform object, because it is what knows the
	// issuer this test's tokens come from. Attestation is turned on afterwards
	// rather than by replacing it.
	h := newHarness(t, nil, objects...)
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Compliance.Attestation.Enabled = true
	if err := h.server.Client.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}
	// The evidence is attached through the project's own registry connection,
	// with the credential the build pushed under, so the connection has to say
	// where that registry is.
	connection := &kitchenv1alpha1.Connection{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "registry"}, connection); err != nil {
		t.Fatal(err)
	}
	connection.Spec.Config = &runtime.RawExtension{
		Raw: []byte(`{"url":"registry.example.com/kitchen"}`),
	}
	if err := h.server.Client.Update(context.Background(), connection); err != nil {
		t.Fatal(err)
	}

	registry := &stubEvidence{}
	h.server.EvidenceReaders = func([]byte, string) (EvidenceReader, error) { return registry, nil }
	return h, registry, digest
}

func TestSubmittedGateResultIsSignedAndAttributedToWhoSentIt(t *testing.T) {
	h, registry, digest := gateHarness(t)

	body := `{
		"gate": "trivy",
		"version": "0.58.0",
		"format": "trivy-json",
		"findings": {"Results": [{"Vulnerabilities": [{"VulnerabilityID": "CVE-2026-9"}]}]}
	}`
	recorder := h.do(t, http.MethodPost, "/api/v1/builds/shop-bld-9/gates", body)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	if len(registry.attached) != 1 {
		t.Fatalf("attached %d envelopes, want 1", len(registry.attached))
	}
	if registry.predicate != attestation.PredicateQualityGate {
		t.Errorf("attached under %s", registry.predicate)
	}
	if want := "registry.example.com/kitchen/shop@" + digest; registry.subject != want {
		t.Errorf("attached to %s, want %s", registry.subject, want)
	}

	statement, err := registry.attached[0].Statement()
	if err != nil {
		t.Fatal(err)
	}
	if !statement.Describes(digest) {
		t.Error("the result is not about the artifact")
	}
	predicate := map[string]any{}
	if err := json.Unmarshal(statement.Predicate, &predicate); err != nil {
		t.Fatal(err)
	}

	// The platform signed it. It did not witness it, and the predicate has to
	// say which — an unattributed submission would read exactly like a scan
	// the platform ran.
	if predicate["reportedBy"] != testCaller {
		t.Errorf("the result is credited to %v, want the caller %q", predicate["reportedBy"], testCaller)
	}
	if predicate["external"] != true {
		t.Error("a submitted result does not say it was submitted")
	}
	if predicate["reportedAt"] == "" || predicate["reportedAt"] == nil {
		t.Error("nothing records when the platform received it")
	}
	for _, forbidden := range []string{"pass", "passed", "verdict", "ok", "allowed"} {
		if _, present := predicate[forbidden]; present {
			t.Errorf("a submitted gate result carries a verdict field %q", forbidden)
		}
	}

	// And the Build says the same thing, so that a screen does not have to go
	// to the registry to find out whose word it has.
	stored := &kitchenv1alpha1.Build{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "shop-bld-9"}, stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Status.Gates) != 1 {
		t.Fatalf("the build records %d gates, want 1", len(stored.Status.Gates))
	}
	gate := stored.Status.Gates[0]
	if gate.Source != "external" || gate.ReportedBy != testCaller {
		t.Errorf("the build credits the result to %+v", gate)
	}
	if gate.Phase != kitchenv1alpha1.GateCompleted || gate.Attested == nil {
		t.Errorf("a submitted result was not recorded as completed evidence: %+v", gate)
	}
}

func TestASubmissionWithoutFindingsIsRefused(t *testing.T) {
	h, registry, _ := gateHarness(t)

	recorder := h.do(t, http.MethodPost, "/api/v1/builds/shop-bld-9/gates",
		`{"gate": "trivy"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(registry.attached) != 0 {
		t.Error("an empty result was attached to the artifact")
	}
}

func TestASubmissionAboutABuildWithNoArtifactIsRefused(t *testing.T) {
	h, registry, _ := gateHarness(t)

	// A build that never pushed anything. There is nothing for a gate result
	// to be about, which is not the same as a build with no gate results.
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-10", Namespace: testNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git:        kitchenv1alpha1.GitRevision{SHA: "def456abc123", Branch: "main"},
		},
		Status: kitchenv1alpha1.BuildStatus{Phase: kitchenv1alpha1.BuildFailed},
	}
	if err := h.server.Client.Create(context.Background(), build); err != nil {
		t.Fatal(err)
	}

	recorder := h.do(t, http.MethodPost, "/api/v1/builds/shop-bld-10/gates",
		`{"gate": "trivy", "findings": {}}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(registry.attached) != 0 {
		t.Error("a result was attached to a build that produced no artifact")
	}
}
