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
	"testing"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/framework"
)

// Which credential each container of a build or a scan pod holds (#424).
//
// These are the tests that keep the split honest: every pod here runs code
// the platform did not write beside code that has to push, and the whole of
// the fix is which volume each container mounts.

// containerNamed finds a container in a pod spec by name.
func containerNamed(t *testing.T, spec corev1.PodSpec, name string) corev1.Container {
	t.Helper()
	for _, container := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		if container.Name == name {
			return container
		}
	}
	t.Fatalf("no container named %q in %v", name, phaseNames(append(spec.InitContainers, spec.Containers...)))
	return corev1.Container{}
}

// phaseNames is the containers' names in order, for a failure worth reading.
func phaseNames(containers []corev1.Container) []string {
	names := make([]string, 0, len(containers))
	for _, container := range containers {
		names = append(names, container.Name)
	}
	return names
}

// volumeSecret is the Secret one named volume projects, "" for a volume that
// projects none — the empty directory an anonymous pull gets.
func volumeSecret(t *testing.T, spec corev1.PodSpec, name string) string {
	t.Helper()
	for _, volume := range spec.Volumes {
		if volume.Name != name {
			continue
		}
		if volume.Secret == nil {
			return ""
		}
		return volume.Secret.SecretName
	}
	t.Fatalf("no volume named %q", name)
	return ""
}

// mountsVolume reports whether a container mounts the named volume.
func mountsVolume(container corev1.Container, name string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == name {
			return true
		}
	}
	return false
}

// assertCredential is the whole assertion in one place: this container holds
// this credential, that one, or neither.
func assertCredential(t *testing.T, spec corev1.PodSpec, container, wantVolume string) {
	t.Helper()
	got := containerNamed(t, spec, container)
	for _, volume := range []string{volumeDockerConfig, volumeDockerConfigRead} {
		mounted := mountsVolume(got, volume)
		if mounted != (volume == wantVolume) {
			t.Errorf("container %q mounts %q = %v, want %v", container, volume, mounted, volume == wantVolume)
		}
	}
	// A container with no credential is not told to look for one either.
	hasDockerConfig := envValue(got.Env, "DOCKER_CONFIG") != ""
	if hasDockerConfig != (wantVolume != "") {
		t.Errorf("container %q has DOCKER_CONFIG = %v, want %v", container, hasDockerConfig, wantVolume != "")
	}
}

func TestBuildpacksPodKeepsTheCredentialOutOfTheRepositorysOwnBuild(t *testing.T) {
	project, build := buildFixtures()
	pod := buildpacksPod(project, build, testWebPlan(project, build), framework.Framework{}, nil,
		credentialsWithRead("kitchen-registry-registry", "kitchen-registry-registry-read"), "", 0).Spec

	// detect and build run the buildpacks, which run the repository's own
	// build: `npm install` and whatever its lifecycle scripts do. Neither
	// mounts a registry credential at all.
	assertCredential(t, pod, "detector", "")
	assertCredential(t, pod, "builder", "")

	// The phases that read the registry read with the credential that
	// cannot push; the one that pushes is the only one holding the one that
	// can.
	assertCredential(t, pod, "analyzer", volumeDockerConfigRead)
	assertCredential(t, pod, "restorer", volumeDockerConfigRead)
	assertCredential(t, pod, "exporter", volumeDockerConfig)

	// And the clone, which needs neither.
	assertCredential(t, pod, "clone", "")

	if got := volumeSecret(t, pod, volumeDockerConfig); got != "kitchen-registry-registry" {
		t.Errorf("the pushing volume names %q", got)
	}
	if got := volumeSecret(t, pod, volumeDockerConfigRead); got != "kitchen-registry-registry-read" {
		t.Errorf("the reading volume names %q", got)
	}

	// The push is still the push: the digest comes back through the pod's
	// own container, which imageWithDigest reads and which never looks at
	// init containers.
	exporter := pod.Containers[0]
	if exporter.Name != "exporter" {
		t.Fatalf("the pod's container is %q, not the phase that pushes", exporter.Name)
	}
}

