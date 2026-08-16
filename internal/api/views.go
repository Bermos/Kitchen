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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
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

// envVarView is one of a project's environment variables. The value of a
// secret- or claim-backed variable is a reference here, never the resolved
// value: the API hands out configuration, not credentials.
type envVarView struct {
	Name         string      `json:"name"`
	Value        string      `json:"value,omitempty"`
	PreviewValue string      `json:"previewValue,omitempty"`
	FromSecret   *keyRefView `json:"fromSecret,omitempty"`
	FromClaim    *keyRefView `json:"fromClaim,omitempty"`
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
		view := envVarView{Name: v.Name, Value: v.Value, PreviewValue: v.PreviewValue}
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
	Name                  string          `json:"name"`
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

func newProjectView(project *kitchenv1alpha1.Project) projectView {
	view := projectView{
		Name:              project.Name,
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
type clusterStatusView struct {
	Name       string `json:"name,omitempty"`
	Nodes      int    `json:"nodes"`
	ReadyNodes int    `json:"readyNodes"`
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
type statusView struct {
	Cluster    clusterStatusView     `json:"cluster"`
	Tunnel     tunnelStatusView      `json:"tunnel"`
	Builds     buildQueueView        `json:"builds"`
	Gateway    gatewayStatusView     `json:"gateway"`
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

type domainView struct {
	Name        string          `json:"name"`
	Hostname    string          `json:"hostname"`
	Environment string          `json:"environment"`
	TLS         string          `json:"tls,omitempty"`
	Verified    bool            `json:"verified"`
	CreatedAt   time.Time       `json:"createdAt"`
	Conditions  []conditionView `json:"conditions,omitempty"`
}

func newDomainView(domain *kitchenv1alpha1.Domain) domainView {
	return domainView{
		Name:        domain.Name,
		Hostname:    domain.Spec.Hostname,
		Environment: domain.Spec.EnvironmentRef.Name,
		TLS:         string(domain.Spec.TLS),
		Verified:    domain.Status.Verified,
		CreatedAt:   domain.CreationTimestamp.Time,
		Conditions:  conditionViews(domain.Status.Conditions),
	}
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
