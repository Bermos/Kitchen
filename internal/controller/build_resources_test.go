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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func buildPodSpec() corev1.PodSpec {
	return corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: "clone"}},
		Containers:     []corev1.Container{{Name: "creator"}},
	}
}

// The ceiling is the request as well as the limit. A limit alone bounds one
// build and tells the scheduler nothing, which is how two builds come to be
// placed where only one fits.
func TestBuildResourcesAreReservedAsWellAsCapped(t *testing.T) {
	spec := buildPodSpec()
	applyBuildResources(context.Background(), &spec,
		kitchenv1alpha1.BuildResourcesSpec{CPU: "2", Memory: "4Gi"})

	for _, container := range append(spec.InitContainers, spec.Containers...) {
		requests, limits := container.Resources.Requests, container.Resources.Limits
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			request, hasRequest := requests[name]
			limit, hasLimit := limits[name]
			if !hasRequest || !hasLimit {
				t.Fatalf("%s carries no %s ceiling: %+v", container.Name, name, container.Resources)
			}
			if request.Cmp(limit) != 0 {
				t.Errorf("%s reserves %s of %s and is capped at %s",
					container.Name, request.String(), name, limit.String())
			}
		}
		if got := limits.Memory().String(); got != "4Gi" {
			t.Errorf("%s is capped at %s memory, not 4Gi", container.Name, got)
		}
	}
}

// An installation that has cleared one half of the ceiling gets the other half
// and no accidental zero: an absent request is not the same as a request for
// nothing.
func TestBuildResourcesLeaveAClearedCeilingAlone(t *testing.T) {
	spec := buildPodSpec()
	applyBuildResources(context.Background(), &spec,
		kitchenv1alpha1.BuildResourcesSpec{Memory: "512Mi"})

	resources := spec.Containers[0].Resources
	if _, ok := resources.Limits[corev1.ResourceCPU]; ok {
		t.Errorf("a cleared CPU ceiling was written anyway: %+v", resources.Limits)
	}
	if got := resources.Limits.Memory().String(); got != "512Mi" {
		t.Errorf("the memory ceiling is %s, not 512Mi", got)
	}
}

// No ceiling at all is what every installation had before the field existed,
// and it stays a pod that declares nothing rather than one that declares zero.
func TestBuildResourcesUnboundedLeavesThePodAsItWas(t *testing.T) {
	spec := buildPodSpec()
	applyBuildResources(context.Background(), &spec, kitchenv1alpha1.BuildResourcesSpec{})

	if len(spec.Containers[0].Resources.Limits) != 0 || len(spec.Containers[0].Resources.Requests) != 0 {
		t.Errorf("an unbounded build was given resources: %+v", spec.Containers[0].Resources)
	}
}

// A quantity nothing can parse reaches here only from something that skipped
// both the API and admission. It is skipped rather than turned into a build
// failure nobody can read.
func TestBuildResourcesSkipAQuantityThatWillNotParse(t *testing.T) {
	spec := buildPodSpec()
	applyBuildResources(context.Background(), &spec,
		kitchenv1alpha1.BuildResourcesSpec{CPU: "two", Memory: "4Gi"})

	limits := spec.Containers[0].Resources.Limits
	if _, ok := limits[corev1.ResourceCPU]; ok {
		t.Errorf("an unparsable CPU ceiling was written: %+v", limits)
	}
	if got := limits.Memory().String(); got != "4Gi" {
		t.Errorf("the memory ceiling was lost with the CPU one: %s", got)
	}
}

// Both shapes of the same ending: the builder killed, and a child of it
// killed with the builder merely dying of it.
func TestOutOfMemoryReadsBothShapesOfTheKill(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure *kitchenv1alpha1.BuildFailureStatus
		want    bool
	}{
		{
			name:    "the kubelet named it",
			failure: &kitchenv1alpha1.BuildFailureStatus{Reason: reasonOOMKilled, ExitCode: ptr.To(int32(137))},
			want:    true,
		},
		{
			name:    "a child was killed and the builder died of it",
			failure: &kitchenv1alpha1.BuildFailureStatus{Reason: "Error", ExitCode: ptr.To(int32(137))},
			want:    true,
		},
		{
			name:    "a compiler that would not compile",
			failure: &kitchenv1alpha1.BuildFailureStatus{Reason: "Error", ExitCode: ptr.To(int32(1))},
			want:    false,
		},
		{
			name:    "a pod that never started",
			failure: &kitchenv1alpha1.BuildFailureStatus{Reason: "ImagePullBackOff"},
			want:    false,
		},
		{name: "no failure at all", failure: nil, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := outOfMemory(tc.failure); got != tc.want {
				t.Errorf("outOfMemory(%+v) = %v, want %v", tc.failure, got, tc.want)
			}
		})
	}
}

// The message is the whole point of the reason existing: it says the build ran
// out of memory, what the ceiling was, and who can move it.
func TestOutOfMemoryMessageNamesTheCeilingAndTheSetting(t *testing.T) {
	message := outOfMemoryMessage(
		&kitchenv1alpha1.BuildFailureStatus{Container: "buildkit", ExitCode: ptr.To(int32(137))}, "4Gi")

	for _, want := range []string{"buildkit", "ran out of memory", "4Gi", "builds.resources.memory"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not mention %q: %s", want, message)
		}
	}
}

// A ceiling the singleton could not be read for still produces a sentence,
// naming the number the platform would have used.
func TestOutOfMemoryMessageFallsBackToTheDefaultCeiling(t *testing.T) {
	message := outOfMemoryMessage(&kitchenv1alpha1.BuildFailureStatus{}, "")
	if !strings.Contains(message, DefaultBuildMemory) {
		t.Errorf("the message names no ceiling at all: %s", message)
	}
}
