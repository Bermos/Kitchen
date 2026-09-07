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
	"sort"
	"strconv"
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
	"github.com/Bermos/Kitchen/internal/provider/service"
)

// The service half of the ResourceClaim reconciler: the edge between two
// projects (#493).
//
// It is the contract that provisions nothing. Every other one asks somebody
// for a resource and writes the credential down; this one resolves an
// offering another Project already makes, checks that the offering admits
// this consumer, and writes an address down. The workload behind that
// address is the providing project's — its quota, its release, its access
// list — and the platform grants nothing to either side to make the call
// possible.
//
// So the whole of the contract is a handful of refusals and, for each class
// of the consumer's environments, a Secret:
//
//   - a project that does not exist, and an offering that project does not
//     make, are both **Failed** with the name in the message. They are
//     configuration to correct rather than a state to wait through: nothing
//     appears on a timer that would make the name right.
//   - an offering the consumer has not been granted is Failed too, and the
//     message says which grant is missing. `visibility: request` is the
//     default, and the flow that turns a request into a grant is #495 — so
//     until it lands the refusal names `open` as what would admit this
//     consumer today, rather than binding on the strength of an approval
//     nothing recorded.
//   - an environment that is not there **yet** is Pending, because that one
//     does appear on a timer: the offering names an environment, and a
//     project's first build for a target creates it.
//   - a class of the consumer's environments that no environment of the
//     provider admits reaches nothing, and the row says what would permit it
//     (#494). Which classes may bind is the *provider environment owners'*
//     declaration — `serves.consumers` — and an environment that declares
//     none admits none, so a claim whose every class is refused is Failed
//     rather than quietly bound to production. That is the change of default
//     this file carries: before #494 every class reached the environment the
//     offering names.
//
// There is deliberately **no NetworkPolicy here.** The spike has the edge
// producing one, and it will — with #497, which puts default-deny between
// application namespaces underneath it. Writing the allow half first would
// not be inert: an ingress policy selecting the provider's pods takes them
// out of the namespace's default-allow, so the first binding on the platform
// would quietly cut off every caller nobody had declared, including the
// Gateway. The two halves are one change and they land together.

// serviceContract is the claimContract for type service. The provider is
// another Project, so conn is always nil and never read.
type serviceContract struct{}

