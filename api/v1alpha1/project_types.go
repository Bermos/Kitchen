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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GitSourceSpec links a Project to a repository via a gitSource Connection.
type GitSourceSpec struct {
	// Connection with the gitSource capability.
	ConnectionRef LocalObjectReference `json:"connectionRef"`

	// Repository in the provider's owner/name form.
	// +kubebuilder:validation:MinLength=1
	Repo string `json:"repo"`

	// Branch whose builds auto-promote to the production Environment.
	// +kubebuilder:default=main
	ProductionBranch string `json:"productionBranch,omitempty"`

	// RequirePullRequest refuses to build a commit on the production branch
	// that the git provider cannot say arrived through a reviewed pull
	// request.
	//
	// It is a project setting because the bar differs by what the project is:
	// the same platform carries an internal tool and a payments service, and
	// forcing one policy on both gets the strict one loosened. An environment
	// may demand more than this — a project's own baseline is a floor, not a
	// ceiling, and promotion checks again.
	//
	// It applies to the production branch only. A pull request's own builds
	// are what produces the request being reviewed, so requiring the review
	// before them would be a deadlock.
	//
	// **A machine identity on the platform's allowlist is exempt** — a
	// release automation commit is never going to have a reviewer, and the
	// alternative to an auditable allowlist is somebody turning this off.
	// +optional
	RequirePullRequest bool `json:"requirePullRequest,omitempty"`
}

// ProjectBuildSpec overrides platform build defaults for one project.
type ProjectBuildSpec struct {
	// +kubebuilder:default=auto
	Strategy BuildStrategy `json:"strategy,omitempty"`

	// Path to the Dockerfile, relative to RootDirectory. Used when the
	// strategy is (or resolves to) dockerfile.
	// +kubebuilder:default=Dockerfile
	DockerfilePath string `json:"dockerfilePath,omitempty"`

	// Directory within the repository to build from (monorepo support).
	// +kubebuilder:default=.
	RootDirectory string `json:"rootDirectory,omitempty"`
}

// RegistrySpec selects where built images are stored.
type RegistrySpec struct {
	// Connection with the imageStore capability.
	ConnectionRef LocalObjectReference `json:"connectionRef"`
}

// PreviewsSpec configures preview environments for pull requests.
type PreviewsSpec struct {
	// +kubebuilder:default=true
	Enabled bool `json:"enabled,omitempty"`

	// Protected gates preview URLs behind platform login: the Environment's
	// route goes through the forward-auth gate, which sends anonymous
	// requests to the identity provider and only proxies signed-in ones to
	// the application. The application itself needs no changes. Production
	// environments are never gated.
	//
	// It defaults to on, so unreleased work is not published to anyone who
	// guesses the URL. With no gate on the platform the Environment gets no
	// route at all rather than a public one — set this to false to serve
	// previews openly on purpose.
	// +kubebuilder:default=true
	// +optional
	Protected *bool `json:"protected,omitempty"`

	// Grace period before a preview Environment is torn down after its
	// pull request closes.
	// +optional
	TTLAfterClosed *metav1.Duration `json:"ttlAfterClosed,omitempty"`
}

// IsProtected reports whether previews of this Project are gated behind
// platform login. Previews written before the field existed are protected: it
// is the safe reading of an absent value, and the API server defaults it to
// true on the next write anyway.
func (p PreviewsSpec) IsProtected() bool {
	return p.Protected == nil || *p.Protected
}

// ScaleToZeroMode says which of a Project's environments may idle down to no
// pods at all.
// +kubebuilder:validation:Enum=previews;always;never
type ScaleToZeroMode string

const (
	// ScaleToZeroPreviews idles preview environments and leaves production
	// running. It is the default: an open pull request costs nothing while
	// nobody is looking at it, and the environment real users are on never
	// pays a cold start.
	ScaleToZeroPreviews ScaleToZeroMode = "previews"
	// ScaleToZeroAlways idles production too — the explicit opt-in without
	// which a production environment never drops below its replica count.
	ScaleToZeroAlways ScaleToZeroMode = "always"
	// ScaleToZeroNever keeps every environment of the Project on plain
	// Deployment routing.
	ScaleToZeroNever ScaleToZeroMode = "never"
)

// DefaultIdleAfter and DefaultMaxReplicas match the CRD defaults, for Projects
// written before the fields existed.
const (
	DefaultIdleAfter   = 5 * time.Minute
	DefaultMaxReplicas = int32(5)
)

