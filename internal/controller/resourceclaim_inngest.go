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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/database"
	"github.com/Bermos/Kitchen/internal/provider/inngest"
)

// The inngest half of the ResourceClaim reconciler: durable background work
// from Inngest, through a backgroundJobs-capable Connection. It is the third
// contract and it is deliberately the postgres one's shape — a Connection
// matched on a capability, a provisioner built from it, typed Requirements
// off the claim's own slice of spec.config, a binding Secret in the
// application namespace, and a branch per preview Environment.
//
// Two providers stand behind it and they do very different things, which is
// why the condition messages are the provisioner's own words rather than
// this file's.
//
// Against **Inngest Cloud** there is no app to create: an Inngest app comes
// into existence the first time a worker connects with its ID, and syncs its
// functions on every connection. And the management API mints no keys. So
// provisioning *reads* — the environment's signing key and event key — and
// the claim's own contribution is the branch environment per preview, which
// is what keeps one pull request's events from triggering another's
// functions. The keys are re-read on every reconcile, so a key rotated in the
// Inngest dashboard reaches the binding without anybody touching the claim.
//
// Against a **self-hosted** Inngest the platform runs the server: one for
// production on a Postgres and a queue of its own, and — the answer to the
// tenancy question #268 left open — one per preview, because a self-hosted
// server has no environments to keep two previews' event streams apart with.
// The keys are minted rather than read, the server parks with the preview
// that reads it, and it is destroyed with the claim.
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

// The self-hosted provider runs an Inngest server per claim and per preview:
// a Deployment, a Service, a Secret and — for a preview's embedded store —
// a PersistentVolumeClaim, plus the CloudNativePG Cluster and the Valkey
// production's server keeps its state in. Every one of those verbs is
// already granted by the postgres and redis halves of this controller and by
// the environment reconciler; they are named here so that a reader of this
// file knows what it writes.
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

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
	requirements := inngest.Requirements{
		App:         cfg.App,
		Environment: cfg.Environment,
		Mode:        cfg.Mode,
	}
	// Serve mode's sync target, which only the platform can compose: the
	// environment's own URL and the claim's serve path. It is empty in
	// connect mode, and while production has no URL yet — the server is
	// told on the reconcile that follows the environment being published.
	if cfg.Mode == kitchenv1alpha1.InngestModeServe {
		requirements.ServeURL = r.inngestServeURL(ctx, claim, kitchenv1alpha1.EnvironmentProduction, cfg.ServePath)
	}
	instance, err := provisioner.Provision(ctx, claimResource(claim), requirements)
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
	claim.Status.InstanceName = instance.Name
	claim.Status.SecretName = secretName
	// The production binding selects the production event stream: what the
	// application sends there is production data, on the platform's own
	// word. Inngest reports no placement, and the status says so by saying
	// nothing.
	claim.Status.DataProvenance = string(database.ProvenanceProduction)
	claim.Status.Residency = ""
	setClaimCondition(claim, condProvisioned, metav1.ConditionTrue, instance.Reason, instance.Message)

	claimType, _ := claim.Type()
	mode := declare(claim, claimType, conn.Spec.Provider)
	// The provider declares what a *connect* binding does to idling, because
	// connect is what every provider serves and what a claim naming no mode
	// gets. A serve binding is the exception and the claim says so for
	// itself: the server calls the environment's own URL, the call crosses
	// the interceptor, and the environment wakes for it — which is the
	// interceptor doing exactly what it exists for.
	if cfg.Mode == kitchenv1alpha1.InngestModeServe {
		claim.Status.KeepsPodsRunning = false
	}
	branchErr := r.reconcileBranches(ctx, claim, project.Name, r.inngestBrancher(claim, provisioner, requirements),
		appNS, conn.Spec.Provider, mode.Isolated())

	r.reportInngestApp(ctx, claim, provisioner, instance.Environment, cfg.App)
	reportConnectWorkers(claim, cfg.Mode)

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

	brancher := r.inngestBrancher(claim, provisioner, inngest.Requirements{})
	for _, branch := range claim.Status.Branches {
		if err := r.deleteBranch(ctx, claim, brancher, appNS, branch); err != nil {
			return err
		}
	}
	if err := r.releaseEnvironments(ctx, claim); err != nil {
		return err
	}

	// A server this platform runs is this platform's to take back, and it
	// goes with the claim — unconditionally, because the type carries no
	// deletionPolicy: there is nothing here a third party is holding for a
	// policy to choose about. Inngest Cloud is not a Deprovisioner and
	// nothing is asked of it: the keys are the account's, the app record is
	// the application's, and archiving a branch environment deleted nothing.
	deprovisioner, ok := provisioner.(inngest.Deprovisioner)
	if ok && claim.Status.InstanceID != "" {
		if err := deprovisioner.Deprovision(ctx, claim.Status.InstanceID); err != nil {
			return err
		}
	}
	return nil
}