func (serviceContract) reconcile(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
	project *kitchenv1alpha1.Project,
	_ *kitchenv1alpha1.Connection,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	cfg := claim.Service()
	if cfg.Project == "" || cfg.Offering == "" {
		return r.failed(ctx, claim, "OfferingNotNamed",
			fmt.Errorf("a service claim names the project that offers and the offering it binds to "+
				"(spec.config.service.project and .offering); this one names %s",
				missingOffering(cfg)))
	}

	provider := &kitchenv1alpha1.Project{}
	key := types.NamespacedName{Namespace: claim.Namespace, Name: cfg.Project}
	if err := r.Get(ctx, key, provider); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return r.failed(ctx, claim, "ProjectUnknown",
			fmt.Errorf("no project named %q offers anything on this platform: this claim binds %s/%s, and "+
				"there is no such project", cfg.Project, cfg.Project, cfg.Offering))
	}

	offering, ok := provider.Offering(cfg.Offering)
	if !ok {
		return r.failed(ctx, claim, "OfferingUnknown",
			fmt.Errorf("project %s makes no offering named %q: %s", provider.Name, cfg.Offering,
				offersOf(provider)))
	}

	if refusal := offeringGrantRefusal(provider, project, offering); refusal != "" {
		return r.failed(ctx, claim, "NotGranted", fmt.Errorf("%s", refusal))
	}

	envName := offering.Environment
	if envName == "" {
		envName = ProductionTargetEnvironmentName(provider)
	}
	env := &kitchenv1alpha1.Environment{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: claim.Namespace, Name: envName}, env); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		// Pending, not Failed: an environment a project has not deployed
		// into yet does not exist yet, and the first build for that target
		// creates it. The consumer's claim binds on the pass after that.
		return r.pending(ctx, claim, "EnvironmentMissing",
			fmt.Errorf("offering %s/%s is served by environment %q of project %s, which does not exist yet — "+
				"it appears when something is deployed there, or when its owners declare it",
				provider.Name, offering.Name, envName, provider.Name))
	}
	if env.Spec.ProjectRef.Name != provider.Name {
		return r.failed(ctx, claim, "EnvironmentNotTheProviders",
			fmt.Errorf("offering %s/%s names environment %q, which belongs to project %s: an offering serves "+
				"an environment of the project that makes it", provider.Name, offering.Name, envName,
				env.Spec.ProjectRef.Name))
	}

	// Which environment of the provider each class of the consumer's own
	// environments reaches, and why a class reaches nothing (#494). The
	// offering names the default; the provider's environment owners say who
	// may bind, and a class nobody admits binds nothing rather than falling
	// back to production.
	served, err := r.servingEnvironments(ctx, claim.Namespace, provider)
	if err != nil {
		return ctrl.Result{}, err
	}

	// The binding Secret lives in the *consumer's* application namespace,
	// which a claim can be bound before that project's first build creates.
	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}

	bindings := make([]kitchenv1alpha1.ClaimServiceBinding, 0, len(kitchenv1alpha1.EnvironmentTypes()))
	resolvedIn := map[string]string{}
	// The first class that could not be addressed at all, kept so that a
	// claim no class resolved fails with the provider's own words rather
	// than with "nobody admits you" — which would be true and beside the
	// point.
	var addressProblem error
	for _, consumer := range kitchenv1alpha1.EnvironmentTypes() {
		secretName := serviceBindingSecretName(claim.Name, consumer)
		target := admittingEnvironment(consumer, env, served)
		if target == nil {
			if err := r.removeBindingSecret(ctx, appNS, secretName); err != nil {
				return ctrl.Result{}, err
			}
			bindings = append(bindings, kitchenv1alpha1.ClaimServiceBinding{
				Consumer: consumer,
				Reason:   notAdmittedRefusal(provider, offering, consumer, served),
			})
			continue
		}
		// What the offering's workload *is* comes from the release that
		// environment is running, and from the project's own declaration
		// only where there is no release to ask: a project whose workloads
		// are declared in kitchen.json has none of them in
		// `spec.processes`, and an offering naming one would otherwise
		// never resolve.
		running, err := r.runningProcesses(ctx, target, provider)
		if err != nil {
			return ctrl.Result{}, err
		}
		address, err := offeringAddress(provider, target, offering, running)
		if err != nil {
			// One class's environment not running the offered workload —
			// a stage on an older release, say — is that class's refusal
			// and not the claim's: the classes that resolve still resolve.
			// The claim fails only when every class did, and then this is
			// what it fails with.
			if addressProblem == nil {
				addressProblem = err
			}
			if err := r.removeBindingSecret(ctx, appNS, secretName); err != nil {
				return ctrl.Result{}, err
			}
			bindings = append(bindings, kitchenv1alpha1.ClaimServiceBinding{
				Consumer: consumer,
				Reason:   err.Error(),
			})
			continue
		}
		if err := r.writeBindingSecret(ctx, claim, appNS, secretName,
			address.binding(cfg, target.Name)); err != nil {
			return ctrl.Result{}, err
		}
		bindings = append(bindings, kitchenv1alpha1.ClaimServiceBinding{
			Consumer:    consumer,
			Environment: target.Name,
			SecretName:  secretName,
			Host:        address.host,
			DataClass:   target.Spec.DataClass,
		})
		resolvedIn[string(consumer)] = target.Name
	}
	if len(resolvedIn) == 0 {
		claim.Status.Service = &kitchenv1alpha1.ClaimServiceStatus{Bindings: bindings}
		// Every Secret was taken back above, so the name of one has to go
		// too: a status naming a Secret that is not there is what sends the
		// finalizer and the claim's own screen looking for it.
		claim.Status.SecretName = ""
		if addressProblem != nil {
			return r.failed(ctx, claim, "OfferingNotAddressed", addressProblem)
		}
		return r.failed(ctx, claim, "NotAdmitted",
			fmt.Errorf("%s", notAdmittedRefusal(provider, offering, "", served)))
	}

	claim.Status.Service = &kitchenv1alpha1.ClaimServiceStatus{Bindings: bindings}
	// SecretName is the binding of the first class that resolved, in the
	// platform's own order, because it is what everything written before a
	// binding could differ per class reads: the claim's own screen, the
	// finalizer, and an environment reconciled by an operator older than
	// this one. Production first, so nothing about an ordinary claim moved.
	claim.Status.SecretName = ""
	for _, binding := range bindings {
		if binding.SecretName != "" {
			claim.Status.SecretName = binding.SecretName
			break
		}
	}
	claim.Status.InstanceID = cfg.Project + "/" + cfg.Offering
	claim.Status.InstanceName = cfg.Offering
	setClaimCondition(claim, condProvisioned, metav1.ConditionTrue, "OfferingResolved",
		fmt.Sprintf("bound to offering %s/%s: %s", provider.Name, offering.Name, bindingSummary(bindings)))

	// What the consumer reaches is another project's running environment, so
	// what comes back through it is that project's data as it stands. There
	// is no masking and no copy in between, and declaring anything gentler
	// than `production` would be the platform vouching for data it never
	// sees.
	claim.Status.DataProvenance = string(database.ProvenanceProduction)
	claimType, _ := claim.Type()
	declare(claim, claimType, service.ProviderName)

	if err := r.bind(ctx, claim, service.ProviderName,
		fmt.Sprintf("claim %s bound: %s, %s", claim.Name, claim.Spec.Type, bindingSummary(bindings)),
		map[string]any{
			"type":            claim.Spec.Type,
			"offeringProject": cfg.Project,
			"offering":        cfg.Offering,
			// Which environment of the provider each class of the
			// consumer's environments reaches. It is the record of a grant
			// being exercised, so it names the class as well as the
			// environment: "the preview reached staging" is the fact
			// somebody comes back to this row for.
			"environments":   resolvedIn,
			"secret":         claim.Status.SecretName,
			"dataProvenance": claim.Status.DataProvenance,
			"previewMode":    claim.Status.PreviewMode,
		}); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled service claim", "claim", claim.Name, "offering", claim.Status.InstanceID,
		"environments", resolvedIn)
	return ctrl.Result{}, nil
}

