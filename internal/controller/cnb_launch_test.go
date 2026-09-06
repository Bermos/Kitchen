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
	"slices"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What a workload is started with, under each of the two strategies (#440). A
// Dockerfile image is started by replacing its entrypoint; a buildpacks image
// is started *through* the entrypoint it has, because the launcher is what
// puts the runtime the command needs on PATH.
func TestLaunchCommand(t *testing.T) {
	for name, tc := range map[string]struct {
		strategy kitchenv1alpha1.BuildStrategy
		command  []string
		args     []string

		wantCommand []string
		wantArgs    []string
	}{
		"a dockerfile image is started exactly as declared": {
			strategy:    kitchenv1alpha1.BuildStrategyDockerfile,
			command:     []string{"node", "scripts/migrate.mjs"},
			args:        []string{"--verbose"},
			wantCommand: []string{"node", "scripts/migrate.mjs"},
			wantArgs:    []string{"--verbose"},
		},
		"a release that recorded no strategy is started exactly as declared": {
			command:     []string{"./server"},
			args:        []string{"--config=prod.toml"},
			wantCommand: []string{"./server"},
			wantArgs:    []string{"--config=prod.toml"},
		},
		"a buildpacks image runs the command through the launcher": {
			strategy:    kitchenv1alpha1.BuildStrategyBuildpacks,
			command:     []string{"node", "scripts/migrate.mjs"},
			wantCommand: []string{CNBLauncherPath},
			wantArgs:    []string{"node", "scripts/migrate.mjs"},
		},
		"a buildpacks image runs command and arguments as one launcher argument list": {
			strategy:    kitchenv1alpha1.BuildStrategyBuildpacks,
			command:     []string{"node", "worker.js"},
			args:        []string{"--queue=default"},
			wantCommand: []string{CNBLauncherPath},
			wantArgs:    []string{"node", "worker.js", "--queue=default"},
		},
		"arguments alone still name the launcher, so a default process cannot swallow them": {
			strategy:    kitchenv1alpha1.BuildStrategyBuildpacks,
			args:        []string{"worker"},
			wantCommand: []string{CNBLauncherPath},
			wantArgs:    []string{"worker"},
		},
		"a buildpacks image with nothing declared starts itself": {
			strategy: kitchenv1alpha1.BuildStrategyBuildpacks,
		},
		"a dockerfile image with nothing declared starts itself": {
			strategy: kitchenv1alpha1.BuildStrategyDockerfile,
		},
	} {
		t.Run(name, func(t *testing.T) {
			command, args := launchCommand(tc.strategy, tc.command, tc.args)
			if !slices.Equal(command, tc.wantCommand) {
				t.Errorf("command = %q, want %q", command, tc.wantCommand)
			}
			if !slices.Equal(args, tc.wantArgs) {
				t.Errorf("args = %q, want %q", args, tc.wantArgs)
			}
		})
	}
}

// The strategy is a fact about one image, so a unit whose workload was built
// the other way is started the other way. A workload with no image of its own
// runs the release's, and so the release's strategy.
func TestReleaseStrategyFor(t *testing.T) {
	release := &kitchenv1alpha1.Release{Spec: kitchenv1alpha1.ReleaseSpec{
		Image:    "registry.example.com/shop@sha256:abc",
		Strategy: kitchenv1alpha1.BuildStrategyBuildpacks,
		Workloads: []kitchenv1alpha1.WorkloadImage{
			{Name: "api", Image: "registry.example.com/shop-api@sha256:def",
				Strategy: kitchenv1alpha1.BuildStrategyDockerfile},
			// An image nothing here built: acquired, started by its own
			// entrypoint, and never handed to a launcher it has not got.
			{Name: "cache", Image: "docker.io/library/redis@sha256:aaa"},
		},
	}}

	for workload, want := range map[string]kitchenv1alpha1.BuildStrategy{
		kitchenv1alpha1.WebProcessName: kitchenv1alpha1.BuildStrategyBuildpacks,
		"worker":                       kitchenv1alpha1.BuildStrategyBuildpacks,
		"api":                          kitchenv1alpha1.BuildStrategyDockerfile,
		"cache":                        "",
	} {
		if got := release.StrategyFor(workload); got != want {
			t.Errorf("StrategyFor(%q) = %q, want %q", workload, got, want)
		}
	}
}

// Every workload of a buildpacks unit is started the same way — a worker, a
// service, a scheduled run and a deploy task all run through processPodSpec,
// which is the one place the question is answered for all four.
func TestProcessPodSpecSendsBuildpacksCommandsThroughTheLauncher(t *testing.T) {
	project := &kitchenv1alpha1.Project{Spec: kitchenv1alpha1.ProjectSpec{
		Registry: &kitchenv1alpha1.RegistrySpec{ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "harbor"}},
	}}
	process := kitchenv1alpha1.ProcessSpec{
		Name: "migrate", Type: kitchenv1alpha1.ProcessTask,
		Command: []string{"node", "scripts/migrate.mjs"},
	}

	for name, tc := range map[string]struct {
		release *kitchenv1alpha1.Release

		wantCommand []string
		wantArgs    []string
	}{
		"buildpacks": {
			release: &kitchenv1alpha1.Release{Spec: kitchenv1alpha1.ReleaseSpec{
				Image:    "registry.example.com/app@sha256:abc",
				Strategy: kitchenv1alpha1.BuildStrategyBuildpacks,
			}},
			wantCommand: []string{CNBLauncherPath},
			wantArgs:    []string{"node", "scripts/migrate.mjs"},
		},
		"dockerfile": {
			release: &kitchenv1alpha1.Release{Spec: kitchenv1alpha1.ReleaseSpec{
				Image:    "registry.example.com/app@sha256:abc",
				Strategy: kitchenv1alpha1.BuildStrategyDockerfile,
			}},
			wantCommand: []string{"node", "scripts/migrate.mjs"},
		},
		// The workload's own image was built from a Dockerfile even though
		// the unit's was not, and it keeps its own answer.
		"a workload built the other way": {
			release: &kitchenv1alpha1.Release{Spec: kitchenv1alpha1.ReleaseSpec{
				Image:    "registry.example.com/app@sha256:abc",
				Strategy: kitchenv1alpha1.BuildStrategyBuildpacks,
				Workloads: []kitchenv1alpha1.WorkloadImage{{
					Name: "migrate", Image: "registry.example.com/app-migrate@sha256:def",
					Strategy: kitchenv1alpha1.BuildStrategyDockerfile,
				}},
			}},
			wantCommand: []string{"node", "scripts/migrate.mjs"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			spec := processPodSpec("env", tc.release, project, nil, process, nil, podInit{})
			container := spec.Containers[0]
			if !slices.Equal(container.Command, tc.wantCommand) {
				t.Errorf("command = %q, want %q", container.Command, tc.wantCommand)
			}
			if !slices.Equal(container.Args, tc.wantArgs) {
				t.Errorf("args = %q, want %q", container.Args, tc.wantArgs)
			}
		})
	}
}

