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

package api

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/attestation"
)

// The API answers in its own shapes rather than in raw custom resources.
// Clients get a stable contract that says nothing about Kubernetes, and the
// operator gets the freedom to change how a resource is stored without
// breaking the UI. Names are the platform's own vocabulary: a Release is
// referenced by name, never by "spec.releaseRef.name".

type conditionView struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

func conditionViews(conditions []metav1.Condition) []conditionView {
	if len(conditions) == 0 {
		return nil
	}
	out := make([]conditionView, 0, len(conditions))
	for _, condition := range conditions {
		out = append(out, conditionView{
			Type:               condition.Type,
			Status:             string(condition.Status),
			Reason:             condition.Reason,
			Message:            condition.Message,
			LastTransitionTime: condition.LastTransitionTime.Time,
		})
	}
	return out
}

// envVarView is one of a project's environment variables, minus its value.
// A literal value is reported as present and never echoed: a plain variable is
// exactly where somebody pastes an API key, and the API never reads a
// credential back. A secret- or claim-backed variable carries the reference it
// was written as, which was never a credential to begin with.
type envVarView struct {
	Name string `json:"name"`
	// Set and PreviewSet are the whole of what a screen needs from a value:
	// whether there is one, so a configured variable reads differently from
	// an empty one. Both are false for a reference-backed variable, which
	// holds no literal value of its own.
	Set        bool        `json:"set"`
	PreviewSet bool        `json:"previewSet"`
	FromSecret *keyRefView `json:"fromSecret,omitempty"`
	FromClaim  *keyRefView `json:"fromClaim,omitempty"`
}

// keyRefView names one key of a Secret or a ResourceClaim binding.
type keyRefView struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

func envVarViews(env []kitchenv1alpha1.EnvVar) []envVarView {
	if len(env) == 0 {
		return nil
	}
	out := make([]envVarView, 0, len(env))
	for _, v := range env {
		view := envVarView{Name: v.Name, Set: v.Value != "", PreviewSet: v.PreviewValue != ""}
		if v.SecretRef != nil {
			view.FromSecret = &keyRefView{Name: v.SecretRef.Name, Key: v.SecretRef.Key}
		}
		if v.FromResourceClaim != nil {
			view.FromClaim = &keyRefView{Name: v.FromResourceClaim.Name, Key: v.FromResourceClaim.Key}
		}
		out = append(out, view)
	}
	return out
}

type projectView struct {
	Name string `json:"name"`
	// Role is the calling account's role on this project — admin, developer
	// or viewer. It travels with every project rather than as a list to join
	// against, because the overview renders a list of them; and it is the
	// role itself rather than a set of capability booleans, so that what the
	// dashboard may offer is derived from the same table the API enforces
	// (internal/api/policy.go) instead of from a second opinion about it.
	//
	// An operator reads `admin` on every project, including one they are not
	// listed on, which is access.ProjectRoleFor's rule and not this view's.
	Role string `json:"role"`
	// Repo, Connection, Registry, ProductionBranch and RequirePullRequest
	// are a project built from a repository. Image is the other kind — the
	// web process running an image this platform did not build — and the two
	// groups never both carry anything: a project's source is one or the
	// other (#307).
	Repo               string           `json:"repo"`
	Connection         string           `json:"connection"`
	Registry           string           `json:"registry"`
	Image              *imageSourceView `json:"image,omitempty"`
	ProductionBranch   string           `json:"productionBranch"`
	RequirePullRequest bool             `json:"requirePullRequest"`
	Previews           bool             `json:"previews"`
	PreviewsProtected  bool             `json:"previewsProtected"`
	BuildStrategy      string           `json:"buildStrategy,omitempty"`
	DockerfilePath     string           `json:"dockerfilePath,omitempty"`
	// DockerfileTarget is the stage of a multi-stage Dockerfile this project
	// ships. Absent is the file's last stage.
	DockerfileTarget string       `json:"dockerfileTarget,omitempty"`
	RootDirectory    string       `json:"rootDirectory,omitempty"`
	Env              []envVarView `json:"env,omitempty"`
	Port             int32        `json:"port,omitempty"`
	Replicas         *int32       `json:"replicas,omitempty"`
	CPU              string       `json:"cpu,omitempty"`
	Memory           string       `json:"memory,omitempty"`
	// Health is what the platform checks the application with, timings
	// resolved. It is always present, because every environment is probed:
	// a project that declared nothing is reported with the default check
	// rather than with nothing, which would read as "not checked".
	Health *healthView `json:"health,omitempty"`
	// Security is the posture the project's workloads run under, resolved,
	// and always present for the same reason the health check is: every
	// workload runs under one, so a project that declared nothing is
	// reported with the platform's rather than with nothing.
	Security *securityView `json:"security,omitempty"`
	// Command and Args are what the application is started with, in exec
	// form; absent means the image's own entrypoint. PreviewArgs is what a
	// preview runs instead of Args — the sibling of an environment
	// variable's preview value, and absent means a preview runs
	// production's, which is the reading an empty list gets too.
	Command     []string `json:"command,omitempty"`
	Args        []string `json:"args,omitempty"`
	PreviewArgs []string `json:"previewArgs,omitempty"`
	// Singleton is a workload two of which must never run at once. It is
	// deployed by stopping the old copy before starting the new one, and it
	// cannot be given more than one replica.
	Singleton bool `json:"singleton"`
	// NotRequestDriven is a workload that does work nobody asked for, so no
	// environment of this project idles down to no pods — not even a
	// preview, which would otherwise idle by default.
	NotRequestDriven      bool            `json:"notRequestDriven"`
	ProductionEnvironment string          `json:"productionEnvironment,omitempty"`
	LatestBuild           string          `json:"latestBuild,omitempty"`
	CreatedAt             time.Time       `json:"createdAt"`
	Conditions            []conditionView `json:"conditions,omitempty"`
	// PromotionStages is the project's staged pipeline, in promotion order.
	// Absent for the default build-straight-to-production flow.
	PromotionStages []promotionStageView `json:"promotionStages,omitempty"`
	// Processes are the project's workloads besides its web process as it declares
	// them *now* — what an environment is actually running is its release's
	// list, on GET /environments/{name}/processes, and the two differ for as
	// long as it takes something to build.
	Processes []processView `json:"processes,omitempty"`
	// DataClass is the sensitivity classification of the data this project
	// handles. Absent means unclassified — a state the screens show as such,
	// never a default.
	DataClass string `json:"dataClass,omitempty"`
	// Criticality is how much it matters that this project's function keeps
	// working, and RTO/RPO the tolerances that come with it. Absent means
	// undesignated — the institution has not said, and Kitchen does not
	// decide.
	Criticality string `json:"criticality,omitempty"`
	RTO         string `json:"rto,omitempty"`
	RPO         string `json:"rpo,omitempty"`
}

