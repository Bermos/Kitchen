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
	Role                  string          `json:"role"`
	Repo                  string          `json:"repo"`
	Connection            string          `json:"connection"`
	Registry              string          `json:"registry"`
	ProductionBranch      string          `json:"productionBranch"`
	Previews              bool            `json:"previews"`
	PreviewsProtected     bool            `json:"previewsProtected"`
	BuildStrategy         string          `json:"buildStrategy,omitempty"`
	DockerfilePath        string          `json:"dockerfilePath,omitempty"`
	RootDirectory         string          `json:"rootDirectory,omitempty"`
	Env                   []envVarView    `json:"env,omitempty"`
	Port                  int32           `json:"port,omitempty"`
	Replicas              *int32          `json:"replicas,omitempty"`
	CPU                   string          `json:"cpu,omitempty"`
	Memory                string          `json:"memory,omitempty"`
	ProductionEnvironment string          `json:"productionEnvironment,omitempty"`
	LatestBuild           string          `json:"latestBuild,omitempty"`
	CreatedAt             time.Time       `json:"createdAt"`
	Conditions            []conditionView `json:"conditions,omitempty"`
}

func newProjectView(project *kitchenv1alpha1.Project, role access.ProjectRole) projectView {
	view := projectView{
		Name:              project.Name,
		Role:              role.String(),
		Repo:              project.Spec.Source.Repo,
		Connection:        project.Spec.Source.ConnectionRef.Name,
		Registry:          project.Spec.Registry.ConnectionRef.Name,
		ProductionBranch:  project.Spec.Source.ProductionBranch,
		Previews:          project.Spec.Previews.Enabled,
		PreviewsProtected: project.Spec.Previews.IsProtected(),
		BuildStrategy:     string(project.Spec.Build.Strategy),
		DockerfilePath:    project.Spec.Build.DockerfilePath,
		RootDirectory:     project.Spec.Build.RootDirectory,
		Env:               envVarViews(project.Spec.Env),
		Port:              project.Spec.Runtime.Port,
		Replicas:          project.Spec.Runtime.Replicas,
		CreatedAt:         project.CreationTimestamp.Time,
		Conditions:        conditionViews(project.Status.Conditions),
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
	return view
}

type revisionView struct {
	SHA         string `json:"sha"`
	Branch      string `json:"branch"`
	Message     string `json:"message,omitempty"`
	Author      string `json:"author,omitempty"`
	PullRequest *int32 `json:"pullRequest,omitempty"`
}

type buildView struct {
	Name              string          `json:"name"`
	Project           string          `json:"project"`
	Phase             string          `json:"phase,omitempty"`
	Git               revisionView    `json:"git"`
	DetectedFramework string          `json:"detectedFramework,omitempty"`
	Image             string          `json:"image,omitempty"`
	StartedAt         *time.Time      `json:"startedAt,omitempty"`
	CompletedAt       *time.Time      `json:"completedAt,omitempty"`
	CreatedAt         time.Time       `json:"createdAt"`
	Conditions        []conditionView `json:"conditions,omitempty"`
	// Artifact is what the build produced, by content, and whether the
	// platform managed to attest it. Absent on a build that never got as far
	// as pushing anything.
	Artifact *artifactView `json:"artifact,omitempty"`
	// Cache is what the layer cache did for this build. Absent on a build
	// that was never run.
	Cache *buildCacheView `json:"cache,omitempty"`
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

// artifactView is the artifact half of a build. It carries whether evidence
// was attached and under which key, but not the evidence itself: that lives in
// the registry against the digest, and the attestations endpoint is where it
// is read from.
type artifactView struct {
	Repository string     `json:"repository,omitempty"`
	Digest     string     `json:"digest,omitempty"`
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
		Name:    build.Name,
		Project: build.Spec.ProjectRef.Name,
		Phase:   string(build.Status.Phase),
		Git: revisionView{
			SHA:         build.Spec.Git.SHA,
			Branch:      build.Spec.Git.Branch,
			Message:     build.Spec.Git.Message,
			Author:      build.Spec.Git.Author,
			PullRequest: build.Spec.Git.PullRequest,
		},
		DetectedFramework: build.Status.DetectedFramework,
		Image:             build.Status.Image,
		Artifact:          newArtifactView(build.Status.Artifact),
		Cache:             newBuildCacheView(build.Status.Cache),
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
	Name         string    `json:"name"`
	Project      string    `json:"project"`
	Build        string    `json:"build"`
	Image        string    `json:"image"`
	Environments []string  `json:"environments,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

func newReleaseView(release *kitchenv1alpha1.Release) releaseView {
	return releaseView{
		Name:         release.Name,
		Project:      release.Spec.ProjectRef.Name,
		Build:        release.Spec.BuildRef.Name,
		Image:        release.Spec.Image,
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

type environmentView struct {
	Name            string               `json:"name"`
	Project         string               `json:"project"`
	Type            string               `json:"type"`
	Release         string               `json:"release"`
	ObservedRelease string               `json:"observedRelease,omitempty"`
	Phase           string               `json:"phase,omitempty"`
	URL             string               `json:"url,omitempty"`
	Preview         *previewView         `json:"preview,omitempty"`
	History         []releaseHistoryView `json:"history,omitempty"`
	CreatedAt       time.Time            `json:"createdAt"`
	Conditions      []conditionView      `json:"conditions,omitempty"`
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
	Name             string          `json:"name"`
	Project          string          `json:"project"`
	Connection       string          `json:"connection"`
	Type             string          `json:"type"`
	Phase            string          `json:"phase,omitempty"`
	Secret           string          `json:"secret,omitempty"`
	DeletionPolicy   string          `json:"deletionPolicy,omitempty"`
	PreviewBranching bool            `json:"previewBranching"`
	CreatedAt        time.Time       `json:"createdAt"`
	Conditions       []conditionView `json:"conditions,omitempty"`
}

func newClaimView(claim *kitchenv1alpha1.ResourceClaim) claimView {
	return claimView{
		Name:             claim.Name,
		Project:          claim.Spec.ProjectRef.Name,
		Connection:       claim.Spec.ConnectionRef.Name,
		Type:             claim.Spec.Type,
		Phase:            string(claim.Status.Phase),
		Secret:           claim.Status.SecretName,
		DeletionPolicy:   string(claim.Spec.DeletionPolicy),
		PreviewBranching: claim.PreviewBranching(),
		CreatedAt:        claim.CreationTimestamp.Time,
		Conditions:       conditionViews(claim.Status.Conditions),
	}
}
