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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
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
			if got := resolveStrategy(project, tc.platformDefault); got != tc.want {
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
