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
	"crypto/sha256"
	"encoding/hex"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

	// ProjectSecretsRevisionAnnotation carries a digest of the project
	// secrets one workload actually reads. It is on the pod template, so
	// rotating a value rolls the pods that use it and leaves every other
	// workload alone.
	//
	// Without it a rotation reaches an application whenever a pod happens to
	// restart — some pods on the old value and some on the new, for as long
	// as nothing redeploys. That is a worse answer than "on the next deploy",
	// because it is not an answer at all.
	ProjectSecretsRevisionAnnotation = "kitchen.bermos.dev/project-secrets-revision"
)

// ProjectSecretsSourceName is the Secret in the platform namespace holding one
// project's secrets. The API writes it and this file mirrors it, so the name
// is spelled once and exported for the API to use.
func ProjectSecretsSourceName(projectName string) string {
	return projectSecretsSourcePrefix + projectName
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
	appNS := appNamespace(project.Name)
	copied := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ProjectSecretsName, Namespace: appNS}}

	source := &corev1.Secret{}
	key := types.NamespacedName{Namespace: project.Namespace, Name: ProjectSecretsSourceName(project.Name)}
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
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      ProjectSecretsSourceName(project.Name),
		Namespace: project.Namespace,
	}}
	if err := r.Delete(ctx, source); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// projectSecretsRevision digests the project secrets a set of container
// variables actually reads, and answers "" for a workload that reads none.
//
// Only the referenced keys are hashed, so adding an unrelated secret to a
// project does not roll every environment it has — the digest changes for the
// workloads whose values changed and for no others.
func projectSecretsRevision(ctx context.Context, c client.Client, appNS string, podEnv []corev1.EnvVar) (string, error) {
	wanted := map[string]bool{}
	for _, variable := range podEnv {
		ref := variable.ValueFrom
		if ref == nil || ref.SecretKeyRef == nil || ref.SecretKeyRef.Name != ProjectSecretsName {
			continue
		}
		wanted[ref.SecretKeyRef.Key] = true
	}
	if len(wanted) == 0 {
		return "", nil
	}

	secret := &corev1.Secret{}
	switch err := c.Get(ctx, types.NamespacedName{Namespace: appNS, Name: ProjectSecretsName}, secret); {
	case apierrors.IsNotFound(err):
		// A variable naming a secret that is not there yet. The container
		// will not start until it is, and the reconcile that mirrors it is
		// what brings this back with a digest.
		return "", nil
	case err != nil:
		return "", err
	}

	keys := make([]string, 0, len(wanted))
	for key := range wanted {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	digest := sha256.New()
	for _, key := range keys {
		digest.Write([]byte(key))
		digest.Write([]byte{0})
		digest.Write(secret.Data[key])
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))[:16], nil
}

// applyProjectSecretsRevision stamps a pod template with the digest, or takes
// the stamp off a template that no longer reads any project secret.
func applyProjectSecretsRevision(template *metav1.ObjectMeta, revision string) {
	if revision == "" {
		delete(template.Annotations, ProjectSecretsRevisionAnnotation)
		return
	}
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[ProjectSecretsRevisionAnnotation] = revision
}
