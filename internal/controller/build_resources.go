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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// DefaultBuildCPU and DefaultBuildMemory are the ceiling a build runs
	// under when the Kitchen object names none. They are the CRD's defaults
	// spelled a second time on purpose: they are exported so the API can
	// report the ceiling a build would actually get, and a settings screen
	// showing an empty box where the platform is in fact holding a build to
	// 4Gi would be its own bug — the same reason DefaultBuildConcurrency is
	// exported.
	DefaultBuildCPU    = "2"
	DefaultBuildMemory = "4Gi"

	// buildOOMExitCode is what a container that was killed exits with:
	// 128 + SIGKILL. In a build pod under a memory limit that is the kernel's
	// out-of-memory killer and effectively nothing else, which matters
	// because the kubelet only says "OOMKilled" when the process it was
	// watching is the one that died — a builder whose child was killed exits
	// 137 with the reason "Error", and that is the common case for a
	// front-end build that runs out of memory inside a subprocess.
	buildOOMExitCode = 137

	// reasonOOMKilled is the kubelet's own word for it, kept unchanged
	// wherever it is what the pod said.
	reasonOOMKilled = "OOMKilled"
)

// applyBuildResources writes the platform's build ceiling onto every container
// of a build pod, as the request and the limit at once.
//
// Every container, init containers included: the pod is scheduled against the
// larger of the two sets, so a clone container with nothing declared would
// leave the pod's own reservation intact but its QoS class Burstable — and a
// clone that fetched a repository the size of the ceiling would be bounded by
// nothing. They never run at the same time, so the pod still reserves one
// ceiling rather than two.
//
// A resource left empty is left alone, which is the installation that has
// decided its builds are unbounded. A quantity that will not parse is logged
// and skipped rather than failing the build: admission and the API both refuse
// one, so reaching here means the object was written by something that did
// not, and a build refused for a setting the operator cannot see would be the
// worse of the two failures.
func applyBuildResources(
	ctx context.Context,
	spec *corev1.PodSpec,
	resources kitchenv1alpha1.BuildResourcesSpec,
) {
	requirements := buildRequirements(ctx, resources)
	if len(requirements.Limits) == 0 {
		return
	}
	for i := range spec.InitContainers {
		spec.InitContainers[i].Resources = *requirements.DeepCopy()
	}
	for i := range spec.Containers {
		spec.Containers[i].Resources = *requirements.DeepCopy()
	}
}

// buildRequirements is the ceiling as Kubernetes takes it: the same quantity
// as the request and as the limit, for each resource that names one.
func buildRequirements(
	ctx context.Context,
	resources kitchenv1alpha1.BuildResourcesSpec,
) corev1.ResourceRequirements {
	requirements := corev1.ResourceRequirements{}
	for _, ceiling := range []struct {
		name  corev1.ResourceName
		value string
	}{
		{corev1.ResourceCPU, resources.CPU},
		{corev1.ResourceMemory, resources.Memory},
	} {
		if ceiling.value == "" {
			continue
		}
		quantity, err := resource.ParseQuantity(ceiling.value)
		if err != nil {
			ctrl.LoggerFrom(ctx).Error(err, "the platform's build ceiling is not a quantity",
				"resource", string(ceiling.name), "value", ceiling.value)
			continue
		}
		if requirements.Requests == nil {
			requirements.Requests = corev1.ResourceList{}
			requirements.Limits = corev1.ResourceList{}
		}
		requirements.Requests[ceiling.name] = quantity
		requirements.Limits[ceiling.name] = quantity
	}
	return requirements
}

// outOfMemory reports whether a build failure is the memory ceiling rather
// than the repository.
//
// Two shapes of the same ending. The kubelet says OOMKilled when the container
// process itself was killed; when a child was killed and the builder merely
// died of it, the reason is the generic "Error" and all that is left is the
// exit code. Both are the build asking for more memory than a build may have,
// and "the build died and I do not know why" is what this exists to stop
// either of them reading as.
func outOfMemory(failure *kitchenv1alpha1.BuildFailureStatus) bool {
	if failure == nil {
		return false
	}
	if failure.Reason == reasonOOMKilled {
		return true
	}
	return failure.ExitCode != nil && *failure.ExitCode == buildOOMExitCode
}

// outOfMemoryMessage is what a build killed for its memory says on the Build,
// on the commit and in the dashboard: that it ran out of memory, what the
// ceiling was, and which of the two people can move it.
//
// It names the setting rather than only the number, because the fix belongs to
// whoever holds the platform and the person reading it is usually the one who
// pushed the commit. Either the build is holding more than it needs to, or the
// platform's ceiling is too low for what this repository builds — the message
// says both, since nothing here can tell which.
func outOfMemoryMessage(failure *kitchenv1alpha1.BuildFailureStatus, ceiling string) string {
	container := "the build"
	if failure != nil && failure.Container != "" {
		container = failure.Container
	}
	if ceiling == "" {
		ceiling = DefaultBuildMemory
	}
	return fmt.Sprintf(
		"%s ran out of memory: it reached the platform's %s build ceiling and was killed. "+
			"Either the build holds less at once, or an operator raises "+
			"builds.resources.memory on the Kitchen object — that ceiling times the build "+
			"concurrency is what the platform's builds may take from the cluster",
		container, ceiling)
}