// finalize takes back the binding Secrets of the classes the shared
// finalizer does not know about.
//
// It removes status.secretName's — the reconciler does that for every type —
// so what is left here is the stage's and the preview's, which exist because
// a consumer's classes can reach different environments of the provider.
// Beyond those Secrets a binding put nothing into the world: the offering
// carries on being offered, and the workload behind it was never this
// claim's.
func (serviceContract) finalize(
	ctx context.Context,
	r *ResourceClaimReconciler,
	claim *kitchenv1alpha1.ResourceClaim,
) error {
	appNS := appNamespace(claim.Spec.ProjectRef.Name)
	for _, consumer := range kitchenv1alpha1.EnvironmentTypes() {
		if err := r.removeBindingSecret(ctx, appNS, serviceBindingSecretName(claim.Name, consumer)); err != nil {
			return err
		}
	}
	return nil
}

// offeringGrantRefusal is the grant check, and "" means the offering admits
// this consumer.
//
// A project binding its own offering is admitted whatever the visibility
// says: the grant is the providing project's to give, and here the two are
// the same project. It is not a useful thing to do — a sibling process is
// already handed its siblings' addresses — but refusing a project access to
// its own offering would be a rule with nobody on the other side of it.
func offeringGrantRefusal(
	provider *kitchenv1alpha1.Project,
	consumer *kitchenv1alpha1.Project,
	offering kitchenv1alpha1.ServiceOffering,
) string {
	if provider.Name == consumer.Name {
		return ""
	}
	if offering.Visibility() == kitchenv1alpha1.OfferingOpen {
		return ""
	}
	return fmt.Sprintf("offering %s/%s admits consumers by request (visibility %s) and no request from "+
		"project %s has been approved. Asking for one, and approving it, is #495; until that lands the "+
		"offering admits a consumer only at visibility %s, which project %s's admins set on the offering",
		provider.Name, offering.Name, kitchenv1alpha1.OfferingRequest, consumer.Name,
		kitchenv1alpha1.OfferingOpen, provider.Name)
}

