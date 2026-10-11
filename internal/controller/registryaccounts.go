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
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider"
)

// Per-project registry accounts.
//
// The bundled registry used to have two accounts for the whole platform: one
// that pushes and one that reads, both copied into every application
// namespace. That made a project's pull secret a key to every other project's
// images, and a Dockerfile build's credential — which a `RUN` step can very
// probably reach through the rootless BuildKit daemon — a key to push over
// them. So on the bundled registry every project is an account of its own,
// two in fact, scoped to the repositories that project pushes:
//
//   - `<project>.push` may pull and push `<project>` and `<project>/…`;
//   - `<project>.pull` may only pull them.
//
// The registry does not hold these accounts. zot's htpasswd and access
// control are files it reads at startup, and a registry restart per project
// created is not a price worth paying, so zot is configured for the Docker
// token flow instead: it trusts a token signed by a key the chart generated,
// and the operator's registry token service (internal/registrytoken) is the
// one that signs. That service is where a password is checked and where the
// scope above is decided, from the Secret written here.
//
// The project is the path segment on this registry and nowhere else — see
// workloadRepository — because a project name holds no slash, which makes
// `<project>/` a boundary no other project's name can cross. `shop-api` used
// to be both project shop's `api` workload and project shop-api's own image.

const (
	// RegistryAccountsAnnotation and its one value are provider's, because
	// the registry target is read there and has to know where a workload's
	// image goes.
	RegistryAccountsAnnotation = provider.RegistryAccountsAnnotation
	RegistryAccountsPerProject = provider.RegistryAccountsPerProject

	// Keys in a project's account Secret.
	RegistryAccountPushPasswordKey = "pushPassword"
	RegistryAccountPullPasswordKey = "pullPassword"

	// Suffixes of the two usernames. A project name is a DNS label, which
	// holds no dot, so the last dot always separates the project from what
	// the account may do.
	registryAccountPushSuffix = ".push"
	registryAccountPullSuffix = ".pull"
)

// RegistryAccountSecretName is the Secret in the platform namespace holding a
// project's two registry passwords. It is the source the application
// namespace's copies are written from, and what the token service checks a
// password against.
func RegistryAccountSecretName(project string) string {
	return "kitchen-registry-account-" + project
}

// RegistryPushUsername and RegistryPullUsername are the project's two
// accounts on the bundled registry.
func RegistryPushUsername(project string) string { return project + registryAccountPushSuffix }
func RegistryPullUsername(project string) string { return project + registryAccountPullSuffix }

// ParseRegistryUsername reads a project account's username back: the project
// it belongs to, and whether it may push. ok is false for anything that is not
// one — the platform's own two accounts among them.
func ParseRegistryUsername(username string) (project string, push bool, ok bool) {
	switch {
	case strings.HasSuffix(username, registryAccountPushSuffix):
		project, push = strings.TrimSuffix(username, registryAccountPushSuffix), true
	case strings.HasSuffix(username, registryAccountPullSuffix):
		project = strings.TrimSuffix(username, registryAccountPullSuffix)
	default:
		return "", false, false
	}
	if project == "" || strings.Contains(project, ".") {
		return "", false, false
	}
	return project, push, true
}

// perProjectAccounts reports whether a registry Connection's credential is
// the platform's and every project is to be given its own instead.
func perProjectAccounts(conn *kitchenv1alpha1.Connection) bool {
	return conn != nil && conn.Annotations[RegistryAccountsAnnotation] == RegistryAccountsPerProject
}

