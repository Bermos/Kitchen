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
// three variables a workload reads its address from.
//
// A claim that is not bound yet contributes nothing and holds nothing up.
// That is deliberate and it is the opposite of what an unbound
// `fromResourceClaim` variable does: that one names a claim the *release*
// declares, so deploying without it would run a release that cannot work,
// while a binding is the platform's own addition and an offering that is not
// resolvable — a project that has not opened it, an environment nobody has
// deployed into — belongs to another team and another timetable. The claim's
// own status says why, and the environment does not sit unready behind it.
func serviceBindingEnv(
	ctx context.Context,
	c client.Client,
	env *kitchenv1alpha1.Environment,
	projectName string,
	appNS string,
) ([]corev1.EnvVar, error) {
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := c.List(ctx, claims, client.InNamespace(env.Namespace)); err != nil {
		return nil, err
	}
	bound := make([]*kitchenv1alpha1.ResourceClaim, 0, len(claims.Items))
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.Type != kitchenv1alpha1.ClaimTypeService ||
			claim.Spec.ProjectRef.Name != projectName ||
			claim.Status.SecretName == "" {
			continue
		}
		bound = append(bound, claim)
	}
	// Name order, because the list a List answers with is not ordered and a
	// pod spec that reshuffles its variables between passes is a Deployment
	// that rolls for nothing.
	sort.Slice(bound, func(i, j int) bool { return bound[i].Name < bound[j].Name })

	vars := make([]corev1.EnvVar, 0, len(bound)*3)
	for _, claim := range bound {
		secret := &corev1.Secret{}
		key := types.NamespacedName{Namespace: appNS, Name: claim.Status.SecretName}
		if err := c.Get(ctx, key, secret); err != nil {
			if apierrors.IsNotFound(err) {
				// The claim says it has a binding and the Secret is not
				// there — the claim's own reconcile writes it again. Naming
				// a key of a Secret that does not exist would stop every pod
				// of this environment from starting, which is a great deal
				// worse than a variable arriving one pass later.
				logf.FromContext(ctx).Info("a service binding names a secret that is not there yet",
					"claim", claim.Name, "secret", claim.Status.SecretName, "environment", env.Name)
				continue
			}
			return nil, err
		}
		prefix := kitchenv1alpha1.ServiceEnvPrefix(claim.Name)
		// The URL is the one key that can be absent: an offering that speaks
		// something other than HTTP is handed over as a host and a port and
		// nothing else, so the variable is left out rather than named
		// against a key nothing wrote.
		if _, ok := secret.Data[service.BindingKeyURL]; ok {
			vars = append(vars, bindingVar(prefix, claim.Status.SecretName, service.BindingKeyURL))
		}
		vars = append(vars,
			bindingVar(prefix+"_HOST", claim.Status.SecretName, service.BindingKeyHost),
			bindingVar(prefix+"_PORT", claim.Status.SecretName, service.BindingKeyPort),
		)
	}
	return vars, nil
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