// ScaleToZeroPolicy is a Project's idle policy: which of its environments may
// park at zero pods, how long they have to be quiet first, and how far a
// cold-started one may scale up.
//
// It is a policy of the Project rather than part of a Release's frozen
// runtime, deliberately. The snapshot exists so a rollback runs the exact code
// and capacity it ran before; whether an idle environment is allowed to park
// is a running-cost decision about the environment as it stands today.
// Rolling back should not quietly un-park an environment, and turning the
// policy on should not have to wait for the next build.
type ScaleToZeroPolicy struct {
	// +kubebuilder:default=previews
	// +optional
	Mode ScaleToZeroMode `json:"mode,omitempty"`

	// IdleAfter is how long an environment goes without a request before it
	// is scaled to zero. Shorter saves more and cold-starts more often.
	// +kubebuilder:default="5m"
	// +optional
	IdleAfter *metav1.Duration `json:"idleAfter,omitempty"`

	// MaxReplicas caps how far request pressure may scale an idling
	// environment up. It is raised to the environment's own replica count
	// where that is higher, so idling can never shrink an environment.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=5
	// +optional
	MaxReplicas *int32 `json:"maxReplicas,omitempty"`
}

// EffectiveMode reads the policy's mode. A Project written before the field
// existed idles its previews: the feature does nothing at all unless the
// platform turns it on, and a platform that has turned it on wants idle
// previews — that is what it is for.
func (p ScaleToZeroPolicy) EffectiveMode() ScaleToZeroMode {
	if p.Mode == "" {
		return ScaleToZeroPreviews
	}
	return p.Mode
}

// Covers reports whether an environment of this type may idle to zero.
func (p ScaleToZeroPolicy) Covers(envType EnvironmentType) bool {
	switch p.EffectiveMode() {
	case ScaleToZeroAlways:
		return true
	case ScaleToZeroNever:
		return false
	default:
		return envType == EnvironmentPreview
	}
}

// IdleAfterOrDefault is how long an environment stays quiet before it parks.
func (p ScaleToZeroPolicy) IdleAfterOrDefault() time.Duration {
	if p.IdleAfter != nil && p.IdleAfter.Duration > 0 {
		return p.IdleAfter.Duration
	}
	return DefaultIdleAfter
}

// MaxReplicasOrDefault is the ceiling a cold-started environment may reach.
func (p ScaleToZeroPolicy) MaxReplicasOrDefault() int32 {
	if p.MaxReplicas != nil && *p.MaxReplicas > 0 {
		return *p.MaxReplicas
	}
	return DefaultMaxReplicas
}

// ProjectSpec defines the desired state of a Project: a repository that
// becomes a running application.
type ProjectSpec struct {
	Source GitSourceSpec `json:"source"`

	// +optional
	Build ProjectBuildSpec `json:"build,omitempty"`

	Registry RegistrySpec `json:"registry"`

	// +optional
	Previews PreviewsSpec `json:"previews,omitempty"`

	// Which of this Project's environments may idle down to no pods at all,
	// cold-starting on the next request. It does nothing unless the platform
	// runs the machinery for it — `spec.scaleToZero.enabled` on the Kitchen
	// object — and every environment then stays on plain Deployment routing.
	// +kubebuilder:default={}
	// +optional
	ScaleToZero ScaleToZeroPolicy `json:"scaleToZero,omitempty"`

	// Access is who may do what with this Project, and it is the whole of the
	// answer: an account with no entry here holds no role on the Project at
	// all, so it is not in their project list and not theirs to build,
	// redeploy or read. The one exception is a platform operator, who holds
	// admin on every project, present and future — see
	// `spec.access.operators` on the Kitchen singleton.
	//
	// Membership lives here, on the object the access is about, rather than
	// in the identity provider or in a token claim. A role carried in a token
	// is a snapshot good for as long as the token is, which means removing
	// somebody leaves them on the project for up to an hour — and removal is
	// the case where that delay matters most. See docs/AUTH.md, "Where
	// membership lives".
	//
	// Entries merge per subject rather than by position (listType=map), so an
	// apply that adds one person cannot drop another.
	// +optional
	// +listType=map
	// +listMapKey=subject
	Access []AccessGrant `json:"access,omitempty"`

	// Environment variables, overlaid per environment type.
	// +optional
	// +listType=map
	// +listMapKey=name
	Env []EnvVar `json:"env,omitempty"`

	// +optional
	Runtime RuntimeSpec `json:"runtime,omitempty"`
}

// ProjectStatus defines the observed state of a Project.
type ProjectStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// The Project's production Environment, once created.
	// +optional
	ProductionEnvironmentRef *LocalObjectReference `json:"productionEnvironmentRef,omitempty"`

	// Most recently created Build.
	// +optional
	LatestBuildRef *LocalObjectReference `json:"latestBuildRef,omitempty"`

	// Provider-side identifier of the registered git webhook.
	// +optional
	WebhookID string `json:"webhookId,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.source.repo`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Project is the Schema for the projects API.
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectSpec   `json:"spec,omitempty"`
	Status ProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProjectList contains a list of Project.
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}