// imageSourceView is an image this platform did not build, as a client reads
// it back: where it lives, which version of it to run, and what it is pulled
// with. The credential itself is never here — it is a Connection's name, like
// every other credential the API names and never reads back.
type imageSourceView struct {
	Repository string `json:"repository"`
	Tag        string `json:"tag,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Connection string `json:"connection,omitempty"`
	// Reference is the two of them as one string — `repository@digest` where
	// a digest is pinned, `repository:tag` otherwise — because that is what
	// every surface showing an image shows, and deriving it in three clients
	// is three chances to derive it differently.
	Reference string `json:"reference"`
}

func newImageSourceView(image *kitchenv1alpha1.ImageSourceSpec) *imageSourceView {
	if image == nil {
		return nil
	}
	return &imageSourceView{
		Repository: image.Repository,
		Tag:        image.Tag,
		Digest:     image.Digest,
		Connection: image.PullConnection(),
		Reference:  image.Reference(),
	}
}

func newProjectView(project *kitchenv1alpha1.Project, role access.ProjectRole) projectView {
	view := projectView{
		Name:               project.Name,
		Role:               role.String(),
		Repo:               project.Spec.Source.GitSource().Repo,
		Connection:         project.Spec.Source.GitSource().ConnectionRef.Name,
		Registry:           project.Spec.RegistryConnection(),
		Image:              newImageSourceView(project.Spec.Source.Image),
		ProductionBranch:   project.Spec.Source.GitSource().ProductionBranch,
		RequirePullRequest: project.Spec.Source.GitSource().RequirePullRequest,
		Previews:           project.Spec.Previews.IsEnabled(),
		PreviewsProtected:  project.Spec.Previews.IsProtected(),
		BuildStrategy:      string(project.Spec.Build.Strategy),
		DockerfilePath:     project.Spec.Build.DockerfilePath,
		DockerfileTarget:   project.Spec.Build.DockerfileTarget,
		RootDirectory:      project.Spec.Build.RootDirectory,
		Env:                envVarViews(project.Spec.Env),
		Port:               project.Spec.Runtime.Port,
		Replicas:           project.Spec.Runtime.Replicas,
		CreatedAt:          project.CreationTimestamp.Time,
		Conditions:         conditionViews(project.Status.Conditions),
		Health:             newHealthView(project.Spec.Runtime.Health),
		Security:           newSecurityView(project.Spec.Runtime.Security),
		Command:            project.Spec.Runtime.Command,
		Args:               project.Spec.Runtime.Args,
		PreviewArgs:        project.Spec.Runtime.PreviewArgs,
		Singleton:          project.Spec.Runtime.Singleton,
		NotRequestDriven:   project.Spec.Runtime.NotRequestDriven,
	}
	if quantity, ok := project.Spec.Runtime.Resources.Limits[corev1.ResourceCPU]; ok {
		view.CPU = quantity.String()
	}
	if quantity, ok := project.Spec.Runtime.Resources.Limits[corev1.ResourceMemory]; ok {
		view.Memory = quantity.String()
	}
	if ref := project.Status.ProductionEnvironmentRef; ref != nil {
		view.ProductionEnvironment = ref.Name
	}
	if ref := project.Status.LatestBuildRef; ref != nil {
		view.LatestBuild = ref.Name
	}
	if promotion := project.Spec.Promotion; promotion != nil {
		for _, stage := range promotion.Stages {
			view.PromotionStages = append(view.PromotionStages, promotionStageView{
				Name:        stage.Name,
				Environment: stage.Environment,
				AutoPromote: stage.AutoPromote,
			})
		}
	}
	for _, process := range project.Spec.Processes {
		view.Processes = append(view.Processes, newProcessView(process, nil, ""))
	}
	view.DataClass = string(project.Spec.DataClass)
	view.Criticality = string(project.Spec.Criticality)
	view.RTO = string(project.Spec.RTO)
	view.RPO = string(project.Spec.RPO)
	return view
}

// promotionStageView is one rung of a project's promotion ladder.
type promotionStageView struct {
	Name        string `json:"name"`
	Environment string `json:"environment"`
	AutoPromote bool   `json:"autoPromote"`
}

// promotionView is one Promotion: what was asked — which release, into which
// environment, by whom — and what became of it. A blocked one names the
// unmet rules by their stable ids; the decision id leads to the stored
// decision with the fired rules' messages and the replayable input.
type promotionView struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Release     string `json:"release"`
	RequestedBy string `json:"requestedBy"`
	Trigger     string `json:"trigger"`
	Reason      string `json:"reason,omitempty"`

	Phase       string     `json:"phase"`
	Verdict     string     `json:"verdict,omitempty"`
	DecisionID  string     `json:"decisionID,omitempty"`
	UnmetRules  []string   `json:"unmetRules,omitempty"`
	Message     string     `json:"message,omitempty"`
	EvaluatedAt *time.Time `json:"evaluatedAt,omitempty"`
	AppliedAt   *time.Time `json:"appliedAt,omitempty"`

	CreatedAt  time.Time       `json:"createdAt"`
	Conditions []conditionView `json:"conditions,omitempty"`
}

func newPromotionView(promotion *kitchenv1alpha1.Promotion) promotionView {
	view := promotionView{
		Name:        promotion.Name,
		Project:     promotion.Spec.ProjectRef.Name,
		Environment: promotion.Spec.EnvironmentRef.Name,
		Release:     promotion.Spec.ReleaseRef.Name,
		RequestedBy: promotion.Spec.RequestedBy,
		Trigger:     string(promotion.Spec.Trigger),
		Reason:      promotion.Spec.Reason,
		Phase:       string(promotion.Status.Phase),
		Verdict:     promotion.Status.Verdict,
		DecisionID:  promotion.Status.DecisionID,
		UnmetRules:  promotion.Status.UnmetRules,
		Message:     promotion.Status.Message,
		CreatedAt:   promotion.CreationTimestamp.Time,
		Conditions:  conditionViews(promotion.Status.Conditions),
	}
	if view.Phase == "" {
		// Created and not yet picked up: the phase the reconciler will stamp,
		// answered now so a 201 does not read as an empty state.
		view.Phase = string(kitchenv1alpha1.PromotionPending)
	}
	if at := promotion.Status.EvaluatedAt; at != nil {
		view.EvaluatedAt = &at.Time
	}
	if at := promotion.Status.AppliedAt; at != nil {
		view.AppliedAt = &at.Time
	}
	return view
}

// exceptionView is one break-glass exception as the register serves it: the
// grant whole — who asked, who approved, what it waives, until when — plus
// what became of it. Phase is the effective phase, judged against the clock
// rather than read off status, so a grant past its expiry never reads Active
// however recently it was reconciled.
type exceptionView struct {
	Name        string   `json:"name"`
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Release     string   `json:"release,omitempty"`
	RuleIDs     []string `json:"ruleIDs"`
	Reason      string   `json:"reason"`
	RequestedBy string   `json:"requestedBy"`
	ApprovedBy  string   `json:"approvedBy"`
	IncidentRef string   `json:"incidentRef,omitempty"`

	ExpiresAt    time.Time `json:"expiresAt"`
	AutoRollback bool      `json:"autoRollback"`

	Phase      string     `json:"phase"`
	UsedBy     []string   `json:"usedBy,omitempty"`
	ResolvedBy string     `json:"resolvedBy,omitempty"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`

	CreatedAt  time.Time       `json:"createdAt"`
	Conditions []conditionView `json:"conditions,omitempty"`
}

func newExceptionView(exception *kitchenv1alpha1.Exception) exceptionView {
	view := exceptionView{
		Name:         exception.Name,
		Project:      exception.Spec.ProjectRef.Name,
		Environment:  exception.Spec.EnvironmentRef.Name,
		RuleIDs:      exception.Spec.RuleIDs,
		Reason:       exception.Spec.Reason,
		RequestedBy:  exception.Spec.RequestedBy,
		ApprovedBy:   exception.Spec.ApprovedBy,
		IncidentRef:  exception.Spec.IncidentRef,
		ExpiresAt:    exception.Spec.ExpiresAt.Time,
		AutoRollback: exception.Spec.AutoRollback,
		Phase:        string(exception.EffectivePhase(time.Now())),
		UsedBy:       exception.Status.UsedBy,
		ResolvedBy:   exception.Status.ResolvedBy,
		CreatedAt:    exception.CreationTimestamp.Time,
		Conditions:   conditionViews(exception.Status.Conditions),
	}
	if exception.Spec.ReleaseRef != nil {
		view.Release = exception.Spec.ReleaseRef.Name
	}
	if at := exception.Status.ResolvedAt; at != nil {
		view.ResolvedAt = &at.Time
	}
	return view
}

type revisionView struct {
	SHA     string `json:"sha"`
	Branch  string `json:"branch"`
	Message string `json:"message,omitempty"`
	// Body is what the commit said under its subject, for a client that
	// offers to show it. Absent for a commit that has none.
	Body        string `json:"body,omitempty"`
	Author      string `json:"author,omitempty"`
	PullRequest *int32 `json:"pullRequest,omitempty"`
}

// newRevisionView answers the subject and the body separately whatever the
// Build holds. One recorded before the platform split them has the whole
// message under `message` and its spec is immutable, so the split happens
// here rather than in each of the three clients.
func newRevisionView(build *kitchenv1alpha1.Build) revisionView {
	git := build.Spec.Git
	body := git.Body
	if body == "" {
		body = kitchenv1alpha1.CommitBody(git.Message)
	}
	return revisionView{
		SHA:         git.SHA,
		Branch:      git.Branch,
		Message:     kitchenv1alpha1.CommitSubject(git.Message),
		Body:        body,
		Author:      git.Author,
		PullRequest: build.PullRequestNumber(),
	}
}

type buildView struct {
	Name              string       `json:"name"`
	Project           string       `json:"project"`
	Phase             string       `json:"phase,omitempty"`
	Git               revisionView `json:"git"`
	DetectedFramework string       `json:"detectedFramework,omitempty"`
	// DockerfileTarget is the stage of the Dockerfile this build was told to
	// produce, absent for the file's last stage. It is what the build was
	// given rather than what the project says today, so an old build reads
	// as the artifact it actually shipped.
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`
	// Config is the kitchen.json the commit carried, when it carried one:
	// where it was read from, and every setting it took over from the
	// project. Absent means the commit had no file, which is the ordinary
	// case and not a fault.
	Config      *repoConfigView `json:"config,omitempty"`
	Image       string          `json:"image,omitempty"`
	StartedAt   *time.Time      `json:"startedAt,omitempty"`
	CompletedAt *time.Time      `json:"completedAt,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
	Conditions  []conditionView `json:"conditions,omitempty"`
	// Artifact is what the build produced, by content, and whether the
	// platform managed to attest it. Absent on a build that never got as far
	// as pushing anything.
	Artifact *artifactView `json:"artifact,omitempty"`
	// Cache is what the layer cache did for this build. Absent on a build
	// that was never run.
	Cache *buildCacheView `json:"cache,omitempty"`
	// Gates is what each quality gate did — not what it found, which is in
	// its attestation.
	Gates []gateView `json:"gates,omitempty"`

	// Source is how the commit reached the branch: through review, or not.
	Source *sourceView `json:"source,omitempty"`

	// Failure is why this build failed, when it did: the container that
	// stopped it, how it exited, and the last of what it printed. Absent on
	// every build that did not fail.
	Failure *buildFailureView `json:"failure,omitempty"`

	// Workloads is the other images this one commit produced, for a project
	// whose unit is more than one workload. Empty for the great majority of
	// projects, which ship one image and whose image is `image` above.
	//
	// The build is over when all of them are, so a row still Running is what
	// the build as a whole is waiting on, and a row that Failed is why the
	// build did.
	Workloads []buildWorkloadView `json:"workloads,omitempty"`

	// Acquisition is what this build resolved, from which reference, when,
	// and what it replaced — for a Build that acquired an image somebody
	// else built rather than building one. Absent on a build of a commit,
	// whose answer to all four is the commit.
	Acquisition *acquisitionView `json:"acquisition,omitempty"`
}

// acquisitionView is a Build with no commit, explaining itself.
//
// It exists to answer "why did this environment change" for software this
// platform did not build. A commit answers that question by being a commit;
// an acquisition has to say what it followed, what it found, when it looked,
// what it replaced, and what made it look.
type acquisitionView struct {
	// Reference is what was followed, as the project declared it.
	Reference string `json:"reference,omitempty"`
	// Image is what it resolved to, always by digest.
	Image string `json:"image,omitempty"`
	// Previous is the image this one replaced, absent for a project's first.
	Previous string `json:"previous,omitempty"`
	// Trigger is what asked: `seed`, `poll` or `request`.
	Trigger string `json:"trigger,omitempty"`
	// Pinned is whether the project named a digest rather than a tag, which
	// is how a project says it does not want to be moved.
	Pinned bool `json:"pinned,omitempty"`
	// ResolvedAt is when the registry was asked.
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`
}

