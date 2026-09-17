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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	PlatformEnvironmentProduction = "production"
	PlatformEnvironmentStage      = "stage"
	PlatformEnvironmentPreview    = "preview"
)

func DefaultPlatformEnvironmentName(envType kitchenv1alpha1.EnvironmentType) string {
	switch envType {
	case kitchenv1alpha1.EnvironmentPreview:
		return PlatformEnvironmentPreview
	case kitchenv1alpha1.EnvironmentStage:
		return PlatformEnvironmentStage
	default:
		return PlatformEnvironmentProduction
	}
}

// PolicyEnvironmentFor resolves the environment's bound policy environment.
// A nil result is a valid "legacy environment-local governance" state.
func PolicyEnvironmentFor(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	env *kitchenv1alpha1.Environment,
) (*kitchenv1alpha1.PlatformEnvironment, error) {
	if env == nil || env.Spec.PolicyEnvironmentRef == nil || env.Spec.PolicyEnvironmentRef.Name == "" {
		return nil, nil
	}
	p := &kitchenv1alpha1.PlatformEnvironment{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: env.Spec.PolicyEnvironmentRef.Name}, p)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ApplyPolicyEnvironment copies policy governance fields onto an Environment.
// Returns true when anything changed.
func ApplyPolicyEnvironment(
	env *kitchenv1alpha1.Environment,
	p *kitchenv1alpha1.PlatformEnvironment,
) bool {
	if env == nil || p == nil {
		return false
	}
	before := env.DeepCopy()
	env.Spec.Owners = append([]string{}, p.Spec.Owners...)
	if p.Spec.Requirements == nil {
		env.Spec.Requirements = nil
	} else {
		env.Spec.Requirements = p.Spec.Requirements.DeepCopy()
	}
	if p.Spec.Serves == nil {
		env.Spec.Serves = nil
	} else {
		env.Spec.Serves = &kitchenv1alpha1.EnvironmentServes{
			Consumers: append([]kitchenv1alpha1.EnvironmentType{}, p.Spec.Serves.Consumers...),
		}
	}
	env.Spec.DataClass = p.Spec.DataClass
	env.Spec.Residency = p.Spec.Residency
	env.Spec.Criticality = p.Spec.Criticality
	env.Spec.RTO = p.Spec.RTO
	env.Spec.RPO = p.Spec.RPO
	return before.Spec.Owners == nil && env.Spec.Owners != nil ||
		!equalStringSlices(before.Spec.Owners, env.Spec.Owners) ||
		!equalRequirements(before.Spec.Requirements, env.Spec.Requirements) ||
		!equalServes(before.Spec.Serves, env.Spec.Serves) ||
		before.Spec.DataClass != env.Spec.DataClass ||
		before.Spec.Residency != env.Spec.Residency ||
		before.Spec.Criticality != env.Spec.Criticality ||
		before.Spec.RTO != env.Spec.RTO ||
		before.Spec.RPO != env.Spec.RPO
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalRequirements(a, b *kitchenv1alpha1.EnvironmentRequirements) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.BundleDigest != b.BundleDigest {
		return false
	}
	if len(a.Parameters) != len(b.Parameters) {
		return false
	}
	for key, value := range a.Parameters {
		if b.Parameters[key] != value {
			return false
		}
	}
	return true
}

func equalServes(a, b *kitchenv1alpha1.EnvironmentServes) bool {
	if a == nil || b == nil {
		return a == b
	}
	if len(a.Consumers) != len(b.Consumers) {
		return false
	}
	for i := range a.Consumers {
		if a.Consumers[i] != b.Consumers[i] {
			return false
		}
	}
	return true
}
