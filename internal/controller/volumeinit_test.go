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
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/volumeinit"
)

// A volume that needs directories created and owned before the process starts
// (#348), rendered.
//
// What these cases hold up is the shape the issue asked for and nothing
// looser: typed steps the platform executes itself, in the workload's own pod,
// under the project's own posture, with no argv from any request anywhere in
// it — and a declaration that cannot be honoured refused before a pod exists
// rather than discovered as a workload that never becomes ready.

const testOperatorImage = "ghcr.io/bermos/kitchen:0.31.0"

// initTestRelease is a Release whose web process prepares one volume.
func initTestRelease(
	init []kitchenv1alpha1.VolumeInit,
	files []kitchenv1alpha1.ConfigFile,
	security *kitchenv1alpha1.SecuritySpec,
) *kitchenv1alpha1.Release {
	return &kitchenv1alpha1.Release{
		Spec: kitchenv1alpha1.ReleaseSpec{
			Image: "registry.example.com/app@sha256:abc",
			ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
				Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8123, Init: init, Security: security},
				Files:   files,
			},
		},
	}
}

func homeAssistant() []kitchenv1alpha1.VolumeInit {
	return []kitchenv1alpha1.VolumeInit{{
		Volume: "config",
		Directories: []kitchenv1alpha1.VolumeInitDirectory{
			{Path: "custom_components"},
			{Path: "secrets", Mode: "0700"},
		},
		Seed: []kitchenv1alpha1.VolumeInitSeed{
			{File: "configuration", Path: "configuration.yaml"},
		},
	}}
}

func configMount() []mountedVolume {
	return []mountedVolume{{
		claim: "config", process: kitchenv1alpha1.WebProcessName,
		claimName: "shop-config", mountPath: "/config", attachOnce: true,
	}}
}

func TestTheInitContainerRunsTypedStepsAndNoShell(t *testing.T) {
	release := initTestRelease(homeAssistant(),
		[]kitchenv1alpha1.ConfigFile{{Name: "configuration", Content: "default_config:\n"}}, nil)

	inits, err := buildVolumeInits(release, configMount(), "shop-production", testOperatorImage)
	if err != nil {
		t.Fatal(err)
	}
	init := inits[kitchenv1alpha1.WebProcessName]
	if !init.declared() {
		t.Fatal("the web process declares an init and nothing was rendered for it")
	}

	container := init.container
	if container.Image != testOperatorImage {
		t.Errorf("the steps run from %q, and the platform's own program is in the operator's image",
			container.Image)
	}
	// The whole of the argv, whatever the project declared. A second word
	// taken from a request is the thing the KEDA install job's rule forbids.
	if len(container.Command) != 1 || container.Command[0] != "/volume-init" {
		t.Errorf("the init container's command is %v, and it has to be the one fixed word", container.Command)
	}
	if len(container.Args) != 0 {
		t.Errorf("the init container takes arguments: %v", container.Args)
	}
	for _, word := range append(append([]string{}, container.Command...), container.Args...) {
		if strings.Contains(word, "sh") && strings.Contains(word, "-c") {
			t.Errorf("something shell-shaped reached the argv: %q", word)
		}
	}

	// Everything that varies is data the platform's own program reads.
	var plan volumeinit.Plan
	found := false
	for _, variable := range container.Env {
		if variable.Name != volumeinit.PlanVariable {
			continue
		}
		found = true
		if err := json.Unmarshal([]byte(variable.Value), &plan); err != nil {
			t.Fatalf("the plan is not readable: %v", err)
		}
	}
	if !found {
		t.Fatal("the plan does not reach the container at all")
	}
	if len(plan.Volumes) != 1 || plan.Volumes[0].MountPath != "/config" {
		t.Fatalf("the plan does not name the volume's own mount path: %+v", plan)
	}
	if len(plan.Volumes[0].Directories) != 2 || plan.Volumes[0].Directories[1].Mode != "0700" {
		t.Errorf("the directories did not reach the plan: %+v", plan.Volumes[0].Directories)
	}
	if len(plan.Volumes[0].Seeds) != 1 || plan.Volumes[0].Seeds[0].Path != "configuration.yaml" {
		t.Errorf("the seed did not reach the plan: %+v", plan.Volumes[0].Seeds)
	}
}