func newAcquisitionView(status *kitchenv1alpha1.AcquisitionStatus) *acquisitionView {
	if status == nil {
		return nil
	}
	view := &acquisitionView{
		Reference: status.Reference,
		Image:     status.Image,
		Previous:  status.Previous,
		Trigger:   string(status.Trigger),
		Pinned:    status.Pinned,
	}
	if at := status.ResolvedAt; at != nil {
		view.ResolvedAt = &at.Time
	}
	return view
}

// buildWorkloadView is one workload's own build within one commit's.
type buildWorkloadView struct {
	Name       string `json:"name"`
	Phase      string `json:"phase,omitempty"`
	Image      string `json:"image,omitempty"`
	Repository string `json:"repository,omitempty"`
	// Reference is what this workload's image was acquired from, as the
	// project declared it. Absent for a workload this platform built.
	Reference string `json:"reference,omitempty"`
	Job       string `json:"job,omitempty"`
	// DockerfileTarget is the stage this workload's build was told to
	// produce, absent for its file's last stage. It is the row's counterpart
	// of the build's own `dockerfileTarget`, and it is here for the same
	// reason: recorded when the Job was created, so an old build reads as
	// the artifacts it actually shipped.
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`
	// DetectedFramework is what this workload's `strategy: auto` made of its
	// own root directory, absent for a workload that named its strategy and
	// so asked detection nothing. The build's own `detectedFramework` above
	// is the project's image; a unit is several directories and can have
	// several answers.
	DetectedFramework string `json:"detectedFramework,omitempty"`
	// Artifact is what this workload's build produced, by content, and
	// whether the platform managed to attest it — the same shape and the
	// same meaning as the build's own `artifact`, about this image. Absent
	// on a workload that never pushed one.
	Artifact *artifactView `json:"artifact,omitempty"`
	// Gates is what each quality gate did over *this workload's* artifact. A
	// gate is a claim about an image, so a unit runs each gate once per image
	// and each run is recorded against the image it ran over.
	Gates   []gateView `json:"gates,omitempty"`
	Message string     `json:"message,omitempty"`
}

func buildWorkloadViews(workloads []kitchenv1alpha1.WorkloadBuildStatus) []buildWorkloadView {
	views := make([]buildWorkloadView, 0, len(workloads))
	for _, workload := range workloads {
		views = append(views, buildWorkloadView{
			Name:              workload.Name,
			Phase:             string(workload.Phase),
			Image:             workload.Image,
			Repository:        workload.Repository,
			Reference:         workload.Reference,
			Job:               workload.Job,
			DockerfileTarget:  workload.DockerfileTarget,
			DetectedFramework: workload.DetectedFramework,
			Artifact:          newArtifactView(workload.Artifact),
			Gates:             gateViews(workload.Gates),
			Message:           workload.Message,
		})
	}
	return views
}

// buildFailureView is a failed build's own account of itself.
//
// The Kubernetes Job behind a build says only "Job has reached the specified
// backoff limit", which is true of every failed build there has ever been.
// This is the answer to the question that sentence leaves: which container,
// what exit, and what it said on the way out.
//
// It is on the build rather than only on the pod because the pod is deleted
// with the job's TTL, and because a pod is the operator's to read while a
// build that failed is the developer's to fix.
type buildFailureView struct {
	// Container that ended the build — the clone as readily as the builder.
	Container string `json:"container,omitempty"`
	// ExitCode it exited with, absent when nothing exited: a pod evicted
	// before it ran, or a container whose image would not pull.
	ExitCode *int32 `json:"exitCode,omitempty"`
	// Reason is Kubernetes' own word for the ending, kept unchanged.
	Reason string `json:"reason,omitempty"`
	// Message is the failure in one line.
	Message string `json:"message,omitempty"`
	// Log is the tail of that container's output, oldest line first. It is a
	// copy taken when the failure was observed, for the case the log store
	// cannot serve — the whole log is at /api/v1/builds/{name}/logs.
	Log []string `json:"log,omitempty"`
}

func newBuildFailureView(failure *kitchenv1alpha1.BuildFailureStatus) *buildFailureView {
	if failure == nil {
		return nil
	}
	return &buildFailureView{
		Container: failure.Container,
		ExitCode:  failure.ExitCode,
		Reason:    failure.Reason,
		Message:   failure.Message,
		Log:       failure.Log,
	}
}

// sourceView is what the git provider said about how the commit arrived.
//
// Every field is a third party's claim, which is why `provider` travels with
// them: the platform did not witness the review, it asked and was answered.
type sourceView struct {
	Provider        string     `json:"provider,omitempty"`
	PullRequest     int32      `json:"pullRequest,omitempty"`
	Title           string     `json:"title,omitempty"`
	Author          string     `json:"author,omitempty"`
	MergedBy        string     `json:"mergedBy,omitempty"`
	Approvers       []string   `json:"approvers,omitempty"`
	SelfApproved    bool       `json:"selfApproved"`
	Independent     bool       `json:"independent"`
	MachineIdentity string     `json:"machineIdentity,omitempty"`
	Exception       string     `json:"exception,omitempty"`
	Required        bool       `json:"required"`
	CheckedAt       *time.Time `json:"checkedAt,omitempty"`
	Message         string     `json:"message,omitempty"`
}

// repoConfigView is the commit's own kitchen.json, as a build reports it.
//
// It answers what it declared rather than what it said: the values are
// already in the release's snapshot and on the environment, and repeating
// them here would be a second copy to disagree with the first. What is not
// answerable anywhere else is which settings stopped being the project's,
// which is what a reader of the settings screen needs to know before editing
// one that will be overwritten on the next build.
type repoConfigView struct {
	// Path the file was read from, relative to the repository root.
	Path string `json:"path"`
	// Declares is every setting the file took over, in dotted form —
	// "build.strategy", "runtime.port", "env.NODE_ENV", "processes".
	Declares []string `json:"declares"`
}

func newRepoConfigView(config *kitchenv1alpha1.RepoConfig) *repoConfigView {
	if config == nil {
		return nil
	}
	declares := config.Declares()
	if declares == nil {
		declares = []string{}
	}
	return &repoConfigView{Path: config.Path, Declares: declares}
}

func newSourceView(source *kitchenv1alpha1.SourceProvenanceStatus) *sourceView {
	if source == nil {
		return nil
	}
	view := &sourceView{
		Provider:        source.Provider,
		PullRequest:     source.PullRequest,
		Title:           source.Title,
		Author:          source.Author,
		MergedBy:        source.MergedBy,
		Approvers:       source.Approvers,
		SelfApproved:    source.SelfApproved,
		Independent:     source.Independent,
		MachineIdentity: source.MachineIdentity,
		Exception:       source.Exception,
		Required:        source.Required,
		Message:         source.Message,
	}
	if at := source.CheckedAt; at != nil {
		stamp := at.Time
		view.CheckedAt = &stamp
	}
	return view
}

// buildCacheView is why a build took as long as it did, as far as the layer
// cache is concerned: a cold build had nothing to reuse, and saying so is what
// keeps it from reading as a regression.
type buildCacheView struct {
	Enabled bool   `json:"enabled"`
	Warm    bool   `json:"warm"`
	Ref     string `json:"ref,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Message string `json:"message,omitempty"`
}

