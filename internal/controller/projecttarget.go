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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Two questions more than one place asks about a project's environments, each
// answered exactly once — like registrySecretName, because two spellings of
// the same derivation is how the build-time break-glass came to look for its
// grant on an environment no staged project has.

// ProductionTargetEnvironmentName is where a project's production deployments
// ultimately land: the last stage's environment when the project declares a
// pipeline, and `<project>-production` when it does not. It is the scope a
// build-time break-glass exception is granted against, and the one
// environment an exception may name before its first build creates it —
// which is why the API reaches for it too (api imports controller).
//
// It is deliberately not the build's *entry* point: a staged build enters at
// stage one, and promoteOrFlip keeps that targeting of its own.
func ProductionTargetEnvironmentName(project *kitchenv1alpha1.Project) string {
	if pipeline := project.Spec.Promotion; pipeline != nil && len(pipeline.Stages) > 0 {
		return pipeline.Stages[len(pipeline.Stages)-1].Environment
	}
	return project.Name + "-production"
}

// DataClassRefusal is the one wording of issue #137's hard check: a release
// of a classified project must not land on an environment rated below the
// project's class, and an unrated environment receives no classified data at
// all. Empty means the flip is admissible.
//
// Every ungated flip path consults it — the build controller's fast path,
// the promotion reconciler's no-requirements branch, and the API's direct
// environment moves — so the refusal reads the same wherever it is met.
// Auto-created environments inherit the project's class at creation and are
// never refused by construction; only an environment somebody narrowed below
// its project refuses, which is the point. An environment that pins a policy
// bundle is judged by the engine instead, where dataclass-le-environment
// reports the same comparison as a named rule.
func DataClassRefusal(project *kitchenv1alpha1.Project, env *kitchenv1alpha1.Environment) string {
	if project == nil || !project.Spec.DataClass.Exceeds(env.Spec.DataClass) {
		return ""
	}
	rating := "rated " + string(env.Spec.DataClass)
	if !env.Spec.DataClass.Classified() {
		rating = "unrated, and an unrated environment receives no classified data"
	}
	return fmt.Sprintf("project %s is classified %s but environment %s is %s: "+
		"classify the environment (dataClass on its requirements endpoint) at or above the "+
		"project's class, or lower the project's class",
		project.Name, project.Spec.DataClass, env.Name, rating)
}
