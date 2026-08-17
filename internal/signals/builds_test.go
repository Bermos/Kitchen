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

package signals

import (
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// build is one Build of the test project, created `age` ago.
func build(name string, phase kitchenv1alpha1.BuildPhase, age time.Duration) kitchenv1alpha1.Build {
	return kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(testNow.Add(-age)),
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
		},
		Status: kitchenv1alpha1.BuildStatus{Phase: phase},
	}
}

// succeeded is a finished build that took `took`, which is what the queue rule
// measures a wait against.
func succeeded(name string, age, took time.Duration) kitchenv1alpha1.Build {
	finished := build(name, kitchenv1alpha1.BuildSucceeded, age)
	started := metav1.NewTime(testNow.Add(-age))
	completed := metav1.NewTime(testNow.Add(-age + took))
	finished.Status.StartedAt = &started
	finished.Status.CompletedAt = &completed
	return finished
}

func TestBuildQueueFiresAgainstTheMedian(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Builds = []kitchenv1alpha1.Build{
		succeeded("build-1", 6*time.Hour, 2*time.Minute),
		succeeded("build-2", 5*time.Hour, 2*time.Minute),
		build("build-3", kitchenv1alpha1.BuildQueued, 40*time.Minute),
	}

	finding := expectOne(t, evaluate(t, SignalBuildQueueBackedUp, snapshot))
	expectDetail(t, finding, "a build here normally takes 2m")
	expectDetail(t, finding, "the oldest has waited 40m")
}

// A wait shorter than the floor is not a backlog, whatever the median is.
func TestBuildQueueRespectsTheFloor(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Builds = []kitchenv1alpha1.Build{
		succeeded("build-1", 6*time.Hour, 2*time.Second),
		build("build-2", kitchenv1alpha1.BuildQueued, time.Minute),
	}
	expectNone(t, evaluate(t, SignalBuildQueueBackedUp, snapshot))
}

// A fresh platform has no median, and every queued build would otherwise look
// stuck.
func TestBuildQueueOnAFreshPlatformUsesTheFloor(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Builds = []kitchenv1alpha1.Build{
		build("build-1", kitchenv1alpha1.BuildQueued, 20*time.Minute),
	}

	expectDetail(t, expectOne(t, evaluate(t, SignalBuildQueueBackedUp, snapshot)),
		"no completed build to compare against")
}

func TestBuildPodPendingFires(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Builds = []kitchenv1alpha1.Build{
		build("build-1", kitchenv1alpha1.BuildRunning, 10*time.Minute),
	}
	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "build-1-xyz",
			Namespace: controller.PlatformNamespace,
			Labels:    map[string]string{labelJobName: "build-1"},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:               corev1.PodScheduled,
				Status:             corev1.ConditionFalse,
				Reason:             "Unschedulable",
				Message:            "0/2 nodes are available: 2 Insufficient cpu",
				LastTransitionTime: metav1.NewTime(testNow.Add(-9 * time.Minute)),
			}},
		},
	}
	snapshot.Pods = []corev1.Pod{pod}

	finding := expectOne(t, evaluate(t, SignalBuildPodPending, snapshot))
	expectDetail(t, finding, "Insufficient cpu")
	expectDetail(t, finding, "nothing is executing")
	if finding.Fingerprint != "build.pod-pending/shop/build-1" {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
}

func TestBuildPodPendingStaysQuietWhileScheduled(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Builds = []kitchenv1alpha1.Build{
		build("build-1", kitchenv1alpha1.BuildRunning, 10*time.Minute),
	}
	snapshot.Pods = []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "build-1-xyz",
			Labels: map[string]string{labelJobName: "build-1"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}}
	expectNone(t, evaluate(t, SignalBuildPodPending, snapshot))
}

func TestBuildFailingRepeatedlyFires(t *testing.T) {
	snapshot := newSnapshot()
	for i := 0; i < 4; i++ {
		snapshot.Builds = append(snapshot.Builds, build(fmt.Sprintf("build-%d", i),
			kitchenv1alpha1.BuildFailed, time.Duration(i+1)*time.Hour))
	}

	finding := expectOne(t, evaluate(t, SignalBuildFailingRepeated, snapshot))
	if finding.Title != "4 builds in a row failed" {
		t.Fatalf("title = %q", finding.Title)
	}
	expectDetail(t, finding, "the project's configuration rather than its commits")
}

// The streak is counted from the present: a project that has recovered is not
// broken.
func TestBuildFailingRepeatedlyEndsAtTheLastSuccess(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Builds = []kitchenv1alpha1.Build{
		build("build-4", kitchenv1alpha1.BuildFailed, 4*time.Hour),
		build("build-3", kitchenv1alpha1.BuildFailed, 3*time.Hour),
		build("build-2", kitchenv1alpha1.BuildFailed, 2*time.Hour),
		succeeded("build-1", time.Hour, time.Minute),
	}
	expectNone(t, evaluate(t, SignalBuildFailingRepeated, snapshot))
}

// A queued build after three failures is a fourth attempt, not a recovery.
func TestBuildFailingRepeatedlySeesPastAQueuedBuild(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Builds = []kitchenv1alpha1.Build{
		build("build-4", kitchenv1alpha1.BuildQueued, time.Minute),
		build("build-3", kitchenv1alpha1.BuildFailed, time.Hour),
		build("build-2", kitchenv1alpha1.BuildFailed, 2*time.Hour),
		build("build-1", kitchenv1alpha1.BuildFailed, 3*time.Hour),
	}
	expectOne(t, evaluate(t, SignalBuildFailingRepeated, snapshot))
}

// A project that was broken last week is not broken now.
func TestBuildFailingRepeatedlyIgnoresOldHistory(t *testing.T) {
	snapshot := newSnapshot()
	for i := 0; i < 4; i++ {
		snapshot.Builds = append(snapshot.Builds, build(fmt.Sprintf("build-%d", i),
			kitchenv1alpha1.BuildFailed, BuildLookback+time.Duration(i)*time.Hour))
	}
	expectNone(t, evaluate(t, SignalBuildFailingRepeated, snapshot))
}
