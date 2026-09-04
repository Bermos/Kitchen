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

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
)

// A unit is several images and every one of them is deployed, so every one of
// them is attested — each about its own digest, not the first one standing in
// for the rest (#300).
func TestAttestWorkloadsSignsOneStatementPerImage(t *testing.T) {
	reconciler, attester, build, project, target := attestFixtures(t)
	web := "registry.example.com/shop@sha256:" + strings.Repeat("a", 64)
	api := "registry.example.com/shop-api@sha256:" + strings.Repeat("b", 64)
	worker := "registry.example.com/shop-worker@sha256:" + strings.Repeat("c", 64)

	build.Status.Workloads = []kitchenv1alpha1.WorkloadBuildStatus{
		{Name: "api", Phase: kitchenv1alpha1.BuildSucceeded, Repository: "registry.example.com/shop-api", Image: api},
		{Name: "worker", Phase: kitchenv1alpha1.BuildSucceeded, Repository: "registry.example.com/shop-worker", Image: worker},
	}
	build.Status.Artifact = reconciler.attestBuild(context.Background(), build, project, target,
		artifactSubject{Strategy: target.Strategy, Image: web})
	reconciler.attestWorkloads(context.Background(), build, project, target,
		[]planOutcome{
			{Plan: buildPlan{}},
			{Plan: buildPlan{Workload: "api", Strategy: kitchenv1alpha1.BuildStrategyDockerfile}},
			{Plan: buildPlan{Workload: "worker", Strategy: kitchenv1alpha1.BuildStrategyBuildpacks}},
		},
		[]kitchenv1alpha1.WorkloadImage{{Name: "api", Image: api}, {Name: "worker", Image: worker}})

	artifacts := build.Artifacts()
	if len(artifacts) != 3 {
		t.Fatalf("the unit carries %d evidence records, want one per image", len(artifacts))
	}
	if !build.FullyAttested() {
		t.Errorf("a unit whose every image was attested reads as not attested: missing %v",
			build.ArtifactsWithoutEvidence())
	}

	// Each statement has to name its *own* subject. Evidence about the web
	// process's digest attached to the worker's would describe something the
	// Release does not deploy under that name.
	if len(attester.attached) != 3 {
		t.Fatalf("attached %d statements, want one per image", len(attester.attached))
	}
	byDigest := map[string]bool{}
	for index, envelope := range attester.attached {
		statement, err := envelope.Statement()
		if err != nil {
			t.Fatal(err)
		}
		for _, subject := range statement.Subject {
			byDigest[subject.Digest["sha256"]] = true
			// Attached where it is about: a statement describing the API's
			// digest pushed against the worker's reference would be evidence
			// sitting beside the wrong artifact.
			if !strings.HasSuffix(attester.refs[index], "@sha256:"+subject.Digest["sha256"]) {
				t.Errorf("a statement about sha256:%s was attached to %q",
					subject.Digest["sha256"], attester.refs[index])
			}
		}
	}
	for _, want := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)} {
		if !byDigest[want] {
			t.Errorf("no statement describes sha256:%s — an image was left with no evidence of its own", want)
		}
	}

	// And the record has to say which image of the unit it is about, or three
	// statements about one commit are indistinguishable.
	named := map[string]bool{}
	for _, envelope := range attester.attached {
		statement, err := envelope.Statement()
		if err != nil {
			t.Fatal(err)
		}
		predicate := string(statement.Predicate)
		switch {
		case strings.Contains(predicate, `"workload":"api"`):
			named["api"] = true
		case strings.Contains(predicate, `"workload":"worker"`):
			named["worker"] = true
		case strings.Contains(predicate, `"workload"`):
			t.Errorf("a build record names a workload nothing built: %s", predicate)
		default:
			named[""] = true
		}
	}
	if !named["api"] || !named["worker"] || !named[""] {
		t.Errorf("the three records do not name three images: %v", named)
	}

	for _, workload := range build.Status.Workloads {
		if workload.Artifact == nil || workload.Artifact.AttestedAt == nil {
			t.Fatalf("workload %s was left unattested", workload.Name)
		}
		if workload.Artifact.SourceType != kitchenv1alpha1.ArtifactSourceBuilt {
			t.Errorf("workload %s carries source type %q, want %q",
				workload.Name, workload.Artifact.SourceType, kitchenv1alpha1.ArtifactSourceBuilt)
		}
	}
}

