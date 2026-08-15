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

// PlatformUpdateSpec asks the platform to upgrade its own Helm release.
//
// The spec is immutable, like a Build's: an upgrade that failed is a new
// PlatformUpdate, never an edit of the one that failed, because the cluster
// it would be retried against is no longer the cluster it was admitted for.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="PlatformUpdate spec is immutable"
type PlatformUpdateSpec struct {
	// Version to upgrade to, as a bare SemVer string with no leading "v".
	//
	// One number covers the whole platform: a release publishes the chart and
	// both images together, and the chart's image.tag defaults to its
	// appVersion, so naming the chart version names the operator that comes
	// with it. It is deliberately the only field — the update job builds its
	// own helm invocation and takes no values from here, because an upgrade
	// that accepted values would be a way to run anything at all as the
	// cluster-admin the job is bound to.
	// +kubebuilder:validation:Pattern=`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$`
	Version string `json:"version"`
}

// PlatformUpdatePhase is the coarse lifecycle summary of a PlatformUpdate.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type PlatformUpdatePhase string

const (
	// PlatformUpdatePending is an update that has not started: it is waiting
	// for another update to finish, or it failed its preflight checks before
	// anything was applied to the cluster.
	PlatformUpdatePending PlatformUpdatePhase = "Pending"
	// PlatformUpdateRunning has a helm job in flight.
	PlatformUpdateRunning PlatformUpdatePhase = "Running"
	// PlatformUpdateSucceeded completed the helm upgrade.
	PlatformUpdateSucceeded PlatformUpdatePhase = "Succeeded"
	// PlatformUpdateFailed did not. Whether anything was applied depends on
	// how far the job got; the job runs with --atomic, so a failure that helm
	// itself observed has been rolled back.
	PlatformUpdateFailed PlatformUpdatePhase = "Failed"
)

// PlatformUpdateStatus defines the observed state of a PlatformUpdate.
//
// Everything here is derived from objects that outlive the operator, because
// the operator does not outlive the upgrade: applying the new manager
// Deployment terminates the pod that started the job. The reconciler that
// reports an upgrade succeeded is a different process — usually a different
// version — from the one that launched it, so it reconstructs the outcome
// from the Job rather than from anything it remembers.
type PlatformUpdateStatus struct {
	// +optional
	Phase PlatformUpdatePhase `json:"phase,omitempty"`

	// FromVersion is what the platform was running when the upgrade started.
	// +optional
	FromVersion string `json:"fromVersion,omitempty"`

	// JobName is the helm job in the platform namespace, for as long as it
	// exists — it is reaped some time after it finishes, while this record
	// stays.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Message explains a Pending or Failed update in the terms of whoever
	// pressed the button: which value to set, which command to run.
	// +optional
	Message string `json:"message,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.status.fromVersion`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PlatformUpdate is one attempt to upgrade Kitchen's own Helm release.
//
// It is cluster-scoped because the thing it upgrades is: there is one release,
// installed once, and the platform namespace it lives in is compiled in. The
// objects are kept after they finish, so the list is the installation's
// upgrade history.
type PlatformUpdate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformUpdateSpec   `json:"spec,omitempty"`
	Status PlatformUpdateStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PlatformUpdateList contains a list of PlatformUpdate.
type PlatformUpdateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlatformUpdate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PlatformUpdate{}, &PlatformUpdateList{})
}
