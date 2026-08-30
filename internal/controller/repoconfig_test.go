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
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/repoconfig"
)

// What the commit's own kitchen.json reaches. The merge itself is
// internal/repoconfig's, and tested there; this is the wiring — that the file
// the build read is what the build pod is actually built from, rather than a
// record on the status nothing consults.

// withConfig records on the build the file a commit was found to carry.
func withConfig(t *testing.T, build *kitchenv1alpha1.Build, file string) {
	t.Helper()
	config, err := repoconfig.Parse([]byte(file))
	if err != nil {
		t.Fatalf("the fixture is not a valid %s: %v", repoconfig.FileName, err)
	}
	config.Path = repoconfig.FileName
	build.Status.Config = config
}

func TestTheCommitsFileDecidesTheStrategy(t *testing.T) {
	project, build := buildFixtures()
	project.Spec.Build.Strategy = kitchenv1alpha1.BuildStrategyBuildpacks

	// The project asked for buildpacks; the commit that added a Dockerfile
	// says so in the same change.
	withConfig(t, build, `{"build": {"strategy": "dockerfile"}}`)
	if got := resolveStrategy(project, build, ""); got != kitchenv1alpha1.BuildStrategyDockerfile {
		t.Errorf("strategy = %q, want the file's", got)
	}

	// A file that says nothing leaves the project's answer alone.
	build.Status.Config = nil
	if got := resolveStrategy(project, build, ""); got != kitchenv1alpha1.BuildStrategyBuildpacks {
		t.Errorf("strategy = %q, want the project's", got)
	}
}

// "auto" in the file means what "auto" on the project means — the platform's
// default, and then detection — rather than the literal string reaching the
// switch that refuses an unsupported strategy.
func TestAutoInTheFileStillMeansTheInstallationsDefault(t *testing.T) {
	project, build := buildFixtures()
	project.Spec.Build.Strategy = kitchenv1alpha1.BuildStrategyDockerfile
	withConfig(t, build, `{"build": {"strategy": "auto"}}`)

	if got := resolveStrategy(project, build, kitchenv1alpha1.BuildStrategyBuildpacks); got != kitchenv1alpha1.BuildStrategyBuildpacks {
		t.Errorf("strategy = %q, want the platform default", got)
	}
	if got := resolveStrategy(project, build, ""); got != kitchenv1alpha1.BuildStrategyAuto {
		t.Errorf("strategy = %q, want auto with no platform default", got)
	}
}

func TestTheCommitsFileDecidesWhichDockerfileIsBuilt(t *testing.T) {
	project, build := buildFixtures()
	project.Spec.Build.DockerfilePath = "Dockerfile"
	withConfig(t, build, `{"build": {"dockerfilePath": "docker/prod.Dockerfile"}}`)

	if got := buildDockerfilePath(project, build); got != "docker/prod.Dockerfile" {
		t.Errorf("dockerfile = %q, want the file's", got)
	}

	// And it reaches the pod, which is the only place it matters: BuildKit is
	// told a filename, and a build that recorded the file and then built the
	// project's Dockerfile would be worse than not reading it at all.
	pod := dockerfilePod(project, build, nil, "creds", "", "registry.example.com/shop:abc123",
		kitchenv1alpha1.BuildAttestationSpec{})
	if args := strings.Join(pod.Spec.Containers[0].Args, " "); !strings.Contains(args, "filename=docker/prod.Dockerfile") {
		t.Errorf("the builder was not told which Dockerfile to use: %s", args)
	}
}

func TestABuildThatReadNoFileBuildsExactlyWhatItDidBefore(t *testing.T) {
	project, build := buildFixtures()
	project.Spec.Build.DockerfilePath = "Containerfile"

	if got := buildDockerfilePath(project, build); got != "Containerfile" {
		t.Errorf("dockerfile = %q, want the project's", got)
	}
}