// What a workload of a unit was built with, read back off the Build the way
// the Release freezes it: the workload's own where it declared a build, and
// the unit's otherwise.
func TestBuildStrategyFor(t *testing.T) {
	build := &kitchenv1alpha1.Build{Status: kitchenv1alpha1.BuildStatus{
		Strategy: kitchenv1alpha1.BuildStrategyBuildpacks,
		Workloads: []kitchenv1alpha1.WorkloadBuildStatus{
			{Name: "api", Strategy: kitchenv1alpha1.BuildStrategyDockerfile},
		},
	}}

	for workload, want := range map[string]kitchenv1alpha1.BuildStrategy{
		"":       kitchenv1alpha1.BuildStrategyBuildpacks,
		"web":    kitchenv1alpha1.BuildStrategyBuildpacks,
		"worker": kitchenv1alpha1.BuildStrategyBuildpacks,
		"api":    kitchenv1alpha1.BuildStrategyDockerfile,
	} {
		if got := buildStrategyFor(build, workload); got != want {
			t.Errorf("buildStrategyFor(%q) = %q, want %q", workload, got, want)
		}
	}
}

// Whether the project supplies a command anywhere, which is what decides
// whether an image that declares no process type can start at all.
func TestWorkloadStart(t *testing.T) {
	snapshot := kitchenv1alpha1.ConfigSnapshot{
		Runtime: kitchenv1alpha1.RuntimeSpec{PreviewArgs: []string{"--config=fake.toml"}},
		Processes: []kitchenv1alpha1.ProcessSpec{
			{Name: "worker", Type: kitchenv1alpha1.ProcessWorker, Command: []string{"node", "worker.js"}},
			{Name: "quiet", Type: kitchenv1alpha1.ProcessWorker},
		},
	}

	// A preview's arguments count: the project supplies a command somewhere,
	// so refusing the whole unit would refuse a deploy that works.
	if got := workloadStart(snapshot, kitchenv1alpha1.WebProcessName); len(got) == 0 {
		t.Error("the web process's preview arguments were not counted as a command")
	}
	if got := workloadStart(snapshot, "worker"); len(got) == 0 {
		t.Error("the worker's own command was not read")
	}
	if got := workloadStart(snapshot, "quiet"); len(got) != 0 {
		t.Errorf("a workload that declares nothing supplies nothing, got %q", got)
	}
	if got := workloadStart(snapshot, "absent"); len(got) != 0 {
		t.Errorf("a workload the snapshot does not carry supplies nothing, got %q", got)
	}
}
