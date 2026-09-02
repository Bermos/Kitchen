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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/inngest"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
)

const (
	resourceClaimFinalizer = "kitchen.bermos.dev/resourceclaim-cleanup"

	// labelClaim marks the binding Secrets a claim wrote, so they can be told
	// apart from anything else in the project namespace.
	labelClaim = "kitchen.bermos.dev/claim"

	condProvisioned = "Provisioned"

	// claimRequeueDelay is how long an unbound claim waits between attempts
	// when nothing it watches has moved.
	claimRequeueDelay = 30 * time.Second
)

// ResourceClaimReconciler reconciles a ResourceClaim: it hands the claim to
// the contract registered for its type (resourceclaim_contracts.go), which
// provisions the resource and writes the binding into a Secret in the
// project namespace. What is shared between contracts — resolving the
// Connection and its capability, the Bound/Pending/Failed transitions, the
// audit and activity records, the binding Secret's removal — lives here;
// what differs lives in the contract's own file.
type ResourceClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Activity feeds the dashboard's recent-activity feed. May be nil.
	Activity *activity.Recorder
	// Databases resolves a database Provisioner for a Connection, for the
	// postgres contract. Defaults to database.Default; tests inject
	// providers pointed at httptest.
	Databases database.Factory
	// Buckets resolves an object store Provisioner for a Connection, for
	// the objectStore contract. Defaults to objectstore.Default; tests
	// inject provisioners over an in-memory store.
	Buckets objectstore.Factory
	// Inngest resolves an Inngest Provisioner for a Connection, for the
	// inngest contract. Defaults to inngest.Default; tests inject providers
	// pointed at httptest.
	Inngest inngest.Factory
	// Audit appends this reconciler's state transitions to the tamper-evident
	// log. Unlike Activity it is waited on: a transition it refuses is a
	// transition this reconciler does not make. May be nil.
	Audit *audit.Recorder
	// Records builds the signed-record store a data-class declaration's
	// envelope is kept in. Nil resolves the real ClickHouse from the
	// singleton's secret; tests inject.
	Records SignedRecordStoreFactory
	// Reader reads straight from the API server, for the volume contract's
	// look at the cluster's StorageClasses. Nil falls back to the cached
	// client, which tests run against directly.
	Reader client.Reader
}

// reader is where the uncached reads go; see Reader.
func (r *ResourceClaimReconciler) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=resourceclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=resourceclaims/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=resourceclaims/finalizers,verbs=update
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects;connections,verbs=get;list;watch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens;domains,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create

// Reconcile drives a ResourceClaim to Bound: find the contract for its type,
// resolve the Connection and require the type's capability where the type
// takes one, and hand over.
func (r *ResourceClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !claim.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, claim)
	}

	if controllerutil.AddFinalizer(claim, resourceClaimFinalizer) {
		if err := r.Update(ctx, claim); err != nil {
			return ctrl.Result{}, err
		}
	}

	project := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Spec.ProjectRef.Name}, project); err != nil {
		return r.pending(ctx, claim, "ProjectMissing", err)
	}

	claimType, contract, ok := claimContractFor(claim)
	if !ok {
		// The CRD's enum refuses a type the table does not know, so this is
		// an object written before its type was removed. It cannot bind and
		// will not start to; the message says which type is missing.
		return r.failed(ctx, claim, "TypeUnknown",
			fmt.Errorf("no contract provisions a %q claim: the type is not one of %v", claim.Spec.Type,
				kitchenv1alpha1.ClaimTypeNames()))
	}

	// A type the platform provisions itself names no Connection, and
	// everything below — capability, credentials, provisioner — is about
	// somebody else.
	var conn *kitchenv1alpha1.Connection
	if claimType.TakesConnection() {
		conn = &kitchenv1alpha1.Connection{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: claim.Connection()}, conn); err != nil {
			return r.pending(ctx, claim, "ConnectionMissing", err)
		}
		if len(conn.Status.Capabilities) == 0 {
			return r.pending(ctx, claim, "ConnectionNotValidated",
				fmt.Errorf("waiting for connection %q to be validated", conn.Name))
		}
		if !hasCapability(conn, claimType.Capability) {
			return r.failed(ctx, claim, "CapabilityMissing",
				fmt.Errorf("connection %q does not provide the %s capability", conn.Name, claimType.Capability))
		}
	}

	return contract.reconcile(ctx, r, claim, project, conn)
}

