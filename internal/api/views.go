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

type projectView struct {
	Name                  string          `json:"name"`
	Repo                  string          `json:"repo"`
	Connection            string          `json:"connection"`
	Registry              string          `json:"registry"`
	ProductionBranch      string          `json:"productionBranch"`
	Previews              bool            `json:"previews"`
	ProductionEnvironment string          `json:"productionEnvironment,omitempty"`
	LatestBuild           string          `json:"latestBuild,omitempty"`
	CreatedAt             time.Time       `json:"createdAt"`
	Conditions            []conditionView `json:"conditions,omitempty"`
}

func newProjectView(project *kitchenv1alpha1.Project) projectView {
	view := projectView{
		Name:             project.Name,
		Repo:             project.Spec.Source.Repo,
		Connection:       project.Spec.Source.ConnectionRef.Name,
		Registry:         project.Spec.Registry.ConnectionRef.Name,
		ProductionBranch: project.Spec.Source.ProductionBranch,
		Previews:         project.Spec.Previews.Enabled,
		CreatedAt:        project.CreationTimestamp.Time,
		Conditions:       conditionViews(project.Status.Conditions),
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
	Name       string          `json:"name"`
	Project    string          `json:"project"`
	Connection string          `json:"connection"`
	Type       string          `json:"type"`
	Phase      string          `json:"phase,omitempty"`
	Secret     string          `json:"secret,omitempty"`
	CreatedAt  time.Time       `json:"createdAt"`
	Conditions []conditionView `json:"conditions,omitempty"`
}

func newClaimView(claim *kitchenv1alpha1.ResourceClaim) claimView {
	return claimView{
		Name:       claim.Name,
		Project:    claim.Spec.ProjectRef.Name,
		Connection: claim.Spec.ConnectionRef.Name,
		Type:       claim.Spec.Type,
		Phase:      string(claim.Status.Phase),
		Secret:     claim.Status.SecretName,
		CreatedAt:  claim.CreationTimestamp.Time,
		Conditions: conditionViews(claim.Status.Conditions),
	}
}