// servingEnvironments is every durable environment of the providing project:
// the set a consumer's class can be admitted by.
//
// A preview of the *provider* is not in it. An offering is served by an
// environment that outlives one pull request, and a binding resolved to a
// preview would be an address that disappears when somebody merges — so a
// preview is never chosen for a consumer, even one whose owners declared
// `serves`. The one exception is the environment an offering names outright,
// which admittingEnvironment honours whatever its class, because naming it is
// the provider's own deliberate act.
func (r *ResourceClaimReconciler) servingEnvironments(
	ctx context.Context,
	namespace string,
	provider *kitchenv1alpha1.Project,
) ([]kitchenv1alpha1.Environment, error) {
	list := &kitchenv1alpha1.EnvironmentList{}
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	out := make([]kitchenv1alpha1.Environment, 0, len(list.Items))
	for i := range list.Items {
		env := list.Items[i]
		if env.Spec.ProjectRef.Name != provider.Name ||
			env.Spec.Type == kitchenv1alpha1.EnvironmentPreview {
			continue
		}
		out = append(out, env)
	}
	// Name order, so that two environments admitting one class are chosen
	// between the same way on every pass. A binding that moved because a
	// List came back in another order would be an address changing under a
	// running application for no reason anybody could name.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// admittingEnvironment is which environment of the provider a consumer of
// this class reaches, and nil when none admits it.
//
// The offering's own environment is the default and is tried first, which is
// what "defaulting to the one the offering names" means. Where that one does
// not admit this class, the choice falls to the durable environments that do,
// **stages before production**: an environment its owners set aside for
// consumers is the conservative destination, and the thing this whole feature
// exists to stop is a consumer landing on production nobody opened.
func admittingEnvironment(
	consumer kitchenv1alpha1.EnvironmentType,
	offered *kitchenv1alpha1.Environment,
	candidates []kitchenv1alpha1.Environment,
) *kitchenv1alpha1.Environment {
	if offered.Admits(consumer) {
		return offered
	}
	for _, class := range []kitchenv1alpha1.EnvironmentType{
		kitchenv1alpha1.EnvironmentStage, kitchenv1alpha1.EnvironmentProduction,
	} {
		for i := range candidates {
			env := &candidates[i]
			if env.Spec.Type == class && env.Admits(consumer) {
				return env
			}
		}
	}
	return nil
}

// notAdmittedRefusal is the sentence a class that reaches nothing carries,
// and it names what would permit it — which is the whole requirement on it.
// An empty consumer is the claim-level refusal, for a binding no class of
// consumer resolved.
//
// It is a **statement of fact and not an instruction**, deliberately: it is
// read on the consumer's own screens, and the person reading it is not the
// person who can act on it — the grant belongs to the *providing* project's
// environment owners, who would answer this reader's call with a 403. So it
// says what each of the provider's environments serves and what would admit
// this one, and leaves the call that makes it so to docs/api/environments.md.
func notAdmittedRefusal(
	provider *kitchenv1alpha1.Project,
	offering kitchenv1alpha1.ServiceOffering,
	consumer kitchenv1alpha1.EnvironmentType,
	candidates []kitchenv1alpha1.Environment,
) string {
	who, admits := "any environment of this consumer", "the class of the environment that needs it"
	if consumer != "" {
		who = "a " + string(consumer) + " environment of this consumer"
		admits = string(consumer)
	}
	return fmt.Sprintf("no environment of project %s admits %s: %s. Offering %s/%s binds here once an "+
		"environment of %s names %s in serves.consumers, which that environment's own owners declare",
		provider.Name, who, servesSummary(candidates), provider.Name, offering.Name, provider.Name, admits)
}

// servesSummary is what each of the provider's durable environments serves,
// for the refusal above: the reader's next question is "then which of them
// does admit one", and the answer is already in hand.
func servesSummary(candidates []kitchenv1alpha1.Environment) string {
	if len(candidates) == 0 {
		return "it has no environment anything could bind to yet"
	}
	parts := make([]string, 0, len(candidates))
	for i := range candidates {
		env := &candidates[i]
		served := env.ServedConsumers()
		if len(served) == 0 {
			parts = append(parts, env.Name+" serves nobody")
			continue
		}
		words := make([]string, 0, len(served))
		for _, class := range served {
			words = append(words, string(class))
		}
		parts = append(parts, env.Name+" serves "+strings.Join(words, ", "))
	}
	return strings.Join(parts, "; ")
}

// bindingSummary is what the claim resolved, in one sentence: which
// environment each class of the consumer's environments reaches.
func bindingSummary(bindings []kitchenv1alpha1.ClaimServiceBinding) string {
	parts := make([]string, 0, len(bindings))
	for _, binding := range bindings {
		reaches := "nothing"
		if binding.Environment != "" {
			reaches = binding.Environment
		}
		parts = append(parts, string(binding.Consumer)+" reaches "+reaches)
	}
	return strings.Join(parts, ", ")
}

// serviceBindingSecretName is the Secret one class of the consumer's
// environments reads its address out of.
//
// Production keeps the name every binding Secret has, so a claim whose
// classes all reach one environment is exactly the object it was before
// #494; the other two classes are suffixed, because they can reach somewhere
// else and one Secret cannot hold two addresses under one key.
func serviceBindingSecretName(claim string, consumer kitchenv1alpha1.EnvironmentType) string {
	if consumer == kitchenv1alpha1.EnvironmentProduction {
		return claimSecretName(claim)
	}
	return claimSecretName(claim) + "-" + string(consumer)
}

// removeBindingSecret takes back the Secret of a class that reaches nothing —
// an offering that was open to previews and is not any more leaves no address
// behind for one to keep reading.
func (r *ResourceClaimReconciler) removeBindingSecret(ctx context.Context, appNS, name string) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}
	if err := r.Delete(ctx, secret); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// runningProcesses is the workload list an offering is resolved against: the
