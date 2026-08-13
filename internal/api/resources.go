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
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// get reads one object out of the platform namespace.
func (s *Server) get(ctx context.Context, name string, into client.Object) error {
	return s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, into)
}

// projectFilter is the `?project=` query every collection understands.
func projectFilter(req *http.Request) string {
	return strings.TrimSpace(req.URL.Query().Get("project"))
}

func (s *Server) listProjects(w http.ResponseWriter, req *http.Request) {
	list := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(req.Context(), list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })

	views := make([]projectView, 0, len(list.Items))
	for i := range list.Items {
		views = append(views, newProjectView(&list.Items[i]))
	}
	writeList(w, views)
}

func (s *Server) getProject(w http.ResponseWriter, req *http.Request) {
	project := &kitchenv1alpha1.Project{}
	if err := s.get(req.Context(), req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newProjectView(project))
}

// builds returns a project's builds, or every build when project is empty,
// newest first — a build list is read from the top.
func (s *Server) builds(ctx context.Context, project string) ([]kitchenv1alpha1.Build, error) {
	list := &kitchenv1alpha1.BuildList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	out := make([]kitchenv1alpha1.Build, 0, len(list.Items))
	for _, build := range list.Items {
		if project == "" || build.Spec.ProjectRef.Name == project {
			out = append(out, build)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreationTimestamp.Equal(&out[j].CreationTimestamp) {
			return out[i].Name > out[j].Name
		}
		return out[j].CreationTimestamp.Before(&out[i].CreationTimestamp)
	})
	return out, nil
}

func (s *Server) writeBuilds(w http.ResponseWriter, builds []kitchenv1alpha1.Build) {
	views := make([]buildView, 0, len(builds))
	for i := range builds {
		views = append(views, newBuildView(&builds[i]))
	}
	writeList(w, views)
}

func (s *Server) listBuilds(w http.ResponseWriter, req *http.Request) {
	builds, err := s.builds(req.Context(), projectFilter(req))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeBuilds(w, builds)
}

func (s *Server) listProjectBuilds(w http.ResponseWriter, req *http.Request) {
	project := &kitchenv1alpha1.Project{}
	if err := s.get(req.Context(), req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	builds, err := s.builds(req.Context(), project.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeBuilds(w, builds)
}

func (s *Server) getBuild(w http.ResponseWriter, req *http.Request) {
	build := &kitchenv1alpha1.Build{}
	if err := s.get(req.Context(), req.PathValue("name"), build); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newBuildView(build))
}

// createBuildRequest asks for a build of one commit. An empty body rebuilds
// whatever the project built last, which is what "rebuild" means when nobody
// says otherwise — a rerun after a flaky build or a changed secret.
type createBuildRequest struct {
	SHA     string `json:"sha,omitempty"`
	Branch  string `json:"branch,omitempty"`
	Message string `json:"message,omitempty"`
}

func (s *Server) createBuild(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := createBuildRequest{}
	if req.ContentLength != 0 {
		if err := decodeBody(req, &body); err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
	}
	body.SHA = strings.TrimSpace(body.SHA)
	body.Branch = strings.TrimSpace(body.Branch)

	revision, err := s.revisionToBuild(ctx, project, body)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	caller, _ := CallerFrom(ctx)
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			// A rebuild of a commit that was already built has to be a new
			// object — Build specs are immutable, and the history of who
			// asked for what is the point. Generated names keep the
			// webhook receiver's deterministic names free for the events
			// it deduplicates.
			GenerateName: buildNamePrefix(project.Name, revision.SHA),
			Namespace:    s.Namespace,
			Labels:       map[string]string{"kitchen.bermos.dev/project": project.Name},
			Annotations:  map[string]string{"kitchen.bermos.dev/requested-by": callerName(caller)},
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			Git:        revision,
		},
	}
	if err := s.Client.Create(ctx, build); err != nil {
		s.writeError(w, err)
		return
	}
	s.log().Info("build requested through the api",
		"project", project.Name, "build", build.Name, "sha", revision.SHA, "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, newBuildView(build))
}