func newBuildCacheView(cache *kitchenv1alpha1.BuildCacheStatus) *buildCacheView {
	if cache == nil {
		return nil
	}
	return &buildCacheView{
		Enabled: cache.Enabled,
		Warm:    cache.Warm,
		Ref:     cache.Ref,
		Mode:    string(cache.Mode),
		Message: cache.Message,
	}
}

// gateView is one quality gate's run over a build's artifact.
//
// It carries no verdict, because nothing at this level has one: what a gate
// found is in the attestation, and whether that is disqualifying is a policy
// question about the environment being deployed to. `Completed` means the gate
// ran, whatever it found; `Failed` means it did not run at all.
type gateView struct {
	Name          string     `json:"name"`
	Phase         string     `json:"phase,omitempty"`
	Source        string     `json:"source,omitempty"`
	ReportedBy    string     `json:"reportedBy,omitempty"`
	PredicateType string     `json:"predicateType,omitempty"`
	Attested      bool       `json:"attested"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	Message       string     `json:"message,omitempty"`
}

func gateViews(gates []kitchenv1alpha1.QualityGateStatus) []gateView {
	if len(gates) == 0 {
		return nil
	}
	views := make([]gateView, 0, len(gates))
	for _, gate := range gates {
		view := gateView{
			Name:          gate.Name,
			Phase:         string(gate.Phase),
			Source:        gate.Source,
			ReportedBy:    gate.ReportedBy,
			PredicateType: gate.PredicateType,
			Attested:      gate.Attested != nil,
			Message:       gate.Message,
		}
		if at := gate.FinishedAt; at != nil {
			stamp := at.Time
			view.FinishedAt = &stamp
		}
		views = append(views, view)
	}
	return views
}

// artifactView is the artifact half of a build. It carries whether evidence
// was attached and under which key, but not the evidence itself: that lives in
// the registry against the digest, and the attestations endpoint is where it
// is read from.
type artifactView struct {
	// Workload is which image of the unit this is — `web` for the project's
	// own. It is set where an artifact is listed among the unit's others and
	// absent where the surrounding object already says which image it is:
	// the build's own `artifact`, and each row of `workloads`.
	Workload   string `json:"workload,omitempty"`
	Repository string `json:"repository,omitempty"`
	Digest     string `json:"digest,omitempty"`
	// SourceType is where this artifact's evidence came from. It reads
	// `built` on everything the platform holds evidence about today, and it
	// is published rather than implied so that a reader can tell a built
	// artifact from one of another kind without knowing which release of
	// Kitchen wrote it.
	SourceType string     `json:"sourceType,omitempty"`
	Attested   bool       `json:"attested"`
	AttestedAt *time.Time `json:"attestedAt,omitempty"`
	KeyID      string     `json:"keyID,omitempty"`
	// Evidence is what is attached, by predicate type — enough for a screen
	// to say "provenance and an SBOM are attached" without asking the
	// registry, and not enough to be mistaken for the evidence itself.
	Evidence []evidenceView `json:"evidence,omitempty"`
	Message  string         `json:"message,omitempty"`
}

// evidenceView names one attestation attached to an artifact.
//
// Kind is derived here rather than in the dashboard, and it is a label rather
// than a verdict. The predicate type travels with it because the URI is the
// authority: a reader that only recognises the label would silently treat a
// predicate type nobody has taught it about as "other" and move on, where one
// holding the URI can look it up.
type evidenceView struct {
	PredicateType string `json:"predicateType"`
	Kind          string `json:"kind"`
	Source        string `json:"source,omitempty"`
	Manifest      string `json:"manifest,omitempty"`
}

// evidenceKind labels a predicate type for display.
func evidenceKind(predicateType string) string {
	switch {
	case attestation.Provenance(predicateType):
		return "provenance"
	case attestation.SBOM(predicateType):
		return "sbom"
	case predicateType == attestation.PredicateBuildRecord:
		return "buildRecord"
	case predicateType == attestation.PredicateDeployment:
		return "deployment"
	default:
		return "other"
	}
}

func newArtifactView(artifact *kitchenv1alpha1.ArtifactStatus) *artifactView {
	if artifact == nil {
		return nil
	}
	view := &artifactView{
		Repository: artifact.Repository,
		Digest:     artifact.Digest,
		SourceType: string(artifact.SourceType),
		Attested:   artifact.AttestedAt != nil,
		KeyID:      artifact.KeyID,
		Message:    artifact.Message,
	}
	if at := artifact.AttestedAt; at != nil {
		stamp := at.Time
		view.AttestedAt = &stamp
	}
	for _, evidence := range artifact.Evidence {
		view.Evidence = append(view.Evidence, evidenceView{
			PredicateType: evidence.PredicateType,
			Kind:          evidenceKind(evidence.PredicateType),
			Source:        evidence.Source,
			Manifest:      evidence.Manifest,
		})
	}
	return view
}

func newBuildView(build *kitchenv1alpha1.Build) buildView {
	view := buildView{
		Name:              build.Name,
		Project:           build.Spec.ProjectRef.Name,
		Phase:             string(build.Status.Phase),
		Git:               newRevisionView(build),
		DetectedFramework: build.Status.DetectedFramework,
		DockerfileTarget:  build.Status.DockerfileTarget,
		Config:            newRepoConfigView(build.Status.Config),
		Image:             build.Status.Image,
		Artifact:          newArtifactView(build.Status.Artifact),
		Cache:             newBuildCacheView(build.Status.Cache),
		Gates:             gateViews(build.Status.Gates),
		Source:            newSourceView(build.Status.Source),
		Failure:           newBuildFailureView(build.Status.Failure),
		Workloads:         buildWorkloadViews(build.Status.Workloads),
		Acquisition:       newAcquisitionView(build.Status.Acquisition),
		CreatedAt:         build.CreationTimestamp.Time,
		Conditions:        conditionViews(build.Status.Conditions),
	}
	if at := build.Status.StartedAt; at != nil {
		view.StartedAt = &at.Time
	}
	if at := build.Status.CompletedAt; at != nil {
		view.CompletedAt = &at.Time
	}
	return view
}

type releaseView struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Build   string `json:"build"`
	Image   string `json:"image"`
	// Workloads is the image each of the unit's other workloads was built
	// to. It is what makes rolling this release back exact for a project
	// that is more than one workload: restoring it restores this set, never
	// the set the project declares today.
	Workloads    []workloadImageView `json:"workloads,omitempty"`
	Environments []string            `json:"environments,omitempty"`
	CreatedAt    time.Time           `json:"createdAt"`
	// Attestation is the unit's own compliance answer: whether every image
	// this release deploys carries signed evidence, and which images do not
	// when some do. It is on the single release read and not on a listing,
	// because answering it means reading the build that produced the release.
	Attestation *unitAttestationView `json:"attestation,omitempty"`
}

// unitAttestationView is the Release-level answer to "is this attested",
// which is per artifact and never a single flag standing in for several.
//
// A release deploys one image per workload of the unit, and a unit of five
// workloads used to ship with provenance and an SBOM for the web process and
// nothing at all for the other four — with nothing saying so, which is a
// compliance surface reporting success over what it never looked at. So
// `attested` is true only when *every* artifact is, and `missing` names the
// ones that are not.
type unitAttestationView struct {
	// Attested is whether every image this release deploys carries the
	// platform's signed build record.
	Attested bool `json:"attested"`
	// Artifacts is one entry per image, the web process's first, each with
	// its own digest, its own evidence index and its own source type.
	Artifacts []artifactView `json:"artifacts"`
	// Missing names every image with no signed evidence, by workload —
	// `web` for the project's own. Empty exactly when `attested` is true.
	Missing []string `json:"missing,omitempty"`
	// Caveat says why the answer is weaker than it looks: the build that
	// produced this release has been pruned, so there is no evidence index
	// left to read. It is not the same as an unattested release, and saying
	// which of the two it is matters.
	Caveat string `json:"caveat,omitempty"`
}

// newUnitAttestationView is the release-level answer, read off the build that
// produced the release. A nil build is a build that has been pruned.
func newUnitAttestationView(build *kitchenv1alpha1.Build) *unitAttestationView {
	if build == nil {
		return &unitAttestationView{
			Caveat: "the build that produced this release is gone, " +
				"so the evidence index it carried cannot be read — the evidence itself " +
				"is still in the registry, attached to the digests above",
		}
	}
	view := &unitAttestationView{
		Attested:  build.FullyAttested(),
		Artifacts: []artifactView{},
		Missing:   build.ArtifactsWithoutEvidence(),
	}
	for _, artifact := range build.Artifacts() {
		entry := newArtifactView(artifact.Artifact)
		entry.Workload = artifact.Name()
		view.Artifacts = append(view.Artifacts, *entry)
	}
	return view
}

// workloadImageView is one workload's frozen image within a release.
type workloadImageView struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

func newReleaseView(release *kitchenv1alpha1.Release) releaseView {
	workloads := make([]workloadImageView, 0, len(release.Spec.Workloads))
	for _, workload := range release.Spec.Workloads {
		workloads = append(workloads, workloadImageView{Name: workload.Name, Image: workload.Image})
	}
	return releaseView{
		Name:         release.Name,
		Project:      release.Spec.ProjectRef.Name,
		Build:        release.Spec.BuildRef.Name,
		Image:        release.Spec.Image,
		Workloads:    workloads,
		Environments: release.Status.Environments,
		CreatedAt:    release.CreationTimestamp.Time,
	}
}

type previewView struct {
	PullRequest int32  `json:"pullRequest"`
	Branch      string `json:"branch"`
}

// releaseHistoryView is one completed stint of a release being current on an
// environment: when it held the environment, and how and by whom it stopped.
type releaseHistoryView struct {
	Release string    `json:"release"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Reason  string    `json:"reason"`
	By      string    `json:"by,omitempty"`
}