// snapshot of the release this environment runs, and the project's own
// declaration where the environment runs nothing yet.
//
// The release is asked first because it is what the environment actually
// materialized — and because a repository's kitchen.json *replaces* the
// project's process list at build time, so for a project configured that way
// the project's own list is empty and the release's is the only true one.
func (r *ResourceClaimReconciler) runningProcesses(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	provider *kitchenv1alpha1.Project,
) ([]kitchenv1alpha1.ProcessSpec, error) {
	if env.Spec.ReleaseRef.Name == "" {
		return provider.Spec.Processes, nil
	}
	release := &kitchenv1alpha1.Release{}
	key := types.NamespacedName{Namespace: env.Namespace, Name: env.Spec.ReleaseRef.Name}
	if err := r.Get(ctx, key, release); err != nil {
		if apierrors.IsNotFound(err) {
			return provider.Spec.Processes, nil
		}
		return nil, err
	}
	if len(release.Spec.ConfigSnapshot.Processes) == 0 {
		return provider.Spec.Processes, nil
	}
	return release.Spec.ConfigSnapshot.Processes, nil
}

// offeringEndpoint is where an offering answers, inside the cluster and
// nowhere else.
type offeringEndpoint struct {
	host     string
	port     int32
	protocol kitchenv1alpha1.OfferingProtocol
}