// ensureRegistryAccount returns a project's account Secret, creating it with
// two fresh passwords the first time anything asks. Both the project
// reconciler and a build ask, whichever comes first, so a creation that loses
// the race reads back the winner's.
//
// It is owned by the Project, which lives in the same namespace, so the
// account goes when the project does even if the finalizer's own delete is
// never reached.
func ensureRegistryAccount(
	ctx context.Context, c client.Client, project *kitchenv1alpha1.Project,
) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: project.Namespace, Name: RegistryAccountSecretName(project.Name)}
	err := c.Get(ctx, key, secret)
	if err == nil {
		if len(secret.Data[RegistryAccountPushPasswordKey]) > 0 && len(secret.Data[RegistryAccountPullPasswordKey]) > 0 {
			return secret, nil
		}
		// Something emptied it. A password is not recoverable, so a new pair
		// is the only repair, and the copies are rewritten from it below.
		return secret, fillRegistryAccount(ctx, c, secret)
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	push, err := randomPassword()
	if err != nil {
		return nil, err
	}
	pull, err := randomPassword()
	if err != nil {
		return nil, err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels:    map[string]string{labelProject: project.Name, labelManagedByKey: labelManagedByValue},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			RegistryAccountPushPasswordKey: []byte(push),
			RegistryAccountPullPasswordKey: []byte(pull),
		},
	}
	if err := controllerutil.SetOwnerReference(project, secret, c.Scheme()); err != nil {
		return nil, err
	}
	if err := c.Create(ctx, secret); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, err
		}
		if err := c.Get(ctx, key, secret); err != nil {
			return nil, err
		}
	}
	return secret, nil
}

func fillRegistryAccount(ctx context.Context, c client.Client, secret *corev1.Secret) error {
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	for _, key := range []string{RegistryAccountPushPasswordKey, RegistryAccountPullPasswordKey} {
		if len(secret.Data[key]) > 0 {
			continue
		}
		password, err := randomPassword()
		if err != nil {
			return err
		}
		secret.Data[key] = []byte(password)
	}
	return c.Update(ctx, secret)
}

func randomPassword() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// registryAccountDockerConfigs is a project's two accounts in the shape every
// consumer of a registry credential reads: a dockerconfigjson naming the
// Connection's registry.
func registryAccountDockerConfigs(
	ctx context.Context, c client.Client, conn *kitchenv1alpha1.Connection, project *kitchenv1alpha1.Project,
) (push, pull []byte, err error) {
	target, err := provider.Registry(conn)
	if err != nil {
		return nil, nil, err
	}
	account, err := ensureRegistryAccount(ctx, c, project)
	if err != nil {
		return nil, nil, fmt.Errorf("the registry account of project %q could not be prepared: %w", project.Name, err)
	}
	push, err = provider.DockerConfigJSON(target.Server, RegistryPushUsername(project.Name),
		string(account.Data[RegistryAccountPushPasswordKey]))
	if err != nil {
		return nil, nil, err
	}
	pull, err = provider.DockerConfigJSON(target.Server, RegistryPullUsername(project.Name),
		string(account.Data[RegistryAccountPullPasswordKey]))
	if err != nil {
		return nil, nil, err
	}
	return push, pull, nil
}

