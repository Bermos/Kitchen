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

// ProjectSourceSpec is where a Project's software comes from, and it is a
// union: exactly one member is set (#307).
//
// Until this existed the answer was always a repository, and a Project's
// `spec.source` *was* the [GitSourceSpec]. That is the first member, and it
// is unchanged in every respect but where it sits. The second is an image
// somebody else built — the home lab's Home Assistant, an upstream service a
// team runs but does not develop — which has no commit for anything upstream
// of a Release to key on.
//
// It is a union rather than two optional fields with a rule about them
// because the two are answers to one question, and a project that set both
// would be saying its web process is two things. Exactly one is set: a
// project always has a web process, and a web process always comes from
// somewhere.
//
// What follows from which member is set is the whole of the difference
// upstream of a Release. A repository is built, so it needs a `registry` to
// push to, produces a Build with a builder Job, and has pull requests to
// preview. An image is acquired, so it needs no registry at all, produces a
// Build that resolves a digest and runs no builder, and has no pull requests
// — which the Project says in words rather than by previews quietly never
// appearing.
//
// +kubebuilder:validation:XValidation:rule="has(self.git) != has(self.image)",message="a project's source is one thing: a repository this platform builds (`git`), or an image somebody else built (`image`) — set exactly one of them"
type ProjectSourceSpec struct {
	// Git is a repository this platform builds.
	// +optional
	Git *GitSourceSpec `json:"git,omitempty"`

	// Image is the web process's image, published by somebody else. The
	// project's other workloads declare their own on [ProcessSpec.Image];
	// this is the web process's, which is `spec.runtime` and has no entry in
	// the process list to declare one on.
	// +optional
	Image *ImageSourceSpec `json:"image,omitempty"`
}

// HasRepository reports whether this project is built from a repository. It
// is the question every path upstream of a Release asks — the webhook, the
// first build, the commit status reporter, previews — and it is asked of the
// source rather than of the presence of a field somewhere, so that there is
// one answer.
func (s ProjectSourceSpec) HasRepository() bool { return s.Git != nil }

// GitSource is the repository this project is built from, zero for a project
// that has none.
//
// It answers a value rather than the pointer so that a reader which has
// already established there is a repository — the whole of the build path —
// does not restate the check, and one that has not gets an empty repository
// and an empty branch rather than a panic. `HasRepository` is the question
// when the answer matters.
func (s ProjectSourceSpec) GitSource() GitSourceSpec {
	if s.Git == nil {
		return GitSourceSpec{}
	}
	return *s.Git
}

// ImageSource is the web process's vendored image, zero for a project built
// from a repository. It reads the way GitSource does, for the same reason.
func (s ProjectSourceSpec) ImageSource() ImageSourceSpec {
	if s.Image == nil {
		return ImageSourceSpec{}
	}
	return *s.Image
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

	// Max is this Project's own ceiling on live preview Environments,
	// overriding the platform's `spec.previews.maxPerProject`. A pull
	// request that would exceed it gets no preview, and says so on the
	// request rather than on the node (#294).
	//
	// Unset inherits the platform's, which is what almost every project
	// should do: the ceiling is a fact about the cluster, and a project
	// raising its own is a project taking room from its neighbours. It is
	// here because "almost" is doing real work in that sentence — the one
	// project a platform exists for, with twelve reviewers and no claims, is
	// exactly the project whose ceiling should differ from the estate's.
	//
	// `0` is no ceiling at all for this project. Nil is the platform's, and
	// the two are told apart by the pointer for the reason every other
	// clearable number here is a pointer: an int32 with `omitempty`
	// serializes 0 as an absent field.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Max *int32 `json:"max,omitempty"`

	// Forks says what a pull request opened from a *fork* gets — a pull
	// request whose head commit lives in some repository other than this
	// project's own.
	//
	// It defaults to `none`, and that default is the whole of issue #422.
	// Nothing in the platform used to have a concept of a fork: the head SHA
	// of a stranger's pull request against a public repository is reachable
	// in the project's own repository through `refs/pull/N/head`, so it built
	// with the project's registry credential and got a preview environment
	// carrying the project's own secrets and, for a claim in preview mode
	// `shared`, production's binding. `protected` did not help — it gates
	// *viewing* a preview, not building the commit or starting the pod that
	// holds the secrets.
	//
	// The three values are what a fork may be given, least to most:
	//
	//   - `none` builds nothing. The pull request is told so where its author
	//     is looking: a `kitchen/<project>/preview` commit status and the
	//     preview comment, the same pair the preview ceiling refuses with.
	//   - `build` builds the fork's commit and stops there — no Environment,
	//     so no project secret, no project variable and no claim binding ever
	//     reaches the fork's code. It answers "does this pull request even
	//     compile" without answering it with a credential.
	//   - `full` treats the fork exactly as the project's own branch, preview
	//     and secrets included. It is for a repository whose forks are its
	//     maintainers' own — and it is the setting to be sure about, because
	//     it is the behaviour this field exists to stop being the default.
	//
	// The platform bounds it: `spec.previews.forksMax` on the Kitchen
	// singleton is the most any project may ask for, and a project asking for
	// more gets the platform's answer. See ForksUnder.
	// +kubebuilder:default=none
	// +optional
	Forks ForkPolicy `json:"forks,omitempty"`

	// Grace period before a preview Environment is torn down after its
	// pull request closes.
	// +optional
	TTLAfterClosed *metav1.Duration `json:"ttlAfterClosed,omitempty"`
}

