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

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/appconfig"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// get reads one object out of the platform namespace.
//
// It first takes whatever the request's guard already read to work out which
// project the request is about (see resolvedObject): every route that names an
// object resolves its project from that object, so without this a guarded
// handler would read the same thing twice on every request.
func (s *Server) get(ctx context.Context, name string, into client.Object) error {
	if resolvedObject(ctx, name, into) {
		return nil
	}
	return s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, into)
}

// projectFilter is the `?project=` query every collection understands.
func projectFilter(req *http.Request) string {
	return strings.TrimSpace(req.URL.Query().Get("project"))
}

// roleOn is the calling account's role on one project, and the one way this
// package works out what it is: the rule — including an operator's admin on
// every project — lives in internal/access, which the preview gate asks too.
func (s *Server) roleOn(ctx context.Context, project *kitchenv1alpha1.Project) access.ProjectRole {
	caller, _ := CallerFrom(ctx)
	return access.ProjectRoleFor(caller.access(), kitchenFrom(ctx), project)
}

func (s *Server) listProjects(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	list := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })

	scope := scopeFrom(ctx)
	views := make([]projectView, 0, len(list.Items))
	for i := range list.Items {
		if !scope.allows(list.Items[i].Name) {
			continue
		}
		views = append(views, newProjectView(&list.Items[i], s.roleOn(ctx, &list.Items[i])))
	}
	writeList(w, views)
}

func (s *Server) getProject(w http.ResponseWriter, req *http.Request) {
	project := &kitchenv1alpha1.Project{}
	if err := s.get(req.Context(), req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	ctx := req.Context()
	writeJSON(w, http.StatusOK, s.withFileContent(ctx, project, newProjectView(project, s.roleOn(ctx, project))))
}

// createProjectRequest is everything the create flow asks for: a name, a
// repository, and the two Connections it builds and stores images with. It
// speaks the API's vocabulary — connections are named, never nested specs —
// and everything else on a Project keeps its default until a flow needs it.
//
// RootDirectory and DockerfilePath are the exception, and they are here
// because of what happens immediately afterwards: creating a project starts a
// build of the production branch. A monorepo whose application is in
// apps/shop, or a Dockerfile somewhere other than the root, would have that
// first build fail and then be corrected by a PATCH nobody realises they need
// — so the two fields the preflight (POST /connections/{name}/detect) exists
// to let somebody correct are the two that can be sent with the project.
type createProjectRequest struct {
	Name string `json:"name"`
	// Repo, Connection and Registry are a project built from a repository:
	// where the source is, what reads it, and where the images it produces
	// are pushed. Image is the other kind of project — software this
	// platform did not build — and exactly one of `repo` and `image` is
	// sent (#307).
	Repo       string `json:"repo,omitempty"`
	Connection string `json:"connection,omitempty"`
	Registry   string `json:"registry,omitempty"`
	// Image is the web process's image, published by somebody else: a
	// repository, a tag or a digest of it, and optionally the Connection to
	// pull it with. A project sent this way has no repository, so it needs
	// no registry, no source connection, no production branch and no
	// previews — and is refused all four rather than having them ignored.
	Image            *appconfig.Image `json:"image,omitempty"`
	ProductionBranch string           `json:"productionBranch,omitempty"`
	Previews         *bool            `json:"previews,omitempty"`
	RootDirectory    string           `json:"rootDirectory,omitempty"`
	DockerfilePath   string           `json:"dockerfilePath,omitempty"`
	// DockerfileTarget is the stage of a multi-stage Dockerfile to ship. It
	// is here for the reason the two paths are: the preflight lists the
	// stages the file declares while the form is still open, and a project
	// created without the one it meant builds a different stage and reports
	// success.
	DockerfileTarget string `json:"dockerfileTarget,omitempty"`
}

// defaultProductionBranch is the CRD's own default for
// spec.source.productionBranch, applied here as well so the response is
// honest against a client the API server never defaulted for.
const defaultProductionBranch = "main"

// validateProjectName checks a name before it becomes namespaces, hostnames
// and generated object names, which is why plain DNS-1123 is not enough.
func validateProjectName(name string) error {
	return appconfig.ValidateProjectName(name)
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

// projectSource settles which kind of project is being created, answering the
// request itself when the body describes neither or both.
//
// A project's source is a union with exactly one member set, and this is
// where a person finds that out — the CRD's rule would refuse the same body
// with the same meaning, but only after a round trip and in the vocabulary of
// an admission webhook. Everything a repository needs is refused of an image
// project here rather than ignored: a registry, a source connection, a
// production branch and a build root all read back as settings that took, and
// none of them would do anything.
func (s *Server) projectSource(
	ctx context.Context,
	w http.ResponseWriter,
	body createProjectRequest,
) (kitchenv1alpha1.ProjectSourceSpec, bool) {
	switch {
	case body.Repo == "" && body.Image == nil:
		badRequest(w, "a project is software that becomes a running application, and this names none: "+
			"send `repo` with the connection that reads it, or `image` with a repository somebody else "+
			"publishes and a tag or a digest of it")
		return kitchenv1alpha1.ProjectSourceSpec{}, false
	case body.Repo != "" && body.Image != nil:
		badRequest(w, "a project's source is one thing: `repo` is a repository this platform builds and "+
			"`image` is software somebody else built — send one of them, not both")
		return kitchenv1alpha1.ProjectSourceSpec{}, false
	case body.Image != nil:
		for _, refused := range []struct{ field, value string }{
			{"registry", body.Registry},
			{"connection", body.Connection},
			{"productionBranch", body.ProductionBranch},
		} {
			if refused.value == "" {
				continue
			}
			badRequest(w, "%s is a repository's setting, and this project's source is an image: "+
				"it builds nothing, pushes nothing and has no branch", refused.field)
			return kitchenv1alpha1.ProjectSourceSpec{}, false
		}
		image, err := appconfig.ImageSource("image", *body.Image)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return kitchenv1alpha1.ProjectSourceSpec{}, false
		}
		// The pull credential, where one was named. A public image needs
		// none, which is why an empty connection is not an error here.
		if image.ConnectionRef != nil &&
			!s.requireConnection(ctx, w, "image.connection", image.ConnectionRef.Name,
				kitchenv1alpha1.CapabilityImageStore) {
			return kitchenv1alpha1.ProjectSourceSpec{}, false
		}
		return kitchenv1alpha1.ProjectSourceSpec{Image: image}, true
	}

	if owner, repo, ok := strings.Cut(body.Repo, "/"); !ok || owner == "" || repo == "" {
		badRequest(w, "repo must be the provider's owner/name form (got %q)", body.Repo)
		return kitchenv1alpha1.ProjectSourceSpec{}, false
	}
	if !s.requireConnection(ctx, w, "connection", body.Connection, kitchenv1alpha1.CapabilityGitSource) {
		return kitchenv1alpha1.ProjectSourceSpec{}, false
	}
	if !s.requireConnection(ctx, w, "registry", body.Registry, kitchenv1alpha1.CapabilityImageStore) {
		return kitchenv1alpha1.ProjectSourceSpec{}, false
	}
	branch := body.ProductionBranch
	if branch == "" {
		branch = defaultProductionBranch
	}
	return kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
		ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: body.Connection},
		Repo:             body.Repo,
		ProductionBranch: branch,
	}}, true
}

// projectOrigin names where a project's software comes from, for the audit
// record and the log line: the repository, or the image reference.
func projectOrigin(source kitchenv1alpha1.ProjectSourceSpec) string {
	if source.HasRepository() {
		return source.GitSource().Repo
	}
	return source.ImageSource().Reference()
}

