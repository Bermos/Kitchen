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
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// `runAsNonRoot` against an image whose USER is a name (#393).
//
// The posture is right, the image already honours it, and every pod is
// refused — because the kubelet has to *verify* the user is not root and can
// only do that against a uid. These pin the one judgement that makes the
// difference: which recorded users the kubelet can check, and which it cannot.

func TestWhichImageUsersTheKubeletCanVerify(t *testing.T) {
	for user, isName := range map[string]bool{
		// What the official Node images and distroless actually ship, which
		// is the whole reason this exists.
		"node":             true,
		"nonroot:nonroot":  true,
		"appuser:appgroup": true,
		// A uid needs no help: the kubelet compares it with zero and starts
		// the container. Refusing these at the API would have been a 400 for
		// a request that works.
		"1001":      false,
		"1001:1000": false,
		"0":         false,
		// An image that declares no user at all runs as root. That is a
		// different refusal, with a different message, and it is the
		// kubelet's to make rather than this one's.
		"":  false,
		" ": false,
	} {
		if got := imageUserIsName(user); got != isName {
			t.Fatalf("imageUserIsName(%q) = %v, want %v", user, got, isName)
		}
	}
}

// The refusal is made of both halves — what the project asked for and what
// the image is — and never of one alone.
func TestOnlyAPostureThatCannotBeHonouredIsRefused(t *testing.T) {
	named := []kitchenv1alpha1.BuildArtifact{{
		Artifact: &kitchenv1alpha1.ArtifactStatus{
			Repository: "registry.example.com/shop", Digest: "sha256:abc", User: "node",
		},
	}}

	// A worker's image, so that a posture declared on that workload alone can
	// be asked about it (#399).
	workerNamed := []kitchenv1alpha1.BuildArtifact{{
		Workload: "worker",
		Artifact: &kitchenv1alpha1.ArtifactStatus{
			Repository: "gcr.io/distroless/static", Digest: "sha256:def", User: "nonroot",
		},
	}}
	unit := func(security *kitchenv1alpha1.SecuritySpec) kitchenv1alpha1.ConfigSnapshot {
		return kitchenv1alpha1.ConfigSnapshot{
			Runtime: kitchenv1alpha1.RuntimeSpec{Security: security},
		}
	}

	for name, tc := range map[string]struct {
		snapshot  kitchenv1alpha1.ConfigSnapshot
		artifacts []kitchenv1alpha1.BuildArtifact
		want      int
	}{
		"a named user under runAsNonRoot with no uid": {
			snapshot: unit(&kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true}), artifacts: named, want: 1,
		},
		"the same, with the uid supplied": {
			snapshot:  unit(&kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true, RunAsUser: 1000}),
			artifacts: named,
		},
		"a project that asked for nothing": {artifacts: named},
		"a numeric user, which needs no help": {
			snapshot: unit(&kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true}),
			artifacts: []kitchenv1alpha1.BuildArtifact{{
				Artifact: &kitchenv1alpha1.ArtifactStatus{Repository: "r", Digest: "sha256:a", User: "1001"},
			}},
		},
		"an image whose user was never read": {
			snapshot: unit(&kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true}),
			artifacts: []kitchenv1alpha1.BuildArtifact{{
				Artifact: &kitchenv1alpha1.ArtifactStatus{Repository: "r", Digest: "sha256:a"},
			}},
		},
		// The four cases the per-workload posture adds. The unit's answer is
		// no longer the workload's, in either direction.
		"a workload that supplied the uid the unit did not": {
			snapshot: kitchenv1alpha1.ConfigSnapshot{
				Runtime: kitchenv1alpha1.RuntimeSpec{
					Security: &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true},
				},
				Processes: []kitchenv1alpha1.ProcessSpec{{
					Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
					Security: &kitchenv1alpha1.SecuritySpec{RunAsUser: 65532},
				}},
			},
			artifacts: workerNamed,
		},
		"a workload that inherits runAsNonRoot and supplies no uid": {
			snapshot: kitchenv1alpha1.ConfigSnapshot{
				Runtime: kitchenv1alpha1.RuntimeSpec{
					Security: &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true},
				},
				Processes: []kitchenv1alpha1.ProcessSpec{{
					Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
					Security: &kitchenv1alpha1.SecuritySpec{ReadOnlyRootFilesystem: true},
				}},
			},
			artifacts: workerNamed,
			want:      1,
		},
		"a workload that asked for runAsNonRoot the unit did not": {
			snapshot: kitchenv1alpha1.ConfigSnapshot{
				Processes: []kitchenv1alpha1.ProcessSpec{{
					Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
					Security: &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true},
				}},
			},
			artifacts: workerNamed,
			want:      1,
		},
		"the unit's uid, which the workload never took off": {
			snapshot: kitchenv1alpha1.ConfigSnapshot{
				Runtime: kitchenv1alpha1.RuntimeSpec{
					Security: &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true, RunAsUser: 1000},
				},
				Processes: []kitchenv1alpha1.ProcessSpec{{
					Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
					Security: &kitchenv1alpha1.SecuritySpec{ReadOnlyRootFilesystem: true},
				}},
			},
			artifacts: workerNamed,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := unverifiableImages(tc.snapshot, tc.artifacts); len(got) != tc.want {
				t.Fatalf("unverifiableImages found %d, want %d: %+v", len(got), tc.want, got)
			}
		})
	}
}

