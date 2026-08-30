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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// A project's own secrets: the credentials Kitchen did not mint.
//
// The shape is the connection credential's, applied one level down, and
// deliberately so — that is the platform's established answer to "a value the
// operator holds and nobody reads back". A value arrives in a request body,
// the operator puts it in a Secret, and no response on this API ever carries
// it again. What can be read is that a secret of that name exists, which is
// what a screen needs to offer it and what a variable needs to reference it.
//
// Two things are different from a connection's credential, and both follow
// from whose secret it is:
//
//   - **It belongs to a project, so it is the project's developers' to
//     write.** A connection is the platform's credential to a third party and
//     is the operator's; this is the application's own credential, and the
//     day job — the same bar as changing an environment variable, which is
//     the thing it exists to stop being written in cleartext.
//   - **It has to reach the application namespace**, which is the operator's
//     to write. So the API writes the source in the platform namespace,
//     owner-referenced by its Project, and the ProjectReconciler mirrors it —
//     see internal/controller/projectsecrets.go for why the copy is the
//     recoverable one.
//
// Nothing here reads a value back. The one read is the list, and the list is
// names.

// projectSecretValueLimit bounds one value. A Secret is capped at 1MiB in
// total by the API server, and a credential is a line of text: the limit is
// here so that the refusal names the cap rather than being the API server's
// complaint about an object that is already too big to write.
const projectSecretValueLimit = 64 << 10

// projectSecretView is one of a project's secrets, minus its value — which is
// the whole of what there is to say about it.
//
// Reference is the `fromSecret` an environment variable is written with to
// read this secret, answered rather than left to be worked out: it saves a
// caller from having to know the name of the object the platform keeps them
// in, and it is the one place that name is published.
type projectSecretView struct {
	Name      string     `json:"name"`
	Reference keyRefView `json:"reference"`
}

// setProjectSecretRequest is a value on its way in. It travels one way: there
// is no response anywhere on this API that carries it back.
type setProjectSecretRequest struct {
	Value string `json:"value"`
}

// projectSecretReference is the `fromSecret` that reads one project secret.
func projectSecretReference(name string) keyRefView {
	return keyRefView{Name: controller.ProjectSecretsName, Key: name}
}

// projectSecretsOf reads what a project's secrets are, by name.
//
// A project with none answers an empty map rather than a missing object: a
// project that has never had a secret and a project whose last one was
// deleted are the same project, and the Secret is only there while it holds
// something.
func (s *Server) projectSecretsOf(ctx context.Context, project string) (map[string][]byte, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: controller.ProjectSecretsSourceName(project)}
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

// projectSecretNames is what a project holds, sorted so a list does not
// reorder itself between two reads of an unchanged project.
func projectSecretNames(secrets map[string][]byte) []string {
	names := make([]string, 0, len(secrets))
	for name := range secrets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *Server) listProjectSecrets(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	secrets, err := s.projectSecretsOf(ctx, project.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}

	names := projectSecretNames(secrets)
	views := make([]projectSecretView, 0, len(names))
	for _, name := range names {
		views = append(views, projectSecretView{Name: name, Reference: projectSecretReference(name)})
	}
	writeList(w, views)
}