// previewsRefusal and previewsRefusalFor are the sentence a project with no
// repository gets when somebody asks it for previews. It is a refusal rather
// than a silent no, because a preview that never appears reads as a fault —
// which is the whole reason this is worded rather than merely enforced.
func previewsRefusal(project *kitchenv1alpha1.Project) string {
	return previewsRefusalFor(project.Name, project.Spec.Source)
}

func previewsRefusalFor(name string, source kitchenv1alpha1.ProjectSourceSpec) string {
	return fmt.Sprintf(
		"previews are environments for pull requests, and %q has no repository to open one against: "+
			"its source is the image %s. Nothing about it changes until that image does, so there is "+
			"nothing for a preview to show.",
		name, source.ImageSource().Reference())
}

// repositorySettingRefusal is the same shape of answer for the other settings
// that belong to a repository: the production branch and the review
// requirement made against it, and the four that say how a commit becomes an
// image.
//
// They are refused rather than ignored, which is the rule the whole of
// [patchProjectRequest] is written to: a build strategy stored on a project
// that builds nothing is a setting that reads back and does nothing, and the
// person who set it goes looking for what it changed. `settings` names the
// ones the request carried, as the sentence says them.
func repositorySettingRefusal(project *kitchenv1alpha1.Project, settings string) string {
	return fmt.Sprintf(
		"%s, and %q has no repository: its source is the image %s",
		settings, project.Name, project.Spec.Source.ImageSource().Reference())
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
	body.RootDirectory = normalizeRootDirectory(body.RootDirectory)
	body.DockerfilePath = normalizeDockerfilePath(body.DockerfilePath)
	body.DockerfileTarget = normalizeDockerfileTarget(body.DockerfileTarget)

	if err := validateProjectName(body.Name); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if err := checkBuildPath("rootDirectory", body.RootDirectory, withinRepository); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if err := checkBuildPath("dockerfilePath", body.DockerfilePath, withinBuildRoot); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if err := checkDockerfileTarget(body.DockerfileTarget); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	// Which kind of project this is, settled before anything else is
	// checked: the two ask for different fields, and every message below
	// depends on knowing which was meant.
	source, ok := s.projectSource(ctx, w, body)
	if !ok {
		return
	}
	// Checked before anything is recorded, so a name somebody already took
	// does not leave a record of a project that was never created. The Create
	// below still has to answer the same way, because two people can want
	// `shop` in the same second and only the API server can settle that.
	if !s.projectNameIsFree(ctx, w, body.Name) {
		return
	}

	branch := source.GitSource().ProductionBranch
	// A project with no repository has no pull requests to preview, so
	// previews are off and asking for them is refused rather than ignored.
	previews := source.HasRepository()
	if body.Previews != nil {
		if *body.Previews && !source.HasRepository() {
			badRequest(w, "%s", previewsRefusalFor(body.Name, source))
			return
		}
		previews = *body.Previews
	}
	var registry *kitchenv1alpha1.RegistrySpec
	if source.HasRepository() {
		registry = &kitchenv1alpha1.RegistrySpec{
			ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: body.Registry},
		}
	}

	caller, _ := CallerFrom(ctx)
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:        body.Name,
			Namespace:   s.Namespace,
			Annotations: map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source:   source,
			Registry: registry,
			Previews: kitchenv1alpha1.PreviewsSpec{Enabled: ptr.To(previews)},
			Build: kitchenv1alpha1.ProjectBuildSpec{
				RootDirectory:    body.RootDirectory,
				DockerfilePath:   body.DockerfilePath,
				DockerfileTarget: body.DockerfileTarget,
			},
			// Creating a project is self-service, and the account that creates
			// one is its admin (docs/AUTH.md, "Who may do what"). The grant is
			// written here rather than implied, because implying it would mean
			// a second rule about who is an admin living outside spec.access —
			// and because a project whose creator is not written down is one
			// nobody can hand over.
			Access: creatorGrant(caller),
		},
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    project,
		Kind:      audit.KindProject,
		Operation: clickhouse.AuditCreate,
		To:        project.Name,
		Project:   project.Name,
		Reason:    fmt.Sprintf("project %s created from %s", project.Name, projectOrigin(source)),
		Details: map[string]any{
			"source":           projectOrigin(source),
			"repo":             body.Repo,
			"productionBranch": branch,
			"sourceConnection": body.Connection,
			"registry":         body.Registry,
		},
	}) {
		return
	}
	if err := s.Client.Create(ctx, project); err != nil {
		if apierrors.IsAlreadyExists(err) {
			nameTaken(w, project.Name)
			return
		}
		s.writeError(w, err)
		return
	}
	s.log().Info("project created through the api",
		"project", project.Name, "source", projectOrigin(source), "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventProjectCreated,
		Project: project.Name,
		Message: fmt.Sprintf("project %s created from %s", project.Name, projectOrigin(source)),
		Actor:   callerName(caller),
	})
	writeJSON(w, http.StatusCreated, newProjectView(project, s.roleOn(ctx, project)))
}

// projectNameIsFree reports whether a project of that name can still be
// created, answering the request itself when it cannot.
func (s *Server) projectNameIsFree(ctx context.Context, w http.ResponseWriter, name string) bool {
	existing := &kitchenv1alpha1.Project{}
	switch err := s.Client.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, existing); {
	case apierrors.IsNotFound(err):
		return true
	case err != nil:
		s.writeError(w, err)
		return false
	default:
		nameTaken(w, name)
		return false
	}
}

// nameTaken says the name has gone, and why there is nothing to appeal to.
//
// The raw AlreadyExists underneath it names a Kubernetes resource in a
// Kubernetes namespace, which is exactly the vocabulary the platform exists to
// keep out of a developer's day. What is worth saying instead is the rule:
// project names are one flat namespace under the base domain — every URL the
// platform generates is a subdomain of it — so there is no scope to qualify
// the name with, and the second person to want `shop` needs a different one.
func nameTaken(w http.ResponseWriter, name string) {
	writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
		"the project name %q is taken: names are one flat namespace under the platform's base domain, "+
			"since every URL the platform generates is a subdomain of it, so they are "+
			"first-come-first-served — choose another name", name)})
}

// creatorGrant is the access list a new project starts with: its creator, as
// admin, named by the issuer's `sub`.
//
// The address is carried beside it so the list reads in `kubectl get -o yaml`
// and in a git diff; nothing resolves against it (see AccessSubject). A caller
// with no subject cannot happen — a token that names none is refused — so the
// empty case is a belt-and-braces guard against writing an entry that would
// match nobody.
func creatorGrant(caller Caller) []kitchenv1alpha1.AccessGrant {
	if caller.Subject == "" {
		return nil
	}
	return []kitchenv1alpha1.AccessGrant{{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: caller.Subject, Email: caller.Email},
		Role:          kitchenv1alpha1.AccessRoleAdmin,
	}}
}

