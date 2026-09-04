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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// A project's configuration files: what software the platform did not build
// is configured by (#311).
//
// **One object, two write surfaces**, and the split is the content rather
// than the declaration. A file is one entry in `spec.files` whatever it holds
// — its name, its path, the workloads it reaches and whether its content is a
// credential — because those are one fact about one file, and a file that
// becomes secret keeps every one of them. Splitting the declaration in two
// lists would mean moving a file house to change one property of it, and
// would leave the screen with two tables answering "which files does this
// project place".
//
// What is split is where the content travels:
//
//   - A **plain** file's content is on `PATCH /projects/{name}` with the rest
//     of the declaration, and reads back on `GET /projects/{name}`. It is not
//     a credential and pretending otherwise would make an editor impossible
//     for the ordinary case.
//   - A **secret** file's content is written here, on a route of its own, and
//     no response on this API carries it back. What is answered is that there
//     is content, how many bytes, and a digest of it — enough for a screen to
//     say "set, and this is not the one you have in your hand" and not enough
//     to be the credential.
//
// The digest is the platform's observation of what it holds, and it is
// answered rather than withheld deliberately: without it a reader has no way
// to tell a file that was written from one that was written twice, and the
// alternative — a timestamp — says less and is no less of a disclosure.
//
// Both are the **admin's** to write, which is one rule rather than two. The
// declaration is a project setting like the port and the replica count, and a
// content route below the declaration's own bar would be the odd inversion of
// letting a developer change the secret file and not the plain one. Reading
// the list is a viewer's, like reading the variables.

// projectFileContentLimit bounds one secret file's content. It is the CRD's
// own cap on a plain file's, so that the two halves of one feature do not
// disagree about how big a configuration file may be.
const projectFileContentLimit = kitchenv1alpha1.ConfigFileContentLimit

// configFileView is one of a project's configuration files as the API
// answers it. A plain file carries its content; a secret one carries the
// facts about content and never the content.
type configFileView struct {
	Name string `json:"name"`
	Path string `json:"path"`
	// Workloads that mount it, empty meaning every workload of the unit.
	Workloads []string `json:"workloads,omitempty"`
	// Secret says the content is a credential the platform holds and this
	// API never answers.
	Secret bool `json:"secret,omitempty"`
	// Content is the file, for a plain file only. It is absent — never
	// empty-stringed — on a secret one, so that a client can tell "there is
	// nothing to show you" from "the file is empty".
	Content *string `json:"content,omitempty"`
	// ContentHash is a short digest of what the platform holds, and Size the
	// number of bytes. Both are answered for a secret file whose content has
	// been written, and both are absent for one whose has not — which is the
	// state a screen has to be able to name, since the workloads that mount
	// it will not start until it is.
	ContentHash string `json:"contentHash,omitempty"`
	Size        int    `json:"size,omitempty"`
}

// setProjectFileRequest is a secret file's content on its way in. It travels
// one way: no response on this API carries it back.
type setProjectFileRequest struct {
	Content string `json:"content"`
}

// contentDigest is what the API answers about a credential it holds: a short
// sha256 of it, which says whether two writes were the same write and says
// nothing else.
func contentDigest(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])[:16]
}

// configFileViews is a project's files as the API answers them, given what
// the platform holds for the secret ones.
func configFileViews(files []kitchenv1alpha1.ConfigFile, held map[string][]byte) []configFileView {
	views := make([]configFileView, 0, len(files))
	for _, file := range files {
		view := configFileView{
			Name:      file.Name,
			Path:      file.Path,
			Workloads: file.Workloads,
			Secret:    file.Secret,
		}
		if !file.Secret {
			content := file.Content
			view.Content = &content
			views = append(views, view)
			continue
		}
		if content, written := held[file.Name]; written {
			view.ContentHash = contentDigest(content)
			view.Size = len(content)
		}
		views = append(views, view)
	}
	return views
}

// projectFilesOf reads the content the platform holds for a project's secret
// files, by name.
//
// A project with none answers an empty map rather than a missing object, the
// way its secrets do: a project that never had one and a project whose last
// one was deleted are the same project.
func (s *Server) projectFilesOf(ctx context.Context, project string) (map[string][]byte, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: controller.ProjectFilesSourceName(project)}
	switch err := s.Client.Get(ctx, key, secret); {
	case apierrors.IsNotFound(err):
		return map[string][]byte{}, nil
	case err != nil:
		return nil, err
	}
	if secret.Data == nil {
		return map[string][]byte{}, nil
	}
	return secret.Data, nil
}