// A registry that issues no read-only credential is not a build that fails:
// every phase reads with the connection's own, which is what it did before.
func TestBuildpacksPodFallsBackToTheConnectionsOwnCredential(t *testing.T) {
	project, build := buildFixtures()
	pod := buildpacksPod(project, build, testWebPlan(project, build), framework.Framework{}, nil,
		credentialsWithRead("kitchen-registry-ghcr", ""), "", 0).Spec

	for _, volume := range []string{volumeDockerConfig, volumeDockerConfigRead} {
		if got := volumeSecret(t, pod, volume); got != "kitchen-registry-ghcr" {
			t.Errorf("volume %q names %q, want the connection's own credential", volume, got)
		}
	}
	// The phases that run the repository's code still hold nothing, which is
	// the half of this that does not depend on the registry.
	assertCredential(t, pod, "detector", "")
	assertCredential(t, pod, "builder", "")
}

func TestGatePodCannotPush(t *testing.T) {
	project, build := buildFixtures()
	gate := kitchenv1alpha1.QualityGateSpec{Name: "trivy", Image: "aquasec/trivy:0.60.0"}
	job := gateJob("shop-bld-1-trivy", "kitchen-app-shop", build, project, gate,
		credentialsWithRead("kitchen-registry-registry", "kitchen-registry-registry-read"),
		"registry.example.com/shop@sha256:feed", "ghcr.io/bermos/kitchen-qualitygate:v1")
	pod := job.Spec.Template.Spec

	// The gate is an image somebody else wrote. It reads the artifact and
	// writes a file; the publisher is what puts the file back in the
	// registry.
	assertCredential(t, pod, "gate", volumeDockerConfigRead)
	assertCredential(t, pod, "publish", volumeDockerConfig)
	if got := volumeSecret(t, pod, volumeDockerConfigRead); got != "kitchen-registry-registry-read" {
		t.Errorf("the gate reads with %q", got)
	}
}

func TestObservedSBOMPodKeepsTheGeneratorFromPushing(t *testing.T) {
	project, build := buildFixtures()
	kitchen := &kitchenv1alpha1.Kitchen{}
	job := observedSBOMJob("shop-bld-1-sbom", "kitchen-app-shop", build, project, kitchen,
		credentialsWithRead("kitchen-registry-registry", "kitchen-registry-registry-read"),
		"registry.example.com/shop@sha256:feed", "anchore/syft:v1.20.0",
		"ghcr.io/bermos/kitchen-qualitygate:v1")
	pod := job.Spec.Template.Spec

	assertCredential(t, pod, "sbom", volumeDockerConfigRead)
	assertCredential(t, pod, "publish", volumeDockerConfig)
}

func TestRescanPodKeepsTheScannerFromPushing(t *testing.T) {
	project, _ := buildFixtures()
	env := &kitchenv1alpha1.Environment{}
	env.Name = "production"
	release := &kitchenv1alpha1.Release{}
	release.Name = "shop-rel-1"
	scanner := kitchenv1alpha1.VulnerabilityScannerSpec{Name: "grype", Image: "anchore/grype:v0.90.0"}
	job := rescanJob("shop-production-rescan", "kitchen-app-shop", project, env, release, scanner,
		"registry.example.com/shop@sha256:feed",
		credentialsWithRead("kitchen-registry-registry", "kitchen-registry-registry-read"),
		"ghcr.io/bermos/kitchen-operator:v1")
	pod := job.Spec.Template.Spec

	// The scanner is the operator's choice of image; the two containers
	// either side of it are the platform's own, and the last of them writes
	// the findings back.
	assertCredential(t, pod, "scan", volumeDockerConfigRead)
	assertCredential(t, pod, "sbom", volumeDockerConfig)
	assertCredential(t, pod, "publish", volumeDockerConfig)
}

func TestReadCredentialSecretName(t *testing.T) {
	if got := readCredentialSecretName("kitchen-connection-registry"); got != "kitchen-connection-registry-read" {
		t.Errorf("readCredentialSecretName() = %q", got)
	}
	// A pod that pulls anonymously names no Secret, and the read-only one
	// beside nothing is nothing: naming a Secret that does not exist would
	// keep the pod from starting at all.
	if got := readCredentialSecretName(""); got != "" {
		t.Errorf("readCredentialSecretName(\"\") = %q", got)
	}
	if credentials := (credentialsWithRead("", "")); credentials.scoped() {
		t.Error("an anonymous pull reports a scoped credential")
	}
	if !credentialsWithRead("creds", "creds-read").scoped() {
		t.Error("a read-only credential is not reported as scoped")
	}
	if credentialsWithRead("creds", "").scoped() {
		t.Error("the fallback is reported as scoped")
	}
}
