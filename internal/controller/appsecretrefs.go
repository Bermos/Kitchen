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
	"fmt"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Which Secrets an environment variable's `fromSecret` may name (#426).
//
// An application namespace is not only the project's. The platform syncs its
// own credentials into it because the things that read them run there: the
// registry's docker config, so a build can push and the kubelet can pull, and
// the git token, so a build can clone. Both are shared by every project on
// that Connection.
//
// A `fromSecret` becomes a `SecretKeyRef` in that same namespace, so without
// a rule a project developer could point a variable at either and read it out
// of their own process — the platform's shared credentials, printed by
// `echo $X`. That is the one thing this platform says it never does: the API
// does not read a credential back, and it must not hand somebody a way to
// make the kubelet read it back for them.
//
// **The rule is the name.** Every Secret the platform writes into an
// application namespace is called `kitchen-…`; nothing else there is. So the
// prefix is reserved, and the two objects the platform puts there *for the
// application* — the project's own secrets and its secret configuration files
// — are named out of the reservation rather than into it.
//
// It is a rule about names rather than a lookup of what is in the namespace
// on purpose, and the alternative is what makes the case: refusing anything
// not carrying the project's label would refuse a Secret that is not there
// yet, which is most of the legitimate ones. A variable is written before the
// secret it reads at least as often as after — the project's own secrets
// object does not exist until the first secret is set, a claim's binding
// arrives when its provider binds it, and a Secret an external operator syncs
// in (the Infisical case docs/CRDS.md describes) appears whenever that
// operator next runs. A rule that consulted the cluster would refuse all
// three and make the order of two writes load-bearing.
const (
	// platformAppSecretPrefix names every Secret the platform writes into an
	// application namespace for its own use — `kitchen-registry-<connection>`
	// and `kitchen-git-<connection>` today, and whatever is added next,
	// which is the point of reserving the prefix rather than listing them.
	platformAppSecretPrefix = "kitchen-"
)

// appSecretReferenceable answers whether a variable's `fromSecret` may name
// this Secret in the application namespace.
//
// The project's own two objects are the exceptions, and both are the
// application's own content rather than the platform's: the secrets written
// through `POST /projects/{name}/secrets`, which the API publishes a
// `fromSecret` reference for, and the content of the project's secret
// configuration files.
func appSecretReferenceable(name string) bool {
	switch name {
	case ProjectSecretsName, ProjectFilesName:
		return true
	}
	return !strings.HasPrefix(name, platformAppSecretPrefix)
}

// CheckEnvSecretRef refuses one variable's `fromSecret` when it names a
// Secret the platform holds in the application namespace for its own use.
//
// The refusal names the rule and what may be referenced instead, because the
// caller who hits it is looking at a name that plainly exists in the
// namespace and needs to be told why it is not theirs.
func CheckEnvSecretRef(variable, secret string) error {
	if appSecretReferenceable(secret) {
		return nil
	}
	return fmt.Errorf(
		"env var %q: fromSecret may not name %q — a Secret called %q… in an application namespace is the "+
			"platform's own, the registry credentials (%s…) and the git token (%s…) among them. "+
			"A variable may name the project's own secrets (%q), its secret configuration files (%q), "+
			"a resource claim's binding, or any other Secret in the project's namespace",
		variable, secret, platformAppSecretPrefix,
		registrySecretName(""), gitSecretName(""),
		ProjectSecretsName, ProjectFilesName)
}

// CheckEnvSecretRefs refuses the first variable of a stored list that names
// one of the platform's own secrets.
//
// The API refuses such a write at the door, which is where somebody finds out
// in time to fix it. This is the other half: a Project written with kubectl
// never passed the door, and a reference stored before this rule existed
// passed a door that had no rule. Either way the reference is only dangerous
// when an Environment turns it into a pod, so the environment's reconcile
// checks the release it is about to materialize and refuses to deploy it.
func CheckEnvSecretRefs(vars []kitchenv1alpha1.EnvVar) error {
	for _, v := range vars {
		if v.SecretRef == nil {
			continue
		}
		if err := CheckEnvSecretRef(v.Name, v.SecretRef.Name); err != nil {
			return err
		}
	}
	return nil
}