// revisionToBuild works out which commit a build request means: the one it
// names, or the one the project built last.
func (s *Server) revisionToBuild(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	body createBuildRequest,
) (kitchenv1alpha1.GitRevision, error) {
	previous, err := s.builds(ctx, project.Name)
	if err != nil {
		return kitchenv1alpha1.GitRevision{}, err
	}

	if body.SHA == "" {
		if len(previous) == 0 {
			return kitchenv1alpha1.GitRevision{}, fmt.Errorf(
				"project %q has never been built, so there is nothing to rebuild: name a commit with {\"sha\": \"...\", \"branch\": \"...\"}",
				project.Name)
		}
		revision := previous[0].Spec.Git.DeepCopy()
		if body.Branch != "" {
			revision.Branch = body.Branch
		}
		if body.Message != "" {
			revision.Message = body.Message
		}
		return *revision, nil
	}

	if len(body.SHA) < 7 {
		return kitchenv1alpha1.GitRevision{}, fmt.Errorf("sha %q is too short: give at least the seven-character short form", body.SHA)
	}

	revision := kitchenv1alpha1.GitRevision{SHA: body.SHA, Branch: body.Branch, Message: body.Message}
	// A commit that has been built before carries its own branch and
	// authorship; a caller naming a fresh sha has to say which branch it is
	// on, unless the production branch is a fair assumption.
	for i := range previous {
		if previous[i].Spec.Git.SHA == body.SHA {
			if revision.Branch == "" {
				revision.Branch = previous[i].Spec.Git.Branch
			}
			if revision.Message == "" {
				revision.Message = previous[i].Spec.Git.Message
			}
			revision.Author = previous[i].Spec.Git.Author
			revision.PullRequest = previous[i].Spec.Git.PullRequest
			break
		}
	}
	if revision.Branch == "" {
		revision.Branch = project.Spec.Source.ProductionBranch
	}
	if revision.Branch == "" {
		return kitchenv1alpha1.GitRevision{}, fmt.Errorf(
			"commit %s has not been built before, so its branch is unknown: pass {\"branch\": \"...\"}", body.SHA)
	}
	return revision, nil
}

// buildNamePrefix mirrors the webhook receiver's naming so that a build looks
// the same whoever asked for it, trimmed to leave room for the generated
// suffix inside the 63-character limit on names.
func buildNamePrefix(project, sha string) string {
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	prefix := fmt.Sprintf("%s-bld-%s-", project, short)
	// Kubernetes appends five characters to a generated name.
	if len(prefix) > 58 {
		prefix = prefix[:57] + "-"
	}
	return prefix
}

// callerName is how a caller is recorded on the objects they create.
func callerName(caller Caller) string {
	if caller.Email != "" {
		return caller.Email
	}
	if caller.Subject != "" {
		return caller.Subject
	}
	return "unknown"
}

func (s *Server) releases(ctx context.Context, project string) ([]kitchenv1alpha1.Release, error) {
	list := &kitchenv1alpha1.ReleaseList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	out := make([]kitchenv1alpha1.Release, 0, len(list.Items))
	for _, release := range list.Items {
		if project == "" || release.Spec.ProjectRef.Name == project {
			out = append(out, release)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreationTimestamp.Equal(&out[j].CreationTimestamp) {
			return out[i].Name > out[j].Name
		}
		return out[j].CreationTimestamp.Before(&out[i].CreationTimestamp)
	})
	return out, nil
}

func (s *Server) writeReleases(w http.ResponseWriter, releases []kitchenv1alpha1.Release) {
	views := make([]releaseView, 0, len(releases))
	for i := range releases {
		views = append(views, newReleaseView(&releases[i]))
	}
	writeList(w, views)
}

func (s *Server) listReleases(w http.ResponseWriter, req *http.Request) {
	releases, err := s.releases(req.Context(), projectFilter(req))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeReleases(w, releases)
}

