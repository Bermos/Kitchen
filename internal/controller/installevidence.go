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

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Who installed a platform dependency is a fact about the cluster, not a
// memory.
//
// The operator installs two dependencies for installations that ask it to —
// KEDA with its HTTP add-on, and CloudNativePG — and records which of them it
// installed, because that record is what decides whether it may ever upgrade
// them. It used to be minted exactly once, in the reconcile that read the
// install job's completion, and read as gospel forever after: a fact
// derivable at one instant only, carried by a single status write. A write
// that does not land is then not "unknown" but the confident opposite — the
// platform disclaiming, permanently, a release it made minutes earlier,
// because every branch afterwards reads the record that overwrote it.
//
// The install job is the durable half of the same fact. This operator created
// it, in the platform namespace, labelled with what it installed and where,
// and it outlives by an hour the reconcile that read it. So ownership is
// re-derived from the job on every reconcile rather than remembered, which
// makes the disclaiming branch unreachable for as long as the evidence is
// there — and by the time the job is reaped, the record has been rewritten
// from it on every pass since.
const (
	// labelInstallVersion and labelInstallAddOnVersion are the chart versions
	// an install job installed. The job's *name* carries them too, but
	// sanitised into a DNS label and so not reversible; these are what they
	// were.
	labelInstallVersion      = "kitchen.bermos.dev/chart-version"
	labelInstallAddOnVersion = "kitchen.bermos.dev/addon-chart-version"

	// labelInstallNamespace is where it installed. Read rather than assumed,
	// because the namespace the singleton names today is not necessarily the
	// one the release went into.
	labelInstallNamespace = "kitchen.bermos.dev/install-namespace"
)

// installLabels adds what an install job installed to its labels, so that a
// later reconcile can tell without a record to read.
//
// A version is only labelled where it is a legal label value: a pin can be
// any string the chart handed the operator, and a job that could not be
// created at all would be a worse failure than one whose version has to be
// recovered from its name.
func installLabels(labels map[string]string, namespace string, versions map[string]string) map[string]string {
	if dnsLabel.MatchString(namespace) {
		labels[labelInstallNamespace] = namespace
	}
	for key, version := range versions {
		if version != "" && len(validation.IsValidLabelValue(version)) == 0 {
			labels[key] = version
		}
	}
	return labels
}

// latestCompletedInstall finds the newest install job of the platform's own
// making that ran to completion, for one dependency.
//
// Newest by completion, because a version bump is a new job rather than a
// rerun of a finished one: what is installed now is what the last job that
// finished installed. A failed job is not evidence — a helm run that died
// half-way may have applied nothing — and is left for runCNPGInstall and
// runKedaInstall to report.
func (r *KitchenReconciler) latestCompletedInstall(
	ctx context.Context,
	component string,
) (*batchv1.Job, error) {
	jobs := &batchv1.JobList{}
	if err := r.List(ctx, jobs,
		client.InNamespace(PlatformNamespace),
		client.MatchingLabels{labelManagedByKey: labelManagedByValue, labelComponentKind: component},
	); err != nil {
		return nil, err
	}

	var newest *batchv1.Job
	for i := range jobs.Items {
		job := &jobs.Items[i]
		// A job on its way out is not evidence of anything: the TTL reaper
		// takes finished jobs, and an installation clearing one out by hand
		// has said what it thinks of it.
		if job.DeletionTimestamp != nil {
			continue
		}
		if complete, _, _ := jobOutcome(job); !complete {
			continue
		}
		if newest == nil || installFinishedAt(job).After(installFinishedAt(newest).Time) {
			newest = job
		}
	}
	return newest, nil
}

// installFinishedAt is when a job finished, or failing that when it was
// created — a completed job without a completion time is not a state the API
// server produces, but ordering must not depend on that.
func installFinishedAt(job *batchv1.Job) metav1.Time {
	if job.Status.CompletionTime != nil {
		return *job.Status.CompletionTime
	}
	return job.CreationTimestamp
}

// installedInto is the namespace an install job installed into, as the job
// itself records it, falling back to what the caller believes where the job
// predates the label.
func installedInto(job *batchv1.Job, fallback string) string {
	if namespace := job.Labels[labelInstallNamespace]; namespace != "" {
		return namespace
	}
	return fallback
}

// installedVersion is a chart version an install job installed, as the job
// itself records it.
//
// A job created before the operator labelled them leaves it empty unless the
// job's name identifies it: the name is derived from the version, so a name
// matching the current pin can only have installed the current pin. Anything
// else stays unknown, which every caller reads as drift — and drift
// reinstalls at the pin and records what it installed, which is the recovery
// rather than a wrong answer.
func installedVersion(job *batchv1.Job, label, nameAtPin, versionAtPin string) string {
	if version := job.Labels[label]; version != "" {
		return version
	}
	if job.Name == nameAtPin {
		return versionAtPin
	}
	return ""
}
