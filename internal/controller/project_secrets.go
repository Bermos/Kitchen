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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// ProjectSecretsAlias is the secret name spec.env[].secretRef uses to mean
	// "the project's synced secrets, whichever environment this is". The
	// Environment reconciler resolves it to one of the names below, which is
	// how the same env var reads production values in production and preview
	// values in previews.
	ProjectSecretsAlias = "kitchen-secrets"

	// defaultInfisicalHost is where secrets sync from when the Connection's
	// config names no other instance: Infisical Cloud.
	defaultInfisicalHost = "https://app.infisical.com"

	condSecretStoreConnected = "SecretStoreConnected"
	condSecretsSynced        = "SecretsSynced"
)

// syncedSecretName is where a project's secrets land in its app namespace for
// one environment type.
func syncedSecretName(t kitchenv1alpha1.EnvironmentType) string {
	return fmt.Sprintf("%s-%s", ProjectSecretsAlias, t)
}

// infisicalSecretGVK addresses Infisical's InfisicalSecret kind. Like
// cert-manager's kinds it is handled as unstructured rather than through the
// provider's Go types: the operator writes one small spec, and importing the
// Infisical operator would tie the build to its release cadence.
func infisicalSecretGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "secrets.infisical.com", Version: "v1alpha1", Kind: "InfisicalSecret"}
}

// infisicalConnectionConfig is the infisical provider's Connection config.
type infisicalConnectionConfig struct {
	// Host is the base URL of the Infisical instance. Defaults to Infisical
	// Cloud; a self-hosted instance keeps secrets on your infrastructure.
	Host string `json:"host,omitempty"`
}

// reconcileSecrets materializes the project's secret sync: one InfisicalSecret
// CR per environment type in the app namespace, each pointed at the store
// environment for that type. The Infisical operator does the actual syncing
// (and re-syncing — rotation propagates on its interval with nothing
// redeployed); what Kitchen owns is that the CRs exist, in the right
// namespace, with the right scope.
//
// It reports through two conditions: SecretStoreConnected for the Connection,
// SecretsSynced for the CRs. Both disappear when the project defines no
// secrets, along with any CRs a previous spec created.
func (r *ProjectReconciler) reconcileSecrets(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	appNS := appNamespace(project.Name)

	if project.Spec.Secrets == nil {
		// Opted out (or never in): tear down what an earlier spec created.
		// The synced Secrets go with the CRs — they are created with
		// creationPolicy Owner.
		for _, t := range []kitchenv1alpha1.EnvironmentType{kitchenv1alpha1.EnvironmentProduction, kitchenv1alpha1.EnvironmentPreview} {
			if !r.deleteInfisicalSecret(ctx, appNS, syncedSecretName(t)) {
				return false
			}
		}
		meta.RemoveStatusCondition(&project.Status.Conditions, condSecretStoreConnected)
		meta.RemoveStatusCondition(&project.Status.Conditions, condSecretsSynced)
		return true
	}

	secrets := project.Spec.Secrets
	conn, err := r.checkConnection(ctx, project, secrets.ConnectionRef.Name,
		kitchenv1alpha1.CapabilitySecretStore, condSecretStoreConnected, setCond)
	if err != nil {
		return false
	}

	// The capability is the contract, but materializing the sync means
	// speaking a concrete CRD — and infisical is the one provider that
	// implements secretStore today.
	if conn.Spec.Provider != "infisical" {
		setCond(condSecretsSynced, metav1.ConditionFalse, "ProviderUnsupported",
			fmt.Sprintf("provider %q offers secretStore but the operator only knows how to drive infisical", conn.Spec.Provider))
		return false
	}

	cfg := infisicalConnectionConfig{}
	if conn.Spec.Config != nil && len(conn.Spec.Config.Raw) > 0 {
		if err := json.Unmarshal(conn.Spec.Config.Raw, &cfg); err != nil {
			setCond(condSecretsSynced, metav1.ConditionFalse, "ConnectionConfigInvalid",
				fmt.Sprintf("connection %q config: %v", conn.Name, err))
			return false
		}
	}
	host := cfg.Host
	if host == "" {
		host = defaultInfisicalHost
	}
	hostAPI := strings.TrimSuffix(host, "/") + "/api"

	// Store environments per Kitchen environment type. The CRD defaults these,
	// but objects written before the fields existed have them empty.
	envSlugs := map[kitchenv1alpha1.EnvironmentType]string{
		kitchenv1alpha1.EnvironmentProduction: secrets.ProductionEnv,
		kitchenv1alpha1.EnvironmentPreview:    secrets.PreviewEnv,
	}
	if envSlugs[kitchenv1alpha1.EnvironmentProduction] == "" {
		envSlugs[kitchenv1alpha1.EnvironmentProduction] = "prod"
	}
	if envSlugs[kitchenv1alpha1.EnvironmentPreview] == "" {
		envSlugs[kitchenv1alpha1.EnvironmentPreview] = "staging"
	}
	secretsPath := secrets.SecretsPath
	if secretsPath == "" {
		secretsPath = "/"
	}

	envTypes := []kitchenv1alpha1.EnvironmentType{kitchenv1alpha1.EnvironmentProduction}
	if project.Spec.Previews.Enabled {
		envTypes = append(envTypes, kitchenv1alpha1.EnvironmentPreview)
	} else if !r.deleteInfisicalSecret(ctx, appNS, syncedSecretName(kitchenv1alpha1.EnvironmentPreview)) {
		return false
	}

	synced := make([]string, 0, len(envTypes))
	for _, t := range envTypes {
		name := syncedSecretName(t)
		if err := r.applyInfisicalSecret(ctx, project, conn, appNS, name, hostAPI, secrets.ProjectSlug, envSlugs[t], secretsPath); err != nil {
			if meta.IsNoMatchError(err) {
				setCond(condSecretsSynced, metav1.ConditionFalse, "SecretStoreOperatorUnavailable",
					"waiting for the secrets.infisical.com API to be served — is the Infisical secrets "+
						"operator installed? The chart ships it (infisical-operator.enabled).")
				return false
			}
			setCond(condSecretsSynced, metav1.ConditionFalse, "SyncNotApplied", err.Error())
			return false
		}
		synced = append(synced, fmt.Sprintf("%s (env %s)", name, envSlugs[t]))
	}

	setCond(condSecretsSynced, metav1.ConditionTrue, "Applied",
		fmt.Sprintf("syncing %s from project %q at %s", strings.Join(synced, ", "), secrets.ProjectSlug, host))
	return true
}

