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
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func terminatedStatus(name string, exit int32, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exit, Reason: reason}},
	}
}

func waitingStatus(name, reason, message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
	}
}

// A pod is read for the container with the most to answer for, not for the
// first one that has anything to say. The build pod is the case: its clone
// runs first and succeeds, so reading in order reports "exit code 0" beside a
// phase of Failed.
func TestPodMessagePicksTheFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{
			name: "a builder that failed behind a clone that did not",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				InitContainerStatuses: []corev1.ContainerStatus{terminatedStatus("clone", 0, "Completed")},
				ContainerStatuses:     []corev1.ContainerStatus{terminatedStatus("creator", 51, "Error")},
			}},
			want: "creator: Error: exit code 51",
		},
		{
			name: "an init container that failed, ahead of a container still waiting on it",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				InitContainerStatuses: []corev1.ContainerStatus{terminatedStatus("clone", 128, "Error")},
				ContainerStatuses:     []corev1.ContainerStatus{waitingStatus("creator", "PodInitializing", "")},
			}},
			want: "clone: Error: exit code 128",
		},
		{
			name: "a pod ended from outside, whose containers all succeeded",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodFailed,
				Reason:            "Evicted",
				Message:           "the node was low on ephemeral-storage",
				ContainerStatuses: []corev1.ContainerStatus{terminatedStatus("creator", 0, "Completed")},
			}},
			want: "Evicted: the node was low on ephemeral-storage",
		},
		{
			name: "a pod nothing will schedule",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
					Reason: "Unschedulable", Message: "0/3 nodes are available",
				}},
				ContainerStatuses: []corev1.ContainerStatus{waitingStatus("creator", "PodInitializing", "")},
			}},
			want: "Unschedulable: 0/3 nodes are available",
		},
		{
			// One container names nothing: the name would be the pod's own.
			name: "an image that will not pull",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{waitingStatus("app", "ImagePullBackOff", "back-off pulling image")},
			}},
			want: "ImagePullBackOff: back-off pulling image",
		},
		{
			name: "a pod that is simply running",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "app", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			}},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podMessage(&tc.pod); got != tc.want {
				t.Errorf("podMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Restarts are counted across every container a pod has, init containers
// included: a sidecar-shaped init container that crash-loops is a restarting
// pod however its own container reads.
func TestPodViewCountsEveryContainersRestarts(t *testing.T) {
	pod := corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodRunning,
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: "clone", RestartCount: 2,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "Completed"}},
		}},
		ContainerStatuses: []corev1.ContainerStatus{{Name: "creator", RestartCount: 3}},
	}}
	if got := newPodView(&pod).Restarts; got != 5 {
		t.Errorf("restarts = %d, want 5", got)
	}
}
