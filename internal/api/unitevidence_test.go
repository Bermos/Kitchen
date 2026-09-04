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
)

// A release deploys every image of its unit, so "is it attested" is a
// question about all of them (#300). The answer names the ones with no
// evidence rather than reporting the first artifact's state for the lot.

// The two extra workloads the fixtures' unit ships, named once so that the
// assertions below and the fixture they are about cannot drift apart.
const (
	unitAPI    = "api"
	unitWorker = "worker"
)

// unitBuild makes the fixtures' build a unit of three images, with `attested`
// deciding which of the workloads carry signed evidence.
func unitBuild(t *testing.T, h *harness, attested map[string]bool) {
	t.Helper()
	now := metav1.Now()
	stamp := func(name string) *metav1.Time {
		if attested[name] {
			return &now
		}
		return nil
	}
	build := &kitchenv1alpha1.Build{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: testBuild}, build); err != nil {
		t.Fatal(err)
	}
	build.Status.Image = "registry.example.com/shop@sha256:" + strings.Repeat("a", 64)
	build.Status.Artifact = &kitchenv1alpha1.ArtifactStatus{
		Repository: "registry.example.com/shop",
		Digest:     "sha256:" + strings.Repeat("a", 64),
		SourceType: kitchenv1alpha1.ArtifactSourceBuilt,
		AttestedAt: stamp("web"),
	}
	build.Status.Workloads = []kitchenv1alpha1.WorkloadBuildStatus{
		{
			Name:       unitAPI,
			Phase:      kitchenv1alpha1.BuildSucceeded,
			Repository: "registry.example.com/shop-api",
			Image:      "registry.example.com/shop-api@sha256:" + strings.Repeat("b", 64),
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "registry.example.com/shop-api",
				Digest:     "sha256:" + strings.Repeat("b", 64),
				SourceType: kitchenv1alpha1.ArtifactSourceBuilt,
				AttestedAt: stamp(unitAPI),
			},
		},
		{
			Name:       unitWorker,
			Phase:      kitchenv1alpha1.BuildSucceeded,
			Repository: "registry.example.com/shop-worker",
			Image:      "registry.example.com/shop-worker@sha256:" + strings.Repeat("c", 64),
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "registry.example.com/shop-worker",
				Digest:     "sha256:" + strings.Repeat("c", 64),
				SourceType: kitchenv1alpha1.ArtifactSourceBuilt,
				AttestedAt: stamp(unitWorker),
				Message:    "the registry refused the referrers write",
			},
		},
	}
	if err := h.server.Client.Status().Update(context.Background(), build); err != nil {
		t.Fatal(err)
	}
}

type releaseAttestationBody struct {
	Attestation struct {
		Attested  bool     `json:"attested"`
		Missing   []string `json:"missing"`
		Artifacts []struct {
			Workload   string `json:"workload"`
			Digest     string `json:"digest"`
			SourceType string `json:"sourceType"`
			Attested   bool   `json:"attested"`
		} `json:"artifacts"`
	} `json:"attestation"`
}

