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

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/framework"
)

func TestResolveStrategy(t *testing.T) {
	cases := map[string]struct {
		project         kitchenv1alpha1.BuildStrategy
		platformDefault kitchenv1alpha1.BuildStrategy
		want            kitchenv1alpha1.BuildStrategy
	}{
		"the project decides for itself": {
			project:         kitchenv1alpha1.BuildStrategyBuildpacks,
			platformDefault: kitchenv1alpha1.BuildStrategyDockerfile,
			want:            kitchenv1alpha1.BuildStrategyBuildpacks,
		},
		"auto takes the platform's default": {
			project:         kitchenv1alpha1.BuildStrategyAuto,
			platformDefault: kitchenv1alpha1.BuildStrategyBuildpacks,
			want:            kitchenv1alpha1.BuildStrategyBuildpacks,
		},
		"a project written before the field existed takes it too": {
			platformDefault: kitchenv1alpha1.BuildStrategyBuildpacks,
			want:            kitchenv1alpha1.BuildStrategyBuildpacks,
		},
		"auto all the way down is left to detection": {
			project:         kitchenv1alpha1.BuildStrategyAuto,
			platformDefault: kitchenv1alpha1.BuildStrategyAuto,
			want:            kitchenv1alpha1.BuildStrategyAuto,
		},
		"and so is a platform that has said nothing": {
			project: kitchenv1alpha1.BuildStrategyAuto,
			want:    kitchenv1alpha1.BuildStrategyAuto,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			project := &kitchenv1alpha1.Project{
				Spec: kitchenv1alpha1.ProjectSpec{
					Build: kitchenv1alpha1.ProjectBuildSpec{Strategy: tc.project},
				},
			}
			if got := resolveStrategy(project, &kitchenv1alpha1.Build{}, tc.platformDefault); got != tc.want {
				t.Errorf("resolveStrategy() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDigestFromTerminationMessage(t *testing.T) {
	cases := map[string]struct {
		message string
		want    string
	}{
		"BuildKit's metadata is JSON": {
			message: `{"containerimage.digest":"sha256:feedface","containerimage.config.digest":"sha256:0ther"}`,
			want:    "sha256:feedface",
		},
		"the lifecycle's report is TOML": {
			message: "[build]\n[image]\n  tags = [\"reg/app:abc\"]\n  digest = \"sha256:cafed00d\"\n  manifest-size = 1234\n",
			want:    "sha256:cafed00d",
		},
		"a builder that said nothing useful": {
			message: "Error: failed to build: exit status 1\n",
		},
		"an empty message": {},
		"JSON without a digest": {
			message: `{"containerimage.config.digest":"sha256:0ther"}`,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := digestFromTerminationMessage(tc.message); got != tc.want {
				t.Errorf("digestFromTerminationMessage() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildRootDir(t *testing.T) {
	cases := map[string]string{
		"":            "",
		".":           "",
		"apps/shop":   "apps/shop",
		"/apps/shop/": "apps/shop",
	}
	for root, want := range cases {
		t.Run("root "+root, func(t *testing.T) {
			project := &kitchenv1alpha1.Project{
				Spec: kitchenv1alpha1.ProjectSpec{
					Build: kitchenv1alpha1.ProjectBuildSpec{RootDirectory: root},
				},
			}
			if got := buildRootDir(project); got != want {
				t.Errorf("buildRootDir(%q) = %q, want %q", root, got, want)
			}
		})
	}
}

// Where the launch point actually has to be for a buildpack to read it: a
// file in the platform directory, not a variable on the container (#468).
//
// The lifecycle rebuilds a buildpack's environment from a fixed include list
// and drops everything else it inherited, so this is the difference between
// the pair being set and the pair arriving. The two phases that run
// buildpacks mount that directory; the three that do not, do not.
func TestBuildpacksPodTellsEveryPhaseWhereTheServerWillBe(t *testing.T) {
	nuxt, ok := framework.Detect(framework.Signals{
		Files:       []string{"package.json"},
		PackageJSON: []byte(`{"dependencies":{"nuxt":"3.14.0"},"scripts":{"build":"nuxt build"}}`),
	})
	if !ok {
		t.Fatal("the fixture manifest detected nothing")
	}

	// What the reconciler writes beside the Job is what the buildpacks read.
	files := buildPlatformEnv(nuxt, 0)
	for name, want := range map[string]string{
		"BP_LAUNCHPOINT":        ".output/server/index.mjs",
		"BP_VERIFY_LAUNCHPOINT": "false",
	} {
		if files[name] != want {
			t.Errorf("the platform directory has %s=%q, wanted %q", name, files[name], want)
		}
	}

	plan := buildPlan{
		Strategy: kitchenv1alpha1.BuildStrategyBuildpacks,
		Tag:      "registry.test/app:abc",
		Job:      "build-abc",
	}
	pod := buildpacksPod(
		&kitchenv1alpha1.Project{}, &kitchenv1alpha1.Build{}, plan,
		framework.Framework{}, nil, registryCredentialsForPod{}, "",
	)

	// The one volume the directory comes from, and the object it names.
	var source *corev1.Volume
	for i, v := range pod.Spec.Volumes {
		if v.Name == volumePlatformEnv {
			source = &pod.Spec.Volumes[i]
		}
	}
	if source == nil {
		t.Fatal("the pod mounts no platform directory")
	}
	if source.ConfigMap == nil || source.ConfigMap.Name != buildPlatformEnvName(plan.Job) {
		t.Errorf("the platform directory comes from %+v, want the Job's own ConfigMap", source)
	}

	// Which phases read it, and which are pointed at it. They are the same
	// two, and they are the two that run somebody else's code.
	wantsIt := map[string]bool{"detector": true, "builder": true}
	containers := append(append([]corev1.Container{}, pod.Spec.InitContainers...), pod.Spec.Containers...)
	phases := 0
	for _, c := range containers {
		if c.Image == GitCloneImage {
			continue
		}
		phases++
		mounted := ""
		for _, m := range c.VolumeMounts {
			if m.Name == volumePlatformEnv {
				mounted = m.MountPath
			}
		}
		told := slices.Contains(c.Args, "-platform="+buildpacksPlatformDir)
		if wantsIt[c.Name] {
			if mounted != buildpacksPlatformDir+"/env" {
				t.Errorf("%s reads its buildpack configuration from %q", c.Name, mounted)
			}
			if !told {
				t.Errorf("%s mounts the platform directory and is not pointed at it: %v", c.Name, c.Args)
			}
			continue
		}
		if mounted != "" {
			t.Errorf("%s runs no buildpack and mounts the platform directory at %q", c.Name, mounted)
		}
		if told {
			// The other three phases do not define the flag at all, so it
			// is not a redundant argument but an unknown one.
			t.Errorf("%s is passed -platform, which it does not accept: %v", c.Name, c.Args)
		}
	}
	if phases != 5 {
		t.Errorf("the pod ran %d lifecycle phases, want 5", phases)
	}

	// And nothing is told any of it on the container, where the lifecycle
	// would drop it before a buildpack ever saw it.
	for _, c := range containers {
		for _, e := range c.Env {
			if _, isBuildpacks := files[e.Name]; isBuildpacks {
				t.Errorf("%s is told %s as an environment variable, where no buildpack reads it", c.Name, e.Name)
			}
		}
	}
}
