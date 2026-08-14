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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const condConnected = "Connected"

// providerCapabilities is what each first-party provider implements. Everything
// else in the operator matches on the capability, never the provider name —
// this table is the one place the mapping lives.
var providerCapabilities = map[string][]kitchenv1alpha1.Capability{
	"github":         {kitchenv1alpha1.CapabilityGitSource, kitchenv1alpha1.CapabilityStatusChecks},
	"gitlab":         {kitchenv1alpha1.CapabilityGitSource, kitchenv1alpha1.CapabilityStatusChecks},
	"gitea":          {kitchenv1alpha1.CapabilityGitSource, kitchenv1alpha1.CapabilityStatusChecks},
	"dockerRegistry": {kitchenv1alpha1.CapabilityImageStore},
	"neon":           {kitchenv1alpha1.CapabilityDatabase},
	"infisical":      {kitchenv1alpha1.CapabilitySecretStore},
}

// ConnectionReconciler reconciles a Connection: it publishes the provider's
// capabilities (which is what Projects match on) and verifies the credentials
// secret exists.
type ConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections/finalizers,verbs=update

// Reconcile publishes what a Connection can do and whether its credentials
// are in place. It does not probe the provider's API — that is the plugin's
// job once plugins do more than declare capabilities.
func (r *ConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	conn := &kitchenv1alpha1.Connection{}
	if err := r.Get(ctx, req.NamespacedName, conn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	setCond := func(status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
			Type:               condConnected,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: conn.Generation,
		})
	}

	capabilities, known := providerCapabilities[conn.Spec.Provider]
	conn.Status.Capabilities = capabilities
	if !known {
		setCond(metav1.ConditionFalse, "ProviderUnknown",
			fmt.Sprintf("no plugin implements provider %q", conn.Spec.Provider))
		return ctrl.Result{}, r.Status().Update(ctx, conn)
	}

	creds := &corev1.Secret{}
	key := types.NamespacedName{Namespace: conn.Namespace, Name: conn.Spec.CredentialsSecretRef.Name}
	if err := r.Get(ctx, key, creds); err != nil {
		setCond(metav1.ConditionFalse, "CredentialsMissing", err.Error())
		if err := r.Status().Update(ctx, conn); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	setCond(metav1.ConditionTrue, "CredentialsPresent",
		fmt.Sprintf("credentials secret %q is in place (not probed against the provider)", key.Name))
	if err := r.Status().Update(ctx, conn); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled connection", "provider", conn.Spec.Provider, "capabilities", capabilities)
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Connection{}).
		Named("connection").
		Complete(r)
}
