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
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider"
)

// reconcileRegistry publishes the bundled image registry and seeds the
// Connection that points at it, so a fresh installation can build a project
// without anyone having a registry account first.
//
// The chart runs the registry, its Service and its volume; this owns the two
// halves the chart cannot. The route belongs here because the shared Gateway
// is created here, and the Connection belongs here because its credential is
// a Secret the platform writes and never reads back — the same rule every
// other credential the API stores follows.
//
// Publishing it on the base domain is not a convenience. The node's container
// runtime is what pulls an image, and it trusts neither an in-cluster CA nor a
// plain-HTTP address unless the node is configured to; configuring nodes is
// what every other in-cluster registry needs, and Kitchen is a chart installed
// into someone else's cluster. The platform's wildcard certificate is publicly
// trusted, so a route on it is the one address that asks the node for nothing
// — at the cost of pulls leaving the cluster and coming back through the
// Gateway, and of the feature not existing in TLS mode none.
func (r *KitchenReconciler) reconcileRegistry(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	registry := resolveRegistry(kitchen)
	if registry == nil {
		// Nothing to publish, for one of two quite different reasons. Either
		// way what is already there comes down: a route to a registry the
		// chart has stopped rendering answers 503, and a Connection naming it
		// is a registry picker entry that fails every build chosen with it.
		if err := r.removeRegistry(ctx, kitchen); err != nil {
			setCond(condRegistryReady, metav1.ConditionFalse, "CleanupFailed", err.Error())
			return false
		}
		if !kitchen.Spec.Registry.Enabled {
			// Switched off deliberately: this installation has a registry of
			// its own, which is an ordinary Connection someone created.
			meta.RemoveStatusCondition(&kitchen.Status.Conditions, condRegistryReady)
			return true
		}
		// Asked for and impossible. Saying so is the whole value here — the
		// alternative is a registry picker that is empty for no stated
		// reason. Nothing retries it: only a change to the Kitchen object can
		// make it possible.
		reason, message := registryUnavailableReason(kitchen)
		setCond(condRegistryReady, metav1.ConditionFalse, reason, message)
		return true
	}

	credential, err := r.registryCredential(ctx, registry)
	if err != nil {
		setCond(condRegistryReady, metav1.ConditionFalse, "CredentialUnavailable", err.Error())
		return false
	}
	if err := r.applyRegistryRoute(ctx, registry, gatewaySection(kitchen)); err != nil {
		setCond(condRegistryReady, metav1.ConditionFalse, "RouteFailed", err.Error())
		return false
	}
	seeded, err := r.seedRegistryConnection(ctx, kitchen, registry, credential)
	if err != nil {
		setCond(condRegistryReady, metav1.ConditionFalse, "ConnectionFailed", err.Error())
		return false
	}

	kitchen.Status.Registry = &kitchenv1alpha1.ImageRegistryStatus{
		Host:       registry.Host,
		Connection: seeded,
	}
	message := fmt.Sprintf("images are pushed to %s", registry.registryURL())
	if seeded != "" {
		message += fmt.Sprintf(", through the %q connection", seeded)
	} else {
		// The seed was taken away, which is allowed and is not a fault: the
		// registry is still published, and a project can be pointed at it by
		// a connection someone writes.
		message += "; the connection that pointed at it was deleted and is not recreated"
	}
	setCond(condRegistryReady, metav1.ConditionTrue, "RegistryPublished", message)
	return true
}

// registryCredentials is the registry's own username and password, as the
// chart wrote them.
type registryCredentials struct {
	Username string
	Password string
}

// registryCredential reads the credential the chart generated. It is the one
// account the registry has: builds push with it, and every application
// namespace pulls with a copy of it.
func (r *KitchenReconciler) registryCredential(
	ctx context.Context,
	registry *platformRegistry,
) (*registryCredentials, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: registry.SecretName}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	credential := &registryCredentials{
		Username: string(secret.Data[registrySecretKeyUsername]),
		Password: string(secret.Data[registrySecretKeyPassword]),
	}
	if credential.Username == "" || credential.Password == "" {
		return nil, fmt.Errorf("secret %q needs both %q and %q: the registry admits no anonymous access",
			registry.SecretName, registrySecretKeyUsername, registrySecretKeyPassword)
	}
	return credential, nil
}

// applyRegistryRoute publishes the registry on the shared Gateway. Every path
// goes to it — a registry's API is the whole of /v2/, and the host is its own.
func (r *KitchenReconciler) applyRegistryRoute(
	ctx context.Context,
	registry *platformRegistry,
	section *gatewayv1.SectionName,
) error {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: RegistryRouteName, Namespace: PlatformNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Labels = registryLabels()
		route.Spec.CommonRouteSpec = gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{
				Name:        SharedGatewayName,
				Namespace:   ptr.To(gatewayv1.Namespace(PlatformNamespace)),
				SectionName: section,
			}},
		}
		route.Spec.Hostnames = []gatewayv1.Hostname{gatewayv1.Hostname(registry.Host)}
		route.Spec.Rules = []gatewayv1.HTTPRouteRule{{
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(registry.Service),
						Port: ptr.To(gatewayv1.PortNumber(registry.Port)),
					},
				},
			}},
		}}
		return nil
	})
	return err
}

