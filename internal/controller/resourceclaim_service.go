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
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
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
// So the whole of the contract is four refusals and a Secret:
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

	// What the offering's workload *is* comes from the release that
	// environment is running, and from the project's own declaration only
	// where there is no release to ask: a project whose workloads are
	// declared in kitchen.json has none of them in `spec.processes`, and an
	// offering naming one would otherwise never resolve.
	running, err := r.runningProcesses(ctx, env, provider)
	if err != nil {
		return ctrl.Result{}, err
	}
	address, err := offeringAddress(provider, env, offering, running)
	if err != nil {
		return r.failed(ctx, claim, "OfferingNotAddressed", err)
	}

	// The binding Secret lives in the *consumer's* application namespace,
	// which a claim can be bound before that project's first build creates.
	appNS := appNamespace(project.Name)
	if err := ensureNamespace(ctx, r.Client, appNS, project.Name); err != nil {
		return ctrl.Result{}, err
	}
	secretName := claimSecretName(claim.Name)
	if err := r.writeBindingSecret(ctx, claim, appNS, secretName, address.binding(cfg, envName)); err != nil {
		return ctrl.Result{}, err
	}
	claim.Status.SecretName = secretName
	claim.Status.InstanceID = cfg.Project + "/" + cfg.Offering
	claim.Status.InstanceName = cfg.Offering
	setClaimCondition(claim, condProvisioned, metav1.ConditionTrue, "OfferingResolved",
		fmt.Sprintf("bound to %s, offered by project %s from environment %s",
			address.host, provider.Name, env.Name))

	// What the consumer reaches is another project's running environment, so
	// what comes back through it is that project's data as it stands. There
	// is no masking and no copy in between, and declaring anything gentler
	// than `production` would be the platform vouching for data it never
	// sees.
	claim.Status.DataProvenance = string(database.ProvenanceProduction)
	claimType, _ := claim.Type()
	declare(claim, claimType, service.ProviderName)

	if err := r.bind(ctx, claim, service.ProviderName,
		fmt.Sprintf("claim %s bound: %s at %s", claim.Name, claim.Spec.Type, address.host),
		map[string]any{
			"type":            claim.Spec.Type,
			"offeringProject": cfg.Project,
			"offering":        cfg.Offering,
			"environment":     env.Name,
			"secret":          claim.Status.SecretName,
			"dataProvenance":  claim.Status.DataProvenance,
			"previewMode":     claim.Status.PreviewMode,
		}); err != nil {
		return ctrl.Result{}, err
	}
	log.Info("reconciled service claim", "claim", claim.Name, "offering", claim.Status.InstanceID,
		"environment", env.Name, "host", address.host)
	return ctrl.Result{}, nil
}

// finalize takes back nothing, because a binding put nothing into the world
// beyond its own Secret — and that Secret is the reconciler's to remove, as
// it is for every other type. The offering carries on being offered.
func (serviceContract) finalize(
	_ context.Context,
	_ *ResourceClaimReconciler,
	_ *kitchenv1alpha1.ResourceClaim,
) error {
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
