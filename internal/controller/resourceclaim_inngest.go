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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/inngest"
)

// The inngest half of the ResourceClaim reconciler: durable background work
// from Inngest, through a backgroundJobs-capable Connection holding an
// Inngest API key. It is the third contract and it is deliberately the
// postgres one's shape — a Connection matched on a capability, a
// provisioner built from it, typed Requirements off the claim's own slice of
// spec.config, a binding Secret in the application namespace, and a branch
// per preview Environment.
//
// What differs is what "provision" does. There is no app to create: an
// Inngest app comes into existence the first time a worker connects with
// its ID, and syncs its functions on every connection. And the management
// API mints no keys. So provisioning *reads* — the environment's signing key
// and event key — and the claim's own contribution is the branch environment
// per preview, which is what keeps one pull request's events from
// triggering another's functions. The keys are re-read on every reconcile,
// so a key rotated in the Inngest dashboard reaches the binding without
// anybody touching the claim.
//
// Two things the claim reports rather than provisions, because both are
// otherwise invisible until they bite. Whether a worker has actually
// connected as the app the claim names (AppConnected): the claim binds
// before one has, since the app is the application's to bring. And how many
// environments read the binding (ConnectWorkers): every running pod holding
// the connect worker is one of the account's concurrent connections, and
// Inngest caps those per plan — 3 on the free plan, 20 on paid
// (https://www.inngest.com/docs/setup/connect) — without exposing the cap
// through the API, so the platform can count but not check.

const (
	// inngestTokenKey is the key in an inngest Connection's credentials
	// secret holding the API key.
	inngestTokenKey = "token"

	// condAppConnected says whether a worker has connected to Inngest as
	// the claim's app. It is separate from Ready on purpose: the binding is
	// correct before any worker has used it, and the condition is how a
	// developer finds out that the process holding the worker never
	// started, or started with another app ID.
	condAppConnected = "AppConnected"

	// condConnectWorkers carries the count of environments reading the
	// binding, against the account's connection cap the platform cannot
	// read.
	condConnectWorkers = "ConnectWorkers"
)

// inngestContract is the claimContract for type inngest.
type inngestContract struct{}

func (inngestContract) reconcile(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
	conn *kitchenv1alpha1.Connection,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	provisioner, err := r.inngestProvisionerFor(ctx, conn)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			return r.pending(ctx, claim, "CredentialsMissing", err)
		case errors.Is(err, inngest.ErrUnsupportedProvider):
			return r.failed(ctx, claim, "ProviderUnsupported", err)
		default:
			return r.failed(ctx, claim, "ProviderError", err)
		}
	}

	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	cfg := claim.Inngest()
	instance, err := provisioner.Provision(ctx, inngest.Requirements{
		App:         cfg.App,
		Environment: cfg.Environment,
		Mode:        cfg.Mode,
	})
	switch {
	case errors.Is(err, inngest.ErrNotReady):
		return r.pending(ctx, claim, "Provisioning", err)
	case errors.Is(err, inngest.ErrUnsatisfiable):
		// Nothing was bound and retrying without somebody acting — creating
		// an event key in the dashboard, dropping the mode — refuses again.
		// The message is the whole answer, and the requeue is what picks
		// the binding up once somebody has.
		return r.failed(ctx, claim, "RequirementsUnsatisfiable", err)
	case err != nil:
		return r.failed(ctx, claim, "ProvisionFailed", err)
	}
	secretName := claimSecretName(claim.Name)
	if err := r.writeBindingSecret(ctx, claim, appNS, secretName, instance.Binding.SecretData()); err != nil {
		return ctrl.Result{}, err
	}
	claim.Status.InstanceID = instance.ID
	claim.Status.SecretName = secretName
	// The production binding selects the production event stream: what the
	// application sends there is production data, on the platform's own
	// word. Inngest reports no placement, and the status says so by saying
	// nothing.
	claim.Status.DataProvenance = string(database.ProvenanceProduction)
	claim.Status.Residency = ""
	setClaimCondition(claim, condProvisioned, metav1.ConditionTrue, "KeysRead",
		fmt.Sprintf("binding read for app %s in Inngest environment %s", instance.ID, instance.Environment))

	claimType, _ := claim.Type()
	mode := declare(claim, claimType, conn.Spec.Provider)
	branchErr := r.reconcileBranches(ctx, claim, project.Name, inngestBrancher(provisioner), appNS,
		conn.Spec.Provider, mode.Isolated())

	r.reportInngestApp(ctx, claim, provisioner, instance.Environment, cfg.App)
	reportConnectWorkers(claim)

	reason := fmt.Sprintf("claim %s bound: %s via %s", claim.Name, claim.Spec.Type, conn.Name)
	if err := r.bind(ctx, claim, conn.Spec.Provider, reason, map[string]any{
		"type":           claim.Spec.Type,
		"connection":     conn.Name,
		"secret":         claim.Status.SecretName,
		"app":            cfg.App,
		"environment":    instance.Environment,
		"dataProvenance": claim.Status.DataProvenance,
		"previewMode":    claim.Status.PreviewMode,
	}); err != nil {
		return ctrl.Result{}, err
	}
	if branchErr != nil {
		return ctrl.Result{RequeueAfter: claimRequeueDelay}, nil
	}
	log.Info("reconciled resource claim", "claim", claim.Name, "secret", claim.Status.SecretName)
	return ctrl.Result{}, nil
}

