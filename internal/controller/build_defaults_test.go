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
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/framework"
)

// The two things a project may leave to the platform, and the precedence they
// are filled in under: the detected framework answers a blank, and never
// overrules something the project said (#468).
func TestRuntimeForFillsBlanksOnly(t *testing.T) {
	for name, tc := range map[string]struct {
		runtime   kitchenv1alpha1.RuntimeSpec
		framework string

		wantPort    int32
		wantCommand []string
	}{
		"a project that declares nothing takes the framework's port and command": {
			framework:   framework.Nuxt,
			wantPort:    3000,
			wantCommand: []string{"node", ".output/server/index.mjs"},
		},
		"an explicit command wins": {
			runtime:     kitchenv1alpha1.RuntimeSpec{Command: []string{"node", "server.mjs"}},
			framework:   framework.Nuxt,
			wantPort:    3000,
			wantCommand: []string{"node", "server.mjs"},
		},
		"an explicit port still wins, and the command is still filled in": {
			runtime:     kitchenv1alpha1.RuntimeSpec{Port: 4000},
			framework:   framework.Nuxt,
			wantPort:    4000,
			wantCommand: []string{"node", ".output/server/index.mjs"},
		},
		// The bug the early return left behind: a project that named a port
		// returned before anything else could be defaulted.
		"an explicit port does not suppress the command": {
			runtime:     kitchenv1alpha1.RuntimeSpec{Port: 8080},
			framework:   framework.SvelteKit,
			wantPort:    8080,
			wantCommand: []string{"node", "build"},
		},
		"a framework with no command of its own leaves the image to start itself": {
			framework: framework.Node,
			wantPort:  3000,
		},
		"a repository built from a Dockerfile is told nothing": {
			framework: framework.Dockerfile,
		},
		"a framework this operator does not know changes nothing": {
			runtime:   kitchenv1alpha1.RuntimeSpec{Port: 9000},
			framework: "something-a-newer-operator-detected",
			wantPort:  9000,
		},
	} {
		t.Run(name, func(t *testing.T) {
			project := &kitchenv1alpha1.Project{Spec: kitchenv1alpha1.ProjectSpec{Runtime: tc.runtime}}
			build := &kitchenv1alpha1.Build{
				Status: kitchenv1alpha1.BuildStatus{DetectedFramework: tc.framework},
			}
			got := runtimeFor(project, build)
			if got.Port != tc.wantPort {
				t.Errorf("port = %d, want %d", got.Port, tc.wantPort)
			}
			if !slices.Equal(got.Command, tc.wantCommand) {
				t.Errorf("command = %q, want %q", got.Command, tc.wantCommand)
			}
		})
	}
}

// The catalogue hands out one slice per framework; a Release that took it
// would be holding the platform's own table.
func TestRuntimeForDoesNotShareTheCatalogueSlice(t *testing.T) {
	build := &kitchenv1alpha1.Build{
		Status: kitchenv1alpha1.BuildStatus{DetectedFramework: framework.Nuxt},
	}
	first := runtimeFor(&kitchenv1alpha1.Project{}, build)
	first.Command[0] = "tampered"

	second := runtimeFor(&kitchenv1alpha1.Project{}, build)
	if second.Command[0] != "node" {
		t.Errorf("the second release starts with %q: the catalogue was written through",
			second.Command)
	}
}

// What the lifecycle is told, per framework: the buildpacks' own
// configuration as detection sorted it, and the platform's heap cap for every
// framework whose build runs under Node.
func TestFrameworkEnv(t *testing.T) {
	nuxt, _ := framework.Detect(framework.Signals{
		Files:       []string{"package.json"},
		PackageJSON: []byte(`{"dependencies":{"nuxt":"3.14.0"},"scripts":{"build":"nuxt build"}}`),
	})
	python, _ := framework.Detect(framework.Signals{Files: []string{"requirements.txt"}})

	for name, tc := range map[string]struct {
		detected framework.Framework
		heapMiB  int64

		want []corev1.EnvVar
	}{
		"a node framework is capped, and the cap leads": {
			detected: nuxt,
			heapMiB:  3072,
			want: []corev1.EnvVar{
				{Name: "NODE_OPTIONS", Value: "--max-old-space-size=3072"},
				{Name: "BP_NODE_RUN_SCRIPTS", Value: "build"},
			},
		},
		"an installation with no ceiling caps nothing": {
			detected: nuxt,
			want:     []corev1.EnvVar{{Name: "BP_NODE_RUN_SCRIPTS", Value: "build"}},
		},
		"a framework that does not run node is not capped": {
			detected: python,
			heapMiB:  3072,
			want:     []corev1.EnvVar{},
		},
		"a build with no framework at all is told nothing": {
			heapMiB: 3072,
			want:    []corev1.EnvVar{},
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := frameworkEnv(tc.detected, tc.heapMiB)
			if !slices.Equal(got, tc.want) {
				t.Errorf("env = %v, want %v", got, tc.want)
			}
		})
	}
}

// The heap a Node build may hold, from the ceiling the platform already holds
// the pod to. V8 sizes its old space from the machine rather than from the
// cgroup, so without this a front-end build grows past the limit and is
// killed with exit 137 and no explanation (#468).
func TestBuildHeapMiB(t *testing.T) {
	for name, tc := range map[string]struct {
		memory string
		want   int64
	}{
		"the platform's default ceiling":   {memory: DefaultBuildMemory, want: 3072},
		"a ceiling in mebibytes":           {memory: "1Gi", want: 768},
		"a ceiling in decimal units":       {memory: "2G", want: 1430},
		"an installation with no ceiling":  {memory: "", want: 0},
		"a ceiling too small to divide":    {memory: "1M", want: 0},
		"a ceiling that is not a quantity": {memory: "plenty", want: 0},
	} {
		t.Run(name, func(t *testing.T) {
			got := buildHeapMiB(context.Background(),
				kitchenv1alpha1.BuildResourcesSpec{CPU: "2", Memory: tc.memory})
			if got != tc.want {
				t.Errorf("heap = %d MiB, want %d", got, tc.want)
			}
		})
	}
}