// setProjectFile writes the content of a secret configuration file, or
// replaces the content of one already written — one route, because setting
// and rotating are the same write with a different history, and the same
// reasoning the project secrets' own route is built on.
func (s *Server) setProjectFile(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	name := strings.TrimSpace(req.PathValue("file"))
	// The declaration comes first, and it has to already exist. A content
	// route that could create one would be a second way to declare a file —
	// with no path and no workloads, so a file nothing could mount — and the
	// refusal names the route that does declare it.
	declared := fileNamed(project.Spec.Files, name)
	switch {
	case declared == nil:
		writeJSON(w, http.StatusNotFound, errorBody{Error: fmt.Sprintf(
			"project %s declares no file %q: declare it on PATCH /projects/%s first — a file needs a path and the "+
				"workloads that read it before it has anywhere to go", project.Name, name, project.Name)})
		return
	case !declared.Secret:
		badRequest(w, "the file %q is not secret, so its content travels with the rest of its declaration: "+
			"send it to PATCH /projects/%s. Marking it secret is how its content stops being read back",
			name, project.Name)
		return
	}

	body := setProjectFileRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.Content == "" {
		badRequest(w, "content is required: the operator stores it and never reads it back to you. "+
			"Removing the file is taking it off spec.files on PATCH /projects/%s", project.Name)
		return
	}
	if len(body.Content) > projectFileContentLimit {
		badRequest(w, "the content is %d bytes, and a configuration file may be at most %d",
			len(body.Content), projectFileContentLimit)
		return
	}

	held, err := s.projectFilesOf(ctx, project.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	_, existed := held[name]

	// Recorded before the content is written, like a project secret and for
	// the same reason: the credential is the part of this request that
	// matters, and one on the cluster that no record mentions is the failure
	// to avoid.
	operation := clickhouse.AuditCreate
	if existed {
		operation = clickhouse.AuditUpdate
	}
	if !s.recorded(w, req, projectFileTransition(project, name, operation)) {
		return
	}

	if err := s.writeProjectFiles(ctx, project, func(data map[string][]byte) {
		data[name] = []byte(body.Content)
	}); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("project file written through the api",
		"project", project.Name, "file", name, "replaced", existed, "caller", callerName(caller))
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	writeJSON(w, status, configFileView{
		Name:        declared.Name,
		Path:        declared.Path,
		Workloads:   declared.Workloads,
		Secret:      true,
		ContentHash: contentDigest([]byte(body.Content)),
		Size:        len(body.Content),
	})
}

// fileNamed is one of a project's declared files, or nil.
func fileNamed(files []kitchenv1alpha1.ConfigFile, name string) *kitchenv1alpha1.ConfigFile {
	for i := range files {
		if files[i].Name == name {
			return &files[i]
		}
	}
	return nil
}

// projectFileTransition is the record one write to a secret file's content
// leaves behind.
//
// It is built here rather than inline for the reason the project secret's is:
// the property the whole feature rests on — **the log records that content
// changed and never the content** — is one function rather than several, and
// a test can hold it to that by looking for the content in every field at
// once.
func projectFileTransition(project *kitchenv1alpha1.Project, file, operation string) audit.Transition {
	reason := fmt.Sprintf("the content of the secret file %s on project %s was set", file, project.Name)
	if operation == clickhouse.AuditUpdate {
		reason = fmt.Sprintf("the content of the secret file %s on project %s was replaced", file, project.Name)
	}
	return audit.Transition{
		Object:     project,
		Kind:       audit.KindProjectFile,
		Operation:  operation,
		Privileged: audit.PrivilegeCredential,
		Project:    project.Name,
		Reason:     reason,
		// The file's name, and whether this write replaced content rather
		// than introducing it. Nothing else about it is knowable from here,
		// which is the point.
		Details: map[string]any{"file": file, "replaced": operation == clickhouse.AuditUpdate},
	}
}

// writeProjectFiles applies one change to the Secret the platform namespace
// holds a project's secret file content in, creating it on the first write
// and deleting it when the last file goes.
//
// It is the project secrets' own shape, one object along: the API cannot
// write into an application's namespace, so it writes the source and
// ProjectReconciler mirrors it — which is what makes the copy the recoverable
// one and this the object that must survive.
func (s *Server) writeProjectFiles(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	change func(map[string][]byte),
) error {
	name := controller.ProjectFilesSourceName(project.Name)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: s.Namespace}}

	_, err := controllerutil.CreateOrUpdate(ctx, s.Client, secret, func() error {
		if secret.Data == nil {
			secret.Data = map[string][]byte{}
		}
		change(secret.Data)
		secret.Type = corev1.SecretTypeOpaque
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[managedByLabelKey] = managedByLabelValue
		secret.Labels[controller.LabelProject] = project.Name
		return controllerutil.SetControllerReference(project, secret, s.Client.Scheme())
	})
	if err != nil {
		return err
	}
	if len(secret.Data) > 0 {
		return nil
	}
	// Nothing left in it, so the object goes and the reconciler's mirror goes
	// with it — rather than a Secret staying behind for a volume to mount an
	// empty file out of.
	if err := s.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// pruneProjectFiles drops the content the platform holds for files the
// project no longer declares as secret.
//
// It runs after a settings PATCH rather than in the reconciler, where the
// declaration is written and the caller can be told if it fails: a credential
// outliving the declaration that named it is exactly the residue the platform
// should not accumulate, and the reconciler has no business deciding a
// credential is unwanted.
func (s *Server) pruneProjectFiles(ctx context.Context, project *kitchenv1alpha1.Project) error {
	held, err := s.projectFilesOf(ctx, project.Name)
	if err != nil {
		return err
	}
	orphans := make([]string, 0, len(held))
	for name := range held {
		if declared := fileNamed(project.Spec.Files, name); declared == nil || !declared.Secret {
			orphans = append(orphans, name)
		}
	}
	if len(orphans) == 0 {
		return nil
	}
	return s.writeProjectFiles(ctx, project, func(data map[string][]byte) {
		for _, name := range orphans {
			delete(data, name)
		}
	})
}

// withFileContent fills in what the platform holds for a project's secret
// files: the digest and the size, which say that content was written and
// nothing about what it is.
//
// It is applied to the routes that answer one project rather than inside
// newProjectView, because the project list would otherwise read one Secret
// per project to fill in a field no list renders.
func (s *Server) withFileContent(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	view projectView,
) projectView {
	if len(project.Spec.Files) == 0 {
		return view
	}
	held, err := s.projectFilesOf(ctx, project.Name)
	if err != nil {
		// The declaration is the answer to this request and it is already in
		// hand. A digest that could not be read is reported as content that
		// has not been written, which is the reading a screen already knows
		// how to render, and the failure is logged rather than turning a
		// successful settings write into a 500.
		s.log().Error(err, "reading what the platform holds for a project's secret files",
			"project", project.Name)
		return view
	}
	view.Files = configFileViews(project.Spec.Files, held)
	return view
}