// The init container mounts the volume it prepares at the same path the
// application's container mounts it, and the platform's copy of what it seeds
// from, and nothing else.
func TestTheInitContainerMountsTheVolumeAndTheSeedsAndNothingElse(t *testing.T) {
	release := initTestRelease(homeAssistant(),
		[]kitchenv1alpha1.ConfigFile{{Name: "configuration", Content: "default_config:\n"}}, nil)
	inits, err := buildVolumeInits(release, configMount(), "shop-production", testOperatorImage)
	if err != nil {
		t.Fatal(err)
	}
	init := inits[kitchenv1alpha1.WebProcessName]

	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range init.container.VolumeMounts {
		mounts[mount.MountPath] = mount
	}
	if len(mounts) != 2 {
		t.Fatalf("the init container mounts %d things: %+v", len(mounts), init.container.VolumeMounts)
	}
	volume, mounted := mounts["/config"]
	if !mounted {
		t.Fatal("the init container does not mount the volume it is preparing")
	}
	if volume.Name != claimVolumeName("config") {
		t.Errorf("the init container mounts %q where the application mounts %q: two spellings of one "+
			"volume is how a relative path lands somewhere unintended", volume.Name, claimVolumeName("config"))
	}
	if volume.ReadOnly {
		t.Error("the volume it prepares is mounted read-only, so nothing it does can take effect")
	}
	seeds, mounted := mounts[volumeinit.SeedDir]
	if !mounted || !seeds.ReadOnly {
		t.Fatalf("the platform's copy of the seeded files is not mounted read-only: %+v", mounts)
	}

	// Items, not the whole object: a file this workload does not seed has no
	// business being readable in its init container.
	if init.seedVolume == nil || init.seedVolume.Projected == nil {
		t.Fatal("nothing projects the seeded file's content")
	}
	sources := init.seedVolume.Projected.Sources
	if len(sources) != 1 || sources[0].ConfigMap == nil {
		t.Fatalf("a plain file's content is the environment's ConfigMap: %+v", sources)
	}
	if sources[0].ConfigMap.Name != configFilesName("shop-production") {
		t.Errorf("the seed reads %q rather than the environment's own files", sources[0].ConfigMap.Name)
	}
	if len(sources[0].ConfigMap.Items) != 1 || sources[0].ConfigMap.Items[0].Key != "configuration" {
		t.Errorf("the projection is not narrowed to the seeded file: %+v", sources[0].ConfigMap.Items)
	}
}

// A secret file's content is never in a Release, so a seed from one reads the
// project's own Secret instead — and only the key it seeds.
func TestASecretFileIsSeededFromTheProjectsSecret(t *testing.T) {
	release := initTestRelease([]kitchenv1alpha1.VolumeInit{{
		Volume: "config",
		Seed:   []kitchenv1alpha1.VolumeInitSeed{{File: "app-ini", Path: "conf/app.ini"}},
	}}, []kitchenv1alpha1.ConfigFile{{Name: "app-ini", Secret: true}}, nil)

	inits, err := buildVolumeInits(release, configMount(), "shop-production", testOperatorImage)
	if err != nil {
		t.Fatal(err)
	}
	sources := inits[kitchenv1alpha1.WebProcessName].seedVolume.Projected.Sources
	if len(sources) != 1 || sources[0].Secret == nil {
		t.Fatalf("a secret file's content has to come from the Secret: %+v", sources)
	}
	if sources[0].Secret.Name != ProjectFilesName {
		t.Errorf("the seed reads the Secret %q", sources[0].Secret.Name)
	}
}

// It is the application's pod and the application's volume: the steps run
// under the posture the project declared, never under a relaxed one.
func TestTheInitContainerRunsUnderTheProjectsOwnPosture(t *testing.T) {
	security := &kitchenv1alpha1.SecuritySpec{
		RunAsNonRoot: true, RunAsUser: 1001, ReadOnlyRootFilesystem: true,
		DropCapabilities: []string{"ALL"}, FSGroup: 1001,
	}
	release := initTestRelease(homeAssistant(),
		[]kitchenv1alpha1.ConfigFile{{Name: "configuration"}}, security)
	inits, err := buildVolumeInits(release, configMount(), "shop-production", testOperatorImage)
	if err != nil {
		t.Fatal(err)
	}
	context := inits[kitchenv1alpha1.WebProcessName].container.SecurityContext
	if context == nil {
		t.Fatal("the init container runs under no posture at all")
	}
	if !ptr.Deref(context.RunAsNonRoot, false) || ptr.Deref(context.RunAsUser, 0) != 1001 {
		t.Errorf("the declared user did not reach the init container: %+v", context)
	}
	if !ptr.Deref(context.ReadOnlyRootFilesystem, false) {
		t.Error("the init container keeps a writable root filesystem the project asked to lose")
	}
	if ptr.Deref(context.AllowPrivilegeEscalation, true) {
		t.Error("the init container may escalate privilege where the application may not")
	}
	if context.Capabilities == nil || len(context.Capabilities.Drop) != 1 {
		t.Errorf("the dropped capabilities did not reach the init container: %+v", context.Capabilities)
	}
	// Ownership is the pod's, which is what makes a directory it creates
	// come out owned by the process that will use it — #347's field doing the
	// work, and the reason there is no `owner` to declare here.
	if ptr.Deref(podSecurityContext(security).FSGroup, 0) != 1001 {
		t.Error("the pod's fsGroup is what owns what the steps create")
	}
}

