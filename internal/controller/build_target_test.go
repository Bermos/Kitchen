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
)

// BuildKit refuses a target it cannot find in its own terms, about an option
// nobody typed. Recognising that is what turns it into a sentence naming the
// stage, the file and where the name was declared.
func TestRecognisingBuildKitRefusingTheStage(t *testing.T) {
	for name, tc := range map[string]struct {
		target  string
		failure *kitchenv1alpha1.BuildFailureStatus
		want    bool
	}{
		"the frontend's current words": {
			target: "runtime",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Log: []string{`ERROR: failed to solve: target stage "runtime" could not be found`},
			},
			want: true,
		},
		"the words it used to use": {
			target: "runtime",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Message: "failed to reach build target runtime in Dockerfile",
			},
			want: true,
		},
		// The phrases are common enough in a build's own output that a build
		// which asked for no stage must not be diagnosed from somebody else's
		// log line.
		"a build that asked for no stage": {
			target: "",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Log: []string{`target stage "runtime" could not be found`},
			},
			want: false,
		},
		"an ordinary broken build": {
			target:  "runtime",
			failure: &kitchenv1alpha1.BuildFailureStatus{Log: []string{"npm ERR! missing script: build"}},
			want:    false,
		},
		"a build with no failure recorded": {target: "runtime", failure: nil, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := targetNotFound(buildPlan{DockerfileTarget: tc.target}, tc.failure); got != tc.want {
				t.Errorf("targetNotFound = %v, want %v", got, tc.want)
			}
		})
	}
}

// The message says the things BuildKit's own sentence leaves out: which file
// was looked in, which stage was asked for, and which of the three places a
// target can be declared to go and change.
func TestTheSentenceAStageThatIsNotThereGets(t *testing.T) {
	project := &kitchenv1alpha1.Project{}
	project.Spec.Build.DockerfilePath = "docker/prod.Dockerfile"
	project.Spec.Build.DockerfileTarget = "runtime"
	build := &kitchenv1alpha1.Build{
		Status: kitchenv1alpha1.BuildStatus{DockerfileTarget: "runtime"},
	}
	web := buildPlan{DockerfileTarget: "runtime"}

	message := targetNotFoundMessage(project, build, web)
	for _, want := range []string{"docker/prod.Dockerfile", `"runtime"`, "build settings", "FROM"} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not carry %q: %s", want, message)
		}
	}

	// A commit that declared the target itself is sent to its own file, not
	// to the project's settings screen.
	build.Status.Config = &kitchenv1alpha1.RepoConfig{
		Build: &kitchenv1alpha1.RepoBuildConfig{DockerfileTarget: "runtime"},
	}
	if message := targetNotFoundMessage(project, build, web); !strings.Contains(message, "kitchen.json") {
		t.Errorf("the message does not send the reader to the file that declared it: %s", message)
	}
}

// One commit builds several images now, so the sentence has to say which of
// them was asked for a stage it has not got — and it names the workload's own
// file rather than the project's.
func TestTheSentenceNamesTheWorkloadThatAskedForTheStage(t *testing.T) {
	project := &kitchenv1alpha1.Project{}
	project.Spec.Build.DockerfilePath = "Dockerfile"
	project.Spec.Processes = []kitchenv1alpha1.ProcessSpec{{
		Name: "worker",
		Type: kitchenv1alpha1.ProcessWorker,
		Build: &kitchenv1alpha1.ProcessBuildSpec{
			DockerfilePath:   "services/worker/Dockerfile",
			DockerfileTarget: "runner",
		},
	}}
	build := &kitchenv1alpha1.Build{}
	plan := buildPlan{Workload: "worker", DockerfileTarget: "runner"}

	message := targetNotFoundMessage(project, build, plan)
	for _, want := range []string{"services/worker/Dockerfile", `"runner"`, `workload "worker"`} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not carry %q: %s", want, message)
		}
	}

	// A workload that named no stage of its own is being built to the unit's,
	// and the message says so rather than sending the reader to a setting the
	// workload does not have.
	project.Spec.Processes[0].Build.DockerfileTarget = ""
	project.Spec.Build.DockerfileTarget = "runtime"
	inherited := targetNotFoundMessage(project, build, buildPlan{Workload: "worker", DockerfileTarget: "runtime"})
	if !strings.Contains(inherited, "inherits") {
		t.Errorf("the message does not say the workload inherited the stage: %s", inherited)
	}
}

// A stage means nothing to a lifecycle that has none, and the refusal has to
// say which workload asked — a unit refused wholesale with no name on it is
// four directories to go and look in.
func TestRefusingAStageOnAWorkloadBuiltWithBuildpacks(t *testing.T) {
	project := &kitchenv1alpha1.Project{}
	project.Spec.Processes = []kitchenv1alpha1.ProcessSpec{{
		Name: "worker",
		Type: kitchenv1alpha1.ProcessWorker,
		Build: &kitchenv1alpha1.ProcessBuildSpec{
			Strategy:         kitchenv1alpha1.BuildStrategyBuildpacks,
			DockerfileTarget: "runner",
		},
	}}
	build := &kitchenv1alpha1.Build{}
	plans := []buildPlan{
		{Strategy: kitchenv1alpha1.BuildStrategyDockerfile},
		{
			Workload:         "worker",
			Strategy:         kitchenv1alpha1.BuildStrategyBuildpacks,
			DockerfileTarget: "runner",
		},
	}

	refused := unbuildableTarget(plans)
	if refused == nil {
		t.Fatal("a stage on a buildpacks workload was not refused")
	}
	// The refusal is a message rather than a decision about the cluster, so
	// what is asserted here is the sentence the reconciler hands to fail().
	message := unbuildableTargetMessage(project, build, *refused)
	for _, want := range []string{`workload "worker"`, `"runner"`, "buildpacks", "dockerfile strategy"} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal does not carry %q: %s", want, message)
		}
	}
}
