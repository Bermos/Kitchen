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

	for name, tc := range map[string]struct {
		security  *kitchenv1alpha1.SecuritySpec
		artifacts []kitchenv1alpha1.BuildArtifact
		want      int
	}{
		"a named user under runAsNonRoot with no uid": {
			security: &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true}, artifacts: named, want: 1,
		},
		"the same, with the uid supplied": {
			security:  &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true, RunAsUser: 1000},
			artifacts: named,
		},
		"a project that asked for nothing": {artifacts: named},
		"a numeric user, which needs no help": {
			security: &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true},
			artifacts: []kitchenv1alpha1.BuildArtifact{{
				Artifact: &kitchenv1alpha1.ArtifactStatus{Repository: "r", Digest: "sha256:a", User: "1001"},
			}},
		},
		"an image whose user was never read": {
			security: &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true},
			artifacts: []kitchenv1alpha1.BuildArtifact{{
				Artifact: &kitchenv1alpha1.ArtifactStatus{Repository: "r", Digest: "sha256:a"},
			}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := unverifiableImages(tc.security, tc.artifacts); len(got) != tc.want {
				t.Fatalf("unverifiableImages found %d, want %d: %+v", len(got), tc.want, got)
			}
		})
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
