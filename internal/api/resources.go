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
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
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

// createProjectRequest is everything the create flow asks for: a name, a
// repository, and the two Connections it builds and stores images with. It
// speaks the API's vocabulary — connections are named, never nested specs —
// and everything else on a Project keeps its default until a flow needs it.
type createProjectRequest struct {
	Name             string `json:"name"`
	Repo             string `json:"repo"`
	Connection       string `json:"connection"`
	Registry         string `json:"registry"`
	ProductionBranch string `json:"productionBranch,omitempty"`
	Previews         *bool  `json:"previews,omitempty"`
}

// maxProjectNameLength is what fits: the platform derives object names from
// the project's — the longest, a Release's "<project>-rel-<12-char sha>",
// adds 17 characters and still has to fit Kubernetes' 63-character limit.
const maxProjectNameLength = 46

// defaultProductionBranch is the CRD's own default for
// spec.source.productionBranch, applied here as well so the response is
// honest against a client the API server never defaulted for.
const defaultProductionBranch = "main"

// validateProjectName checks a name before it becomes namespaces, hostnames
// and generated object names, which is why plain DNS-1123 is not enough.
func validateProjectName(name string) error {
	if name == "" {
		return errors.New("name is required")
	}
	if len(name) > maxProjectNameLength {
		return fmt.Errorf(
			"name must be at most %d characters: the names the platform derives from it (releases, namespaces, hostnames) have to fit Kubernetes' 63-character limit",
			maxProjectNameLength)
	}
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("name must work as a DNS label — lowercase letters, digits and '-', starting and ending alphanumeric (got %q)", name)
	}
	return nil
}

// requireConnection answers whether the named Connection can back the given
// capability, writing the response when it cannot. A Connection that has not
// reported capabilities yet is accepted, mirroring what the project
// controller tolerates: the Project's own conditions say so if it turns out
// not to fit. A missing Connection is a 400, not a 404 — the endpoint exists,
// the body names something that does not.
func (s *Server) requireConnection(
	ctx context.Context,
	w http.ResponseWriter,
	field, name string,
	capability kitchenv1alpha1.Capability,
) bool {
	if name == "" {
		badRequest(w, "%s is required: the name of a Connection with the %s capability", field, capability)
		return false
	}
	conn := &kitchenv1alpha1.Connection{}
	if err := s.get(ctx, name, conn); err != nil {
		if apierrors.IsNotFound(err) {
			badRequest(w, "%s %q does not exist: create the Connection first", field, name)
		} else {
			s.writeError(w, err)
		}
		return false
	}
	if len(conn.Status.Capabilities) == 0 {
		return true
	}
	for _, c := range conn.Status.Capabilities {
		if c == capability {
			return true
		}
	}
	badRequest(w, "connection %q does not provide the %s capability", name, capability)
	return false
}

func (s *Server) createProject(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := createProjectRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Repo = strings.TrimSpace(body.Repo)
	body.Connection = strings.TrimSpace(body.Connection)
	body.Registry = strings.TrimSpace(body.Registry)
	body.ProductionBranch = strings.TrimSpace(body.ProductionBranch)

	if err := validateProjectName(body.Name); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if owner, repo, ok := strings.Cut(body.Repo, "/"); !ok || owner == "" || repo == "" {
		badRequest(w, "repo must be the provider's owner/name form (got %q)", body.Repo)
		return
	}
	if !s.requireConnection(ctx, w, "connection", body.Connection, kitchenv1alpha1.CapabilityGitSource) {
		return
	}
	if !s.requireConnection(ctx, w, "registry", body.Registry, kitchenv1alpha1.CapabilityImageStore) {
		return
	}

	branch := body.ProductionBranch
	if branch == "" {
		branch = defaultProductionBranch
	}
	previews := true
	if body.Previews != nil {
		previews = *body.Previews
	}

	caller, _ := CallerFrom(ctx)
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:        body.Name,
			Namespace:   s.Namespace,
			Annotations: map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.GitSourceSpec{
				ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: body.Connection},
				Repo:             body.Repo,
				ProductionBranch: branch,
			},
			Registry: kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: body.Registry},
			},
			Previews: kitchenv1alpha1.PreviewsSpec{Enabled: previews},
		},
	}
	if err := s.Client.Create(ctx, project); err != nil {
		s.writeError(w, err)
		return
	}
	s.log().Info("project created through the api",
		"project", project.Name, "repo", body.Repo, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventProjectCreated,
		Project: project.Name,
		Message: fmt.Sprintf("project %s created from %s", project.Name, body.Repo),
		Actor:   callerName(caller),
	})
	writeJSON(w, http.StatusCreated, newProjectView(project))
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

	// Moving to an older release is a rollback; anything else superseded the
	// one running. Releases are immutable, so creation time is the order they
	// were cut in. A deleted outgoing release cannot be compared any more and
	// counts as superseded.
	outgoing := env.Spec.ReleaseRef.Name
	reason := kitchenv1alpha1.ReleaseMoveSuperseded
	previous := &kitchenv1alpha1.Release{}
	if err := s.get(ctx, outgoing, previous); err == nil &&
		release.CreationTimestamp.Before(&previous.CreationTimestamp) {
		reason = kitchenv1alpha1.ReleaseMoveRolledBack
	}

	// The activity feed tells the same story in its own vocabulary: the
	// history's reason describes what happened to the outgoing release,
	// the feed entry describes what was done to the environment.
	moveType, verb := clickhouse.EventReleasePromoted, "promoted to"
	if reason == kitchenv1alpha1.ReleaseMoveRolledBack {
		moveType, verb = clickhouse.EventReleaseRolledBack, "rolled back to"
	}

	patch := client.MergeFrom(env.DeepCopy())
	env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: release.Name}
	if err := s.Client.Patch(ctx, env, patch); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	// Persist what the audit log line below already knows: how the outgoing
	// release stopped being current, and who moved the environment off it.
	base := env.DeepCopy()
	if env.RecordReleaseMove(outgoing, reason, callerName(caller)) {
		if err := s.Client.Status().Patch(ctx, env, client.MergeFrom(base)); err != nil {
			// The spec change went through either way; the environment
			// reconciler still records the move, just without the caller.
			s.log().Error(err, "failed to record release history",
				"environment", env.Name, "release", outgoing)
		}
	}

	s.log().Info("environment moved to another release through the api",
		"environment", env.Name, "release", release.Name, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:        moveType,
		Project:     env.Spec.ProjectRef.Name,
		Environment: env.Name,
		Release:     release.Name,
		Message:     fmt.Sprintf("%s %s release %s", env.Name, verb, release.Name),
		Actor:       callerName(caller),
	})
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