// A unit whose workloads run under different postures is refused for the one
// workload the combination cannot work on, and for no other (#399).
func TestOnlyTheWorkloadWhoseOwnPostureCannotBeHonouredIsNamed(t *testing.T) {
	snapshot := kitchenv1alpha1.ConfigSnapshot{
		Runtime: kitchenv1alpha1.RuntimeSpec{
			Security: &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true, RunAsUser: 1000},
		},
		Processes: []kitchenv1alpha1.ProcessSpec{
			// This one takes the unit's uid off the table by asking for a
			// posture of its own that names none: the merge leaves it with
			// runAsNonRoot and uid 1000, which is fine.
			{Name: "worker", Type: kitchenv1alpha1.ProcessWorker,
				Security: &kitchenv1alpha1.SecuritySpec{ReadOnlyRootFilesystem: true}},
			// This one is the distroless image, and it declares a uid the
			// unit does not have — 65532 — so it is fine as well.
			{Name: "sidecar", Type: kitchenv1alpha1.ProcessWorker,
				Security: &kitchenv1alpha1.SecuritySpec{RunAsUser: 65532}},
		},
	}
	artifacts := []kitchenv1alpha1.BuildArtifact{
		{Artifact: &kitchenv1alpha1.ArtifactStatus{Repository: "r", Digest: "sha256:a", User: "node"}},
		{Workload: "worker", Artifact: &kitchenv1alpha1.ArtifactStatus{
			Repository: "r", Digest: "sha256:b", User: "node"}},
		{Workload: "sidecar", Artifact: &kitchenv1alpha1.ArtifactStatus{
			Repository: "r", Digest: "sha256:c", User: "nonroot"}},
	}
	if found := unverifiableImages(snapshot, artifacts); len(found) != 0 {
		t.Fatalf("nothing should be refused here: %+v", found)
	}

	// Now take the unit's uid away. The web process and the worker inherit a
	// runAsNonRoot with nothing to verify it against; the sidecar keeps its
	// own uid and is left alone.
	snapshot.Runtime.Security = &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true}
	found := unverifiableImages(snapshot, artifacts)
	if len(found) != 2 {
		t.Fatalf("the web process and the worker should be refused, got %+v", found)
	}
	for i, want := range []string{kitchenv1alpha1.WebProcessName, "worker"} {
		if found[i].Name() != want {
			t.Fatalf("refusal %d names %q, want %q", i, found[i].Name(), want)
		}
	}
}

// The message is the whole fix: which workload, which image, the user found
// in it, and the field that makes it work.
func TestTheRefusalNamesTheImageTheUserAndTheFix(t *testing.T) {
	message := unverifiableImagesMessage([]kitchenv1alpha1.BuildArtifact{
		{Artifact: &kitchenv1alpha1.ArtifactStatus{
			Repository: "registry.example.com/shop", Digest: "sha256:abc", User: "node",
		}},
		{Workload: "worker", Artifact: &kitchenv1alpha1.ArtifactStatus{
			Repository: "gcr.io/distroless/static", Digest: "sha256:def", User: "nonroot:nonroot",
		}},
	})

	for _, want := range []string{
		kitchenv1alpha1.WebProcessName,
		"worker",
		"registry.example.com/shop@sha256:abc",
		"gcr.io/distroless/static@sha256:def",
		`"node"`,
		`"nonroot:nonroot"`,
		"runAsUser",
		"runAsNonRoot",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("the refusal does not mention %q: %s", want, message)
		}
	}
}