// requirementsView is the bar an environment declares: the policy bundle it
// pins, and the parameters its owners tuned it with. Neither is a secret —
// the whole point of a declared requirement is that the team deploying into
// the environment can read what it will be judged against.
type requirementsView struct {
	BundleDigest string            `json:"bundleDigest"`
	Parameters   map[string]string `json:"parameters,omitempty"`
}

func newRequirementsView(requirements *kitchenv1alpha1.EnvironmentRequirements) *requirementsView {
	if requirements == nil {
		return nil
	}
	return &requirementsView{
		BundleDigest: requirements.BundleDigest,
		Parameters:   requirements.Parameters,
	}
}

// eligibilityEvidenceView is one attestation as an eligibility answer counts
// it: what kind of claim, who attached it, and whether its signature checked
// out against the platform's key. The evidence itself stays in the registry.
type eligibilityEvidenceView struct {
	PredicateType string `json:"predicateType"`
	// Source is `platform` or `builder` for evidence the platform indexed,
	// and empty for evidence found in the registry that nothing here
	// attached — present, but nobody's claim about who made it.
	Source   string `json:"source,omitempty"`
	Verified bool   `json:"verified"`
	// Workload is which image of the unit this evidence is attached to,
	// `web` for the project's own. A release deploys one image per workload
	// and each is attested in its own right, so a list that did not say
	// which would read as one artifact carrying five SBOMs.
	Workload string `json:"workload,omitempty"`
}