// finalize archives the preview environments and releases the Environments
// they held. Nothing else is the platform's to take back: the keys are the
// account's, and the app record is the application's — Inngest keeps it
// until somebody archives it in the dashboard, which is said where the
// claim is deleted.
func (inngestContract) finalize(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
) error {
	log := logf.FromContext(ctx)
	appNS := appNamespace(claim.Spec.ProjectRef.Name)

	// Best-effort, as for a database: with the Connection or its credentials
	// gone there is no provider to talk to, and blocking deletion on one
	// would wedge the claim and the project teardown behind it.
	var provisioner inngest.Provisioner
	conn := &kitchenv1alpha1.Connection{}
	key := types.NamespacedName{Namespace: claim.Namespace, Name: claim.Connection()}
	err := r.Get(ctx, key, conn)
	if err == nil {
		provisioner, err = r.inngestProvisionerFor(ctx, conn)
	}
	if err != nil {
		log.Info("finalizing claim without its provider", "claim", claim.Name, "reason", err.Error())
	}

	for _, branch := range claim.Status.Branches {
		if err := r.deleteBranch(ctx, claim, inngestBrancher(provisioner), appNS, branch); err != nil {
			return err
		}
	}
	return r.releaseEnvironments(ctx, claim)
}

// inngestProvisionerFor builds the provisioner for a Connection, reading the
// API key from its credentials secret. The key never appears in any status,
// log line or error this controller writes.
func (r *ResourceClaimReconciler) inngestProvisionerFor(
	ctx context.Context,
	conn *kitchenv1alpha1.Connection,
) (inngest.Provisioner, error) {
	creds := &corev1.Secret{}
	key := types.NamespacedName{Namespace: conn.Namespace, Name: conn.Spec.CredentialsSecretRef.Name}
	if err := r.Get(ctx, key, creds); err != nil {
		return nil, err
	}
	token := string(creds.Data[inngestTokenKey])
	if token == "" {
		return nil, fmt.Errorf("credentials secret %q has no %q key", creds.Name, inngestTokenKey)
	}
	factory := r.Inngest
	if factory == nil {
		factory = inngest.Default
	}
	return factory(inngest.Options{Connection: conn, Token: token})
}

// reportInngestApp records whether a worker has connected as the claim's
// app. A lookup that fails is recorded as such rather than failing the
// claim: the binding is right whether or not the report could be made.
func (r *ResourceClaimReconciler) reportInngestApp(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner inngest.Provisioner,
	environment, appID string,
) {
	app, err := provisioner.App(ctx, environment, appID)
	switch {
	case err != nil:
		setClaimCondition(claim, condAppConnected, metav1.ConditionUnknown, "LookupFailed", err.Error())
	case !app.Found:
		setClaimCondition(claim, condAppConnected, metav1.ConditionFalse, "NotConnected",
			fmt.Sprintf("no worker has connected to Inngest as app %q in environment %s yet: the app appears "+
				"when the process holding the connect worker starts with this binding and an Inngest client "+
				"created with id %q", appID, environment, appID))
	case app.SyncStatus == "error":
		setClaimCondition(claim, condAppConnected, metav1.ConditionFalse, "SyncFailed",
			fmt.Sprintf("app %q connected, and its last sync failed: %s", appID, app.SyncError))
	case app.Method != "" && app.Method != "CONNECT" && app.Method != "UNSPECIFIED":
		setClaimCondition(claim, condAppConnected, metav1.ConditionTrue, "Serving",
			fmt.Sprintf("app %q is registered in %s mode rather than connect: %d function(s) synced%s. A "+
				"serve-mode app is reached by Inngest over HTTP, which a protected preview answers with a "+
				"login page", appID, app.Method, app.Functions, sdkNote(app)))
	default:
		setClaimCondition(claim, condAppConnected, metav1.ConditionTrue, "Connected",
			fmt.Sprintf("app %q has connected: %d function(s) synced%s", appID, app.Functions, sdkNote(app)))
	}
}

func sdkNote(app inngest.App) string {
	if app.SDK == "" {
		return ""
	}
	return " by the " + app.SDK + " SDK"
}

// reportConnectWorkers counts what reads the binding against the cap the
// platform cannot read. The numbers are Inngest's, from the connect docs
// and the pricing page, and the sentence says why they are not checked.
func reportConnectWorkers(claim *kitchenv1alpha1.ResourceClaim) {
	environments := 1 + len(claim.Status.Branches)
	setClaimCondition(claim, condConnectWorkers, metav1.ConditionTrue, "Counted",
		fmt.Sprintf("%d environment(s) of this project read the binding, and every running pod of the process "+
			"holding the connect worker is one of the account's concurrent worker connections — 3 on Inngest's "+
			"free plan, 20 on paid plans, at most 10 apps per connection. The Inngest API does not expose the "+
			"plan's cap, so it is counted here and checked on the account's billing page", environments))
}

// inngestBrancher is an Inngest provisioner as the branch machinery
// (resourceclaim_postgres.go) sees it: a preview's branch is a branch
// environment of its own, and its data — an event stream nothing has
// written to yet — is synthetic. A nil provisioner is a nil interface, so
// finalization without a Connection skips the provider.
//
// The instance ID the machinery passes is not used: an Inngest branch
// environment is named within the account the Connection authenticates as,
// not under an app.
func inngestBrancher(provisioner inngest.Provisioner) claimBrancher {
	if provisioner == nil {
		return nil
	}
	return inngestBranchOps{provisioner}
}

type inngestBranchOps struct{ inngest.Provisioner }

func (i inngestBranchOps) createBranch(ctx context.Context, _, name string) (claimBranchResult, error) {
	branch, err := i.CreateBranch(ctx, name)
	if err != nil {
		return claimBranchResult{}, err
	}
	return claimBranchResult{
		ID:         branch.ID,
		Provenance: string(database.ProvenanceSynthetic),
		Data:       branch.Binding.SecretData(),
	}, nil
}

func (i inngestBranchOps) deleteBranch(ctx context.Context, _, branchID string) error {
	return i.DeleteBranch(ctx, branchID)
}
