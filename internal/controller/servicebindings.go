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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/service"
)

// What a project's bindings to other projects' offerings look like from
// inside its own workloads (#493).
//
// They look like a sibling process, and that is the point. A service claim's
// binding arrives as `KITCHEN_SERVICE_<NAME>` with `_HOST` and `_PORT`
// beside it — the same three variables under the same prefix that
// `serviceEnv` hands a workload for its own environment's service processes
// — because from inside the application another team's service and a sibling
// are the same thing: an address it did not have to work out. The cost of
// the reuse is that a binding and a process cannot share a name, and that is
// refused where a name is chosen, at the API, rather than disambiguated
// here.
//
// The variables are the *claim's*, not the Release's, so unlike everything
// resolveEnv produces they are not in the snapshot a rollback replays. That
// is correct for what they carry: the address of somebody else's environment
// is a fact about the platform now, not about the commit — a rolled-back
// release calls the same offering at the same address, and would be broken
// by an old one.

// serviceBindingEnv is every bound service claim of this project, as the
// three variables a workload reads its address from, and the claims that
// bind nothing *here* with the reason each gives.
//
// Which binding this environment reads is decided by what class of
// environment it is (#494): the claim resolved one per class, because the
// provider's environment owners say who may bind to each of theirs, and a
// preview of the consumer may well reach a different environment of the
// provider than its production does — or none at all.
//
// A claim that is not bound yet contributes nothing and holds nothing up.
// That is deliberate and it is the opposite of what an unbound
// `fromResourceClaim` variable does: that one names a claim the *release*
// declares, so deploying without it would run a release that cannot work,
// while a binding is the platform's own addition and an offering that is not
// resolvable — a project that has not opened it, an environment nobody has
// deployed into, an environment whose owners admit no consumer of this class
// — belongs to another team and another timetable. The claim's own status
// says why, this environment carries the same sentence on its ClaimsBound
// condition, and neither sits unready behind it.
func serviceBindingEnv(
	ctx context.Context,
	c client.Client,
	env *kitchenv1alpha1.Environment,
	projectName string,
	appNS string,
) ([]corev1.EnvVar, []string, int, error) {
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := c.List(ctx, claims, client.InNamespace(env.Namespace)); err != nil {
		return nil, nil, 0, err
	}
	bound := make([]*kitchenv1alpha1.ResourceClaim, 0, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.Type != kitchenv1alpha1.ClaimTypeService ||
			claim.Spec.ProjectRef.Name != projectName {
			continue
		}
		// A claim that resolved something for *some* class is here even
		// when it resolved nothing for this one, so that the environment
		// can say so. A claim that has never resolved anything is not: it
		// is Failed or Pending and its own status is where that is read.
		if claim.Status.SecretName == "" && claim.Status.Service == nil {
			continue
		}
		bound = append(bound, claim)
	}
	// Name order, because the list a List answers with is not ordered and a
	// pod spec that reshuffles its variables between passes is a Deployment
	// that rolls for nothing.
	sort.Slice(bound, func(i, j int) bool { return bound[i].Name < bound[j].Name })

	vars := make([]corev1.EnvVar, 0, len(bound)*3)
	var unbound []string
	// How many of those are waiting for a person rather than refused by
	// one. The environment says the same sentence either way, but what it
	// *is* differs — a request nobody has answered is not a refusal — and
	// the condition's reason is what the screens read that off (#495).
	awaiting := 0
	for _, claim := range bound {
		secretName, refusal := bindingForEnvironment(claim, env)
		if refusal != "" {
			unbound = append(unbound, claim.Name+": "+refusal)
			if awaitingApproval(claim) {
				awaiting++
			}
			continue
		}
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: appNS, Name: secretName}
		if err := c.Get(ctx, key, secret); err != nil {
			if apierrors.IsNotFound(err) {
				// The claim says it has a binding and the Secret is not
				// there — the claim's own reconcile writes it again. Naming
				// a key of a Secret that does not exist would stop every pod
				// of this environment from starting, which is a great deal
				// worse than a variable arriving one pass later.
				logf.FromContext(ctx).Info("a service binding names a secret that is not there yet",
					"claim", claim.Name, "secret", secretName, "environment", env.Name)
				continue
			}
			return nil, nil, 0, err
		}
		prefix := kitchenv1alpha1.ServiceEnvPrefix(claim.Name)
		// The URL is the one key that can be absent: an offering that speaks
		// something other than HTTP is handed over as a host and a port and
		// nothing else, so the variable is left out rather than named
		// against a key nothing wrote.
		if _, ok := secret.Data[service.BindingKeyURL]; ok {
			vars = append(vars, bindingVar(prefix, secretName, service.BindingKeyURL))
		}
		vars = append(vars,
			bindingVar(prefix+"_HOST", secretName, service.BindingKeyHost),
			bindingVar(prefix+"_PORT", secretName, service.BindingKeyPort),
		)
	}
	return vars, unbound, awaiting, nil
}

