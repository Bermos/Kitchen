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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

// reconcileObjectStore seeds the Connection that points at the bundled
// object store, so a fresh installation can give a project a bucket without
// anyone opening an account at a cloud.
//
// The chart runs the store, its Service and its volume; this owns the one
// half the chart cannot, the Connection, because its credential is a Secret
// the platform writes and never reads back. Unlike the registry there is no
// route: an application runs in the cluster and reaches the store at its
// Service address, and nothing outside the cluster needs to.
func (r *KitchenReconciler) reconcileObjectStore(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	store := resolveObjectStore(kitchen)
	if store == nil {
		// Switched off, deliberately or by default: an installation that
		// wants a bucket brings a store of its own, which is an ordinary s3
		// Connection someone creates. What was seeded comes down with it — a
		// Connection naming a store the chart has stopped rendering fails
		// every claim chosen with it.
		if err := r.removeObjectStore(ctx, kitchen); err != nil {
			setCond(condObjectStoreReady, metav1.ConditionFalse, "CleanupFailed", err.Error())
			return false
		}
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condObjectStoreReady)
		return true
	}

	credential, err := r.objectStoreCredential(ctx, store)
	if err != nil {
		setCond(condObjectStoreReady, metav1.ConditionFalse, "CredentialUnavailable", err.Error())
		return false
	}
	seeded, err := r.seedObjectStoreConnection(ctx, kitchen, store, credential)
	if err != nil {
		setCond(condObjectStoreReady, metav1.ConditionFalse, "ConnectionFailed", err.Error())
		return false
	}

	kitchen.Status.ObjectStore = &kitchenv1alpha1.ObjectStoreStatus{
		Endpoint:   store.endpoint(),
		Connection: seeded,
	}
	message := fmt.Sprintf("buckets are provisioned at %s", store.endpoint())
	if seeded != "" {
		message += fmt.Sprintf(", through the %q connection", seeded)
	} else {
		message += "; the connection that pointed at it was deleted and is not recreated"
	}
	setCond(condObjectStoreReady, metav1.ConditionTrue, "ObjectStoreSeeded", message)
	return true
}

// objectStoreCredentials is the store's root access key pair, as the chart
// wrote it.
type objectStoreCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// objectStoreCredential reads the credential the chart generated. It is the
// store's root: what mints every claim's own scoped credential, and never
// what an application is handed.
func (r *KitchenReconciler) objectStoreCredential(
	ctx context.Context,
	store *platformObjectStore,
) (*objectStoreCredentials, error) {
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: store.SecretName}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	credential := &objectStoreCredentials{
		AccessKeyID:     string(secret.Data[objectstore.CredentialKeyAccessKeyID]),
		SecretAccessKey: string(secret.Data[objectstore.CredentialKeySecretAccessKey]),
	}
	if credential.AccessKeyID == "" || credential.SecretAccessKey == "" {
		return nil, fmt.Errorf("secret %q needs both %q and %q: the store's root credential is what mints "+
			"every bucket's own", store.SecretName, objectstore.CredentialKeyAccessKeyID,
			objectstore.CredentialKeySecretAccessKey)
	}
	return credential, nil
}

// seedObjectStoreConnection gives a fresh installation a store to claim
// through, on exactly the registry's terms: a seed, not a fixture. Created
// once, the name remembered in status, a deletion left alone; kept in step
// — endpoint and credential — for as long as it is there and still
// labelled as the platform's. A Connection of the name that the platform
// did not create is refused rather than overwritten.
func (r *KitchenReconciler) seedObjectStoreConnection(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	store *platformObjectStore,
	credential *objectStoreCredentials,
) (string, error) {
	name := ObjectStoreConnectionName
	if status := kitchen.Status.ObjectStore; status != nil && status.Connection != "" {
		name = status.Connection
	}
	seededBefore := kitchen.Status.ObjectStore != nil && kitchen.Status.ObjectStore.Connection != ""

	existing := &kitchenv1alpha1.Connection{}
	err := r.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: name}, existing)
	switch {
	case apierrors.IsNotFound(err) && seededBefore:
		return "", nil
	case err != nil && !apierrors.IsNotFound(err):
		return "", err
	case err == nil && existing.Labels[labelManagedByKey] != labelManagedByValue:
		return "", fmt.Errorf("connection %q already exists and was not created by the platform, so it is left alone", name)
	}

	if err := r.writeObjectStoreCredentialSecret(ctx, credential); err != nil {
		return "", err
	}

	config, err := json.Marshal(objectstore.Config{
		Endpoint:       store.endpoint(),
		Region:         store.Region,
		ForcePathStyle: true,
		InCluster:      true,
	})
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
		conn.Spec.Provider = objectstore.ProviderS3
		conn.Spec.CredentialsSecretRef = kitchenv1alpha1.CredentialsReference{
			Name: ObjectStoreCredentialsSecretName,
		}
		conn.Spec.Config = &runtime.RawExtension{Raw: config}
		return nil
	}); err != nil {
		return "", err
	}
	return name, nil
}

// writeObjectStoreCredentialSecret stores the root credential in the shape
// every s3 Connection's Secret has — the two keys the REST API writes — so
// that nothing downstream treats the seeded connection as a special case.
func (r *KitchenReconciler) writeObjectStoreCredentialSecret(
	ctx context.Context,
	credential *objectStoreCredentials,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: ObjectStoreCredentialsSecretName, Namespace: PlatformNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		if secret.Labels == nil {
			secret.Labels = map[string]string{}
		}
		secret.Labels[labelManagedByKey] = labelManagedByValue
		secret.Type = corev1.SecretTypeOpaque
		secret.Data = map[string][]byte{
			objectstore.CredentialKeyAccessKeyID:     []byte(credential.AccessKeyID),
			objectstore.CredentialKeySecretAccessKey: []byte(credential.SecretAccessKey),
		}
		return nil
	})
	return err
}

// removeObjectStore takes the seeded Connection and its credential down —
// only while it is still the one the platform seeded.
func (r *KitchenReconciler) removeObjectStore(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) error {
	status := kitchen.Status.ObjectStore
	if status == nil || status.Connection == "" {
		kitchen.Status.ObjectStore = nil
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
		// Replaced under the same name by one somebody made. Not ours.
	default:
		if err := r.Delete(ctx, conn); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: ObjectStoreCredentialsSecretName, Namespace: PlatformNamespace,
		}}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	kitchen.Status.ObjectStore = nil
	return nil
}