// bind records a claim reaching Bound: the audit entry first — a transition
// the log refuses is a transition this reconciler does not make — then the
// status, then the activity feed. Both claim types come through here, so the
// two of them cannot drift into recording their bindings differently.
//
// `provider` names who declared the claim's data provenance — the
// Connection's provider kind, or "kitchen" for the platform's own identity
// provider — and rides into the signed declaration record a first bind mints.
func (r *ResourceClaimReconciler) bind(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provider string,
	reason string,
	details map[string]any,
) error {
	wasBound := claim.Status.Phase == kitchenv1alpha1.ClaimBound
	if !wasBound {
		if err := r.Audit.Record(ctx, audit.Transition{
			Object:     claim,
			Kind:       audit.KindResourceClaim,
			Controller: actorResourceClaimController,
			From:       string(claim.Status.Phase),
			To:         string(kitchenv1alpha1.ClaimBound),
			Project:    claim.Spec.ProjectRef.Name,
			Reason:     reason,
			Details:    details,
		}); err != nil {
			return err
		}
	}
	claim.Status.Phase = kitchenv1alpha1.ClaimBound
	// A claim that binds to a mount rather than to a Secret has no secret
	// to name; the contract's own sentence stands in for it.
	bound := reason
	if claim.Status.SecretName != "" {
		bound = fmt.Sprintf("binding written to secret %s", claim.Status.SecretName)
	}
	setClaimCondition(claim, condReady, metav1.ConditionTrue, "Bound", bound)
	if err := r.Status().Update(ctx, claim); err != nil {
		return err
	}
	if !wasBound {
		r.Activity.Record(ctx, clickhouse.Event{
			Type:    clickhouse.EventClaimBound,
			Project: claim.Spec.ProjectRef.Name,
			Claim:   claim.Name,
			Message: reason,
		})
		// The signed record, once per binding: the provider's declaration,
		// attested under the platform's key and kept in the store. A claim
		// whose provider declared nothing mints nothing — the absence is on
		// the status and in the inventory.
		r.recordDataClassDeclaration(ctx, claim, "", claim.Status.DataProvenance, provider)
	}
	return nil
}

// finalize cleans up what the claim put into the world: the contract takes
// back what it provisioned — a database under deletionPolicy Delete, an
// OAuth client always — and then the binding Secret goes, because it exists
// for an application the platform runs and not for the data.
//
// A claim whose type has no contract any more is still let go: the Secret
// and the finalizer are the platform's, and a claim that cannot be deleted
// blocks the project's teardown behind it.
func (r *ResourceClaimReconciler) finalize(ctx context.Context, claim *kitchenv1alpha1.ResourceClaim) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(claim, resourceClaimFinalizer) {
		return ctrl.Result{}, nil
	}

	if _, contract, ok := claimContractFor(claim); ok {
		if err := contract.finalize(ctx, r, claim); err != nil {
			return ctrl.Result{}, err
		}
	} else {
		logf.FromContext(ctx).Info("finalizing a claim of a type nothing provisions any more",
			"claim", claim.Name, "type", claim.Spec.Type)
	}

	if claim.Status.SecretName != "" {
		appNS := appNamespace(claim.Spec.ProjectRef.Name)
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: claim.Status.SecretName, Namespace: appNS}}
		if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	}

	if err := r.Audit.Record(ctx, audit.Transition{
		Object:     claim,
		Kind:       audit.KindResourceClaim,
		Operation:  clickhouse.AuditDelete,
		Controller: actorResourceClaimController,
		From:       string(claim.Status.Phase),
		Project:    claim.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf("claim %s removed under deletion policy %s",
			claim.Name, claim.Spec.DeletionPolicy),
		Details: map[string]any{
			"type":           claim.Spec.Type,
			"deletionPolicy": string(claim.Spec.DeletionPolicy),
			"instance":       claim.Status.InstanceID,
		},
	}); err != nil {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(claim, resourceClaimFinalizer)
	return ctrl.Result{}, r.Update(ctx, claim)
}

// pending records a claim that cannot bind yet but plausibly will: the
// Connection is missing, not validated, or its credentials are not there.
func (r *ResourceClaimReconciler) pending(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	reason string,
	cause error,
) (ctrl.Result, error) {
	claim.Status.Phase = kitchenv1alpha1.ClaimPending
	setClaimCondition(claim, condReady, metav1.ConditionFalse, reason, cause.Error())
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: claimRequeueDelay}, nil
}