// patchProjectRequest carries the parts of a project a user can change after
// creating it. Every field is optional; absent ones keep their value. The
// repository and the two connections are deliberately not here: rebinding a
// project to another repository or registry is a different project.
type patchProjectRequest struct {
	ProductionBranch *string `json:"productionBranch,omitempty"`
	// RequirePullRequest refuses to build a production-branch commit the git
	// provider cannot say arrived through a reviewed pull request. It is an
	// admin's setting because it is a decision about how the project is run,
	// not about a deploy.
	RequirePullRequest *bool `json:"requirePullRequest,omitempty"`
	Previews           *bool `json:"previews,omitempty"`
	PreviewsProtected  *bool `json:"previewsProtected,omitempty"`
	// PreviewsMax is this project's own ceiling on live preview
	// environments, overriding the platform's `previewsMaxPerProject`
	// (#294). `0` is no ceiling for this project, and a **negative** number
	// clears the override so the project takes the platform's again — the
	// same shape as an empty string clearing a text setting, since 0 is a
	// setting here and cannot also mean "unset".
	PreviewsMax    *int32  `json:"previewsMax,omitempty"`
	BuildStrategy  *string `json:"buildStrategy,omitempty"`
	DockerfilePath *string `json:"dockerfilePath,omitempty"`
	// DockerfileTarget is the stage of a multi-stage Dockerfile to ship; an
	// empty string clears it, which is the file's last stage again.
	DockerfileTarget *string `json:"dockerfileTarget,omitempty"`
	RootDirectory    *string `json:"rootDirectory,omitempty"`
	// Env is on this request only so that it can be refused by name. This
	// route is the project's own settings and is the admin's; environment
	// variables are the day job and are the developer's, on
	// PATCH /projects/{name}/env. Decoding refuses a field it has never heard
	// of, so leaving Env off the struct would answer with an unknown-field
	// error — true, and no help at all to a client that used to send it here.
	// Dropping it silently would be worse: a lost write that read as a
	// successful one.
	Env      *[]envVarRequest `json:"env,omitempty"`
	Port     *int32           `json:"port,omitempty"`
	Replicas *int32           `json:"replicas,omitempty"`
	CPU      *string          `json:"cpu,omitempty"`
	Memory   *string          `json:"memory,omitempty"`
	// Health is what the platform asks the application before it sends
	// anyone to it, and how often. Sending it replaces the whole check;
	// sending `{}` restores the default one, which is a TCP connect to the
	// container's port — every environment is probed either way.
	Health *healthRequest `json:"health,omitempty"`
	// Security is the posture every workload of this project runs under —
	// the web process, its workers and its scheduled runs, since they are
	// one image. Sending it replaces the whole posture; sending `{}` takes
	// it back off, restoring the platform's default, which every workload
	// runs under either way.
	Security *securityRequest `json:"security,omitempty"`
	// Command replaces the image's entrypoint and Args its arguments, in
	// exec form: a list of words, never a shell line. PreviewArgs replaces
	// Args in preview environments, the way an environment variable's
	// previewValue replaces its value — same commit, same artifact,
	// different flags.
	//
	// Each replaces its whole list and `[]` clears it — which for
	// PreviewArgs is how an override is taken away, since an empty override
	// is no override just as an empty previewValue is. Leaving a field out
	// keeps whatever it had.
	Command     *[]string `json:"command,omitempty"`
	Args        *[]string `json:"args,omitempty"`
	PreviewArgs *[]string `json:"previewArgs,omitempty"`
	// Singleton declares that two of this workload must never run at once,
	// which deploys it by stopping the old copy before starting the new one.
	// It refuses `replicas` above 1 rather than clamping it: a value quietly
	// lowered reads back as a setting that did not take.
	Singleton *bool `json:"singleton,omitempty"`
	// NotRequestDriven declares that this workload does work nobody asked
	// for, which turns idling off for every one of the project's
	// environments — previews included, which is where it matters, since
	// previews idle by default. The environment says so in its ScaleToZero
	// condition rather than merely not idling.
	NotRequestDriven *bool `json:"notRequestDriven,omitempty"`
	// PromotionStages replaces the project's staged pipeline wholesale, in
	// promotion order; an empty list removes it, restoring the default
	// build-straight-to-production flow. The stages are topology — what each
	// environment demands stays on the Environment, owned by its owners —
	// which is why arranging them is the project admin's.
	PromotionStages *[]promotionStageRequest `json:"promotionStages,omitempty"`
	// DataClass classifies the data this project handles: public, internal,
	// confidential or strictlyConfidential; an empty string removes the
	// classification. The change is always allowed — including one that
	// leaves environments rated below the new class, which the promotion
	// rule and the compliance inventory surface as non-compliance rather
	// than the API refusing the correction — and it is audit-logged with
	// the previous value, as a privileged record.
	DataClass *string `json:"dataClass,omitempty"`
	// Criticality designates how much it matters that this project's function
	// keeps working: nonCritical, important or critical; an empty string
	// removes the designation. RTO and RPO are its disruption tolerances,
	// as Go durations of whole hours and minutes ("4h", "30m"); empty
	// removes them.
	//
	// All three are the institution's inputs, not the platform's judgement —
	// Kitchen refuses nothing on them and gates no deployment behind them.
	// What they change is the mapping (GET /compliance/criticality), what the
	// policy engine can be asked, and how loudly this project's production
	// environments alert. Each change is audit-logged with the previous
	// value, as a privileged record, for the same reason a data class is.
	Criticality *string `json:"criticality,omitempty"`
	RTO         *string `json:"rto,omitempty"`
	RPO         *string `json:"rpo,omitempty"`
	// Processes replaces the project's other workloads wholesale — its
	// workers, its scheduled jobs and the services the rest of the unit talks
	// to; an empty list removes them all. It is here, on the settings route,
	// rather than on a route of its own because what a project ships and how
	// much of it it runs is one decision — the same admin who sets the
	// replica count and the resources of the web process sets the workers'.
	//
	// It is also why a repository that ships four images is one project with
	// four entries here rather than four projects, or a tier above the
	// project: the deployable unit is the project, and a tier above it would
	// double every route in this table (#271).
	//
	// The list is the project's declaration, so it reaches an environment only
	// through the next Release: an existing environment keeps running the
	// workloads its release declared until something builds. That is the same
	// rule the port and the replica count follow, and it is what makes a
	// rollback exact.
	Processes *[]processRequest `json:"processes,omitempty"`
	// Files replaces the project's configuration files wholesale, in the
	// same read-modify-write bargain the variables make: a file whose
	// `content` the request leaves out keeps the content it has, which is
	// what lets a client that was never shown a secret file's content send
	// the rest of the list back.
	//
	// An empty list removes every file. A file marked `secret` carries no
	// content here at all — that is
	// PUT /projects/{name}/files/{file}, which no response reads back — and
	// sending one with content is refused rather than stored in the clear.
	//
	// Like the process list it is the project's declaration, so it reaches an
	// environment only through the next Release. A file is frozen into the
	// snapshot for exactly that reason: a rollback restores the file its
	// release ran with.
	Files *[]fileRequest `json:"files,omitempty"`
}

// fileRequest and the validation behind it live in internal/appconfig, for
// the reason processRequest does: a repository's kitchen.json is a second way
// to declare the same file, and one shape is the only way two validators
// agree.
type fileRequest = appconfig.File

// filesFromRequest turns the request's files into the spec's. `stored` is
// what the project holds — consulted only for the content of a file whose
// request left it out — and `processes` the workloads a file may name.
func filesFromRequest(
	requests []fileRequest,
	stored []kitchenv1alpha1.ConfigFile,
	processes []kitchenv1alpha1.ProcessSpec,
) ([]kitchenv1alpha1.ConfigFile, error) {
	names := make([]string, 0, len(processes))
	for _, process := range processes {
		names = append(names, process.Name)
	}
	return appconfig.Files(requests, stored, names)
}