// ForkPolicy is what a pull request from a fork is given: nothing, a build, or
// everything the project's own branches get (#422).
//
// The three are ordered, least to most, and that ordering is the whole reason
// the platform's ceiling is spelled the same way rather than as a boolean.
// One vocabulary reads in one sentence — "the project asks for `full`, the
// platform allows at most `build`, so it gets `build`" — where a boolean
// beside an enum would have to be read together anyway and still could not say
// the stricter thing an operator worried enough to set anything is most likely
// to want: no fork builds at all, estate-wide.
// +kubebuilder:validation:Enum=none;build;full
type ForkPolicy string

const (
	// ForkPolicyNone gives a fork pull request nothing at all: no Build, no
	// Release, no preview Environment. It is the default, and the honest one
	// — it is what every hosted CI does with a first-time contributor.
	ForkPolicyNone ForkPolicy = "none"
	// ForkPolicyBuild builds the fork's commit and publishes no environment.
	// The build's own push credential is still a build's own push credential
	// — that is what a build is — but nothing the *project* configured is
	// handed to the fork's code, because nothing runs it.
	ForkPolicyBuild ForkPolicy = "build"
	// ForkPolicyFull treats the fork as the project's own branch. Everything
	// a preview of the project's own commit gets, a fork's gets.
	ForkPolicyFull ForkPolicy = "full"
)

// forkPolicyOrder ranks the three so a ceiling can be applied to a request.
// An unrecognized value — an empty string on an object written before the
// field existed, or a spelling a newer operator wrote and this one has never
// heard of — ranks with `none`: the safe reading is the only one a security
// default may take.
func forkPolicyOrder(p ForkPolicy) int {
	switch p {
	case ForkPolicyFull:
		return 2
	case ForkPolicyBuild:
		return 1
	default:
		return 0
	}
}

// Normalized is the policy as a reader should apply it: one of the three
// words, with anything else — including the empty string an object written
// before the field existed carries — read as `none`.
func (p ForkPolicy) Normalized() ForkPolicy {
	switch p {
	case ForkPolicyFull:
		return ForkPolicyFull
	case ForkPolicyBuild:
		return ForkPolicyBuild
	default:
		return ForkPolicyNone
	}
}

// AtMost is this policy bounded by a ceiling: the lesser of the two.
func (p ForkPolicy) AtMost(ceiling ForkPolicy) ForkPolicy {
	if forkPolicyOrder(ceiling) < forkPolicyOrder(p) {
		return ceiling.Normalized()
	}
	return p.Normalized()
}

// BuildsForks reports whether a fork's commit is built at all.
func (p ForkPolicy) BuildsForks() bool { return forkPolicyOrder(p) >= forkPolicyOrder(ForkPolicyBuild) }

// PreviewsForks reports whether a fork's pull request gets a preview
// environment — which is also whether it is handed the project's secrets,
// since an environment is how they reach anything.
func (p ForkPolicy) PreviewsForks() bool { return p.Normalized() == ForkPolicyFull }

// ForksUnder is the fork policy in force for this Project: what it asks for,
// never more than the platform's ceiling allows.
func (p PreviewsSpec) ForksUnder(ceiling ForkPolicy) ForkPolicy {
	return p.Forks.AtMost(ceiling)
}