func TestADeclarationThatCannotBeHonouredIsRefusedBeforeAnyPodExists(t *testing.T) {
	cases := []struct {
		name   string
		init   []kitchenv1alpha1.VolumeInit
		files  []kitchenv1alpha1.ConfigFile
		mounts []mountedVolume
		image  string
		says   string
	}{
		{
			name:   "a volume this workload does not mount",
			init:   []kitchenv1alpha1.VolumeInit{{Volume: "media", Directories: []kitchenv1alpha1.VolumeInitDirectory{{Path: "a"}}}},
			mounts: configMount(),
			image:  testOperatorImage,
			says:   "does not mount it",
		},
		{
			name:  "a volume mounted read-only",
			init:  homeAssistant(),
			files: []kitchenv1alpha1.ConfigFile{{Name: "configuration"}},
			mounts: []mountedVolume{{
				claim: "config", process: kitchenv1alpha1.WebProcessName,
				claimName: "shop-config", mountPath: "/config", readOnly: true,
			}},
			image: testOperatorImage,
			says:  "read-only",
		},
		{
			name:   "a seed from a file this release does not carry",
			init:   homeAssistant(),
			mounts: configMount(),
			image:  testOperatorImage,
			says:   "does not carry",
		},
		{
			name:   "a platform with no image to run the steps from",
			init:   homeAssistant(),
			files:  []kitchenv1alpha1.ConfigFile{{Name: "configuration"}},
			mounts: configMount(),
			image:  "",
			says:   "without an operator image",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release := initTestRelease(tc.init, tc.files, nil)
			_, err := buildVolumeInits(release, tc.mounts, "shop-production", tc.image)
			if err == nil {
				t.Fatalf("%s was accepted, and the pod would come up against a volume nothing prepared", tc.name)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal reads %q and does not say %q", err, tc.says)
			}
			// Whatever went wrong, the sentence says which workload it was
			// about: a unit has four and the reader has to know which.
			if !strings.Contains(err.Error(), "web workload") {
				t.Errorf("the refusal does not name the workload: %v", err)
			}
		})
	}
}

// A project that has withdrawn its init loses the container, which is why it
// is written whole every reconcile rather than only where it was asked for.
func TestAWithdrawnInitTakesItsContainerOffTheWorkload(t *testing.T) {
	spec := &corev1.PodSpec{
		InitContainers: []corev1.Container{{Name: VolumeInitContainerName, Image: testOperatorImage}},
		Containers:     []corev1.Container{{Name: AppContainerName}},
	}
	volumeInitOnPod(spec, podInit{})
	if len(spec.InitContainers) != 0 {
		t.Fatalf("yesterday's init container is still on the workload: %+v", spec.InitContainers)
	}
}

// A step that fails has to leave the environment saying why, in the step's own
// words — which the kubelet copies out of the pod's termination log.
func TestAFailedStepIsReadOffThePod(t *testing.T) {
	said := `directory "data" on volume "config": mkdir /config/data: permission denied`
	pod := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: VolumeInitContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Message: said,
			}},
		}},
	}}
	if got := volumeInitFailure(pod); got != said {
		t.Errorf("the environment would report %q rather than the step's own words", got)
	}

	// A minute later the kubelet has put the container into backoff, and the
	// same failure is only in the last termination state. A reader arriving
	// then gets the same sentence.
	backoff := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  VolumeInitContainerName,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reasonCrashLoop}},
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1, Message: said,
			}},
		}},
	}}
	if got := volumeInitFailure(backoff); got != said {
		t.Errorf("a crash-looping init container reports %q", got)
	}

	// A step that succeeded is not a failure, and neither is a pod that has
	// not run one. An init container the kubelet refused outright is not one
	// either: that is a refused container like any other, reported by the
	// refusal pass that walks a pod's init statuses beside its containers.
	fine := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  VolumeInitContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}},
	}}
	if got := volumeInitFailure(fine); got != "" {
		t.Errorf("a finished init container reads as a failure: %q", got)
	}
	if got := volumeInitFailure(&corev1.Pod{}); got != "" {
		t.Errorf("a pod with no init container reads as a failure: %q", got)
	}

	// One that exited saying nothing still gets a sentence rather than
	// silence, because "not available" with no reason is the state this
	// feature exists to end.
	quiet := &corev1.Pod{Status: corev1.PodStatus{
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  VolumeInitContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 2}},
		}},
	}}
	if got := volumeInitFailure(quiet); !strings.Contains(got, "exited 2") {
		t.Errorf("a step that said nothing reports %q", got)
	}
}