type environmentView struct {
	Name            string            `json:"name"`
	Project         string            `json:"project"`
	Type            string            `json:"type"`
	Release         string            `json:"release"`
	ObservedRelease string            `json:"observedRelease,omitempty"`
	Phase           string            `json:"phase,omitempty"`
	URL             string            `json:"url,omitempty"`
	Preview         *previewView      `json:"preview,omitempty"`
	Owners          []string          `json:"owners,omitempty"`
	Requirements    *requirementsView `json:"requirements,omitempty"`
	// DataClass is the highest sensitivity class this environment is rated
	// to hold, declared by its owners; absent means unrated. Residency is
	// where its data is declared to be — declared, not observed.
	DataClass string `json:"dataClass,omitempty"`
	Residency string `json:"residency,omitempty"`
	// Criticality, RTO and RPO are what this environment itself declares —
	// absent means it declares nothing, which is not the same as nothing
	// applying. What applies is the effective designation, which a production
	// environment inherits from its project; GET /compliance/criticality
	// answers with that, resolved, and says which fields were inherited.
	Criticality string               `json:"criticality,omitempty"`
	RTO         string               `json:"rto,omitempty"`
	RPO         string               `json:"rpo,omitempty"`
	History     []releaseHistoryView `json:"history,omitempty"`
	CreatedAt   time.Time            `json:"createdAt"`
	Conditions  []conditionView      `json:"conditions,omitempty"`
}

func newEnvironmentView(env *kitchenv1alpha1.Environment) environmentView {
	view := environmentView{
		Name:            env.Name,
		Project:         env.Spec.ProjectRef.Name,
		Type:            string(env.Spec.Type),
		Release:         env.Spec.ReleaseRef.Name,
		ObservedRelease: env.Status.ObservedRelease,
		Phase:           string(env.Status.Phase),
		URL:             env.Status.URL,
		Owners:          env.Spec.Owners,
		Requirements:    newRequirementsView(env.Spec.Requirements),
		DataClass:       string(env.Spec.DataClass),
		Residency:       env.Spec.Residency,
		Criticality:     string(env.Spec.Criticality),
		RTO:             string(env.Spec.RTO),
		RPO:             string(env.Spec.RPO),
		CreatedAt:       env.CreationTimestamp.Time,
		Conditions:      conditionViews(env.Status.Conditions),
	}
	if preview := env.Spec.Preview; preview != nil {
		view.Preview = &previewView{PullRequest: preview.PullRequest, Branch: preview.Branch}
	}
	for _, entry := range env.Status.History {
		view.History = append(view.History, releaseHistoryView{
			Release: entry.Release,
			From:    entry.From.Time,
			To:      entry.To.Time,
			Reason:  string(entry.Reason),
			By:      entry.By,
		})
	}
	return view
}

// The introspection views answer a different question from the ones above:
// not what the platform was asked for, but what is running because of it. A
// Deployment's replica counts, its pods' restarts and the objects the
// reconciler materialized are Kubernetes' own vocabulary, and the views keep
// it — an operator reading them is looking for exactly the words `kubectl`
// would have shown, and inventing synonyms would only make them translate.

// replicaCountsView is the "3 of 3" the environment view reads at a glance,
// with the two counts that tell a rollout from a steady state underneath it.
type replicaCountsView struct {
	Desired   int32 `json:"desired"`
	Ready     int32 `json:"ready"`
	Available int32 `json:"available"`
	Updated   int32 `json:"updated"`
}

// resourcesView is what the workload asked the scheduler for, as written —
// the quantities come straight off the container, so `250m` stays `250m`.
type resourcesView struct {
	CPURequest    string `json:"cpuRequest,omitempty"`
	CPULimit      string `json:"cpuLimit,omitempty"`
	MemoryRequest string `json:"memoryRequest,omitempty"`
	MemoryLimit   string `json:"memoryLimit,omitempty"`
}

type podView struct {
	Name      string     `json:"name"`
	Phase     string     `json:"phase"`
	Ready     bool       `json:"ready"`
	Restarts  int32      `json:"restarts"`
	Node      string     `json:"node,omitempty"`
	StartedAt *time.Time `json:"startedAt,omitempty"`
	// Message is why the pod is not running: the container's waiting reason
	// (ImagePullBackOff, CrashLoopBackOff) or the pod's own status message.
	// It is the line an operator would otherwise go to `kubectl describe` for.
	Message string `json:"message,omitempty"`
}

// workloadView is the Deployment behind an Environment as it is running now.
type workloadView struct {
	Environment string            `json:"environment"`
	Namespace   string            `json:"namespace"`
	Deployment  string            `json:"deployment,omitempty"`
	Image       string            `json:"image,omitempty"`
	Replicas    replicaCountsView `json:"replicas"`
	// Restarts is every restart across the environment's pods, which is the
	// number that says a workload is crash-looping rather than merely slow.
	Restarts int32 `json:"restarts"`
	// StartedAt is when the oldest running pod started, so uptime is measured
	// against the workload rather than against the Environment object, which
	// outlives any number of rollouts.
	StartedAt *time.Time     `json:"startedAt,omitempty"`
	Resources *resourcesView `json:"resources,omitempty"`
	Pods      []podView      `json:"pods,omitempty"`
	// Message explains an environment with no workload at all, which is a
	// normal state — a preview whose route is withheld, an environment the
	// reconciler has not reached yet — and not an error to report as one.
	Message string `json:"message,omitempty"`
}

