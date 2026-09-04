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
	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What a pod pulls with (#307).
//
// Until a workload could run an image this platform did not build there was
// one answer: the credential the build pushed under. `registrySecretName` is
// still that name, and for everything the platform builds it is still the
// answer — a registry that wanted a credential to push wants one to pull.
//
// A vendored image is pulled from somewhere the platform never writes, and
// usually from somewhere it holds no account at all. So the question is asked
// per workload rather than per project, and it has three answers:
//
//   - The workload's own image names a Connection: pull with that.
//   - The workload's own image names none: it is public, and the pod pulls
//     anonymously. Naming a Secret that does not exist would be worse than
//     naming nothing — the kubelet reports it and the pod never starts.
//   - The workload has no image of its own: it runs what the project built,
//     and pulls with what the project pushed.
//
// It is one function because a Deployment, a CronJob and a scan pod all ask
// it, and three readings of "which credential" is how a build and a pull come
// to disagree — which is a pod in ImagePullBackOff behind a build, a release
// and a route that all read as healthy.

// workloadImageSource is the vendored image one workload declares, nil for a
// workload running something this platform built.
//
// `processes` is the process list to look the workload up in, and it is the
// *Release's* snapshot wherever one is being materialized: what an
// environment runs is what its release declared, so a workload the project
// has since changed is still pulled the way that release said. The web
// process is not in any process list — it is `spec.runtime` — so its image is
// the project's own source.
func workloadImageSource(
	project *kitchenv1alpha1.Project,
	processes []kitchenv1alpha1.ProcessSpec,
	workload string,
) *kitchenv1alpha1.ImageSourceSpec {
	if workload == "" || workload == kitchenv1alpha1.WebProcessName {
		return project.Spec.Source.Image
	}
	for i := range processes {
		if processes[i].Name == workload {
			return processes[i].Image
		}
	}
	return nil
}

// pullSecretName is the Secret in the application namespace one workload's
// pods pull with, empty when they pull anonymously.
func pullSecretName(
	project *kitchenv1alpha1.Project,
	processes []kitchenv1alpha1.ProcessSpec,
	workload string,
) string {
	if image := workloadImageSource(project, processes, workload); image != nil {
		if connection := image.PullConnection(); connection != "" {
			return registrySecretName(connection)
		}
		return ""
	}
	if connection := project.Spec.RegistryConnection(); connection != "" {
		return registrySecretName(connection)
	}
	return ""
}

// pullSecretsFor is that answer as a pod spec wants it: a list of one, or no
// list at all.
func pullSecretsFor(
	project *kitchenv1alpha1.Project,
	processes []kitchenv1alpha1.ProcessSpec,
	workload string,
) []corev1.LocalObjectReference {
	name := pullSecretName(project, processes, workload)
	if name == "" {
		return nil
	}
	return []corev1.LocalObjectReference{{Name: name}}
}
