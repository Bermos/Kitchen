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

// EnvironmentType distinguishes the single production Environment from
// ephemeral previews.
// +kubebuilder:validation:Enum=production;preview
type EnvironmentType string

const (
	EnvironmentProduction EnvironmentType = "production"
	EnvironmentPreview    EnvironmentType = "preview"
)

// PreviewInfo links a preview Environment to its pull request.
type PreviewInfo struct {
	// +kubebuilder:validation:Minimum=1
	PullRequest int32 `json:"pullRequest"`

	// +kubebuilder:validation:MinLength=1
	Branch string `json:"branch"`
}

// EnvironmentSpec defines a running instance of a Release with a URL.
// Rollback is changing ReleaseRef to an older Release.
type EnvironmentSpec struct {
	ProjectRef LocalObjectReference `json:"projectRef"`

	// +kubebuilder:default=production
	Type EnvironmentType `json:"type,omitempty"`

	ReleaseRef LocalObjectReference `json:"releaseRef"`

	// Required when Type is preview.
	// +optional
	Preview *PreviewInfo `json:"preview,omitempty"`
}

// EnvironmentPhase is the coarse lifecycle summary of an Environment.
// +kubebuilder:validation:Enum=Pending;Deploying;Live;Degraded;Terminating
type EnvironmentPhase string

const (
	EnvironmentPending     EnvironmentPhase = "Pending"
	EnvironmentDeploying   EnvironmentPhase = "Deploying"
	EnvironmentLive        EnvironmentPhase = "Live"
	EnvironmentDegraded    EnvironmentPhase = "Degraded"
	EnvironmentTerminating EnvironmentPhase = "Terminating"
)

// ReleaseMoveReason is how an Environment moved off a Release.
// +kubebuilder:validation:Enum=promoted;rolledBack;superseded
type ReleaseMoveReason string

const (
	// ReleaseMovePromoted: a fresh build's Release was auto-promoted over it.
	ReleaseMovePromoted ReleaseMoveReason = "promoted"
	// ReleaseMoveRolledBack: the Environment retreated to an older Release.
	ReleaseMoveRolledBack ReleaseMoveReason = "rolledBack"
	// ReleaseMoveSuperseded: another Release replaced it outside those two
	// flows — a manual move forward through the API, or a direct spec edit.
	ReleaseMoveSuperseded ReleaseMoveReason = "superseded"
)

// ReleaseHistoryEntry is one completed stint of a Release being current on
// this Environment: which Release, when it held the spec, and how and by whom
// it stopped being current.
type ReleaseHistoryEntry struct {
	// +kubebuilder:validation:MinLength=1
	Release string `json:"release"`

	// From is when the Release became current.
	From metav1.Time `json:"from"`

	// To is when it stopped being current.
	To metav1.Time `json:"to"`

	Reason ReleaseMoveReason `json:"reason"`

	// By names who caused the move: the API caller, or the Build whose
	// Release was promoted. Empty for moves made directly on the spec.
	// +optional
	By string `json:"by,omitempty"`
}

// MaxReleaseHistory bounds status.history; the timeline needs recency,
// not an archive.
const MaxReleaseHistory = 10

// GitReport is what the platform last told the git provider about this
// Environment: the deploy status on the commit's deployment, and the pull
// request comment carrying the preview URL.
//
// It is bookkeeping, in the same spirit as a Project's webhook ID: an
// Environment reconciles far more often than it changes, and without a record
// of what was already said, every pass would post the same deployment status
// again. Repeating a report is only ever noise — never a second deploy — so
// nothing here is load-bearing beyond keeping the platform quiet.
type GitReport struct {
	// Revision is the commit the report was about.
	// +optional
	Revision string `json:"revision,omitempty"`

	// State reported for the deployment, in the provider-neutral vocabulary
	// (in_progress, success, failure, inactive).
	// +optional
	State string `json:"state,omitempty"`

	// URL published with the report.
	// +optional
	URL string `json:"url,omitempty"`

	// CommentID is the provider-side ID of the pull request comment the
	// platform keeps up to date, so the next report rewrites that comment
	// instead of hunting for it — or appending a second one.
	// +optional
	CommentID string `json:"commentID,omitempty"`

	// Error is why the last attempt did not land, empty when it did. A
	// provider that refuses the report is recorded here and nowhere else:
	// posting is commentary on a deployment, never a condition of it.
	// +optional
	Error string `json:"error,omitempty"`

	// At is when the last attempt ran.
	// +optional
	At *metav1.Time `json:"at,omitempty"`
}

// Matches reports whether a report says the same thing about the same commit
// as the one already posted. The comment ID and timestamp are deliberately
// not compared: they are how the report was delivered, not what it said.
func (g *GitReport) Matches(other *GitReport) bool {
	if g == nil || other == nil {
		return false
	}
	return g.Revision == other.Revision &&
		g.State == other.State &&
		g.URL == other.URL &&
		g.Error == other.Error
}

// EnvironmentStatus defines the observed state of an Environment.
type EnvironmentStatus struct {
	// +optional
	Phase EnvironmentPhase `json:"phase,omitempty"`

	// Primary URL the Environment is reachable at.
	// +optional
	URL string `json:"url,omitempty"`

	// Release the running workload was last reconciled to.
	// +optional
	ObservedRelease string `json:"observedRelease,omitempty"`

	// History of Releases that stopped being current, newest first.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	History []ReleaseHistoryEntry `json:"history,omitempty"`

	// GitReport is what was last posted back to the git provider about this
	// Environment. Absent on a platform that reports nothing — a source
	// connection without the statusChecks capability.
	// +optional
	GitReport *GitReport `json:"gitReport,omitempty"`

	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// RecordReleaseMove prepends a history entry for the Release the Environment
// is moving off. The stint began where the previous one ended — or at the
// Environment's creation for the first. It reports whether it recorded
// anything: the newest entry already naming the outgoing Release means
// another writer got there first (the spec writer and the environment
// reconciler both call this for the same move), and an empty outgoing name
// means there is no stint to close.
func (e *Environment) RecordReleaseMove(outgoing string, reason ReleaseMoveReason, by string) bool {
	if outgoing == "" {
		return false
	}
	history := e.Status.History
	if len(history) > 0 && history[0].Release == outgoing {
		return false
	}
	from := e.CreationTimestamp
	if len(history) > 0 {
		from = history[0].To
	}
	history = append([]ReleaseHistoryEntry{{
		Release: outgoing,
		From:    from,
		To:      metav1.Now(),
		Reason:  reason,
		By:      by,
	}}, history...)
	if len(history) > MaxReleaseHistory {
		history = history[:MaxReleaseHistory]
	}
	e.Status.History = history
	return true
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.projectRef.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Environment is the Schema for the environments API.
type Environment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   EnvironmentSpec   `json:"spec,omitempty"`
	Status EnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// EnvironmentList contains a list of Environment.
type EnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Environment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Environment{}, &EnvironmentList{})
}