// failed records a claim the provider (or its configuration) refused, with
// the provider's own words in the condition.
func (r *ResourceClaimReconciler) failed(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	reason string,
	cause error,
) (ctrl.Result, error) {
	wasFailed := claim.Status.Phase == kitchenv1alpha1.ClaimFailed
	if !wasFailed {
		if err := r.Audit.Record(ctx, audit.Transition{
			Object:     claim,
			Kind:       audit.KindResourceClaim,
			Controller: actorResourceClaimController,
			From:       string(claim.Status.Phase),
			To:         string(kitchenv1alpha1.ClaimFailed),
			Project:    claim.Spec.ProjectRef.Name,
			Reason:     cause.Error(),
			Details:    map[string]any{"type": claim.Spec.Type, "reason": reason},
		}); err != nil {
			return ctrl.Result{}, err
		}
	}
	claim.Status.Phase = kitchenv1alpha1.ClaimFailed
	setClaimCondition(claim, condReady, metav1.ConditionFalse, reason, cause.Error())
	if reason == "ProvisionFailed" {
		setClaimCondition(claim, condProvisioned, metav1.ConditionFalse, reason, cause.Error())
	}
	if err := r.Status().Update(ctx, claim); err != nil {
		return ctrl.Result{}, err
	}
	if !wasFailed {
		r.Activity.Record(ctx, clickhouse.Event{
			Type:    clickhouse.EventClaimFailed,
			Project: claim.Spec.ProjectRef.Name,
			Claim:   claim.Name,
			Message: fmt.Sprintf("claim %s failed: %s", claim.Name, cause.Error()),
		})
	}
	return ctrl.Result{RequeueAfter: claimRequeueDelay}, nil
}

func setClaimCondition(claim *kitchenv1alpha1.ResourceClaim, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&claim.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: claim.Generation,
	})
}

func hasCapability(conn *kitchenv1alpha1.Connection, capability kitchenv1alpha1.Capability) bool {
	for _, c := range conn.Status.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// claimSecretName is the shared binding Secret's name in the project
// namespace — the one every contract writes its binding to.
func claimSecretName(claim string) string {
	return claim + "-binding"
}

// mapEnvironmentToClaims enqueues every claim of an Environment's project.
//
// Environments appearing and disappearing is what drives both halves of this
// controller: a preview's database branch, and an OAuth client's redirect
// list. It used to be narrowed to previews, because branches are a preview
// thing; the redirect list is not — a production environment's URL is on it
// too, and a claim that only heard about previews would register the wrong
// list on the first deployment and never correct it.
func (r *ResourceClaimReconciler) mapEnvironmentToClaims(ctx context.Context, obj client.Object) []ctrl.Request {
	env, ok := obj.(*kitchenv1alpha1.Environment)
	if !ok {
		return nil
	}
	return r.claimsOfProject(ctx, env.Namespace, env.Spec.ProjectRef.Name)
}

// mapDomainToClaims enqueues the claims of the project a custom Domain
// belongs to, so that a domain becoming verified reaches the redirect list of
// an oidcClient claim. The Domain names an Environment rather than a project,
// so the Environment is what says whose it is.
func (r *ResourceClaimReconciler) mapDomainToClaims(ctx context.Context, obj client.Object) []ctrl.Request {
	domain, ok := obj.(*kitchenv1alpha1.Domain)
	if !ok {
		return nil
	}
	env := &kitchenv1alpha1.Environment{}
	key := types.NamespacedName{Namespace: domain.Namespace, Name: domain.Spec.EnvironmentRef.Name}
	if err := r.Get(ctx, key, env); err != nil {
		return nil
	}
	return r.claimsOfProject(ctx, domain.Namespace, env.Spec.ProjectRef.Name)
}

// mapConnectionToClaims enqueues every claim referencing a Connection, so a
// claim waiting on validation binds as soon as the capability appears.
func (r *ResourceClaimReconciler) mapConnectionToClaims(ctx context.Context, obj client.Object) []ctrl.Request {
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(obj.GetNamespace())); err != nil {
		logf.FromContext(ctx).Error(err, "could not list claims after a connection change")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(claims.Items))
	for i := range claims.Items {
		if claims.Items[i].Connection() != obj.GetName() {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: claims.Items[i].Namespace, Name: claims.Items[i].Name,
		}})
	}
	return requests
}

func (r *ResourceClaimReconciler) claimsOfProject(ctx context.Context, namespace, project string) []ctrl.Request {
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(namespace)); err != nil {
		logf.FromContext(ctx).Error(err, "could not list claims after an environment change")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(claims.Items))
	for i := range claims.Items {
		if claims.Items[i].Spec.ProjectRef.Name != project {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: claims.Items[i].Namespace, Name: claims.Items[i].Name,
		}})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.ResourceClaim{}).
		Watches(&kitchenv1alpha1.Environment{}, handler.EnqueueRequestsFromMapFunc(r.mapEnvironmentToClaims)).
		Watches(&kitchenv1alpha1.Connection{}, handler.EnqueueRequestsFromMapFunc(r.mapConnectionToClaims)).
		Watches(&kitchenv1alpha1.Domain{}, handler.EnqueueRequestsFromMapFunc(r.mapDomainToClaims)).
		// A volume claim's PVC binding is what records the PersistentVolume
		// on its status — and, under Retain, what makes that volume outlive
		// the namespace.
		Watches(&corev1.PersistentVolumeClaim{}, handler.EnqueueRequestsFromMapFunc(r.mapVolumeToClaim)).
		Named("resourceclaim").
		Complete(r)
}