// criticalityFromRequest validates one criticality value, empty meaning
// undesignated. The refusal names the vocabulary, in order.
func criticalityFromRequest(value string) (kitchenv1alpha1.Criticality, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	designation := kitchenv1alpha1.Criticality(value)
	if !designation.Designated() {
		names := make([]string, 0, len(kitchenv1alpha1.Criticalities()))
		for _, c := range kitchenv1alpha1.Criticalities() {
			names = append(names, string(c))
		}
		return "", fmt.Errorf(
			"criticality must be one of %s, in ascending order, or empty to remove the designation (got %q)",
			strings.Join(names, ", "), value)
	}
	return designation, nil
}

// toleranceFromRequest validates one rto or rpo, empty meaning undeclared.
// The refusal spells the format out rather than echoing the CRD's pattern,
// because a caller who wrote "4 hours" needs to be told what to write instead.
func toleranceFromRequest(field, value string) (kitchenv1alpha1.Tolerance, error) {
	tolerance := kitchenv1alpha1.Tolerance(strings.TrimSpace(value))
	if !tolerance.Declared() {
		return "", nil
	}
	if !tolerance.Valid() {
		return "", fmt.Errorf(
			"%s must be a duration of whole hours and minutes — \"4h\", \"30m\", \"1h30m\", "+
				"\"0m\" for none at all — or empty to remove it (got %q)", field, value)
	}
	return tolerance, nil
}

// dataClassFromRequest validates one dataClass value, empty meaning
// unclassify. The refusal names the vocabulary, in order.
func dataClassFromRequest(value string) (kitchenv1alpha1.DataClass, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	class := kitchenv1alpha1.DataClass(value)
	if !class.Classified() {
		classes := kitchenv1alpha1.DataClasses()
		names := make([]string, 0, len(classes))
		for _, c := range classes {
			names = append(names, string(c))
		}
		return "", fmt.Errorf("dataClass must be one of %s, in ascending sensitivity, or empty to unclassify (got %q)",
			strings.Join(names, ", "), value)
	}
	return class, nil
}

// promotionStageRequest is one rung of the pipeline as a PATCH names it.
type promotionStageRequest struct {
	Name        string `json:"name"`
	Environment string `json:"environment"`
	AutoPromote bool   `json:"autoPromote,omitempty"`
}

// promotionStagesFromRequest validates and converts a replacement pipeline.
// An environment a stage names either does not exist yet — the first build
// for the stage creates it — or must belong to this project: a stage naming a
// stranger's environment would point the project's builds at it.
func (s *Server) promotionStagesFromRequest(
	ctx context.Context, project *kitchenv1alpha1.Project, stages []promotionStageRequest,
) (*kitchenv1alpha1.PromotionPolicySpec, error) {
	if len(stages) == 0 {
		return nil, nil
	}
	spec := &kitchenv1alpha1.PromotionPolicySpec{}
	names, environments := map[string]bool{}, map[string]bool{}
	for _, stage := range stages {
		name := strings.TrimSpace(stage.Name)
		environment := strings.TrimSpace(stage.Environment)
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			return nil, fmt.Errorf("stage name %q is not a DNS label: lower-case letters, digits and dashes", stage.Name)
		}
		if environment == "" {
			return nil, fmt.Errorf("stage %q names no environment", name)
		}
		if names[name] {
			return nil, fmt.Errorf("stage %q appears twice", name)
		}
		if environments[environment] {
			return nil, fmt.Errorf("environment %q appears in two stages", environment)
		}
		names[name], environments[environment] = true, true

		env := &kitchenv1alpha1.Environment{}
		if err := s.get(ctx, environment, env); err == nil {
			if env.Spec.ProjectRef.Name != project.Name {
				return nil, fmt.Errorf("environment %q belongs to project %q, not %q",
					environment, env.Spec.ProjectRef.Name, project.Name)
			}
		} else if !apierrors.IsNotFound(err) {
			return nil, err
		}
		spec.Stages = append(spec.Stages, kitchenv1alpha1.PromotionStage{
			Name:        name,
			Environment: environment,
			AutoPromote: stage.AutoPromote,
		})
	}
	return spec, nil
}

// patchProjectEnvRequest is the whole of the environment-variable write: the
// list, which replaces the project's.
//
// It is a pointer so that a body carrying no `env` at all is refused rather
// than read as "replace the list with nothing". Clearing every variable is a
// thing somebody may well mean, and `{"env": []}` is how they say it — but an
// empty body is a client that forgot the field, and answering that by deleting
// the project's configuration is not a reading of it anybody wants.
type patchProjectEnvRequest struct {
	Env *[]envVarRequest `json:"env"`
}

// envVarRequest is one variable on its way in. It no longer mirrors
// envVarView, because a value only travels one way: a client cannot write back
// a value it was never shown. An absent value therefore keeps the stored one
// and an empty one clears it — the same bargain a connection's credential
// fields make. At most one source may be set.
type envVarRequest struct {
	Name         string      `json:"name"`
	Value        *string     `json:"value,omitempty"`
	PreviewValue *string     `json:"previewValue,omitempty"`
	FromSecret   *keyRefView `json:"fromSecret,omitempty"`
	FromClaim    *keyRefView `json:"fromClaim,omitempty"`
}

// envVarsFromRequest turns the request's variables into the spec's, refusing
// ambiguity: a variable naming two sources would silently prefer one of them.
//
// The list replaces the project's, but a variable whose value the request left
// out keeps the value the variable of that name already had. That is what
// makes "read the project, edit, write back" survive the API not reading
// values back: the client has nothing to send for the variables it is not
// changing. A variable being repointed at a secret or a claim keeps nothing —
// the reference is what replaces the value.
func envVarsFromRequest(vars []envVarRequest, existing []kitchenv1alpha1.EnvVar) ([]kitchenv1alpha1.EnvVar, error) {
	stored := make(map[string]kitchenv1alpha1.EnvVar, len(existing))
	for _, v := range existing {
		stored[v.Name] = v
	}
	out := make([]kitchenv1alpha1.EnvVar, 0, len(vars))
	seen := map[string]bool{}
	for _, v := range vars {
		name := strings.TrimSpace(v.Name)
		if name == "" {
			return nil, errors.New("every env var needs a name")
		}
		if seen[name] {
			return nil, fmt.Errorf("env var %q appears twice", name)
		}
		seen[name] = true
		value, previewValue := v.Value, v.PreviewValue
		if v.FromSecret == nil && v.FromClaim == nil {
			if prior, ok := stored[name]; ok {
				if value == nil {
					value = &prior.Value
				}
				if previewValue == nil {
					previewValue = &prior.PreviewValue
				}
			}
		}
		sources := 0
		if value != nil && *value != "" {
			sources++
		}
		if v.FromSecret != nil {
			sources++
		}
		if v.FromClaim != nil {
			sources++
		}
		if sources > 1 {
			return nil, fmt.Errorf("env var %q names more than one source: use value, fromSecret or fromClaim, not several", name)
		}
		spec := kitchenv1alpha1.EnvVar{Name: name}
		if value != nil {
			spec.Value = *value
		}
		if previewValue != nil {
			spec.PreviewValue = *previewValue
		}
		if v.FromSecret != nil {
			if v.FromSecret.Name == "" || v.FromSecret.Key == "" {
				return nil, fmt.Errorf("env var %q: fromSecret needs both a name and a key", name)
			}
			spec.SecretRef = &kitchenv1alpha1.SecretKeySelector{Name: v.FromSecret.Name, Key: v.FromSecret.Key}
		}
		if v.FromClaim != nil {
			if v.FromClaim.Name == "" || v.FromClaim.Key == "" {
				return nil, fmt.Errorf("env var %q: fromClaim needs both a name and a key", name)
			}
			spec.FromResourceClaim = &kitchenv1alpha1.ResourceClaimKeySelector{Name: v.FromClaim.Name, Key: v.FromClaim.Key}
		}
		out = append(out, spec)
	}
	return out, nil
}