func TestAReleaseIsAttestedOnlyWhenEveryArtifactItDeploysIs(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	unitBuild(t, h, map[string]bool{"web": true, unitAPI: true})

	recorder := h.do(t, http.MethodGet, "/api/v1/releases/"+testRelease, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := releaseAttestationBody{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Attestation.Attested {
		t.Error("a release with an unattested worker reports itself attested")
	}
	if len(body.Attestation.Missing) != 1 || body.Attestation.Missing[0] != unitWorker {
		t.Errorf("the answer names %v as missing evidence, want [worker]", body.Attestation.Missing)
	}
	if len(body.Attestation.Artifacts) != 3 {
		t.Fatalf("the release lists %d artifacts, want one per image", len(body.Attestation.Artifacts))
	}
	// Each artifact answers for itself: its own digest, its own source type.
	seen := map[string]string{}
	for _, artifact := range body.Attestation.Artifacts {
		seen[artifact.Workload] = artifact.Digest
		if artifact.SourceType != string(kitchenv1alpha1.ArtifactSourceBuilt) {
			t.Errorf("artifact %s carries source type %q", artifact.Workload, artifact.SourceType)
		}
	}
	if seen["web"] == seen[unitAPI] || seen[unitWorker] == "" {
		t.Errorf("the artifacts do not carry their own digests: %v", seen)
	}
}

func TestAFullyAttestedUnitReadsAsAttested(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	unitBuild(t, h, map[string]bool{"web": true, unitAPI: true, unitWorker: true})

	recorder := h.do(t, http.MethodGet, "/api/v1/releases/"+testRelease, "")
	body := releaseAttestationBody{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !body.Attestation.Attested || len(body.Attestation.Missing) != 0 {
		t.Errorf("a fully attested unit reads as %+v", body.Attestation)
	}
}

// The evidence endpoints answer about one image of the unit, named. Asking
// for a workload nothing built is a refusal that says what the build did
// produce, rather than the project's own image standing in for it.
func TestEvidenceEndpointsAnswerPerArtifact(t *testing.T) {
	h, registry, _ := gateHarness(t)
	build := &kitchenv1alpha1.Build{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "shop-bld-9"}, build); err != nil {
		t.Fatal(err)
	}
	build.Status.Workloads = []kitchenv1alpha1.WorkloadBuildStatus{{
		Name:       unitWorker,
		Phase:      kitchenv1alpha1.BuildSucceeded,
		Repository: "registry.example.com/kitchen/shop-worker",
		Image:      "registry.example.com/kitchen/shop-worker@sha256:" + strings.Repeat("c", 64),
		Artifact: &kitchenv1alpha1.ArtifactStatus{
			Repository: "registry.example.com/kitchen/shop-worker",
			Digest:     "sha256:" + strings.Repeat("c", 64),
			SourceType: kitchenv1alpha1.ArtifactSourceBuilt,
		},
	}}
	if err := h.server.Client.Status().Update(context.Background(), build); err != nil {
		t.Fatal(err)
	}
	_ = registry

	if got := h.do(t, http.MethodGet,
		"/api/v1/builds/shop-bld-9/attestations?workload=worker", ""); got.Code != http.StatusOK {
		t.Errorf("reading the worker's evidence answered %d: %s", got.Code, got.Body.String())
	}
	refused := h.do(t, http.MethodGet, "/api/v1/builds/shop-bld-9/attestations?workload=nothing", "")
	if refused.Code != http.StatusBadRequest {
		t.Errorf("asking about a workload nothing built answered %d", refused.Code)
	}
	if !strings.Contains(refused.Body.String(), unitWorker) {
		t.Errorf("the refusal does not say what the build produced: %s", refused.Body.String())
	}

	// A gate result submitted about the worker is recorded against the
	// worker, and nothing about it reaches the project's own image.
	body := `{"gate":"trivy","workload":"` + unitWorker + `","findings":{"Results":[]}}`
	created := h.do(t, http.MethodPost, "/api/v1/builds/shop-bld-9/gates", body)
	if created.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", created.Code, created.Body.String())
	}
	stored := &kitchenv1alpha1.Build{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: "shop-bld-9"}, stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Status.Gates) != 0 {
		t.Errorf("the worker's gate result was recorded against the project's own image: %+v",
			stored.Status.Gates)
	}
	if len(stored.Status.Workloads[0].Gates) != 1 {
		t.Fatalf("the worker's gate result was not recorded against the worker: %+v",
			stored.Status.Workloads[0].Gates)
	}
	if len(stored.Status.Workloads[0].Artifact.Evidence) != 1 {
		t.Errorf("the worker's evidence index did not grow: %+v", stored.Status.Workloads[0].Artifact)
	}
	if len(stored.Status.Artifact.Evidence) != 0 {
		t.Errorf("the project's own evidence index grew from a worker's submission: %+v",
			stored.Status.Artifact)
	}
}