// materializedObjectView is one Kubernetes object the operator created for an
// Environment, carrying the object itself.
type materializedObjectView struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	// Present is false for an object the reconciler has not created yet. It
	// is reported rather than omitted, because "the HTTPRoute is missing" is
	// the answer to most of the questions this endpoint gets asked.
	Present bool `json:"present"`
	// Manifest is the object as the API server holds it, minus the
	// bookkeeping (managedFields, the last-applied annotation) that no reader
	// of a manifest wants. Status is kept: for an HTTPRoute it is where the
	// Gateway says whether it accepted the route.
	Manifest map[string]any `json:"manifest,omitempty"`
	Message  string         `json:"message,omitempty"`
}

type objectsView struct {
	Environment string                   `json:"environment"`
	Namespace   string                   `json:"namespace"`
	Objects     []materializedObjectView `json:"objects"`
}

// clusterStatusView is the cluster the platform owns, as the status bar shows
// it: "chef · 8 nodes".
//
// The name is everybody's — it is what this installation is called. The counts
// are the operator's, and they are pointers so that a member's answer has no
// counts at all rather than two zeroes, which would read as a platform with
// nothing running on it.
type clusterStatusView struct {
	Name       string `json:"name,omitempty"`
	Nodes      *int   `json:"nodes,omitempty"`
	ReadyNodes *int   `json:"readyNodes,omitempty"`
	// Message carries why the nodes could not be counted, which on an
	// installation upgraded from before this endpoint means the operator's
	// ClusterRole has not been rolled forward yet.
	Message string `json:"message,omitempty"`
}

type tunnelStatusView struct {
	Enabled   bool   `json:"enabled"`
	Connected bool   `json:"connected"`
	Message   string `json:"message,omitempty"`
}

// buildQueueView is the "1 of 2" in the status bar: builds running against the
// platform's concurrency limit, and how many are waiting for a slot.
type buildQueueView struct {
	Running  int32 `json:"running"`
	Capacity int32 `json:"capacity"`
	Queued   int32 `json:"queued"`
	// OldestWaitSeconds is how long the build that has been queued longest has
	// been waiting. The count alone does not say whether a queue is moving:
	// three builds waiting twenty seconds is a busy platform, and one waiting
	// forty minutes is a stuck one.
	OldestWaitSeconds int64 `json:"oldestWaitSeconds,omitempty"`
	// Waiting is the queued builds themselves, longest wait first, so the
	// screen can name what is stuck rather than only counting it.
	Waiting []queuedBuildView `json:"waiting,omitempty"`
}

// queuedBuildView is one build that has been admitted but not started.
type queuedBuildView struct {
	Name        string `json:"name"`
	Project     string `json:"project"`
	QueuedAt    string `json:"queuedAt"`
	WaitSeconds int64  `json:"waitSeconds"`
}

type gatewayStatusView struct {
	Address    string `json:"address,omitempty"`
	Programmed bool   `json:"programmed"`
	Message    string `json:"message,omitempty"`
}

// componentStatusView is one platform workload out of the Kitchen singleton's
// component survey — the operator's own answer to "is what the chart installed
// actually running".
type componentStatusView struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Healthy   bool   `json:"healthy"`
	Available int32  `json:"available"`
	Desired   int32  `json:"desired"`
	Message   string `json:"message,omitempty"`
}

// statusView is everything the dashboard's status bar shows, in one request.
// It overlaps /settings on the gateway deliberately: settings is the platform
// as configured, this is the platform as it is running.
//
// It is the one payload that varies by role, and docs/AUTH.md says why: it is
// the home page for both of the platform's people, so it keeps the build queue
// for everyone — "why is my build waiting" is a developer's question — and
// drops the platform's own health for a member. A second endpoint would have
// doubled the surface for one payload.
//
// The operator's three halves are pointers and a slice for one reason: a
// withheld field is absent, never zeroed. The dashboard has to be able to tell
// "no tunnel is configured" from "you are not allowed to know", and an empty
// component survey reads as a healthy platform running nothing.
type statusView struct {
	Cluster clusterStatusView `json:"cluster"`
	Builds  buildQueueView    `json:"builds"`

	Tunnel     *tunnelStatusView     `json:"tunnel,omitempty"`
	Gateway    *gatewayStatusView    `json:"gateway,omitempty"`
	Components []componentStatusView `json:"components,omitempty"`
}