// applyResource sets one compute resource as both request and limit —
// applications get the guaranteed class, not a burstable surprise — or clears
// it for an empty value.
func applyResource(resources *corev1.ResourceRequirements, name corev1.ResourceName, value string) error {
	return appconfig.ApplyResource(resources, name, value)
}

// applyProjectBuildAndRuntime sets the build and runtime half of a settings
// PATCH: how the image is built, and what the Deployment asks for once it is.
// It is lifted out of patchProject because that handler is one long sequence
// of "was this field sent", and two features arriving at once pushed the
// sequence past what gocyclo will read. Nothing here touches the cluster or
// the caller — it edits the object in place and hands back the refusal for
// patchProject to write, so the handler keeps every response in one place.
// applyProjectPreviews writes the three preview settings — whether pull
// requests get an environment, whether it is gated behind platform login, and
// how many of them may be live at once. It answers false when it has already
// written a refusal.
//
// It is a function of its own rather than three blocks in patchProject
// because the three belong together and patchProject is already at the
// complexity the linter allows.
func applyProjectPreviews(
	w http.ResponseWriter,
	project *kitchenv1alpha1.Project,
	body patchProjectRequest,
) bool {
	if body.Previews != nil {
		if *body.Previews && !project.Spec.Source.HasRepository() {
			badRequest(w, "%s", previewsRefusal(project))
			return false
		}
		project.Spec.Previews.Enabled = body.Previews
	}
	if body.PreviewsProtected != nil {
		project.Spec.Previews.Protected = body.PreviewsProtected
	}
	if body.PreviewsMax != nil {
		// A negative number clears the project's own ceiling, so it takes the
		// platform's again: 0 is a setting here — no ceiling for this project
		// — and cannot also mean "unset".
		if *body.PreviewsMax < 0 {
			project.Spec.Previews.Max = nil
		} else {
			project.Spec.Previews.Max = body.PreviewsMax
		}
	}
	return true
}

func applyProjectBuildAndRuntime(project *kitchenv1alpha1.Project, body patchProjectRequest) error {
	if body.BuildStrategy != nil {
		strategy := kitchenv1alpha1.BuildStrategy(strings.TrimSpace(*body.BuildStrategy))
		switch strategy {
		case kitchenv1alpha1.BuildStrategyAuto, kitchenv1alpha1.BuildStrategyDockerfile, kitchenv1alpha1.BuildStrategyBuildpacks:
			project.Spec.Build.Strategy = strategy
		default:
			return fmt.Errorf("buildStrategy must be auto, dockerfile or buildpacks (got %q)", *body.BuildStrategy)
		}
	}
	if body.DockerfilePath != nil {
		dockerfile := normalizeDockerfilePath(*body.DockerfilePath)
		if err := checkBuildPath("dockerfilePath", dockerfile, withinBuildRoot); err != nil {
			return err
		}
		project.Spec.Build.DockerfilePath = dockerfile
	}
	if body.DockerfileTarget != nil {
		target := normalizeDockerfileTarget(*body.DockerfileTarget)
		if err := checkDockerfileTarget(target); err != nil {
			return err
		}
		project.Spec.Build.DockerfileTarget = target
	}
	if body.RootDirectory != nil {
		root := normalizeRootDirectory(*body.RootDirectory)
		if err := checkBuildPath("rootDirectory", root, withinRepository); err != nil {
			return err
		}
		project.Spec.Build.RootDirectory = root
	}
	if body.Port != nil {
		// Zero is not "no port": it is the project handing the question back
		// to the platform, which answers it from the framework each build
		// detects. Anything else is a port someone chose.
		if *body.Port < 0 || *body.Port > 65535 {
			return fmt.Errorf("port must be between 1 and 65535, or 0 to derive it from the detected framework (got %d)", *body.Port)
		}
		project.Spec.Runtime.Port = *body.Port
	}
	if body.Replicas != nil {
		if *body.Replicas < 1 {
			return fmt.Errorf("replicas must be at least 1 (got %d) — production never scales to zero", *body.Replicas)
		}
		project.Spec.Runtime.Replicas = body.Replicas
	}
	if body.Singleton != nil {
		project.Spec.Runtime.Singleton = *body.Singleton
	}
	if body.NotRequestDriven != nil {
		project.Spec.Runtime.NotRequestDriven = *body.NotRequestDriven
	}
	// Checked after both, because a PATCH may carry either one alone: the
	// combination is what is refused, and it is refused here so the caller
	// reads a sentence rather than the CRD's admission rule quoting CEL at
	// them. The same rule is on the CRD, since not everything writes here.
	if project.Spec.Runtime.Singleton && project.Spec.Runtime.Replicas != nil && *project.Spec.Runtime.Replicas > 1 {
		return fmt.Errorf(
			"this project declares its workload a singleton, so it cannot run %d replicas: "+
				"set replicas to 1, or turn singleton off",
			*project.Spec.Runtime.Replicas)
	}
	if body.CPU != nil {
		if err := applyResource(&project.Spec.Runtime.Resources, corev1.ResourceCPU, strings.TrimSpace(*body.CPU)); err != nil {
			return err
		}
	}
	if body.Memory != nil {
		if err := applyResource(&project.Spec.Runtime.Resources, corev1.ResourceMemory, strings.TrimSpace(*body.Memory)); err != nil {
			return err
		}
	}
	if body.Health != nil {
		health, err := healthFromRequest(*body.Health, "health", false)
		if err != nil {
			return err
		}
		project.Spec.Runtime.Health = health
	}
	if body.Security != nil {
		security, err := securityFromRequest(*body.Security, "security")
		if err != nil {
			return err
		}
		project.Spec.Runtime.Security = security
	}
	// Exec form throughout, so nothing here is split, quoted or handed to a
	// shell: the words arrive as words and reach the container as words.
	// An empty list is kept as an empty list rather than folded to nil,
	// because "started with no arguments" is a thing to be able to say.
	for _, list := range []struct {
		words *[]string
		into  *[]string
	}{
		{body.Command, &project.Spec.Runtime.Command},
		{body.Args, &project.Spec.Runtime.Args},
		{body.PreviewArgs, &project.Spec.Runtime.PreviewArgs},
	} {
		if list.words != nil {
			*list.into = *list.words
		}
	}
	return nil
}

// refusedWorkloadBuiltFromNoRepository answers the request when a workload
// declares a build and the project has no repository to build it from, and
// reports whether it did.
//
// It is the one cross-field rule a workload's own spec cannot state — `image`
// and `build` exclude each other there, and neither knows what the project's
// source is — so the CRD carries it as a CEL rule and this carries it as a
// sentence, the way the singleton pair does: a caller who asked for the wrong
// one of two answers is told which one is available here rather than handed
// CEL.
func refusedWorkloadBuiltFromNoRepository(
	w http.ResponseWriter, project *kitchenv1alpha1.Project, processes []kitchenv1alpha1.ProcessSpec,
) bool {
	if project.Spec.Source.HasRepository() {
		return false
	}
	for _, workload := range processes {
		if workload.Build == nil {
			continue
		}
		badRequest(w, "workload %q is built from the repository, and %q has no repository: "+
			"its source is the image %s, so every workload of it runs an image somebody else "+
			"built — give %q an `image` instead of a `build`",
			workload.Name, project.Name, project.Spec.Source.ImageSource().Reference(), workload.Name)
		return true
	}
	return false
}

