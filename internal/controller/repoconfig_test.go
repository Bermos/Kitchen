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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	// told a directory and a filename inside it, and a build that recorded the
	// file and then built the project's Dockerfile would be worse than not
	// reading it at all.
	pod := dockerfilePod(project, build, testWebPlan(project, build), nil, "creds", "",
		kitchenv1alpha1.BuildAttestationSpec{})
	args := strings.Join(pod.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--local dockerfile="+buildContextSourceDir+"/docker ") ||
		!strings.Contains(args, "--opt filename=prod.Dockerfile") {
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

// The project's posture is the ceiling, and what the file may not change is
// ignored rather than fatal — so the one thing that has to reach anybody is
// the Build saying which field it dropped (#431).
func TestABuildSaysWhichSecurityFieldsTheCeilingWouldNotTake(t *testing.T) {
	project, build := buildFixtures()
	project.Spec.Runtime.Security = &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true, RunAsUser: 1000}

	// A file that only tightens says nothing at all: a build with a condition
	// on every commit is noise rather than an answer.
	withConfig(t, build, `{"runtime": {"security": {"readOnlyRootFilesystem": true}}}`)
	noteIgnoredSecurity(t.Context(), build, project, build.Status.Config)
	if meta.FindStatusCondition(build.Status.Conditions, condConfigHonoured) != nil {
		t.Fatalf("a file that only tightens should be unremarkable: %+v", build.Status.Conditions)
	}

	withConfig(t, build, `{"runtime": {"security": {
	  "allowPrivilegeEscalation": true, "runAsUser": 65532
	}}}`)
	noteIgnoredSecurity(t.Context(), build, project, build.Status.Config)
	condition := meta.FindStatusCondition(build.Status.Conditions, condConfigHonoured)
	if condition == nil {
		t.Fatalf("nothing said the posture was ignored: %+v", build.Status.Conditions)
	}
	if condition.Status != metav1.ConditionFalse || condition.Reason != reasonSecurityCeiling {
		t.Errorf("condition = %s/%s, want False/%s", condition.Status, condition.Reason, reasonSecurityCeiling)
	}
	// Naming the fields is the whole of what it is for: "the file was
	// ignored" sends somebody to read the merge.
	for _, field := range []string{"allowPrivilegeEscalation", "runAsUser", repoconfig.FileName} {
		if !strings.Contains(condition.Message, field) {
			t.Errorf("the message does not name %s: %s", field, condition.Message)
		}
	}
	// And the build is not failed over it.
	if build.Status.Phase == kitchenv1alpha1.BuildFailed {
		t.Errorf("a posture the ceiling would not take failed the build")
	}

	// A workload's own block is named as the workload's, because "the file's
	// runtime.security" and "the file's worker" are two different lines to go
	// and look at.
	build.Status.Conditions = nil
	withConfig(t, build, `{"processes": [
	  {"name": "worker", "type": "worker", "command": ["node", "w.js"],
	   "security": {"allowPrivilegeEscalation": true}}
	]}`)
	noteIgnoredSecurity(t.Context(), build, project, build.Status.Config)
	condition = meta.FindStatusCondition(build.Status.Conditions, condConfigHonoured)
	if condition == nil {
		t.Fatalf("a workload's posture was ignored silently: %+v", build.Status.Conditions)
	}
	for _, phrase := range []string{"worker", "allowPrivilegeEscalation"} {
		if !strings.Contains(condition.Message, phrase) {
			t.Errorf("the message does not name %s: %s", phrase, condition.Message)
		}
	}
}
