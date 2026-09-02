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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConfigSnapshot is a frozen copy of a Project's deployable configuration at
// build time. Old Releases do not drift when the Project spec changes later,
// which is what makes rollback exact.
type ConfigSnapshot struct {
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// +optional
	Runtime RuntimeSpec `json:"runtime,omitempty"`

	// Processes are the project's workers and scheduled jobs as they stood
	// when this Release was built. They are here for the same reason the
	// environment variables are: a rollback that ran today's worker command
	// against yesterday's image would not be a rollback.
	// +optional
	// +listType=map
	// +listMapKey=name
	Processes []ProcessSpec `json:"processes,omitempty"`
}

// ReleaseSpec is an immutable snapshot: an image digest plus the configuration
// it should run with. Named Release (not Deployment) to avoid colliding with
// apps/v1 Deployment.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="Release spec is immutable"
type ReleaseSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	BuildRef LocalObjectReference `json:"buildRef"`

	// Image reference by digest, never by tag. It is the *web* process's
	// image, and the image every workload that declared no build of its own
	// runs — which for a project with one workload is all of them, and is
	// why the field is spelled as it always was.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Workloads are the images the other workloads of this unit were built
	// to, one entry per workload that declared a build of its own (#271).
	//
	// It is the half of "one commit, one coordinated release" that the
	// config snapshot cannot carry: the snapshot freezes what each workload
	// *is*, and this freezes what each workload *was built to*. Both are
	// needed for a rollback to be one — restoring a release has to bring
	// back the exact set of images that release declared, not the set the
	// project declares today, and a workload added since must not appear at
	// all.
	//
	// A workload named here and absent from the snapshot's process list is
	// nothing: the process list is what is materialized, and this only says
	// which image each entry of it runs.
	// +optional
	// +listType=map
	// +listMapKey=name
	Workloads []WorkloadImage `json:"workloads,omitempty"`

	// +optional
	ConfigSnapshot ConfigSnapshot `json:"configSnapshot,omitempty"`
}

// WorkloadImage is one workload's own image within a Release: which workload,
// and the digest reference it was built to.
type WorkloadImage struct {
	// Name is the process's, as the config snapshot spells it.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Image reference by digest, never by tag — the same rule the Release's
	// own image follows, for the same reason.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
}

// ImageFor is the image one workload of this Release runs: its own where it
// was built with one, and the Release's otherwise.
//
// Everything that materializes a workload asks this rather than reading
// `spec.image` — the web process included, which passes [WebProcessName] and
// gets the Release's image because a web process never has one of its own.
func (r *Release) ImageFor(workload string) string {
	for i := range r.Spec.Workloads {
		if r.Spec.Workloads[i].Name == workload {
			return r.Spec.Workloads[i].Image
		}
	}
	return r.Spec.Image
}

// ReleaseStatus defines the observed state of a Release.
type ReleaseStatus struct {
	// Environments currently running this Release (informational).
	// +optional
	Environments []string `json:"environments,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Build",type=string,JSONPath=`.spec.buildRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Release is the Schema for the releases API.
type Release struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReleaseSpec   `json:"spec,omitempty"`
	Status ReleaseStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ReleaseList contains a list of Release.
type ReleaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Release `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Release{}, &ReleaseList{})
}