// inngestServeURL is where Inngest calls the application for one environment
// of the claim's project: the environment's own URL and the claim's serve
// path. Empty while the environment does not exist or has not been published
// — the server is told on the reconcile that follows, and a server told
// nothing syncs nothing rather than polling an address that does not answer.
//
// It is the environment's *public* URL on purpose. That is the address the
// interceptor is in front of, so an idle environment wakes for the call it
// is being given work by — which is the whole of why a serve binding does
// not hold pods up. It is also the address a protected preview's gate
// answers, which is why connect is still the mode for those, and why the
// claim's own condition says so.
func (r *ResourceClaimReconciler) inngestServeURL(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	environment kitchenv1alpha1.EnvironmentType,
	servePath string,
) string {
	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environments, client.InNamespace(claim.Namespace)); err != nil {
		return ""
	}
	for i := range environments.Items {
		env := &environments.Items[i]
		if env.Spec.ProjectRef.Name != claim.Spec.ProjectRef.Name || env.Spec.Type != environment {
			continue
		}
		return joinServeURL(env.Status.URL, servePath)
	}
	return ""
}

// inngestPreviewServeURL is the same for one named preview Environment.
func (r *ResourceClaimReconciler) inngestPreviewServeURL(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	envName, servePath string,
) string {
	env := &kitchenv1alpha1.Environment{}
	key := types.NamespacedName{Namespace: claim.Namespace, Name: envName}
	if err := r.Get(ctx, key, env); err != nil {
		return ""
	}
	return joinServeURL(env.Status.URL, servePath)
}