// awaitingApproval reports whether this claim binds nothing here because the
// providing project has not answered its request yet (#495) — which is a
// person's turn to act rather than a refusal, and is why the environment's
// condition carries a reason of its own for it.
func awaitingApproval(claim *kitchenv1alpha1.ResourceClaim) bool {
	grant := claim.ServiceGrant()
	return grant != nil && grant.State == kitchenv1alpha1.ServiceGrantRequested
}

// bindingForEnvironment is the Secret this environment reads a claim's
// address out of, or the reason it reads none.
//
// Two refusals, and neither is new machinery. The first is the provider's
// grant: the claim resolved a binding per class of consumer environment, and
// a class no environment of the provider admits has none. The second is the
// data class, made and worded by the one function every other data-class
// refusal on the platform goes through (dataClassRefusalBetween, over
// DataClass.Exceeds — the same ordering the policy bundle's
// dataclass-le-environment rule reads): an environment rated above the
// environment it would be calling does not call it, because data does not
// flow somewhere rated below it.
func bindingForEnvironment(
	claim *kitchenv1alpha1.ResourceClaim,
	env *kitchenv1alpha1.Environment,
) (secretName string, refusal string) {
	if claim.Status.Service == nil {
		if claim.Status.SecretName == "" {
			// Never resolved: Failed or Pending, and the claim's own status
			// carries the provider's words. It is a refusal here rather
			// than a wait, because an offering that is not resolvable
			// belongs to another team and another timetable — see the file
			// comment.
			return "", "the binding has not resolved; the claim's own status says why"
		}
		// Bound by an operator older than #494, which resolved one address
		// for every class. It keeps reading it until its claim is
		// reconciled again, which is the next pass — a binding that
		// vanished for the length of an upgrade would take an application
		// down for a fact about the platform.
		return claim.Status.SecretName, ""
	}
	// The providing project's answer comes before anything about classes of
	// environment: a binding nobody has admitted reaches nothing anywhere,
	// and the environment says so in the same words the claim does rather
	// than in the ones about who serves whom (#495).
	if reason := serviceGrantReason(claim); reason != "" {
		return "", reason
	}
	binding, ok := claim.ServiceBinding(env.Spec.Type)
	if !ok {
		return "", serviceBindingReason(claim, env.Spec.Type)
	}
	refusal = dataClassRefusalBetween(
		dataClassHolder{noun: "environment", name: env.Name, class: env.Spec.DataClass},
		dataClassHolder{noun: "environment", name: binding.Environment, class: binding.DataClass},
	)
	if refusal == "" {
		return binding.SecretName, ""
	}
	return "", refusal
}

// serviceGrantReason is why a claim the providing project has not admitted
// reaches nothing here, and "" for one it has — including every claim on an
// open offering and every project binding its own.
//
// Both sentences are the claim's own, from the one place they are written
// (grantAwaited and grantRefused), because this reader and the reader of the
// claim's status are looking at one fact and must not be told it two ways.
// Like every other sentence a service binding writes it is a **statement of
// fact rather than an instruction**: the reader is the consumer, and the acts
// it names belong to the providing project's admins.
func serviceGrantReason(claim *kitchenv1alpha1.ResourceClaim) string {
	grant := claim.ServiceGrant()
	if grant == nil || grant.Admitted() {
		return ""
	}
	cfg := claim.Service()
	if grant.State == kitchenv1alpha1.ServiceGrantDenied {
		return grantRefused(cfg.Project, cfg.Offering, grant)
	}
	return grantAwaited(cfg.Project, cfg.Offering)
}

// serviceBindingReason is the claim's own words about why this class of
// environment reaches nothing, and a sentence of last resort for a claim
// that recorded no row at all for it.
func serviceBindingReason(
	claim *kitchenv1alpha1.ResourceClaim,
	consumer kitchenv1alpha1.EnvironmentType,
) string {
	for _, binding := range claim.Status.Service.Bindings {
		if binding.Consumer == consumer && binding.Reason != "" {
			return binding.Reason
		}
	}
	return fmt.Sprintf("no environment of the providing project admits a %s consumer", consumer)
}

// bindingVar is one variable read out of a binding Secret. The address is
// not a credential and could have been written as a literal; it is read from
// the Secret so that the binding is the one place the address is spelled,
// and an offering that moves moves for every workload at once.
func bindingVar(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: secret},
			Key:                  key,
		}},
	}
}