// writeRegistryCredentials puts the credentials a project's pods use for one
// registry Connection into its application namespace: the one that pushes
// under registrySecretName, and the one that cannot beside it under the read
// suffix (#424). It returns the two names, the second empty where the
// registry issues no narrower credential.
//
// On a Connection with per-project accounts both are the project's own, and
// nothing of the Connection's credential leaves the platform namespace. On
// any other they are copies of what the Connection holds, as they always
// were.
func writeRegistryCredentials(
	ctx context.Context, c client.Client,
	conn *kitchenv1alpha1.Connection, project *kitchenv1alpha1.Project, appNS string,
) (push, read string, err error) {
	push = registrySecretName(conn.Name)
	read = readCredentialSecretName(push)
	if perProjectAccounts(conn) {
		pushConfig, pullConfig, err := registryAccountDockerConfigs(ctx, c, conn, project)
		if err != nil {
			return "", "", err
		}
		if err := writeDockerConfigCopy(ctx, c, appNS, push, corev1.SecretTypeDockerConfigJson,
			map[string][]byte{corev1.DockerConfigJsonKey: pushConfig}); err != nil {
			return "", "", err
		}
		if err := writeDockerConfigCopy(ctx, c, appNS, read, corev1.SecretTypeDockerConfigJson,
			map[string][]byte{corev1.DockerConfigJsonKey: pullConfig}); err != nil {
			return "", "", err
		}
		return push, read, nil
	}

	src := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: conn.Namespace, Name: conn.Spec.CredentialsSecretRef.Name}, src); err != nil {
		return "", "", err
	}
	if err := writeDockerConfigCopy(ctx, c, appNS, push, src.Type, src.Data); err != nil {
		return "", "", err
	}

	// The read-only credential beside the Connection's own, where the
	// registry issues one. It is "" when it does not, which is the common
	// case for a registry the platform does not run. That is not an error —
	// the Connection's status is where it is reported — but a stale copy is:
	// a credential the platform stopped writing must stop being handed out,
	// so the copy is removed when the source is gone.
	readSrc := &corev1.Secret{}
	key := types.NamespacedName{Namespace: conn.Namespace, Name: readCredentialSecretName(conn.Spec.CredentialsSecretRef.Name)}
	if err := c.Get(ctx, key, readSrc); err != nil {
		if !apierrors.IsNotFound(err) {
			return "", "", err
		}
		stale := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: read, Namespace: appNS}}
		if err := c.Delete(ctx, stale); err != nil && !apierrors.IsNotFound(err) {
			return "", "", err
		}
		return push, "", nil
	}
	if err := writeDockerConfigCopy(ctx, c, appNS, read, readSrc.Type, readSrc.Data); err != nil {
		return "", "", err
	}
	return push, read, nil
}

// writeDockerConfigCopy writes one credential into an application namespace.
func writeDockerConfigCopy(
	ctx context.Context, c client.Client, namespace, name string, kind corev1.SecretType, data map[string][]byte,
) error {
	dst := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, c, dst, func() error {
		if dst.CreationTimestamp.IsZero() {
			dst.Type = kind
		}
		dst.Labels = map[string]string{labelManagedByKey: labelManagedByValue}
		dst.Data = data
		return nil
	})
	return err
}

// syncRegistryAccounts keeps a project's application namespace holding its
// own registry accounts for every per-project Connection it uses — the one it
// pushes to, and any its vendored images are pulled through.
//
// A build writes them too, but an installation that upgrades into per-project
// accounts has namespaces full of copies of the platform's credential and
// environments that may not build again for weeks. Doing it on every project
// reconcile is what takes the platform's credential back out of them.
func (r *ProjectReconciler) syncRegistryAccounts(ctx context.Context, project *kitchenv1alpha1.Project) error {
	appNS := appNamespace(project.Name)
	for _, name := range projectRegistryConnections(project) {
		conn := &kitchenv1alpha1.Connection{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: name}, conn); err != nil {
			if apierrors.IsNotFound(err) {
				// Reported by checkConnection, which is the condition
				// somebody looks at.
				continue
			}
			return err
		}
		if !perProjectAccounts(conn) {
			continue
		}
		if _, _, err := writeRegistryCredentials(ctx, r.Client, conn, project, appNS); err != nil {
			return err
		}
	}
	return nil
}

// projectRegistryConnections is every registry Connection a project's pods
// pull through, each once.
func projectRegistryConnections(project *kitchenv1alpha1.Project) []string {
	var names []string
	add := func(name string) {
		if name == "" {
			return
		}
		for _, existing := range names {
			if existing == name {
				return
			}
		}
		names = append(names, name)
	}
	add(project.Spec.RegistryConnection())
	if image := project.Spec.Source.Image; image != nil {
		add(image.PullConnection())
	}
	for i := range project.Spec.Processes {
		if image := project.Spec.Processes[i].Image; image != nil {
			add(image.PullConnection())
		}
	}
	return names
}

// deleteRegistryAccount removes a project's account with the project. The
// owner reference would get there too; this is the finalizer not leaving it
// to the garbage collector, the way it leaves nothing else.
func (r *ProjectReconciler) deleteRegistryAccount(ctx context.Context, project *kitchenv1alpha1.Project) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: RegistryAccountSecretName(project.Name), Namespace: project.Namespace,
	}}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