// url is the address for an offering that speaks HTTP, and empty for one
// that does not — the platform is not going to invent a scheme for a wire
// protocol it was never told about.
func (e offeringEndpoint) url() string {
	if e.protocol != kitchenv1alpha1.OfferingHTTP {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", e.host, e.port)
}

// binding is the Secret this endpoint is written into the consumer's
// namespace as.
func (e offeringEndpoint) binding(cfg kitchenv1alpha1.ServiceConfig, environment string) map[string][]byte {
	data := map[string][]byte{
		service.BindingKeyHost:        []byte(e.host),
		service.BindingKeyPort:        []byte(strconv.Itoa(int(e.port))),
		service.BindingKeyProject:     []byte(cfg.Project),
		service.BindingKeyOffering:    []byte(cfg.Offering),
		service.BindingKeyEnvironment: []byte(environment),
	}
	if url := e.url(); url != "" {
		data[service.BindingKeyURL] = []byte(url)
	}
	return data
}

// offeringAddress is where the offering's workload answers in the named
// environment of the project that offers it.
//
// It is derived from the two objects that decide it — the project's own
// declaration and the environment's name — rather than read off anything
// running, for the reason KITCHEN_URL is derived: an address read from a
// status is an address the consumer does not have until the provider's
// reconciler has written one, and this address is a fact about names.
func offeringAddress(
	provider *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	offering kitchenv1alpha1.ServiceOffering,
	processes []kitchenv1alpha1.ProcessSpec,
) (offeringEndpoint, error) {
	appNS := appNamespace(provider.Name)
	endpoint := offeringEndpoint{protocol: offering.Protocol()}
	name := offering.ProcessName()
	if name == kitchenv1alpha1.WebProcessName {
		// The environment's own Service, which every environment of every
		// project has: it is what the route publishes on a public project,
		// and it is there just the same on an internal one, where it is the
		// only way in.
		endpoint.host = env.Name + "." + appNS + ".svc.cluster.local"
		endpoint.port = servicePort
		return endpoint, nil
	}
	for _, process := range processes {
		if process.Name != name {
			continue
		}
		if !process.Addressed() {
			return offeringEndpoint{}, fmt.Errorf(
				"offering %s of project %s names the %s workload %q, and nothing addresses one: a %s has no "+
					"Service in front of it, so there is no address to hand a consumer. Offer the web "+
					"workload, or a service workload",
				offering.Name, provider.Name, process.Type, name, process.Type)
		}
		endpoint.host = ProcessServiceHost(env.Name, appNS, process.Name)
		endpoint.port = process.Port
		return endpoint, nil
	}
	names := []string{kitchenv1alpha1.WebProcessName}
	for _, process := range processes {
		names = append(names, process.Name)
	}
	return offeringEndpoint{}, fmt.Errorf(
		"offering %s of project %s names the workload %q, and environment %s is running none by that name: "+
			"it runs %s. A workload declared in the repository arrives with the release that declares it",
		offering.Name, provider.Name, name, env.Name, strings.Join(names, ", "))
}

// missingOffering names which half of a service claim's config is absent,
// for the refusal above.
func missingOffering(cfg kitchenv1alpha1.ServiceConfig) string {
	switch {
	case cfg.Project == "" && cfg.Offering == "":
		return "neither"
	case cfg.Project == "":
		return "only the offering"
	default:
		return "only the project"
	}
}

// offersOf is what a project does offer, for the refusal that names what it
// does not.
func offersOf(provider *kitchenv1alpha1.Project) string {
	names := provider.OfferingNames()
	if len(names) == 0 {
		return "it offers nothing at all. An offering is a project setting: its admins add one on the " +
			"project's Offerings pane, or with `kitchen api PATCH /projects/" + provider.Name + "`"
	}
	return "its offerings are " + strings.Join(names, ", ")
}