func (s *Server) listProjectReleases(w http.ResponseWriter, req *http.Request) {
	project := &kitchenv1alpha1.Project{}
	if err := s.get(req.Context(), req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	releases, err := s.releases(req.Context(), project.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeReleases(w, releases)
}

func (s *Server) getRelease(w http.ResponseWriter, req *http.Request) {
	release := &kitchenv1alpha1.Release{}
	if err := s.get(req.Context(), req.PathValue("name"), release); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newReleaseView(release))
}

func (s *Server) environments(ctx context.Context, project string) ([]kitchenv1alpha1.Environment, error) {
	list := &kitchenv1alpha1.EnvironmentList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		return nil, err
	}
	out := make([]kitchenv1alpha1.Environment, 0, len(list.Items))
	for _, env := range list.Items {
		if project == "" || env.Spec.ProjectRef.Name == project {
			out = append(out, env)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Server) writeEnvironments(w http.ResponseWriter, environments []kitchenv1alpha1.Environment) {
	views := make([]environmentView, 0, len(environments))
	for i := range environments {
		views = append(views, newEnvironmentView(&environments[i]))
	}
	writeList(w, views)
}

func (s *Server) listEnvironments(w http.ResponseWriter, req *http.Request) {
	environments, err := s.environments(req.Context(), projectFilter(req))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeEnvironments(w, environments)
}

func (s *Server) listProjectEnvironments(w http.ResponseWriter, req *http.Request) {
	project := &kitchenv1alpha1.Project{}
	if err := s.get(req.Context(), req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	environments, err := s.environments(req.Context(), project.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeEnvironments(w, environments)
}

func (s *Server) getEnvironment(w http.ResponseWriter, req *http.Request) {
	env := &kitchenv1alpha1.Environment{}
	if err := s.get(req.Context(), req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newEnvironmentView(env))
}

// patchEnvironmentRequest changes which Release an Environment runs. That one
// field is the whole of promotion and rollback: Releases are immutable
// snapshots of an image and its configuration, so pointing an Environment at
// an older one puts back exactly what was running.
type patchEnvironmentRequest struct {
	Release string `json:"release"`
}

func (s *Server) patchEnvironment(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}

	body := patchEnvironmentRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Release = strings.TrimSpace(body.Release)
	if body.Release == "" {
		badRequest(w, "release is required: {\"release\": \"<release name>\"}")
		return
	}

	release := &kitchenv1alpha1.Release{}
	if err := s.get(ctx, body.Release, release); err != nil {
		s.writeError(w, err)
		return
	}
	// Rolling one project's environment onto another project's release would
	// deploy a stranger's image under this project's URL.
	if release.Spec.ProjectRef.Name != env.Spec.ProjectRef.Name {
		badRequest(w, "release %q belongs to project %q, but environment %q belongs to project %q",
			release.Name, release.Spec.ProjectRef.Name, env.Name, env.Spec.ProjectRef.Name)
		return
	}

	if env.Spec.ReleaseRef.Name == release.Name {
		writeJSON(w, http.StatusOK, newEnvironmentView(env))
		return
	}

	patch := client.MergeFrom(env.DeepCopy())
	env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: release.Name}
	if err := s.Client.Patch(ctx, env, patch); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("environment moved to another release through the api",
		"environment", env.Name, "release", release.Name, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newEnvironmentView(env))
}

func (s *Server) listConnections(w http.ResponseWriter, req *http.Request) {
	list := &kitchenv1alpha1.ConnectionList{}
	if err := s.Client.List(req.Context(), list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })

	views := make([]connectionView, 0, len(list.Items))
	for i := range list.Items {
		views = append(views, newConnectionView(&list.Items[i]))
	}
	writeList(w, views)
}

func (s *Server) getConnection(w http.ResponseWriter, req *http.Request) {
	connection := &kitchenv1alpha1.Connection{}
	if err := s.get(req.Context(), req.PathValue("name"), connection); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newConnectionView(connection))
}

func (s *Server) listDomains(w http.ResponseWriter, req *http.Request) {
	list := &kitchenv1alpha1.DomainList{}
	if err := s.Client.List(req.Context(), list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}

	environment := strings.TrimSpace(req.URL.Query().Get("environment"))
	views := make([]domainView, 0, len(list.Items))
	for i := range list.Items {
		if environment != "" && list.Items[i].Spec.EnvironmentRef.Name != environment {
			continue
		}
		views = append(views, newDomainView(&list.Items[i]))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Hostname < views[j].Hostname })
	writeList(w, views)
}

func (s *Server) getDomain(w http.ResponseWriter, req *http.Request) {
	domain := &kitchenv1alpha1.Domain{}
	if err := s.get(req.Context(), req.PathValue("name"), domain); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newDomainView(domain))
}

func (s *Server) listClaims(w http.ResponseWriter, req *http.Request) {
	list := &kitchenv1alpha1.ResourceClaimList{}
	if err := s.Client.List(req.Context(), list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}

	project := projectFilter(req)
	views := make([]claimView, 0, len(list.Items))
	for i := range list.Items {
		if project != "" && list.Items[i].Spec.ProjectRef.Name != project {
			continue
		}
		views = append(views, newClaimView(&list.Items[i]))
	}
	sort.Slice(views, func(i, j int) bool { return views[i].Name < views[j].Name })
	writeList(w, views)
}

func (s *Server) getClaim(w http.ResponseWriter, req *http.Request) {
	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(req.Context(), req.PathValue("name"), claim); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newClaimView(claim))
}