// The whole of the compatibility promise: a project of one workload signs
// exactly the statement it signed before a unit could be several images.
func TestASingleWorkloadUnitSignsTheStatementItAlwaysDid(t *testing.T) {
	image := "registry.example.com/shop@sha256:" + strings.Repeat("a", 64)

	// A fixed clock, because the record carries the build's own times and a
	// comparison against "roughly now" would pass by luck.
	ran := metav1.NewTime(time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC))

	statementFor := func(t *testing.T, workloads []kitchenv1alpha1.WorkloadBuildStatus) []byte {
		t.Helper()
		reconciler, attester, build, project, target := attestFixtures(t)
		build.Status.Workloads = workloads
		build.Status.StartedAt = ptr.To(ran)
		build.Status.CompletedAt = ptr.To(ran)
		build.Status.Artifact = reconciler.attestBuild(context.Background(), build, project, target,
			artifactSubject{Strategy: target.Strategy, Image: image})
		if len(attester.attached) != 1 {
			t.Fatalf("attached %d statements for the project's own image", len(attester.attached))
		}
		statement, err := attester.attached[0].Statement()
		if err != nil {
			t.Fatal(err)
		}
		return statement.Predicate
	}

	alone := statementFor(t, nil)
	// The same commit, in a project that also ships a worker: the project's
	// own image still signs its own record and nothing about the worker
	// leaks into it.
	beside := statementFor(t, []kitchenv1alpha1.WorkloadBuildStatus{
		{Name: "worker", Phase: kitchenv1alpha1.BuildSucceeded, DetectedFramework: "python"},
	})
	if string(alone) != string(beside) {
		t.Errorf("the web process's build record changed when a workload was added:\n%s\n%s", alone, beside)
	}
	if strings.Contains(string(alone), "workload") {
		t.Errorf("a single-image project's build record grew a workload key: %s", alone)
	}
	if !strings.Contains(string(alone), `"framework":"nextjs"`) {
		t.Errorf("the project's own record lost its detected framework: %s", alone)
	}
}

// A build whose fourth workload could not be attested is not an attested
// release, and the answer has to name the fourth workload.
func TestAReleaseIsAttestedOnlyWhenEveryArtifactIs(t *testing.T) {
	now := metav1.Now()
	build := &kitchenv1alpha1.Build{
		Status: kitchenv1alpha1.BuildStatus{
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "registry.example.com/shop",
				Digest:     "sha256:" + strings.Repeat("a", 64),
				AttestedAt: &now,
			},
			Workloads: []kitchenv1alpha1.WorkloadBuildStatus{
				{
					Name:  "api",
					Phase: kitchenv1alpha1.BuildSucceeded,
					Artifact: &kitchenv1alpha1.ArtifactStatus{
						Digest:     "sha256:" + strings.Repeat("b", 64),
						AttestedAt: &now,
					},
				},
				{
					Name:  "worker",
					Phase: kitchenv1alpha1.BuildSucceeded,
					Artifact: &kitchenv1alpha1.ArtifactStatus{
						Digest:  "sha256:" + strings.Repeat("c", 64),
						Message: "the registry refused the referrers write",
					},
				},
			},
		},
	}

	if build.FullyAttested() {
		t.Error("a unit with an unattested worker reports the release as attested")
	}
	missing := build.ArtifactsWithoutEvidence()
	if len(missing) != 1 || missing[0] != "worker" {
		t.Errorf("the answer names %v as missing evidence, want [worker]", missing)
	}

	// And once the worker is attested, the unit is.
	build.Status.Workloads[1].Artifact.AttestedAt = &now
	if !build.FullyAttested() {
		t.Errorf("a fully attested unit reports missing evidence: %v", build.ArtifactsWithoutEvidence())
	}

	// A workload the platform has no artifact record for at all is missing
	// evidence too: from the outside it is the same fact.
	build.Status.Workloads = append(build.Status.Workloads,
		kitchenv1alpha1.WorkloadBuildStatus{Name: "migrations", Phase: kitchenv1alpha1.BuildSucceeded})
	if missing := build.ArtifactsWithoutEvidence(); len(missing) != 1 || missing[0] != "migrations" {
		t.Errorf("a workload with no artifact record is not named: %v", missing)
	}
}

// Each artifact's evidence index is its own. Nothing about the API's SBOM
// should be readable as the worker's.
func TestArtifactsAreIndexedSeparately(t *testing.T) {
	build := &kitchenv1alpha1.Build{
		Status: kitchenv1alpha1.BuildStatus{
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Digest: "sha256:" + strings.Repeat("a", 64),
				Evidence: []kitchenv1alpha1.ArtifactEvidence{
					{PredicateType: attestation.PredicateBuildRecord, Source: sourcePlatform},
				},
			},
			Workloads: []kitchenv1alpha1.WorkloadBuildStatus{{
				Name: "api",
				Artifact: &kitchenv1alpha1.ArtifactStatus{
					Digest: "sha256:" + strings.Repeat("b", 64),
					Evidence: []kitchenv1alpha1.ArtifactEvidence{
						{PredicateType: attestation.PredicateQualityGate, Source: sourcePlatform},
					},
				},
			}},
		},
	}
	if got := build.ArtifactFor("api"); got == nil || got.Digest != "sha256:"+strings.Repeat("b", 64) {
		t.Errorf("the workload's own artifact was not found: %+v", got)
	}
	if got := build.ArtifactFor(""); got == nil || got.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("the web process's artifact was not found: %+v", got)
	}
	if build.ArtifactFor("nothing") != nil {
		t.Error("a workload nothing built produced an artifact")
	}
}