// MaxOrPlatform is the ceiling in force for this Project: its own where it
// has one, the platform's otherwise. 0 is no ceiling.
func (p PreviewsSpec) MaxOrPlatform(platform int32) int32 {
	if p.Max == nil {
		return platform
	}
	if *p.Max < 0 {
		return 0
	}
	return *p.Max
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
//
// It is a policy of the Project rather than of each workload for a second
// reason, which is the mechanism: only the web process is idled. docs/CRDS.md
// says why a mixed posture is not expressible and what to do instead.
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

// ProjectSpec defines the desired state of a Project: software that becomes a
// running application.
//
// It was "a repository that becomes a running application" until #307, and
// the two rules below are the whole of what changed about the *project* when
// it stopped having to be one. Each refuses a combination that would
// otherwise be a setting nothing reads:
//
//   - A registry is where builds are *pushed*, so a project that builds
//     needs one and a project that builds nothing has no use for one. It is
//     tied to the source rather than left optional in both directions,
//     because a registry on a vendored project reads as the place its image
//     comes from and is not.
//   - A workload built from the repository needs a repository. This is the
//     one cross-field rule a workload's own spec cannot state: `spec.image`
//     and `spec.build` exclude each other there, but neither knows whether
//     the project has a repository at all.
//
// Previews are the one refusal that is deliberately *not* here. They follow
// pull requests and an image source has none, but `previews.enabled` defaults
// to true, so an admission rule against it would refuse every vendored
// project ever written — including one that never mentioned previews. The
// refusal is the API's, where a person asking for them can be answered, and
// the Project's `Previews` condition, which says the same thing to one who
// merely inherited the default. A preview that silently never appears reads
// as a fault; a condition saying why does not.
//
// +kubebuilder:validation:XValidation:rule="has(self.source.git) == has(self.registry)",message="a registry is where built images are pushed: a project built from a repository needs one, and a project whose source is an image builds nothing and has nothing to push there"
// +kubebuilder:validation:XValidation:rule="has(self.source.git) || !has(self.processes) || !self.processes.exists(p, has(p.build))",message="a workload built from the repository needs a repository: this project's source is an image, so every workload of it runs an image somebody else built (`image`)"
type ProjectSpec struct {
	Source ProjectSourceSpec `json:"source"`

	// +optional
	Build ProjectBuildSpec `json:"build,omitempty"`

	// Registry is where this project's builds push their images: a
	// Connection with the imageStore capability. It is required of a project
	// built from a repository and refused of one whose source is an image,
	// which builds nothing — see the rules on this type.
	//
	// It is not the credential a vendored image is *pulled* with. That is
	// named on the image itself ([ImageSourceSpec.ConnectionRef]), because a
	// vendor's registry is somewhere the platform never writes and often
	// somewhere it needs no account at all.
	// +optional
	Registry *RegistrySpec `json:"registry,omitempty"`

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
	//
	// Idling is the web process's: the signal behind scale to zero is request
	// pressure at the KEDA interceptor, and the interceptor sits on the
	// environment's public route, where the web process is the only thing
	// behind it. So it is the web Deployment that parks and the web Deployment
	// a request wakes; a worker, a service, a cron and a task are left running
	// whatever the mode says.
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

	// Files are the configuration files this project's workloads are handed
	// — software the platform did not build is configured by one, and the
	// platform had only variables (#311). Each names its content, the path
	// it is mounted at, and the workloads that read it.
	//
	// They are here beside the variables rather than in a volume because
	// that is what they are: configuration, small, changing with a deploy,
	// and snapshotted into every Release so that a rollback restores the
	// file that release ran with.
	//
	// A file marked `secret` carries no content here. The platform holds it
	// where nothing reads it back, and this list carries the declaration —
	// see [ConfigFile].
	//
	// Entries merge per name rather than by position (listType=map), so two
	// people adding two files do not drop each other's.
	// +optional
	// +listType=map
	// +listMapKey=name
	Files []ConfigFile `json:"files,omitempty"`
}

// RegistryConnection is the Connection this project pushes its builds to,
// empty for a project that builds nothing. It is the one spelling of that
// question, for the same reason `registrySecretName` is the one spelling of
// the Secret's name: two readings of "which registry" is how a build and a
// pull come to disagree.
func (p ProjectSpec) RegistryConnection() string {
	if p.Registry == nil {
		return ""
	}
	return p.Registry.ConnectionRef.Name
}

// Builds reports whether anything of this project is built by the platform.
// It is the source's question and not the registry's: a project is built
// exactly when it has a repository to build from.
func (p ProjectSpec) Builds() bool { return p.Source.HasRepository() }

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

	// ImagePoll is what the digest poll last did for this project (#308):
	// when it last asked the registry whether a watched tag had moved, and
	// what stopped it where something did.
	//
	// It is on the status rather than held in the operator's memory for two
	// reasons, and the second is the one that matters. The interval survives
	// a restart, so an operator rolled out at noon does not re-ask every
	// registry the estate follows at the same instant. And a poll that
	// cannot read a registry says so *here*, once, which is what keeps a
	// registry that stays down from producing one failed Build every
	// interval for a week.
	// +optional
	ImagePoll *ImagePollStatus `json:"imagePoll,omitempty"`

	// Previews is what the preview ceiling (#294) is doing to this project:
	// how many previews are live, what the ceiling in force is, and which
	// pull requests were refused one while the project sat at it.
	//
	// It is on the status because a refusal that only exists as a comment on
	// a pull request is a refusal the dashboard, the CLI and the next person
	// to ask "why has this request no preview" cannot see.
	// +optional
	Previews *PreviewCapacityStatus `json:"previews,omitempty"`
}

