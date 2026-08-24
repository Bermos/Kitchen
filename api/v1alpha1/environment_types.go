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

// EnvironmentRequirements is the bar this environment sets: a policy bundle,
// pinned by digest, that an artifact's attested evidence is judged against
// before a release may land here.
//
// The digest is the point. A bundle named by digest cannot drift under a
// decision that cited it, which is what makes every verdict reproducible:
// the same bundle, the same evidence, the same answer, however much later
// the question is asked again. Evaluation reads the attestations attached
// to the artifact — never a live check at promotion time.
type EnvironmentRequirements struct {
	// BundleDigest names the policy bundle by its sha256, in the
	// `sha256:<64 hex>` form every artifact digest already uses. Anything
	// else is refused at admission.
	// +kubebuilder:validation:Pattern=`^sha256:[a-f0-9]{64}$`
	BundleDigest string `json:"bundleDigest"`

	// Parameters reach the policy as input.parameters: the owner's tuning of
	// the bundle's rules — which gate to require, a severity ceiling —
	// without a bundle of their own for every environment.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
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

	// Owners name who may change this environment's Requirements. Entries
	// use the AccessSubject vocabulary: the issuer's `sub`, or an email
	// address — a value containing `@` is read as one, matched
	// case-insensitively and honoured only for a token whose address is
	// verified. Platform operators always may.
	//
	// This list is segregation of duties written as a schema: the team that
	// deploys into an environment holds project roles, the people who decide
	// what it demands are named here, and neither grants the other. An empty
	// or absent list leaves the requirements to the platform's operators
	// alone — the safe default, since an environment nobody owns must not be
	// one whoever deploys into it can lower the bar of.
	// +optional
	Owners []string `json:"owners,omitempty"`

	// Requirements is what an artifact must bring in order to land here,
	// declared by the environment's owners rather than by the project that
	// deploys into it. Absent means this environment declares no bar, which
	// is every environment's starting state and exactly today's behaviour.
	// +optional
	Requirements *EnvironmentRequirements `json:"requirements,omitempty"`

	// DataClass is the highest sensitivity class of data this environment is
	// rated to hold — its ceiling, not a label on its contents. At promotion
	// the policy engine refuses a project whose class exceeds it (rule
	// dataclass-le-environment), which includes a classified project landing
	// on an environment nobody has rated. Absent means unclassified,
	// surfaced as such and never defaulted. Like requirements, it is the
	// owners' declaration rather than the deploying team's, and it is
	// written through the same owner-gated endpoint.
	// +optional
	DataClass DataClass `json:"dataClass,omitempty"`

	// Residency declares where this environment's data is located — a
	// region or jurisdiction in the operator's own vocabulary ("CH",
	// "eu-central-1"). It is declared, not observed: the platform runs in
	// the one cluster it is installed into and cannot see past it, so this
	// records the answer the institution is accountable for. Absent falls
	// back to the platform default on the Kitchen object, and reads as
	// "unknown" in the inventory when neither is set. Provisioned resources
	// record their *actual* placement separately, on the claim's status.
	// +optional
	Residency string `json:"residency,omitempty"`
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

// RescanPhase is where one environment stands in the continuous
// re-evaluation pass.
// +kubebuilder:validation:Enum=Scanning;Evaluated;Failed
type RescanPhase string

const (
	// RescanScanning: a scanner pod is in flight over the deployed artifact.
	RescanScanning RescanPhase = "Scanning"
	// RescanEvaluated: the scan finished, its findings were signed onto the
	// artifact, and the environment's bar was re-run over them. Verdict says
	// what came out — including "blocked", which is a completed pass and not
	// a failed one.
	RescanEvaluated RescanPhase = "Evaluated"
	// RescanFailed: the pass could not be made. The scanner did not run, the
	// artifact carries no bill of materials to match against, the findings
	// could not be read back. Nothing is known either way, which is a
	// different thing from knowing the release is fine — the same distinction
	// a quality gate keeps between Completed and Failed.
	RescanFailed RescanPhase = "Failed"
)

// EnvironmentRescanStatus is what the last re-evaluation of this environment's
// deployed release found.
//
// Every field is about one (environment, release, moment) triple. A release
// that moves resets it: the answer was about the artifact that was running,
// and carrying it forward onto a new one would be reporting a scan that never
// happened.
type EnvironmentRescanStatus struct {
	// Phase is where the pass stands.
	// +optional
	Phase RescanPhase `json:"phase,omitempty"`

	// Release is the release that was scanned, and Artifact the
	// repository@digest it deploys. Both are recorded because a release can
	// be deleted and the digest is what the evidence is attached to.
	// +optional
	Release string `json:"release,omitempty"`
	// +optional
	Artifact string `json:"artifact,omitempty"`

	// JobName is the scanner Job in the application's namespace while one is
	// in flight. It is the sweep's own bookkeeping and is left behind after a
	// pass so that a person can look at what happened before the Job's TTL
	// collects it.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// StartedAt and FinishedAt time-stamp the scan itself. FinishedAt is what
	// the sweep counts the interval from, so a scan that never finishes
	// delays the next one rather than doubling it.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// DataSnapshot identifies the vulnerability database the findings were
	// produced against. It is the field that makes a scan reproducible: the
	// same artifact matched against the same snapshot yields the same
	// findings, and a decision that cannot name its snapshot can only be
	// repeated, never reproduced.
	// +optional
	DataSnapshot string `json:"dataSnapshot,omitempty"`

	// Findings is how many the scanner reported, after the platform's
	// normalization. Zero with an Evaluated phase means a clean scan; zero
	// with a Failed one means nothing was scanned.
	// +optional
	Findings int32 `json:"findings,omitempty"`

	// Verdict, UnmetRules and DecisionID are the re-evaluation's outcome and
	// the way back to the stored decision that holds its whole input.
	// +optional
	Verdict string `json:"verdict,omitempty"`
	// +optional
	// +listType=atomic
	UnmetRules []string `json:"unmetRules,omitempty"`
	// +optional
	DecisionID string `json:"decisionID,omitempty"`

	// Message is the one line a person reads.
	// +optional
	Message string `json:"message,omitempty"`
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

	// Rescan is where the continuous re-evaluation pass (#134) keeps its
	// place: what it last scanned, against which vulnerability database, and
	// what the environment's own bar said about the result. It is the
	// sweep's working state as much as its answer — the sweep is stateless
	// between passes and reads its next move off this.
	// +optional
	Rescan *EnvironmentRescanStatus `json:"rescan,omitempty"`

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