// refusedRepositorySettings answers the request itself when it carries a
// setting only a repository has and the project has none, and reports whether
// it did.
//
// They are refused rather than ignored: a production branch that read back as
// the empty string on a project with no branch, or a build strategy stored on
// a project that builds nothing, is a setting somebody would go looking for
// the effect of.
func refusedRepositorySettings(
	w http.ResponseWriter, project *kitchenv1alpha1.Project, body patchProjectRequest,
) bool {
	if project.Spec.Source.HasRepository() {
		return false
	}
	if body.ProductionBranch != nil || body.RequirePullRequest != nil {
		badRequest(w, "%s", repositorySettingRefusal(project,
			"a production branch and a pull request requirement are a repository's settings"))
		return true
	}
	// The four that say how a commit becomes an image. A project whose source
	// is an image builds nothing, so there is no commit for a strategy to be
	// chosen for and no directory of anything for a build root to name — the
	// workloads of such a project each name the image they run, which is
	// `processes`.
	if body.BuildStrategy != nil || body.DockerfilePath != nil ||
		body.DockerfileTarget != nil || body.RootDirectory != nil {
		badRequest(w, "%s", repositorySettingRefusal(project,
			"a build strategy, a Dockerfile, a Dockerfile stage and a root directory are a "+
				"repository's settings, and this project builds nothing"))
		return true
	}
	return false
}

func (s *Server) patchProject(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := patchProjectRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.Env != nil {
		badRequest(w, "environment variables are not changed here any more: send them to "+
			"PATCH /projects/%s/env, which needs developer rather than admin", project.Name)
		return
	}

	patch := client.MergeFrom(project.DeepCopy())
	if refusedRepositorySettings(w, project, body) {
		return
	}
	if body.ProductionBranch != nil {
		branch := strings.TrimSpace(*body.ProductionBranch)
		if branch == "" {
			badRequest(w, "productionBranch cannot be empty: every project has a branch whose builds go to production")
			return
		}
		project.Spec.Source.Git.ProductionBranch = branch
	}
	if body.RequirePullRequest != nil {
		project.Spec.Source.Git.RequirePullRequest = *body.RequirePullRequest
	}
	if !applyProjectPreviews(w, project, body) {
		return
	}
	if err := applyProjectBuildAndRuntime(project, body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.PromotionStages != nil {
		stages, err := s.promotionStagesFromRequest(ctx, project, *body.PromotionStages)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		project.Spec.Promotion = stages
	}
	if body.Processes != nil {
		processes, err := processesFromRequest(*body.Processes)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		if refusedWorkloadBuiltFromNoRepository(w, project, processes) {
			return
		}
		project.Spec.Processes = processes
	}
	// After the workloads, so that one request may add a worker and a file
	// for it: the names a file may mention are the ones this project will
	// have when the write lands, not the ones it had when it arrived.
	if body.Files != nil {
		files, err := filesFromRequest(*body.Files, project.Spec.Files, project.Spec.Processes)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		project.Spec.Files = files
	}
	var nextClass *kitchenv1alpha1.DataClass
	if body.DataClass != nil {
		class, err := dataClassFromRequest(*body.DataClass)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		nextClass = &class
	}
	continuity, err := continuityFromRequest(body.Criticality, body.RTO, body.RPO)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	details := projectSettingsDetails(project, body, nextClass, continuity)
	if nextClass != nil {
		project.Spec.DataClass = *nextClass
	}
	continuity.apply(&project.Spec.Criticality, &project.Spec.RTO, &project.Spec.RPO)

	// A settings PATCH is ordinary — a branch, a preview policy — except when
	// it moves the data class, which is an input to policy: the record is
	// classified so the two are separable in the log.
	privilege := audit.Privilege("")
	if nextClass != nil {
		privilege = audit.PrivilegeClassification
	}
	if !s.recorded(w, req, audit.Transition{
		Object:     project,
		Kind:       audit.KindProject,
		Operation:  clickhouse.AuditUpdate,
		Privileged: privilege,
		Project:    project.Name,
		Reason:     fmt.Sprintf("project %s settings changed", project.Name),
		Details:    details,
	}) {
		return
	}
	if err := s.Client.Patch(ctx, project, patch); err != nil {
		s.writeError(w, err)
		return
	}

	// The content the platform holds for a secret file the project no longer
	// declares goes with the declaration. It is done after the write rather
	// than before it, so that a settings PATCH that fails leaves the
	// credential where it was.
	if body.Files != nil {
		if err := s.pruneProjectFiles(ctx, project); err != nil {
			s.writeError(w, err)
			return
		}
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("project settings changed through the api",
		"project", project.Name, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, s.withFileContent(ctx, project, newProjectView(project, s.roleOn(ctx, project))))
}

// patchProjectEnv is the developer's half of a project: its environment
// variables, which docs/AUTH.md puts in the day job next to builds, redeploys
// and rollbacks, while the project's own settings stay the admin's.
//
// It is a second route rather than a role check inside patchProject because a
// whole route is the unit of authorization on this platform. The merge
// semantics are patchProject's own, unchanged — envVarsFromRequest is the one
// implementation of them — so a client that used to send `env` alongside the
// settings sends the same list to a different path.
func (s *Server) patchProjectEnv(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := patchProjectEnvRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.Env == nil {
		badRequest(w, "env is required: it is the whole list, and it replaces the project's. "+
			"Send [] to clear every variable")
		return
	}

	env, err := envVarsFromRequest(*body.Env, project.Spec.Env)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	patch := client.MergeFrom(project.DeepCopy())
	project.Spec.Env = env

	// Recorded the way every other project write is, and for the same reason
	// changedProjectFields records no values: the variables are exactly where
	// somebody pastes an API key, and a log that copied them would be a second
	// place secrets live.
	if !s.recorded(w, req, audit.Transition{
		Object:    project,
		Kind:      audit.KindProject,
		Operation: clickhouse.AuditUpdate,
		Project:   project.Name,
		Reason:    fmt.Sprintf("project %s environment variables changed", project.Name),
		Details:   map[string]any{"fields": []string{"env"}},
	}) {
		return
	}
	if err := s.Client.Patch(ctx, project, patch); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("project environment variables changed through the api",
		"project", project.Name, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newProjectView(project, s.roleOn(ctx, project)))
}

func (s *Server) deleteProject(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    project,
		Kind:      audit.KindProject,
		Operation: clickhouse.AuditDelete,
		From:      project.Name,
		Project:   project.Name,
		Reason: fmt.Sprintf(
			"project %s deleted, with its environments, builds, releases, domains and claims", project.Name),
		Details: map[string]any{"repo": project.Spec.Source.GitSource().Repo},
	}) {
		return
	}
	if err := s.Client.Delete(ctx, project); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("project deleted through the api",
		"project", project.Name, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventProjectDeleted,
		Project: project.Name,
		Message: fmt.Sprintf("project %s deleted", project.Name),
		Actor:   callerName(caller),
	})
	// 202, not 200: the operator's finalizer still has environments to tear
	// down and a namespace to remove when this response goes out.
	writeJSON(w, http.StatusAccepted, newProjectView(project, s.roleOn(ctx, project)))
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

