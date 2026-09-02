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

	// Path to the Dockerfile, relative to RootDirectory, which it may not
	// leave. Used when the strategy is (or resolves to) dockerfile.
	// +kubebuilder:default=Dockerfile
	DockerfilePath string `json:"dockerfilePath,omitempty"`

	// DockerfileTarget is the stage of a multi-stage Dockerfile that
	// produces the image to run — BuildKit's `--target`. Empty is the file's
	// last stage, which is what every build shipped before this existed.
	//
	// It is a setting because the last stage is often not the one to run: a
	// file that also builds a test image, a toolchain or a migration runner
	// ends on whichever of them was written last, and a build of the wrong
	// one **succeeds**. Naming the stage is how a project says which
	// artifact it meant, and a name the Dockerfile does not have fails the
	// build rather than shipping something else.
	//
	// It means nothing to the buildpacks lifecycle, which has no stages at
	// all. A build that resolves to buildpacks with a target set fails
	// saying so rather than quietly ignoring it — a target silently dropped
	// is the same wrong image this exists to stop.
	// +kubebuilder:validation:Pattern=`^[A-Za-z][A-Za-z0-9_.-]*$`
	// +kubebuilder:validation:MaxLength=128
	// +optional
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`

	// RootDirectory is the build root: the directory within the repository
	// that is built (monorepo support), and the directory every path this
	// project declares is relative to — DockerfilePath, and the commit's own
	// kitchen.json. Nothing above it is part of the build.
	//
	// Both strategies mean the same directory by it and reach it
	// differently: BuildKit takes the commit as a git context and is handed
	// this directory as the whole of it, while the buildpacks lifecycle is
	// pointed at it inside a clone of the repository.
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
	// Enabled says whether a pull request against this Project gets a preview
	// environment of its own. It defaults to on: the preview is the review
	// vehicle, and a project that wants pull requests generally wants them
	// looked at somewhere.
	//
	// It is a pointer for the reason Protected next to it is: a plain bool
	// with omitempty cannot express false — the field is dropped from the
	// serialized object, and the API server then applies the default, so
	// turning previews off would be silently undone on every write.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

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

// IsEnabled reports whether this Project gets preview environments. Previews
// written before the field existed get them: it is the reading that matches
// the default, and the API server defaults it to true on the next write
// anyway.
func (p PreviewsSpec) IsEnabled() bool {
	return p.Enabled == nil || *p.Enabled
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

// PromotionStage is one rung of a project's promotion ladder: a name for the
// rung, the Environment that is the rung, and whether an artifact climbs onto
// the next one by itself.
type PromotionStage struct {
	// Name of the stage — "staging", "production" — a DNS label so it can
	// appear in generated names.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +kubebuilder:validation:MaxLength=63
	Name string `json:"name"`

	// Environment names the Environment this stage deploys into. It must
	// belong to this project — validated where the stages are written and
	// where they are acted on, since a cross-object rule cannot live in CEL.
	// The first build for a stage creates the Environment if it is missing.
	// +kubebuilder:validation:MinLength=1
	Environment string `json:"environment"`

	// AutoPromote creates the next Promotion into this stage automatically
	// when the stage before it applies successfully. Whether that promotion
	// then goes through is this stage's environment's own requirements —
	// evidence-gating an automatic promotion is a rule on the environment
	// (a required gate, a severity ceiling), not a second mechanism here.
	// +optional
	AutoPromote bool `json:"autoPromote,omitempty"`
}

// PromotionPolicySpec is a project's ordered stages. One artifact — the same
// image digest, never rebuilt — moves through them in order, judged at each
// boundary by that environment's requirements. The last stage is the
// production environment. No stages means today's behaviour: a
// production-branch build targets `<project>-production` directly.
type PromotionPolicySpec struct {
	// Stages, in promotion order. The order is the pipeline, so this is a
	// plain ordered list rather than a merge-by-name map.
	// +kubebuilder:validation:MinItems=1
	Stages []PromotionStage `json:"stages"`
}

// StageIndex answers which stage deploys into the named environment, or -1.
func (p *PromotionPolicySpec) StageIndex(environment string) int {
	if p == nil {
		return -1
	}
	for i := range p.Stages {
		if p.Stages[i].Environment == environment {
			return i
		}
	}
	return -1
}

// NextStage is the stage after the one deploying into the named environment,
// or nil when that stage is the last — or not a stage at all.
func (p *PromotionPolicySpec) NextStage(environment string) *PromotionStage {
	index := p.StageIndex(environment)
	if index < 0 || index+1 >= len(p.Stages) {
		return nil
	}
	return &p.Stages[index+1]
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

	// Promotion is the project's staged pipeline, absent for the default
	// build-straight-to-production flow. The stages are topology — which
	// environments, in which order — and deliberately not policy: what each
	// environment demands lives on the Environment (spec.requirements), owned
	// by its owners, so the team that arranges the pipeline cannot lower any
	// stage's bar by rearranging it.
	// +optional
	Promotion *PromotionPolicySpec `json:"promotion,omitempty"`

	// DataClass is the sensitivity classification of the data this project
	// handles: public, internal, confidential or strictlyConfidential, in
	// that order. Absent means unclassified — surfaced as such, never
	// defaulted. It is the parent of the classification hierarchy: a claim's
	// class may not exceed it (checked at the API), and at promotion the
	// policy engine checks it does not exceed the target environment's
	// (rule dataclass-le-environment). Reclassifying a project is always
	// possible — it makes environments below the new class non-compliant,
	// which the drift and inventory views surface rather than the API
	// refusing the correction.
	// +optional
	DataClass DataClass `json:"dataClass,omitempty"`

	// Criticality is how much it matters that this project's function keeps
	// working: nonCritical, important or critical. Absent means undesignated
	// — surfaced as such, never defaulted, because Kitchen does not decide
	// what is critical and must not appear to have. It is the institution's
	// input; what the platform does with it is map it onto the resources
	// that serve the function (GET /compliance/criticality) and let it
	// decide how loudly the environments running it alert.
	//
	// A project's designation reaches its *production* environments as a
	// fallback and reaches its previews not at all — see
	// [EffectiveContinuity] for why criticality does not inherit the way a
	// data class does.
	// +optional
	Criticality Criticality `json:"criticality,omitempty"`

	// RTO is the recovery time objective: how long this project's function
	// may be unavailable before the institution's own tolerance is breached.
	// A Go duration of whole hours and minutes — "4h", "30m". Absent means
	// no tolerance has been set, which is not the same as zero.
	// +optional
	RTO Tolerance `json:"rto,omitempty"`

	// RPO is the recovery point objective: how much data this project's
	// function may lose. Same spelling as RTO.
	//
	// Kitchen carries and maps it and does not yet alert on it, because the
	// platform observes no recovery points to measure it against — see
	// docs/OBSERVABILITY.md, "What an RPO would take".
	// +optional
	RPO Tolerance `json:"rpo,omitempty"`

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

	// Processes are the project's other processes: the queue workers and the
	// scheduled jobs that share the web process's image and environment and
	// are started differently. The web process itself is `spec.runtime` and
	// is not listed here — see [ProcessType] for why it cannot be.
	//
	// The list is snapshotted into every Release, so a rollback runs the
	// processes that release declared rather than the ones the project
	// declares now. Removing a process from the list is how it is deleted:
	// the environment reconciler tears down anything it materialized that
	// the current Release no longer names.
	//
	// Entries merge per name rather than by position (listType=map), so two
	// people adding two workers do not drop each other's.
	// +optional
	// +listType=map
	// +listMapKey=name
	Processes []ProcessSpec `json:"processes,omitempty"`
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

	// InitialBuildRef is the Build the platform created for the project
	// itself rather than for a push: the production branch as it stood when
	// the project was created, so that connecting a repository deploys it
	// instead of waiting for somebody to commit something.
	//
	// It records that the seeding happened, not that the Build still exists.
	// A first build somebody deleted stays deleted and the next push is what
	// builds the project after that — the same reading as the platform's
	// seeded registry Connection.
	// +optional
	InitialBuildRef *LocalObjectReference `json:"initialBuildRef,omitempty"`
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
