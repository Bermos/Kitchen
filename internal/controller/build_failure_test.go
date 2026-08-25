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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func terminated(name string, exit int32, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: exit, Reason: reason,
		}},
	}
}

func waiting(name, reason, message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
	}
}

// A buildpacks pod is a clone that worked in front of a builder that did not,
// which is the shape that makes reading containers in order the wrong answer.
func TestFailureFromPodBlamesTheContainerThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pod           corev1.Pod
		wantContainer string
		wantExit      *int32
		wantReason    string
		wantMessage   string
	}{
		{
			name: "the builder behind a clone that succeeded",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				InitContainerStatuses: []corev1.ContainerStatus{terminated("clone", 0, "Completed")},
				ContainerStatuses:     []corev1.ContainerStatus{terminated("creator", 51, "Error")},
			}},
			wantContainer: "creator",
			wantExit:      ptr.To(int32(51)),
			wantReason:    "Error",
			wantMessage:   "creator exited 51",
		},
		{
			name: "the clone, when it is the clone",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				InitContainerStatuses: []corev1.ContainerStatus{terminated("clone", 128, "Error")},
				ContainerStatuses:     []corev1.ContainerStatus{waiting("creator", reasonPodInitializing, "")},
			}},
			wantContainer: "clone",
			wantExit:      ptr.To(int32(128)),
			wantReason:    "Error",
			wantMessage:   "clone exited 128",
		},
		{
			name: "a container that never started",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{
					waiting("creator", "ImagePullBackOff", "back-off pulling image"),
				},
			}},
			wantContainer: "creator",
			wantReason:    "ImagePullBackOff",
			wantMessage:   "creator never started: ImagePullBackOff: back-off pulling image",
		},
		{
			name: "an ending no container can explain",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				Reason:                "Evicted",
				Message:               "the node was low on ephemeral-storage",
				InitContainerStatuses: []corev1.ContainerStatus{terminated("clone", 0, "Completed")},
				ContainerStatuses:     []corev1.ContainerStatus{terminated("creator", 0, "Completed")},
			}},
			wantReason:  "Evicted",
			wantMessage: "Evicted: the node was low on ephemeral-storage",
		},
		{
			name: "the kubelet's own account of the exit",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{terminated("creator", 137, "OOMKilled")},
			}},
			wantContainer: "creator",
			wantExit:      ptr.To(int32(137)),
			wantReason:    "OOMKilled",
			wantMessage:   "creator exited 137 (OOMKilled)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := failureFromPod(&tc.pod)
			if failure == nil {
				t.Fatal("failureFromPod() = nil, want a failure")
			}
			if failure.Container != tc.wantContainer {
				t.Errorf("container = %q, want %q", failure.Container, tc.wantContainer)
			}
			if tc.wantExit == nil && failure.ExitCode != nil {
				t.Errorf("exitCode = %d, want none", *failure.ExitCode)
			}
			if tc.wantExit != nil && (failure.ExitCode == nil || *failure.ExitCode != *tc.wantExit) {
				t.Errorf("exitCode = %v, want %d", failure.ExitCode, *tc.wantExit)
			}
			if failure.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", failure.Reason, tc.wantReason)
			}
			if failure.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", failure.Message, tc.wantMessage)
			}
		})
	}
}

// A pod that says nothing is not a failure this can invent one for: the
// caller falls back to the Job's own sentence rather than to a guess.
func TestFailureFromPodSaysNothingWhenItHasNothing(t *testing.T) {
	pod := corev1.Pod{Status: corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "creator", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
	}}
	if failure := failureFromPod(&pod); failure != nil {
		t.Fatalf("failureFromPod() = %+v, want nil", failure)
	}
	if got := failureMessage(nil, "Job has reached the specified backoff limit"); got != "Job has reached the specified backoff limit" {
		t.Errorf("failureMessage() = %q, want the job's own message", got)
	}
}

// The kubelet's termination message is a file the builder wrote, so it is
// somebody else's bytes and gets bounded like any other.
func TestTerminatedMessageBoundsWhatTheBuilderWrote(t *testing.T) {
	long := strings.Repeat("x", buildFailureMessageMax*2)
	message := terminatedMessage("creator", &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: long})
	if !strings.HasSuffix(message, "…") {
		t.Errorf("message = %q, want it truncated", message[:64])
	}
	if len(message) > buildFailureMessageMax+64 {
		t.Errorf("message is %d bytes, want it bounded near %d", len(message), buildFailureMessageMax)
	}
}

func TestTailLines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		out   string
		limit int
		want  []string
	}{
		{name: "nothing at all", out: "", limit: 5},
		{name: "only blank lines", out: "\n\n\n", limit: 5},
		{
			name:  "the last of them, oldest first",
			out:   "one\ntwo\nthree\nfour\n",
			limit: 2,
			want:  []string{"three", "four"},
		},
		{
			name:  "carriage returns and trailing blanks removed",
			out:   "\nfirst\r\nsecond\r\n\n",
			limit: 5,
			want:  []string{"first", "second"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tailLines(tc.out, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("tailLines() = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The pod that failed is the one worth reading, even when the job left
// another behind.
func TestFailedPodPrefersTheOneThatFailed(t *testing.T) {
	pods := []corev1.Pod{
		{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}},
	}
	pod := failedPod(pods)
	if pod == nil || pod.Status.Reason != "Evicted" {
		t.Fatalf("failedPod() = %+v, want the evicted one", pod)
	}
	if failedPod(nil) != nil {
		t.Error("failedPod(nil) should be nil")
	}
}