// PreviewCapacityStatus is the project's preview ceiling as the platform last
// measured it.
type PreviewCapacityStatus struct {
	// Live is how many preview Environments of this project exist and are
	// not being torn down. Production and promoted environments are not
	// counted — the ceiling is on the ephemeral half.
	Live int32 `json:"live"`

	// Max is the ceiling in force: the project's own `spec.previews.max`, or
	// the platform's `spec.previews.maxPerProject`. `0` is no ceiling.
	Max int32 `json:"max"`

	// Refused are the pull requests that asked for a preview while the
	// project was at its ceiling, oldest first.
	//
	// It is a record, not a queue. Nothing here is retried on a timer: a
	// refused request gets its preview on its next push, once a slot has
	// been freed by another preview closing. A queue would have the platform
	// deploy a commit nobody asked it to deploy, minutes or days after the
	// push, which is worse than the wait.
	// +optional
	// +kubebuilder:validation:MaxItems=20
	Refused []RefusedPreview `json:"refused,omitempty"`
}

// RefusedPreview is one pull request that was refused a preview environment.
type RefusedPreview struct {
	// PullRequest is the request's number at the git provider.
	PullRequest int32 `json:"pullRequest"`

	// Commit is the revision that was refused, so that a push after the
	// refusal is visibly a different one.
	// +optional
	Commit string `json:"commit,omitempty"`

	// At is when the refusal was recorded, refreshed on every push that is
	// refused again.
	At metav1.Time `json:"at"`
}

// MaxRefusedPreviews bounds the record above. Twenty is far past a
// single-node cluster's plausible backlog, and the bound exists so that a
// project left at its ceiling for a month cannot grow its own status without
// end.
const MaxRefusedPreviews = 20

// RecordRefusedPreview puts a pull request on the refusal list, or refreshes
// the entry it already has. It reports whether anything changed, so a
// reconcile that refuses the same commit twice writes no status.
//
// The oldest entry is dropped when the list is full: a refusal nobody has
// pushed to in twenty requests' time has been overtaken, and the record is
// there to explain the previews that are missing now.
func (s *PreviewCapacityStatus) RecordRefusedPreview(pullRequest int32, commit string, at metav1.Time) bool {
	for i := range s.Refused {
		if s.Refused[i].PullRequest != pullRequest {
			continue
		}
		if s.Refused[i].Commit == commit {
			return false
		}
		s.Refused[i].Commit = commit
		s.Refused[i].At = at
		return true
	}
	s.Refused = append(s.Refused, RefusedPreview{PullRequest: pullRequest, Commit: commit, At: at})
	if len(s.Refused) > MaxRefusedPreviews {
		s.Refused = s.Refused[len(s.Refused)-MaxRefusedPreviews:]
	}
	return true
}

// ClearRefusedPreview takes a pull request off the refusal list — it got its
// preview after all — and reports whether it was on it.
func (s *PreviewCapacityStatus) ClearRefusedPreview(pullRequest int32) bool {
	for i := range s.Refused {
		if s.Refused[i].PullRequest != pullRequest {
			continue
		}
		s.Refused = append(s.Refused[:i], s.Refused[i+1:]...)
		return true
	}
	return false
}

// ImagePollStatus is the digest poll's own record for one project.
//
// What it deliberately does not hold is the digest each reference resolved
// to. That is on the Build the acquisition produced — `status.acquisition` —
// because the Build is what the audit trail, the evidence index and every
// build screen already key on, and a second copy of it here would be a second
// answer to "what is this project running" that nothing keeps in step.
type ImagePollStatus struct {
	// LastPolledAt is when the registry was last asked.
	// +optional
	LastPolledAt *metav1.Time `json:"lastPolledAt,omitempty"`

	// Message explains a poll that could not ask — a registry that would not
	// answer, a pull credential that could not be read. It is cleared by the
	// next poll that could, and while it is set the platform has already
	// produced the one failed Build that says so.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo",type=string,JSONPath=`.spec.source.git.repo`
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.source.image.repository`
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