func joinServeURL(base, servePath string) string {
	if base == "" {
		return ""
	}
	if servePath == "" {
		servePath = kitchenv1alpha1.InngestDefaultServePath
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(servePath, "/")
}

// inngestProvisionerFor builds the provisioner for a Connection, reading the
// API key from its credentials secret. The key never appears in any status,
// log line or error this controller writes.
//
// A Connection that names no credentials secret is not an error here: the
// self-hosted provider has none, because it runs the server itself with the
// operator's own account. The CRD refuses an empty credentialsSecretRef for
// every other provider, which is what keeps that the exception rather than a
// hole.
func (r *ResourceClaimReconciler) inngestProvisionerFor(
	ctx context.Context,
	conn *kitchenv1alpha1.Connection,
) (inngest.Provisioner, error) {
	token := ""
	if secretName := conn.Spec.CredentialsSecretRef.Name; secretName != "" {
		creds := &corev1.Secret{}
		key := types.NamespacedName{Namespace: conn.Namespace, Name: secretName}
		if err := r.Get(ctx, key, creds); err != nil {
			return nil, err
		}
		token = string(creds.Data[inngestTokenKey])
		if token == "" {
			return nil, fmt.Errorf("credentials secret %q has no %q key", creds.Name, inngestTokenKey)
		}
	}
	factory := r.Inngest
	if factory == nil {
		factory = inngest.Default
	}
	// Where the self-hosted provisioner runs its servers is the Connection's
	// own `namespace` config, and kitchen-inngest otherwise — deliberately
	// not a project's application namespace, which is deleted with its
	// project while a claim's server should outlive nothing but its claim.
	return factory(inngest.Options{
		Connection: conn,
		Token:      token,
		Cluster:    r.Client,
		Namespace:  inngest.DefaultServerNamespace,
	})
}

// reportInngestApp records whether a worker has connected as the claim's
// app. A lookup that fails is recorded as such rather than failing the
// claim: the binding is right whether or not the report could be made.
//
// A provider that publishes no app inventory says so rather than reporting
// a guess. A self-hosted server is one: its own dashboard, at the binding's
// INNGEST_BASE_URL, is where its apps and their functions are, and there is
// no documented API this operator could read them from.
func (r *ResourceClaimReconciler) reportInngestApp(
	ctx context.Context,
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner inngest.Provisioner,
	environment, appID string,
) {
	reporter, ok := provisioner.(inngest.AppReporter)
	if !ok {
		setClaimCondition(claim, condAppConnected, metav1.ConditionUnknown, "NotReported",
			fmt.Sprintf("this Inngest publishes no app inventory the platform can read, so whether a worker "+
				"has connected as app %q is not something this claim can say. The server's own dashboard, at "+
				"the INNGEST_BASE_URL in this claim's binding, lists the apps that have synced and the "+
				"functions each registered", appID))
		return
	}
	app, err := reporter.App(ctx, environment, appID)
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
//
// A claim in serve mode holds no connections at all — Inngest calls the
// application rather than the other way round — and the condition says that
// instead of a number nothing would be measured against.
func reportConnectWorkers(claim *kitchenv1alpha1.ResourceClaim, mode string) {
	if mode == kitchenv1alpha1.InngestModeServe {
		setClaimCondition(claim, condConnectWorkers, metav1.ConditionTrue, "NotConnectMode",
			"this claim is served rather than connected: Inngest calls the application over HTTP, so nothing "+
				"holds a worker connection and no connection cap applies. The call is made to the "+
				"environment's own URL, which wakes an idle environment — and which a protected preview "+
				"answers with a login page, so a protected preview wants connect")
		return
	}
	environments := 1 + len(claim.Status.Branches)
	setClaimCondition(claim, condConnectWorkers, metav1.ConditionTrue, "Counted",
		fmt.Sprintf("%d environment(s) of this project read the binding, and every running pod of the process "+
			"holding the connect worker is one of the account's concurrent worker connections — 3 on Inngest's "+
			"free plan, 20 on paid plans, at most 10 apps per connection. The Inngest API does not expose the "+
			"plan's cap, so it is counted here and checked on the account's billing page", environments))
}

// inngestBrancher is an Inngest provisioner as the branch machinery
// (resourceclaim_postgres.go) sees it: what a preview gets of its own —
// a branch environment at Inngest Cloud, a server of its own when the
// platform runs Inngest — created and torn down by name, whose binding
// becomes the data of the preview's Secret. Its data is an event stream
// nothing has written to yet, which is synthetic under either provider. A
// nil provisioner is a nil interface, so finalization without a Connection
// skips the provider.
func (r *ResourceClaimReconciler) inngestBrancher(
	claim *kitchenv1alpha1.ResourceClaim,
	provisioner inngest.Provisioner,
	requirements inngest.Requirements,
) claimBrancher {
	if provisioner == nil {
		return nil
	}
	return inngestBranchOps{
		Provisioner:  provisioner,
		requirements: requirements,
		// Serve mode's sync target is the *preview's* own URL, not
		// production's, so it is resolved per branch rather than carried in
		// the requirements the claim was provisioned with.
		serveURL: func(ctx context.Context, envName string) string {
			if requirements.Mode != kitchenv1alpha1.InngestModeServe {
				return ""
			}
			return r.inngestPreviewServeURL(ctx, claim, envName, claim.Inngest().ServePath)
		},
	}
}

type inngestBranchOps struct {
	inngest.Provisioner
	requirements inngest.Requirements
	serveURL     func(ctx context.Context, envName string) string
}

func (i inngestBranchOps) createBranch(ctx context.Context, instanceID, name string) (claimBranchResult, error) {
	requirements := i.requirements
	if i.serveURL != nil {
		requirements.ServeURL = i.serveURL(ctx, name)
	}
	branch, err := i.CreateBranch(ctx, instanceID, name, requirements)
	if err != nil {
		return claimBranchResult{}, err
	}
	return claimBranchResult{
		ID:         branch.ID,
		Provenance: string(database.ProvenanceSynthetic),
		Data:       branch.Binding.SecretData(),
	}, nil
}

func (i inngestBranchOps) deleteBranch(ctx context.Context, instanceID, branchID string) error {
	return i.DeleteBranch(ctx, instanceID, branchID)
}

// idler is the self-hosted half of this contract: a preview's own server is
// scaled to no pods with its preview and back up on wake, and the volume its
// runs are on survives the park. Inngest Cloud is not an IdlingProvisioner
// and answers nil — the branch environment is Inngest's to run and this
// platform has no lever on it.
func (i inngestBranchOps) idler() claimIdler {
	idling, ok := i.Provisioner.(inngest.IdlingProvisioner)
	if !ok {
		return nil
	}
	return branchIdler{idle: idling.IdleBranch, wake: idling.WakeBranch}
}