// visibleTo keeps the items belonging to a project this caller can see. It is
// what the table's "filtered to the caller's projects" rows do to their
// answers, and it is deliberately one function: a collection that filtered by
// hand would be the one that forgot to.
//
// A `?project=` naming a project the caller cannot see needs nothing extra —
// it filters everything out, which is the same answer as a project that does
// not exist.
func visibleTo[T any](scope projectScope, items []T, project func(*T) string) []T {
	if scope.all {
		return items
	}
	out := make([]T, 0, len(items))
	for i := range items {
		if scope.allows(project(&items[i])) {
			out = append(out, items[i])
		}
	}
	return out
}

func buildProject(build *kitchenv1alpha1.Build) string { return build.Spec.ProjectRef.Name }

func releaseProject(release *kitchenv1alpha1.Release) string { return release.Spec.ProjectRef.Name }

func environmentProject(env *kitchenv1alpha1.Environment) string { return env.Spec.ProjectRef.Name }

func (s *Server) listBuilds(w http.ResponseWriter, req *http.Request) {
	builds, err := s.builds(req.Context(), projectFilter(req))
	if err != nil {
		s.writeError(w, err)
		return
	}
	s.writeBuilds(w, visibleTo(scopeFrom(req.Context()), builds, buildProject))
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
	if !s.recorded(w, req, audit.Transition{
		Object:      build,
		Kind:        audit.KindBuild,
		Operation:   clickhouse.AuditCreate,
		Correlation: revision.SHA,
		To:          string(kitchenv1alpha1.BuildQueued),
		Project:     project.Name,
		Reason:      fmt.Sprintf("a build of %s was requested", revision.SHA),
		Details:     map[string]any{"commit": revision.SHA, "branch": revision.Branch},
	}) {
		return
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
	// Whatever the caller pasted in — a subject, or a whole `git log` entry —
	// is split the same way a push is.
	subject, commitBody := kitchenv1alpha1.SplitCommitMessage(body.Message)

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
		if subject != "" {
			revision.Message, revision.Body = subject, commitBody
		}
		return *revision, nil
	}

	if len(body.SHA) < 7 {
		return kitchenv1alpha1.GitRevision{}, fmt.Errorf("sha %q is too short: give at least the seven-character short form", body.SHA)
	}

	revision := kitchenv1alpha1.GitRevision{SHA: body.SHA, Branch: body.Branch, Message: subject, Body: commitBody}
	// A commit that has been built before carries its own branch and
	// authorship; a caller naming a fresh sha has to say which branch it is
	// on, unless the production branch is a fair assumption.
	for i := range previous {
		if previous[i].Spec.Git.SHA == body.SHA {
			if revision.Branch == "" {
				revision.Branch = previous[i].Spec.Git.Branch
			}
			if revision.Message == "" {
				// The pair, or a subject from one build with a body from
				// another: two commits' messages spliced into one revision.
				revision.Message, revision.Body = previous[i].Spec.Git.Message, previous[i].Spec.Git.Body
			}
			revision.Author = previous[i].Spec.Git.Author
			// From the spec, not Build.PullRequestNumber(): this writes a
			// new immutable spec, and what a rebuild inherits is what the
			// event that created the original established — see the note on
			// requiresPullRequest.
			revision.PullRequest = previous[i].Spec.Git.PullRequest
			break
		}
	}
	if revision.Branch == "" {
		revision.Branch = project.Spec.Source.GitSource().ProductionBranch
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
	s.writeReleases(w, visibleTo(scopeFrom(req.Context()), releases, releaseProject))
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
	ctx := req.Context()
	release := &kitchenv1alpha1.Release{}
	if err := s.get(ctx, req.PathValue("name"), release); err != nil {
		s.writeError(w, err)
		return
	}
	view := newReleaseView(release)
	// The unit's compliance answer, which is per artifact: a release is
	// attested when every image it deploys is, and names the ones that are
	// not otherwise. It is read off the build because the build is what holds
	// the evidence index — a pruned build answers with a caveat rather than
	// with "not attested", which would be a different and untrue statement.
	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, release.Spec.BuildRef.Name, build); err != nil {
		if !apierrors.IsNotFound(err) {
			s.writeError(w, err)
			return
		}
		view.Attestation = newUnitAttestationView(nil)
	} else {
		view.Attestation = newUnitAttestationView(build)
	}
	writeJSON(w, http.StatusOK, view)
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
	s.writeEnvironments(w, visibleTo(scopeFrom(req.Context()), environments, environmentProject))
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
//
// An environment that declares requirements is not moved from here at all:
// the request becomes a Promotion — answered 202 with the promotion, phase
// Pending — and the promotion reconciler evaluates the bar, records the
// decision and applies or refuses it. One door into a gated environment,
// whichever route knocks.
type patchEnvironmentRequest struct {
	Release string `json:"release"`
	// Reason is carried onto the Promotion when the environment's
	// requirements route this move through one. Optional; ignored on the
	// direct path, where the audit record already says who and what.
	Reason string `json:"reason,omitempty"`
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

	// An environment with requirements takes releases only through a
	// Promotion: the policy engine decides, the decision is stored, and the
	// promotion reconciler makes the move. The 202 says exactly that — the
	// move is accepted for evaluation, not made.
	if env.Spec.Requirements != nil {
		caller, _ := CallerFrom(ctx)
		promotion := s.manualPromotion(caller, env.Spec.ProjectRef.Name, env, release,
			strings.TrimSpace(body.Reason))
		if !s.recorded(w, req, promotionTransition(promotion)) {
			return
		}
		if err := s.Client.Create(ctx, promotion); err != nil {
			s.writeError(w, err)
			return
		}
		s.log().Info("environment move routed through a promotion",
			"environment", env.Name, "release", release.Name,
			"promotion", promotion.Name, "caller", callerName(caller))
		writeJSON(w, http.StatusAccepted, newPromotionView(promotion))
		return
	}

	// The hard check behind issue #137 guards the direct path the same way it
	// guards the build controller's: a classified project's release does not
	// land — by promotion or by rollback — on an environment rated below it.
	// A gated environment took the promotion branch above, where the pinned
	// bundle's dataclass-le-environment rule makes the same comparison.
	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, env.Spec.ProjectRef.Name, project); err == nil {
		if refusal := controller.DataClassRefusal(project, env); refusal != "" {
			badRequest(w, "%s", refusal)
			return
		}
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

	if !s.recorded(w, req, audit.Transition{
		Object:  env,
		Kind:    audit.KindEnvironment,
		From:    outgoing,
		To:      release.Name,
		Project: env.Spec.ProjectRef.Name,
		Reason:  fmt.Sprintf("environment %s was %s release %s", env.Name, verb, release.Name),
		Details: map[string]any{
			"release":         release.Name,
			"previousRelease": outgoing,
			"image":           release.Spec.Image,
			"move":            string(reason),
		},
	}) {
		return
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

// cancelBuild stops a queued or running build: the BuildKit job is deleted and
// the Build itself is kept, phase Cancelled — the history of who asked for
// what is the point of Build objects, so cancellation never removes one.
func (s *Server) cancelBuild(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, req.PathValue("name"), build); err != nil {
		s.writeError(w, err)
		return
	}
	switch build.Status.Phase {
	case kitchenv1alpha1.BuildSucceeded, kitchenv1alpha1.BuildFailed, kitchenv1alpha1.BuildCancelled:
		writeJSON(w, http.StatusConflict, errorBody{
			Error: fmt.Sprintf("build %s already finished (%s): there is nothing to cancel", build.Name, build.Status.Phase),
		})
		return
	}

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:      build.Name,
		Namespace: controller.AppNamespace(build.Spec.ProjectRef.Name),
	}}
	// Background propagation takes the build pod with the job; a cancelled
	// build that keeps building would only be a lie.
	if !s.recorded(w, req, audit.Transition{
		Object:      build,
		Kind:        audit.KindBuild,
		Correlation: build.Spec.Git.SHA,
		From:        string(build.Status.Phase),
		To:          string(kitchenv1alpha1.BuildCancelled),
		Project:     build.Spec.ProjectRef.Name,
		Reason:      fmt.Sprintf("build %s was cancelled", build.Name),
		Details:     map[string]any{"commit": build.Spec.Git.SHA},
	}) {
		return
	}
	if err := s.Client.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil &&
		!apierrors.IsNotFound(err) {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	build.Status.Phase = kitchenv1alpha1.BuildCancelled
	build.Status.CompletedAt = ptr.To(metav1.Now())
	meta.SetStatusCondition(&build.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "BuildCancelled",
		Message:            fmt.Sprintf("cancelled by %s", callerName(caller)),
		ObservedGeneration: build.Generation,
	})
	if err := s.Client.Status().Update(ctx, build); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("build cancelled through the api",
		"project", build.Spec.ProjectRef.Name, "build", build.Name, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, newBuildView(build))
}

