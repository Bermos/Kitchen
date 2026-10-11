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

package registrytoken

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// ClusterAccounts are the accounts the platform holds for the bundled
// registry: its own two, which the chart generated, and every project's two,
// which the operator writes when the project is created (see
// internal/controller/registryaccounts.go).
type ClusterAccounts struct {
	Client client.Reader
	// Namespace is the platform namespace, where every one of them lives.
	Namespace string

	mu      sync.Mutex
	keyFrom string
	key     *rsa.PrivateKey
}

// errNoRegistry is a token request on an installation that runs no registry,
// which the route should never have delivered.
var errNoRegistry = errors.New("this installation runs no bundled registry")

// platform reads the registry's host and the chart's Secret.
func (a *ClusterAccounts) platform(ctx context.Context) (string, *corev1.Secret, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := a.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		return "", nil, err
	}
	host, name, ok := controller.PlatformRegistry(kitchen)
	if !ok {
		return "", nil, errNoRegistry
	}
	secret := &corev1.Secret{}
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: name}, secret); err != nil {
		return "", nil, err
	}
	return host, secret, nil
}

// Authenticate implements Accounts.
func (a *ClusterAccounts) Authenticate(ctx context.Context, username, password string) (*Grant, error) {
	host, platform, err := a.platform(ctx)
	if err != nil {
		return nil, err
	}

	// The platform's own two. Their grant is everywhere: the operator reads
	// and attaches evidence to every project's artifacts, and the Connection
	// the platform seeded is tested with the first of them.
	for _, account := range []struct {
		userKey, passwordKey string
		grant                Grant
	}{
		{controller.RegistrySecretKeyUsername, controller.RegistrySecretKeyPassword, PlatformGrant()},
		{controller.RegistrySecretKeyReadUsername, controller.RegistrySecretKeyReadPassword, ReadGrant()},
	} {
		stored := string(platform.Data[account.userKey])
		if stored == "" || stored != username {
			continue
		}
		if !matches(platform.Data[account.passwordKey], password) {
			return nil, nil
		}
		grant := account.grant
		grant.Subject = username
		return &grant, nil
	}

	project, push, ok := controller.ParseRegistryUsername(username)
	if !ok {
		return nil, nil
	}
	owner := &kitchenv1alpha1.Project{}
	if err := a.Client.Get(ctx, types.NamespacedName{Namespace: a.Namespace, Name: project}, owner); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if !owner.DeletionTimestamp.IsZero() {
		// Its images are about to go with it; nothing more is pushed there
		// or pulled from it.
		return nil, nil
	}
	account := &corev1.Secret{}
	key := types.NamespacedName{Namespace: a.Namespace, Name: controller.RegistryAccountSecretName(project)}
	if err := a.Client.Get(ctx, key, account); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	passwordKey := controller.RegistryAccountPullPasswordKey
	if push {
		passwordKey = controller.RegistryAccountPushPasswordKey
	}
	if !matches(account.Data[passwordKey], password) {
		return nil, nil
	}
	grant := ProjectGrant(project, push)
	grant.Subject = username
	if grant.Legacy, err = a.legacyRepositories(ctx, host, project); err != nil {
		return nil, err
	}
	return &grant, nil
}

// legacyRepositories are the `<project>-<workload>` repositories a project's
// Releases still run images from. Until per-project accounts a workload's
// image went beside the project's rather than under it, and an environment
// rolled out before that still pulls from there whenever a pod lands on a
// node that has not got it cached.
//
// They are read off the Releases rather than matched by pattern, and only
// where the Release says the platform built that workload: a pattern would
// hand project shop the images of a project called shop-api, which is the
// very collision this ends, and a vendored image is something the project
// named rather than something the platform pushed for it.
func (a *ClusterAccounts) legacyRepositories(ctx context.Context, host, project string) (map[string]bool, error) {
	releases := &kitchenv1alpha1.ReleaseList{}
	if err := a.Client.List(ctx, releases, client.InNamespace(a.Namespace)); err != nil {
		return nil, err
	}
	legacy := map[string]bool{}
	for i := range releases.Items {
		release := &releases.Items[i]
		if release.Spec.ProjectRef.Name != project {
			continue
		}
		for _, workload := range release.Spec.Workloads {
			if workload.Strategy == "" {
				continue
			}
			repository, ok := repositoryOn(host, workload.Image)
			if ok && repository == project+"-"+workload.Name {
				legacy[repository] = true
			}
		}
	}
	return legacy, nil
}

// repositoryOn is an image reference's repository when it is on host.
func repositoryOn(host, image string) (string, bool) {
	rest, ok := strings.CutPrefix(image, host+"/")
	if !ok {
		return "", false
	}
	if at := strings.Index(rest, "@"); at >= 0 {
		rest = rest[:at]
	}
	if colon := strings.LastIndex(rest, ":"); colon >= 0 && !strings.Contains(rest[colon:], "/") {
		rest = rest[:colon]
	}
	return rest, rest != ""
}

// matches compares a stored password with an offered one in constant time,
// over digests so that the comparison does not even say how long it is.
func matches(stored []byte, offered string) bool {
	if len(stored) == 0 {
		return false
	}
	want := sha256.Sum256(stored)
	got := sha256.Sum256([]byte(offered))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// SigningKey implements Accounts. The key is parsed once per version of the
// Secret it is in.
func (a *ClusterAccounts) SigningKey(ctx context.Context) (*rsa.PrivateKey, error) {
	_, platform, err := a.platform(ctx)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	version := string(platform.UID) + "/" + platform.ResourceVersion
	if a.key != nil && a.keyFrom == version {
		return a.key, nil
	}
	key, err := ParseKey(platform.Data[controller.RegistrySecretKeyTokenKey])
	if err != nil {
		return nil, fmt.Errorf("secret %q: %w", platform.Name, err)
	}
	a.key, a.keyFrom = key, version
	return key, nil
}

// ParseKey reads an RSA private key in either PEM encoding: PKCS #1, which is
// what Helm's genSelfSignedCert writes, or PKCS #8.
func ParseKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("the registry token signing key is missing or is not PEM; " +
			"upgrading the chart generates one")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the registry token signing key cannot be parsed: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("the registry token signing key is not an RSA key")
	}
	return key, nil
}
