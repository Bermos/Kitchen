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

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// A project's own secrets: the credentials Kitchen did not mint.
//
// A database the project runs itself, a third-party API key, an SMTP
// password — values the platform has no way to produce and every project
// eventually needs. Until this existed the only two ways to give an
// application one were `kubectl create secret` in a namespace the platform
// owns, and `env[].value`, which puts the credential in the Project spec in
// cleartext where the API reads it back to everyone on the project.
//
// **They live twice, and which copy is which matters.** The REST API writes
// the *source*, in the platform namespace, next to every other secret the
// platform holds and owner-referenced by the Project it belongs to. The
// reconciler mirrors it into the application namespace, where a container can
// actually name it — which is what makes the copy recoverable rather than
// precious: a namespace deleted by hand is recreated by the next reconcile
// with the project's secrets in it, because the values were never only there.
//
// It is one Secret per project rather than one per value: an environment
// variable names a Secret *and* a key, so one object with a key per secret is
// one name to know instead of one per credential, and one write to keep in
// step.
//
// What makes a rotated value reach a pod that is already running is not here:
// it is the digest in secretrevision.go, which covers every Secret a workload
// reads rather than only these.
const (
	// ProjectSecretsName is the Secret an application's namespace holds its
	// project's own credentials in, one key per secret. It is compiled in
	// rather than configurable because an environment variable references it
	// by name: a name somebody could change is a reference that stops
	// resolving.
	ProjectSecretsName = "kitchen-project-secrets"

	// projectSecretsSourcePrefix names the platform-namespace copy the API
	// writes and the reconciler mirrors from.
	projectSecretsSourcePrefix = "kitchen-project-secrets-"

	// ProjectFilesName is the Secret an application's namespace holds its
	// project's *secret configuration files* in, one key per file (#311). It
	// is compiled in for the reason ProjectSecretsName is: a pod's volume
	// references it by name, and a name somebody could change is a reference
	// that stops resolving.
	//
	// It is a second object rather than another key in the one above because
	// the two are different vocabularies sharing one namespace of keys: a
	// project with a secret named `config` and a secret file named `config`
	// is an ordinary project, and one object would make it a collision.
	ProjectFilesName = "kitchen-project-files"

	// projectFilesSourcePrefix names the platform-namespace copy the API
	// writes and the reconciler mirrors from.
	projectFilesSourcePrefix = "kitchen-project-files-"
)

// ProjectSecretsSourceName is the Secret in the platform namespace holding one
// project's secrets. The API writes it and this file mirrors it, so the name
// is spelled once and exported for the API to use.
func ProjectSecretsSourceName(projectName string) string {
	return projectSecretsSourcePrefix + projectName
}

// ProjectFilesSourceName is the Secret in the platform namespace holding one
// project's secret configuration files. It reads the way the secrets' does,
// for the same reason.
func ProjectFilesSourceName(projectName string) string {
	return projectFilesSourcePrefix + projectName
}

// mirrorProjectSecrets keeps the application namespace's copy of the
// project's secrets in step with the source the API writes.
//
// It runs on every reconcile rather than at namespace creation, for the
// reason every other piece of namespace state does: a namespace is created
// once and found by every reconcile after it, so anything written only at
// creation never reaches an installation that already has projects.
//
// A source that is not there takes the copy with it. That is what makes
// deleting the last secret remove the object instead of leaving an empty one
// behind for an environment variable to reference.
func (r *ProjectReconciler) mirrorProjectSecrets(ctx context.Context, project *kitchenv1alpha1.Project) error {
	// Two objects, one mechanism: the project's own credentials, and the
	// content of its secret configuration files. Both are written by the API
	// in the platform namespace and neither can be written there by it, so
	// both are mirrored here.
	for source, copied := range map[string]string{
		ProjectSecretsSourceName(project.Name): ProjectSecretsName,
		ProjectFilesSourceName(project.Name):   ProjectFilesName,
	} {
		if err := r.mirrorProjectSecret(ctx, project, source, copied); err != nil {
			return err
		}
	}
	return nil
}

// mirrorProjectSecret keeps one application-namespace copy in step with the
// platform-namespace source the API writes.
func (r *ProjectReconciler) mirrorProjectSecret(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	sourceName, copiedName string,
) error {
	appNS := appNamespace(project.Name)
	copied := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: copiedName, Namespace: appNS}}

	source := &corev1.Secret{}
	key := types.NamespacedName{Namespace: project.Namespace, Name: sourceName}
	switch err := r.Get(ctx, key, source); {
	case apierrors.IsNotFound(err):
		if err := r.Delete(ctx, copied); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	case err != nil:
		return err
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, copied, func() error {
		copied.Labels = map[string]string{
			labelProject:      project.Name,
			labelManagedByKey: labelManagedByValue,
		}
		copied.Type = corev1.SecretTypeOpaque
		copied.Data = source.Data
		return nil
	})
	return err
}

// deleteProjectSecrets removes the source the API wrote. The copy in the
// application namespace goes with the namespace; this one is in the platform
// namespace, where nothing else is about to be deleted.
//
// The Secret is owner-referenced by the Project as well, so a cluster's
// garbage collector would eventually remove it. This is what makes it
// deterministic, which is the same reason every other dependent is deleted by
// name in the finalizer rather than left to ownership.
func (r *ProjectReconciler) deleteProjectSecrets(ctx context.Context, project *kitchenv1alpha1.Project) error {
	for _, name := range []string{
		ProjectSecretsSourceName(project.Name),
		ProjectFilesSourceName(project.Name),
	} {
		source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: project.Namespace}}
		if err := r.Delete(ctx, source); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
