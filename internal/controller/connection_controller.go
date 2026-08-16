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
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider"
)

const (
	condConnected        = "Connected"
	condCredentialsValid = "CredentialsValid"

	reasonCredentialsMissing     = "CredentialsMissing"
	reasonCredentialsMalformed   = "CredentialsMalformed"
	reasonCredentialsRejected    = "CredentialsRejected"
	reasonProviderNotImplemented = "ProviderNotImplemented"
	reasonProviderUnreachable    = "ProviderUnreachable"

	// connectionRecheckInterval is how often a settled credential is
	// re-validated: often enough that a token revoked at the provider is a
	// red condition before it is a failed build, rarely enough to be polite
	// to the provider's API.
	connectionRecheckInterval = 10 * time.Minute

	// connectionRetryInterval retries a probe that could not run — the
	// provider did not answer, or the credentials secret is missing. An
	// outage should be seen ending sooner than the periodic recheck.
	connectionRetryInterval = time.Minute
)

// ConnectionReconciler validates a Connection's credential against the live
// provider and keeps status.capabilities and the Connected/CredentialsValid
// conditions current — so a bad credential is a red condition on the
// connections page rather than a failed build later. The credentials Secret
// is watched, so a rotated credential is revalidated immediately; the
// credential itself never appears in status, events or logs.
type ConnectionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Probes resolves the credential probe for a Connection. Defaults to
	// provider.Default; tests inject fakes.
	Probes provider.Factory
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=connections/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile probes the provider and writes the verdict into status.
func (r *ConnectionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	conn := &kitchenv1alpha1.Connection{}
	if err := r.Get(ctx, req.NamespacedName, conn); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !conn.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	setCond := func(condType string, status metav1.ConditionStatus, reason, message string) {
		meta.SetStatusCondition(&conn.Status.Conditions, metav1.Condition{
			Type:               condType,
			Status:             status,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: conn.Generation,
		})
	}

	// Capabilities are what the platform can do through this provider, and
	// nothing more: a provider without an implementation reports none, so
	// capability matching never selects a connection nothing can use.
	conn.Status.Capabilities = provider.Capabilities(conn.Spec.Provider)

	requeueAfter := r.probe(ctx, conn, setCond)

	if err := r.Status().Update(ctx, conn); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// probe runs the provider probe and sets both conditions. Connected is
// reachability, CredentialsValid the provider's ruling on the credential —
// kept distinct so a registry that is down never reads as a wrong password.
// The returned interval is when to look again; zero for a provider nothing
// implements, where only a new operator changes the answer.
func (r *ConnectionReconciler) probe(
	ctx context.Context,
	conn *kitchenv1alpha1.Connection,
	setCond func(string, metav1.ConditionStatus, string, string),
) time.Duration {
	creds := &corev1.Secret{}
	key := types.NamespacedName{Namespace: conn.Namespace, Name: conn.Spec.CredentialsSecretRef.Name}
	if err := r.Get(ctx, key, creds); err != nil {
		setCond(condConnected, metav1.ConditionUnknown, reasonCredentialsMissing,
			"not probed: "+err.Error())
		setCond(condCredentialsValid, metav1.ConditionFalse, reasonCredentialsMissing, err.Error())
		return connectionRetryInterval
	}

	factory := r.Probes
	if factory == nil {
		factory = provider.Default
	}
	credProbe, err := factory(conn, creds)
	if errors.Is(err, provider.ErrNotImplemented) {
		// gitlab and gitea pass admission but nothing in the platform can use
		// them yet: an honest Unknown beats a fake green.
		message := fmt.Sprintf("the platform has no %s implementation yet, so the credential cannot be checked",
			conn.Spec.Provider)
		setCond(condConnected, metav1.ConditionUnknown, reasonProviderNotImplemented, message)
		setCond(condCredentialsValid, metav1.ConditionUnknown, reasonProviderNotImplemented, message)
		return 0
	}
	if err != nil {
		// The secret exists but does not hold what the provider needs.
		// Rotating it is the fix, and the secret watch picks that up.
		setCond(condConnected, metav1.ConditionUnknown, reasonCredentialsMalformed,
			"not probed: "+err.Error())
		setCond(condCredentialsValid, metav1.ConditionFalse, reasonCredentialsMalformed, err.Error())
		return connectionRecheckInterval
	}

	result := credProbe.Probe(ctx)
	if !result.Reachable {
		setCond(condConnected, metav1.ConditionFalse, reasonProviderUnreachable, result.Message)
		setCond(condCredentialsValid, metav1.ConditionUnknown, reasonProviderUnreachable,
			"the provider could not be reached, so the credential was not checked")
		return connectionRetryInterval
	}
	setCond(condConnected, metav1.ConditionTrue, "ProviderReachable", "the provider answered")
	switch {
	case !result.CredentialChecked:
		setCond(condCredentialsValid, metav1.ConditionUnknown, "ProviderErrored", result.Message)
		return connectionRetryInterval
	case !result.CredentialValid:
		setCond(condCredentialsValid, metav1.ConditionFalse, reasonCredentialsRejected, result.Message)
		return connectionRecheckInterval
	default:
		// A credential the provider accepted can still be short of a
		// permission the platform wants. That is not a failed condition — the
		// platform works — so it rides along in the message rather than
		// turning the connection red.
		message := result.Message
		if len(result.Warnings) > 0 {
			message += " — " + strings.Join(result.Warnings, "; ")
		}
		setCond(condCredentialsValid, metav1.ConditionTrue, "Validated", message)
		return connectionRecheckInterval
	}
}

// mapSecretToConnections enqueues every Connection whose credentials live in
// the changed Secret, so a rotated credential is revalidated immediately
// rather than at the next periodic recheck.
func (r *ConnectionReconciler) mapSecretToConnections(ctx context.Context, obj client.Object) []ctrl.Request {
	connections := &kitchenv1alpha1.ConnectionList{}
	if err := r.List(ctx, connections, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	requests := make([]ctrl.Request, 0, len(connections.Items))
	for i := range connections.Items {
		if connections.Items[i].Spec.CredentialsSecretRef.Name != obj.GetName() {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: connections.Items[i].Namespace,
			Name:      connections.Items[i].Name,
		}})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager. The generation
// predicate keeps the reconciler's own status writes from retriggering a
// probe; the periodic recheck comes from RequeueAfter, not from watch events.
func (r *ConnectionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Connection{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.mapSecretToConnections)).
		Named("connection").
		Complete(r)
}