// setProjectSecret sets a secret, or replaces the value of one that is already
// there — one route, because "set" and "rotate" are the same write with a
// different history, and a caller that had to know which it was doing would
// have to be told whether a value exists.
func (s *Server) setProjectSecret(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	name := strings.TrimSpace(req.PathValue("secret"))
	if errs := validation.IsConfigMapKey(name); len(errs) > 0 {
		badRequest(w, "%q cannot be the name of a secret: use letters, digits, '-', '_' and '.', "+
			"at most 253 characters — it is the key an environment variable references", name)
		return
	}

	body := setProjectSecretRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.Value == "" {
		badRequest(w, "value is required: the operator stores it and never reads it back to you. "+
			"Removing a secret is DELETE on this path")
		return
	}
	if len(body.Value) > projectSecretValueLimit {
		badRequest(w, "the value is %d bytes, and a project secret may be at most %d",
			len(body.Value), projectSecretValueLimit)
		return
	}

	secrets, err := s.projectSecretsOf(ctx, project.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	_, existed := secrets[name]

	// Recorded before the value is written, like a connection's credential
	// and for the same reason: the secret is the part of this request that
	// matters, and a credential on the cluster that no record mentions is the
	// failure to avoid.
	operation := clickhouse.AuditCreate
	if existed {
		operation = clickhouse.AuditUpdate
	}
	if !s.recorded(w, req, projectSecretTransition(project, name, operation)) {
		return
	}

	if err := s.writeProjectSecrets(ctx, project, func(data map[string][]byte) {
		data[name] = []byte(body.Value)
	}); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("project secret written through the api",
		"project", project.Name, "secret", name, "rotated", existed, "caller", callerName(caller))
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	writeJSON(w, status, projectSecretView{Name: name, Reference: projectSecretReference(name)})
}

func (s *Server) deleteProjectSecret(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	name := strings.TrimSpace(req.PathValue("secret"))

	secrets, err := s.projectSecretsOf(ctx, project.Name)
	if err != nil {
		s.writeError(w, err)
		return
	}
	if _, ok := secrets[name]; !ok {
		writeJSON(w, http.StatusNotFound, errorBody{Error: fmt.Sprintf(
			"project %s has no secret %q", project.Name, name)})
		return
	}

	// An environment variable pointing at a secret that is not there leaves
	// the container unable to start, so the deletion names what still reads
	// it rather than discovering it at the next deploy. It is a refusal, not
	// a warning: the undo for a value nobody has any more is to go and find
	// it again.
	if users := projectSecretReaders(project, name); len(users) > 0 {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"the secret %q is still read by %s: point those somewhere else first",
			name, strings.Join(users, ", "))})
		return
	}

	if !s.recorded(w, req, projectSecretTransition(project, name, clickhouse.AuditDelete)) {
		return
	}

	if err := s.writeProjectSecrets(ctx, project, func(data map[string][]byte) {
		delete(data, name)
	}); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("project secret deleted through the api",
		"project", project.Name, "secret", name, "caller", callerName(caller))
	w.WriteHeader(http.StatusNoContent)
}

// projectSecretTransition is the record one write to a project's secrets
// leaves behind.
//
// It is built here rather than inline at each call site so that the property
// the whole feature rests on — **the log records that a value changed and
// never the value** — is one function rather than three, and so that a test
// can hold it to that by looking for the value in every field at once.
//
// It is classified as a credential write, which is what puts it in the same
// `?privileged=true` view as a connection's rotation: from a reviewer's side
// of the platform, "a credential this installation holds was replaced" is one
// question regardless of who holds it.
func projectSecretTransition(project *kitchenv1alpha1.Project, secret, operation string) audit.Transition {
	reason := ""
	switch operation {
	case clickhouse.AuditCreate:
		reason = fmt.Sprintf("the secret %s was set on project %s", secret, project.Name)
	case clickhouse.AuditUpdate:
		reason = fmt.Sprintf("the value of the secret %s on project %s was replaced", secret, project.Name)
	default:
		reason = fmt.Sprintf("the secret %s was deleted from project %s", secret, project.Name)
	}
	return audit.Transition{
		Object:     project,
		Kind:       audit.KindProjectSecret,
		Operation:  operation,
		Privileged: audit.PrivilegeCredential,
		Project:    project.Name,
		Reason:     reason,
		// The secret's name, and the fact that this write replaced a value
		// rather than introducing one. Nothing else about it is knowable from
		// here, which is the point.
		Details: map[string]any{"secret": secret, "rotated": operation == clickhouse.AuditUpdate},
	}
}

// projectSecretReaders is every environment variable of the project whose
// value comes from this secret.
func projectSecretReaders(project *kitchenv1alpha1.Project, name string) []string {
	var readers []string
	for _, variable := range project.Spec.Env {
		ref := variable.SecretRef
		if ref != nil && ref.Name == controller.ProjectSecretsName && ref.Key == name {
			readers = append(readers, variable.Name)
		}
	}
	sort.Strings(readers)
	return readers
}

// writeProjectSecrets applies one change to the Secret the platform namespace
// holds a project's secrets in, creating it on the first write and deleting it
// when the last secret goes.
//
// The Secret is owner-referenced by its Project, which is what "owned by the
// project" means here: the two are in the same namespace, so the reference is
// a real one rather than the cross-namespace kind Kubernetes ignores. The
// copy that reaches the application namespace is the reconciler's, and is
// rebuilt from this one — so this is the object that must survive, and the
// other is the one that must be recreatable.
func (s *Server) writeProjectSecrets(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	change func(map[string][]byte),
) error {
	name := controller.ProjectSecretsSourceName(project.Name)
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
	// Nothing left in it. The object goes rather than staying behind empty,
	// so the reconciler's mirror is removed too and a variable referencing a
	// deleted secret fails at the container rather than reading an empty
	// value as the credential.
	if err := s.Client.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