// seedRegistryConnection gives a fresh installation a registry to pick.
//
// It is a seed, not a fixture: it is created once, and the name of what was
// created is remembered in status. A Connection someone deletes afterwards
// stays deleted — an installation that would rather use Harbor or GHCR should
// be able to end up with only its own, and a platform that kept reinstating
// this one would make that impossible. What is kept in step, for as long as
// the seeded Connection is still there and still labelled as the platform's,
// is where it points and what it authenticates with: a base domain that moves
// or a credential that rotates would otherwise leave it quietly wrong.
//
// It returns the name of the Connection that exists, or "" when the seed has
// been deleted.
func (r *KitchenReconciler) seedRegistryConnection(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	registry *platformRegistry,
	credential *registryCredentials,
) (string, error) {
	name := RegistryConnectionName
	if status := kitchen.Status.Registry; status != nil && status.Connection != "" {
		name = status.Connection
	}
	seededBefore := kitchen.Status.Registry != nil && kitchen.Status.Registry.Connection != ""

	existing := &kitchenv1alpha1.Connection{}
	err := r.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err) && seededBefore:
		return "", nil
	case err != nil && !apierrors.IsNotFound(err):
		return "", err
	case err == nil && existing.Labels[labelManagedByKey] != labelManagedByValue:
		// Something else owns this name — a connection someone created by
		// hand before the platform ever seeded one. Leave it entirely alone
		// rather than overwriting a credential that is not the platform's.
		return "", fmt.Errorf("connection %q already exists and was not created by the platform, so it is left alone", name)
	}

	if err := r.writeRegistryCredentialSecret(ctx, registry, credential); err != nil {
		return "", err
	}

	config, err := json.Marshal(map[string]string{"url": registry.registryURL()})
	if err != nil {
		return "", err
	}
	conn := &kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: PlatformNamespace,
	}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, conn, func() error {
		if conn.Labels == nil {
			conn.Labels = map[string]string{}
		}
		conn.Labels[labelManagedByKey] = labelManagedByValue
		conn.Spec.Provider = registryProviderName
		conn.Spec.CredentialsSecretRef = kitchenv1alpha1.LocalObjectReference{
			Name: RegistryCredentialsSecretName,
		}
		conn.Spec.Config = &runtime.RawExtension{Raw: config}
		return nil
	}); err != nil {
		return "", err
	}
	return name, nil
}

// writeRegistryCredentialSecret stores the registry's credential in the shape
// every consumer of a dockerRegistry Connection already reads: a
// dockerconfigjson for the registry's host. The build reconciler mounts a copy
// of it as DOCKER_CONFIG, and the environment reconciler hands the same copy
// to the kubelet as an image pull secret.
func (r *KitchenReconciler) writeRegistryCredentialSecret(
	ctx context.Context,
	registry *platformRegistry,
	credential *registryCredentials,
) error {
	dockerConfig, err := provider.DockerConfigJSON(registry.Host, credential.Username, credential.Password)
	if err != nil {
		return err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: RegistryCredentialsSecretName, Namespace: PlatformNamespace,
	}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		// The same label the API puts on the credentials it writes, so
		// deleting this connection from the connections page takes its secret
		// with it.
		secret.Labels[labelManagedByKey] = labelManagedByValue
		if secret.CreationTimestamp.IsZero() {
			secret.Type = corev1.SecretTypeDockerConfigJson
		}
		secret.Data = map[string][]byte{corev1.DockerConfigJsonKey: dockerConfig}
		return nil
	})
	return err
}

// removeRegistry takes down what the operator published. The Connection goes
// with it — it names a registry that is no longer served, so leaving it would
// leave a registry picker entry that fails every build chosen with it — but
// only while it is still the one the platform seeded.
func (r *KitchenReconciler) removeRegistry(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) error {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: RegistryRouteName, Namespace: PlatformNamespace,
	}}
	if err := r.Delete(ctx, route); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	status := kitchen.Status.Registry
	if status == nil || status.Connection == "" {
		kitchen.Status.Registry = nil
		return nil
	}
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: status.Connection}
	err := r.Get(ctx, key, conn)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return err
	case conn.Labels[labelManagedByKey] != labelManagedByValue:
		// Someone replaced the seeded connection with one of their own under
		// the same name. Not ours to delete.
	default:
		if err := r.Delete(ctx, conn); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: RegistryCredentialsSecretName, Namespace: PlatformNamespace,
		}}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	kitchen.Status.Registry = nil
	return nil
}

func registryLabels() map[string]string {
	return platformLabels(RegistryRouteName, "registry")
}