// applyInfisicalSecret writes one InfisicalSecret CR: sync <projectSlug>/<envSlug>
// at <secretsPath> into the k8s Secret <name> in the app namespace. The
// machine identity is referenced where it lives — the Connection's credentials
// secret in the platform namespace — rather than copied around.
func (r *ProjectReconciler) applyInfisicalSecret(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	conn *kitchenv1alpha1.Connection,
	appNS, name, hostAPI, projectSlug, envSlug, secretsPath string,
) error {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(infisicalSecretGVK())
	obj.SetName(name)
	obj.SetNamespace(appNS)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		obj.SetLabels(map[string]string{
			labelProject:      project.Name,
			labelManagedByKey: labelManagedByValue,
		})
		return unstructured.SetNestedMap(obj.Object, map[string]any{
			"hostAPI": hostAPI,
			"authentication": map[string]any{
				"universalAuth": map[string]any{
					"credentialsRef": map[string]any{
						"secretName":      conn.Spec.CredentialsSecretRef.Name,
						"secretNamespace": conn.Namespace,
					},
					"secretsScope": map[string]any{
						"projectSlug": projectSlug,
						"envSlug":     envSlug,
						"secretsPath": secretsPath,
						"recursive":   true,
					},
				},
			},
			"managedKubeSecretReferences": []any{map[string]any{
				"secretName":      name,
				"secretNamespace": appNS,
				// Owner: the synced Secret is cleaned up with its CR when the
				// project opts out again.
				"creationPolicy": "Owner",
			}},
		}, "spec")
	})
	return err
}

// deleteInfisicalSecret removes one sync CR, tolerating both its absence and
// the whole CRD's (a platform that never installed the Infisical operator has
// nothing to clean up). Returns false only on a real error.
func (r *ProjectReconciler) deleteInfisicalSecret(ctx context.Context, appNS, name string) bool {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(infisicalSecretGVK())
	obj.SetName(name)
	obj.SetNamespace(appNS)
	err := r.Delete(ctx, obj)
	return err == nil || apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}