type connectionView struct {
	Name         string          `json:"name"`
	Provider     string          `json:"provider"`
	Capabilities []string        `json:"capabilities,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
	Conditions   []conditionView `json:"conditions,omitempty"`
}

// newConnectionView deliberately says nothing about the Connection's
// credentials: the secret it names is the operator's business, not an API
// client's.
func newConnectionView(connection *kitchenv1alpha1.Connection) connectionView {
	capabilities := make([]string, 0, len(connection.Status.Capabilities))
	for _, capability := range connection.Status.Capabilities {
		capabilities = append(capabilities, string(capability))
	}
	return connectionView{
		Name:         connection.Name,
		Provider:     connection.Spec.Provider,
		Capabilities: capabilities,
		CreatedAt:    connection.CreationTimestamp.Time,
		Conditions:   conditionViews(connection.Status.Conditions),
	}
}

// connectionChoiceView is a connection as somebody *choosing* one sees it,
// which is the whole of what a project's members are told: what it is called,
// what it can back, and whether the platform has it working.
//
// It is a type of its own rather than connectionView with fields left empty,
// and that is the point of it. A blanked-out view is one forgetful `if` away
// from publishing the operator's; worse, a field added to connectionView later
// would be published to everybody by a struct they happen to share, without
// anybody deciding to. Widening what a member sees has to be an edit to this
// struct.
//
// What is deliberately not here: the provider, the config, the conditions and
// their messages. A condition's message is the provider's own words — a
// hostname, an API endpoint, an authentication failure — and it is the
// operator's business. Ready plus Capabilities is enough to fill a dropdown
// and to say why an entry cannot be chosen; the rest of the answer lives on
// the operator's Connections screen, which is where fixing it lives too.
type connectionChoiceView struct {
	Name         string   `json:"name"`
	Capabilities []string `json:"capabilities"`
	Ready        bool     `json:"ready"`
}

// credentialsValidCondition is the condition the ConnectionReconciler writes
// its verdict on the stored credential into. It is spelled here as well
// because that reconciler keeps it unexported; the constant it has to agree
// with is `condCredentialsValid` in internal/controller.
const credentialsValidCondition = "CredentialsValid"

// newConnectionChoiceView reads readiness off the one condition that answers
// "would this work if a project named it": the platform has reached the
// provider and the provider accepted the credential. A connection nothing has
// assessed yet is not ready — which is the honest answer, and reads in a
// dropdown as an entry to wait for rather than one to blame a failed build on.
func newConnectionChoiceView(connection *kitchenv1alpha1.Connection) connectionChoiceView {
	capabilities := make([]string, 0, len(connection.Status.Capabilities))
	for _, capability := range connection.Status.Capabilities {
		capabilities = append(capabilities, string(capability))
	}
	return connectionChoiceView{
		Name:         connection.Name,
		Capabilities: capabilities,
		Ready:        meta.IsStatusConditionTrue(connection.Status.Conditions, credentialsValidCondition),
	}
}

// domainVerificationView is the DNS change that proves ownership, exactly as
// the user has to type it into their zone. Either record satisfies the check.
type domainVerificationView struct {
	TXTRecord string `json:"txtRecord"`
	TXTValue  string `json:"txtValue"`
	// CNAMETarget both verifies and routes: the record the hostname needs
	// anyway to reach the platform.
	CNAMETarget string `json:"cnameTarget,omitempty"`
}

type domainView struct {
	Name        string `json:"name"`
	Hostname    string `json:"hostname"`
	Environment string `json:"environment"`
	// TLS is the spec's own mode; empty means it inherits the platform's.
	// EffectiveTLS is the mode actually in effect, as the reconciler observed.
	TLS          string                  `json:"tls,omitempty"`
	EffectiveTLS string                  `json:"effectiveTLS,omitempty"`
	Verified     bool                    `json:"verified"`
	Verification *domainVerificationView `json:"verification,omitempty"`
	CreatedAt    time.Time               `json:"createdAt"`
	Conditions   []conditionView         `json:"conditions,omitempty"`
}

func newDomainView(domain *kitchenv1alpha1.Domain) domainView {
	view := domainView{
		Name:         domain.Name,
		Hostname:     domain.Spec.Hostname,
		Environment:  domain.Spec.EnvironmentRef.Name,
		TLS:          string(domain.Spec.TLS),
		EffectiveTLS: string(domain.Status.TLSMode),
		Verified:     domain.Status.Verified,
		CreatedAt:    domain.CreationTimestamp.Time,
		Conditions:   conditionViews(domain.Status.Conditions),
	}
	if v := domain.Status.Verification; v != nil {
		view.Verification = &domainVerificationView{
			TXTRecord:   v.TXTRecord,
			TXTValue:    v.TXTValue,
			CNAMETarget: v.CNAMETarget,
		}
	}
	return view
}

type claimView struct {
	Name           string `json:"name"`
	Project        string `json:"project"`
	Connection     string `json:"connection"`
	Type           string `json:"type"`
	Phase          string `json:"phase,omitempty"`
	Secret         string `json:"secret,omitempty"`
	DeletionPolicy string `json:"deletionPolicy,omitempty"`
	// PreviewMode is what a preview environment of the project binds to, as
	// the reconciler resolved it from the provider's declaration and the
	// claim's own choice: branch, fresh, shared or none. PreviewReason is
	// the sentence behind it — the provider's own words, or why previews
	// get nothing. Both are empty until the claim has been reconciled.
	// PreviewChoice is what the claim itself asked for, empty for the
	// provider's default.
	PreviewMode   string `json:"previewMode,omitempty"`
	PreviewReason string `json:"previewReason,omitempty"`
	PreviewChoice string `json:"previewChoice,omitempty"`
	// KeepsPodsRunning and ForcesRecreate are the provider's declarations
	// of what the binding does to the workload that reads it: no
	// environment reading the claim idles to zero, and the workload is
	// deployed by recreation with a gap in serving. Absent means neither.
	KeepsPodsRunning bool `json:"keepsPodsRunning,omitempty"`
	ForcesRecreate   bool `json:"forcesRecreate,omitempty"`
	// Redis is what a redis claim asked its instance to be — the usage, the
	// memory limit, the version. Absent when it asked for nothing in
	// particular.
	Redis *claimRedisView `json:"redis,omitempty"`

	// DataClass is the claim's declared sensitivity class — never above its
	// project's, which the create refuses. Absent means unclassified.
	DataClass string `json:"dataClass,omitempty"`
	// DataProvenance is the provider's declaration of what the provisioned
	// data derives from: production, masked or synthetic. Absent means the
	// provider declared nothing — shown as undeclared, treated by policy as
	// the worst case. Residency is where the provider reported the resource
	// actually is; absent means it reported nothing.
	DataProvenance string `json:"dataProvenance,omitempty"`
	Residency      string `json:"residency,omitempty"`
	// InstanceName is what the provider calls the resource this claim is
	// bound to — kitchen-<project>-<claim>, or the unqualified name a claim
	// bound before provider-side names carried the project keeps. It is
	// answered because it is what an operator needs to know to hand a
	// resource over (docs/api/claims.md, "Rebinding a retained resource");
	// it is a name, never a credential. Absent until the claim binds.
	InstanceName string          `json:"instanceName,omitempty"`
	CreatedAt    time.Time       `json:"createdAt"`
	Conditions   []conditionView `json:"conditions,omitempty"`

	// Postgres is what the claim asked the database itself to be — the major
	// version, the extensions, the volume. Absent when the claim asked for
	// nothing in particular, which is most of them. Whether it was *granted*
	// is the claim's phase and its conditions: a claim asking for an
	// extension no image can supply is Failed, with the refusal as the
	// condition's message.
	Postgres *claimPostgresView `json:"postgres,omitempty"`

	// ObjectStore is what the claim asked its bucket to be — versioned,
	// publicly readable, held to a size. Absent when it asked for nothing.
	ObjectStore *claimObjectStoreView `json:"objectStore,omitempty"`
	// Volume is what a volume claim asked for — the process, the size, the
	// class, the mount path — and, once the reconciler has looked, the
	// access mode the class was detected to support and the PVC behind it.
	Volume *claimVolumeView `json:"volume,omitempty"`

	// RedirectURIs is what an oidcClient claim's client currently accepts as
	// a callback — the list the operator keeps in step with the project's
	// environments. It is the one part of that automation anybody can check,
	// which is why it is answered rather than left on the custom resource.
	RedirectURIs []string `json:"redirectURIs,omitempty"`

	// CallbackPaths and Scopes are what the claim asked to be registered
	// with, with the platform's defaults filled in, so that a claim never
	// answers "unset" to a question it does have an answer to.
	CallbackPaths []string `json:"callbackPaths,omitempty"`
	Scopes        []string `json:"scopes,omitempty"`

	// Inngest is what an inngest claim binds — the app the worker connects
	// as, the Inngest environment production reads, the mode — with the
	// defaults filled in. Whether a worker has connected yet is the
	// AppConnected condition.
	Inngest *claimInngestView `json:"inngest,omitempty"`
}

// newClaimView is the claim as the API answers it. The fields every type
// has are filled here; the type's own — the postgres block, the OAuth
// client's registration — are filled by its shaper, so that a type nobody
// registered answers nothing rather than something another type's.
func newClaimView(claim *kitchenv1alpha1.ResourceClaim) claimView {
	view := claimView{
		Name:             claim.Name,
		Project:          claim.Spec.ProjectRef.Name,
		Connection:       claim.Connection(),
		Type:             claim.Spec.Type,
		Phase:            string(claim.Status.Phase),
		Secret:           claim.Status.SecretName,
		DeletionPolicy:   string(claim.Spec.DeletionPolicy),
		PreviewMode:      claim.Status.PreviewMode,
		PreviewReason:    claim.Status.PreviewReason,
		PreviewChoice:    claim.PreviewChoice(),
		KeepsPodsRunning: claim.Status.KeepsPodsRunning,
		ForcesRecreate:   claim.Status.ForcesRecreate,
		DataClass:        string(claim.Spec.DataClass),
		DataProvenance:   claim.Status.DataProvenance,
		Residency:        claim.Status.Residency,
		InstanceName:     claim.Status.InstanceName,
		CreatedAt:        claim.CreationTimestamp.Time,
		Conditions:       conditionViews(claim.Status.Conditions),
		RedirectURIs:     claim.Status.RedirectURIs,
	}
	if _, shaper, ok := claimShaperFor(claim.Spec.Type); ok {
		shaper.view(claim, &view)
	}
	return view
}