// deleteEnvironment removes a stuck preview. Only previews: the production
// environment is the project — it goes down when the project does, and a
// stray DELETE must not be able to take a live site with it.
func (s *Server) deleteEnvironment(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}
	if env.Spec.Type != kitchenv1alpha1.EnvironmentPreview {
		badRequest(w, "environment %q is the production environment: it is torn down with its project, not on its own", env.Name)
		return
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    env,
		Kind:      audit.KindEnvironment,
		Operation: clickhouse.AuditDelete,
		From:      env.Spec.ReleaseRef.Name,
		Project:   env.Spec.ProjectRef.Name,
		Reason:    fmt.Sprintf("preview environment %s was removed", env.Name),
		Details:   map[string]any{"type": string(env.Spec.Type)},
	}) {
		return
	}
	if err := s.Client.Delete(ctx, env); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("environment deleted through the api",
		"project", env.Spec.ProjectRef.Name, "environment", env.Name, "caller", callerName(caller))
	// 202: the environment's finalizer still has its workload to remove.
	writeJSON(w, http.StatusAccepted, newEnvironmentView(env))
}

// listConnections is the one connection route that is not the operator's
// alone, and the only one that answers two shapes.
//
// A project needs a `gitSource` and a `registry` connection to exist at all,
// so a member who cannot see that any connection exists cannot create a
// project — self-service would stop at the first form field and hand them back
// to an operator, which is the bottleneck the whole role model is trying to
// remove. So the route is filtered by role rather than refused: an operator
// gets the connections, and everybody else gets the picker's own shape
// (connectionChoiceView) — names, capabilities and readiness, and no way in
// from here to read, create, test, change or delete one. The only other route
// a member reaches is the repository listing next door, which is the same
// form's next field; everything else under /connections/ stays the
// operator's.
func (s *Server) listConnections(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	list := &kitchenv1alpha1.ConnectionList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })

	if !platformRoleFrom(ctx).AtLeast(access.PlatformOperator) {
		choices := make([]connectionChoiceView, 0, len(list.Items))
		for i := range list.Items {
			choices = append(choices, newConnectionChoiceView(&list.Items[i]))
		}
		writeList(w, choices)
		return
	}

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
	ctx := req.Context()
	list := &kitchenv1alpha1.DomainList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	// A Domain names an environment, not a project, so answering "whose is
	// this" takes the environments as well — once, rather than per domain.
	scope := scopeFrom(ctx)
	projectOfEnvironment := map[string]string{}
	if !scope.all {
		environments, err := s.environments(ctx, "")
		if err != nil {
			s.writeError(w, err)
			return
		}
		for i := range environments {
			projectOfEnvironment[environments[i].Name] = environments[i].Spec.ProjectRef.Name
		}
	}

	environment := strings.TrimSpace(req.URL.Query().Get("environment"))
	views := make([]domainView, 0, len(list.Items))
	for i := range list.Items {
		if environment != "" && list.Items[i].Spec.EnvironmentRef.Name != environment {
			continue
		}
		if !scope.allows(projectOfEnvironment[list.Items[i].Spec.EnvironmentRef.Name]) {
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
	scope := scopeFrom(req.Context())
	views := make([]claimView, 0, len(list.Items))
	for i := range list.Items {
		if project != "" && list.Items[i].Spec.ProjectRef.Name != project {
			continue
		}
		if !scope.allows(list.Items[i].Spec.ProjectRef.Name) {
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

// projectSettingsDetails is the audit detail of a settings PATCH, built apart
// from the recording so a test can hold it up to the light without a store.
// Field names, never values — with one deliberate exception: a dataClass
// change carries the previous class and the next, because the class decides
// what promotions the policy engine will refuse and the trail has to show
// what the bar was before. A classification is a label, not a secret. The
// record itself is classified `classification` at the call site, which is
// where the privilege vocabulary lives.
func projectSettingsDetails(
	project *kitchenv1alpha1.Project,
	body patchProjectRequest,
	nextClass *kitchenv1alpha1.DataClass,
	continuity continuityChange,
) map[string]any {
	details := map[string]any{"fields": changedProjectFields(body, continuity)}
	if nextClass != nil {
		details["previousDataClass"] = string(project.Spec.DataClass)
		details["dataClass"] = string(*nextClass)
	}
	// A criticality change is privileged for the same reason a class change
	// is: it decides how loudly this project's environments alert and what a
	// policy bundle may demand of them, so the trail has to show what it was.
	continuity.recordInto(details, kitchenv1alpha1.Continuity{
		Criticality: project.Spec.Criticality,
		RTO:         project.Spec.RTO,
		RPO:         project.Spec.RPO,
	})
	return details
}

// changedProjectFields names the settings a PATCH actually carried, for the
// audit record's details.
//
// The values are deliberately not recorded. A project's environment variables
// go through this endpoint, and an audit log that copied them would be a
// second place secrets live — which is precisely the sort of thing the log
// exists to make impossible. What changed is the auditable fact; what it
// changed to is on the object.
func changedProjectFields(body patchProjectRequest, continuity continuityChange) []string {
	fields := []string{}
	for _, field := range []struct {
		name    string
		changed bool
	}{
		{"productionBranch", body.ProductionBranch != nil},
		{"previews", body.Previews != nil},
		{"previewsProtected", body.PreviewsProtected != nil},
		{"previewsMax", body.PreviewsMax != nil},
		{"buildStrategy", body.BuildStrategy != nil},
		{"dockerfilePath", body.DockerfilePath != nil},
		{"dockerfileTarget", body.DockerfileTarget != nil},
		{"rootDirectory", body.RootDirectory != nil},
		{"port", body.Port != nil},
		{"replicas", body.Replicas != nil},
		{"cpu", body.CPU != nil},
		{"memory", body.Memory != nil},
		{"health", body.Health != nil},
		{"command", body.Command != nil},
		{"args", body.Args != nil},
		{"previewArgs", body.PreviewArgs != nil},
		{"singleton", body.Singleton != nil},
		{"notRequestDriven", body.NotRequestDriven != nil},
		{"promotionStages", body.PromotionStages != nil},
		{"processes", body.Processes != nil},
		{"files", body.Files != nil},
		{"dataClass", body.DataClass != nil},
	} {
		if field.changed {
			fields = append(fields, field.name)
		}
	}
	return append(fields, continuity.changedFields()...)
}
