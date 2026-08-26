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
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// condStalled is set on a running Build whose Job is not moving. It is
	// deliberately not a failure: the Job may still be admitted a minute
	// later, and a build that recovers should not have been declared dead.
	// What it is for is that the reason exists somewhere the developer can
	// read it while the build still says Running.
	condStalled = "Stalled"

	// reasonJobNoPod is a build Job that has never had a pod. The pods were
	// refused at admission, or a quota refused them, or the API server
	// refused them for something the job-controller will keep retrying: in
	// every case the Job's own status stays empty and says nothing.
	reasonJobNoPod = "JobHasNoPod"

	// reasonJobProgressing clears the above once a pod exists.
	reasonJobProgressing = "JobProgressing"

	// reasonBuildStalled ends a build that never got a pod at all. It is not
	// reasonBuildFailed: nothing about the commit caused it, so it reports
	// on the commit as an error rather than as a failing build.
	reasonBuildStalled = "BuildStalled"

	// buildStallGrace is how long a build Job may exist with no pod before
	// the Build says so. Creating a pod is immediate on an idle cluster and
	// takes a moment on a busy one, so this is well past normal rather than
	// tight — what it looks for is a Job that is never going to create one.
	buildStallGrace = 2 * time.Minute

	// buildStallDeadline is how long that may go on before the build is
	// failed. A Job created with BackoffLimit 0 has no natural end when its
	// pods are refused before they exist: the job-controller retries the
	// creation with backoff forever, so without this the Build reports
	// Running for as long as somebody leaves it there.
	//
	// It is short enough that the FailedCreate event explaining it is still
	// in the cluster — events expire after an hour — and long enough that a
	// quota somebody is in the middle of raising is not a lost build.
	buildStallDeadline = 10 * time.Minute

	// buildRunningRequeue is how often a running build looks at its Job
	// again.
	//
	// The Job watch is enough for a Job that finishes, because finishing
	// writes a condition. It is not enough for one that never starts:
	// FailedCreate leaves job.Status entirely untouched — no pod counted,
	// no condition written — so nothing re-enqueues the Build, and the
	// running path used to return a bare Result and wait on a watch that
	// would never fire again.
	buildRunningRequeue = 30 * time.Second
)

// observeRunning is the running path of a build: the Job exists, it has not
// finished, and the only question left is whether it is actually moving.
//
// A Job that is moving needs nothing from this — it will finish, and finishing
// is what the watch is for. A Job with no pod is the case this exists for: it
// is reported on the Build after buildStallGrace and ends the build after
// buildStallDeadline, so that "Running" is never the last thing a build has to
// say for itself.
func (r *BuildReconciler) observeRunning(
	ctx context.Context,
	build *kitchenv1alpha1.Build,
	project *kitchenv1alpha1.Project,
	job *batchv1.Job,
) (ctrl.Result, error) {
	changed := build.Status.Phase != kitchenv1alpha1.BuildRunning
	build.Status.Phase = kitchenv1alpha1.BuildRunning

	noPod := jobHasNoPod(job)
	since := jobStalledSince(job)

	switch {
	case noPod && since >= buildStallDeadline:
		message := fmt.Sprintf("the build job created no pod in %s", since.Round(time.Second))
		if warning := latestWarning(ctx, r.APIReader, &job.ObjectMeta); warning != "" {
			message += ": " + warning
		}
		// Written down for the same reason a failed pod's account is: the
		// Job and the events explaining it are collected long before anybody
		// asks what happened, and the Build is what outlives them.
		build.Status.Failure = &kitchenv1alpha1.BuildFailureStatus{
			Reason:  reasonJobNoPod,
			Message: message,
		}
		return r.fail(ctx, build, project, reasonBuildStalled, message)
	case noPod && since >= buildStallGrace:
		changed = meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
			Type: condStalled, Status: metav1.ConditionTrue, Reason: reasonJobNoPod,
			Message:            r.stallMessage(ctx, job),
			ObservedGeneration: build.Generation,
		}) || changed
	case !noPod && meta.FindStatusCondition(build.Status.Conditions, condStalled) != nil:
		// Only once it has been said: a build that was never stalled does
		// not need a condition announcing it, and one that is still inside
		// the grace has not been called stalled yet.
		changed = meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
			Type: condStalled, Status: metav1.ConditionFalse, Reason: reasonJobProgressing,
			Message: "the build job has a pod", ObservedGeneration: build.Generation,
		}) || changed
	}

	result := ctrl.Result{RequeueAfter: buildRunningRequeue}
	if !changed {
		return result, nil
	}
	return result, r.Status().Update(ctx, build)
}

// stallMessage is what the Stalled condition says.
//
// It carries no elapsed time on purpose. The condition's own
// lastTransitionTime is when the stall started, and a message that counted
// upwards would differ on every pass — which is a status write every
// buildRunningRequeue for as long as the build is stuck.
func (r *BuildReconciler) stallMessage(ctx context.Context, job *batchv1.Job) string {
	if warning := latestWarning(ctx, r.APIReader, &job.ObjectMeta); warning != "" {
		return "the build job has created no pod: " + warning
	}
	return "the build job has created no pod, and nothing on the job says why"
}

// jobHasNoPod is a Job that has not got as far as a pod.
//
// It is read off the Job's own counters rather than by listing pods, because
// they are the thing that stays at zero: a pod refused at admission is never
// created, so it is counted nowhere and the rejection lands on the Job as an
// event instead. A pod that was created and has since gone still leaves
// Succeeded or Failed behind it.
func jobHasNoPod(job *batchv1.Job) bool {
	return job.Status.Active == 0 && job.Status.Succeeded == 0 && job.Status.Failed == 0
}

// jobStalledSince is how long the Job has had to create a pod in.
//
// It counts from the Job's start time where there is one — that is when the
// job-controller took it, and it is set whether or not a pod follows — and
// from creation otherwise, for the window between the two.
func jobStalledSince(job *batchv1.Job) time.Duration {
	from := job.CreationTimestamp.Time
	if job.Status.StartTime != nil {
		from = job.Status.StartTime.Time
	}
	if from.IsZero() {
		return 0
	}
	return time.Since(from)
}
