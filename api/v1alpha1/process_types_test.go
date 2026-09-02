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

package v1alpha1

import (
	"testing"

	"k8s.io/utils/ptr"
)

// Whether a workload runs in a preview turns on its type, and that is the
// decision rather than an omission: a worker draining the production queue is
// a bad afternoon, and a preview whose own service is missing is a broken
// preview rather than a protected one.
func TestWhichWorkloadsAPreviewRuns(t *testing.T) {
	cases := []struct {
		name    string
		process ProcessSpec
		want    bool
	}{
		{"a worker stays out unless asked for", ProcessSpec{Type: ProcessWorker}, false},
		{"a scheduled job stays out unless asked for", ProcessSpec{Type: ProcessCron}, false},
		{"a worker that opted in", ProcessSpec{Type: ProcessWorker, Previews: ptr.To(true)}, true},
		{"a service comes up unless taken out", ProcessSpec{Type: ProcessService}, true},
		{"a service taken out stays out", ProcessSpec{Type: ProcessService, Previews: ptr.To(false)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.process.RunsIn(EnvironmentPreview); got != tc.want {
				t.Fatalf("RunsIn(preview) = %v, want %v", got, tc.want)
			}
			if !tc.process.RunsIn(EnvironmentProduction) {
				t.Fatal("production runs everything the release declares")
			}
		})
	}
}

// The name a workload's siblings find it under. It has to be injective over
// DNS labels, which it is: a label cannot contain an underscore, so nothing
// but a dash ever becomes one.
func TestTheVariableASiblingReadsAServiceAs(t *testing.T) {
	for name, want := range map[string]string{
		"api":          "KITCHEN_SERVICE_API",
		"api-gateway":  "KITCHEN_SERVICE_API_GATEWAY",
		"queue2":       "KITCHEN_SERVICE_QUEUE2",
		"a-b-c":        "KITCHEN_SERVICE_A_B_C",
		"billingsvc-1": "KITCHEN_SERVICE_BILLINGSVC_1",
	} {
		if got := ServiceEnvPrefix(name); got != want {
			t.Errorf("ServiceEnvPrefix(%q) = %q, want %q", name, got, want)
		}
	}
}

// A Release freezes what each workload was built to, which is half of what
// makes a rollback exact — the other half is the process list beside it.
func TestWhichImageAWorkloadRuns(t *testing.T) {
	release := &Release{Spec: ReleaseSpec{
		Image: "registry.example.com/shop@sha256:aaaa",
		Workloads: []WorkloadImage{
			{Name: "api", Image: "registry.example.com/shop-api@sha256:bbbb"},
		},
	}}

	if got := release.ImageFor("api"); got != "registry.example.com/shop-api@sha256:bbbb" {
		t.Errorf("a workload built with its own image does not run it: %s", got)
	}
	for _, workload := range []string{WebProcessName, "worker", "nothing-declared"} {
		if got := release.ImageFor(workload); got != release.Spec.Image {
			t.Errorf("%s should run the release's own image, got %s", workload, got)
		}
	}
}

// A workload's strategy defaults the way the CRD defaults it, so a spec
// written before the field had one reads the same as one written after.
//
// Its two paths deliberately have no such helper here: what a build root is
// and how one is spelled lives in internal/detect, which this package cannot
// import — see EffectiveStrategy's own comment, and
// internal/detect/buildroot_test.go for the rules themselves.
func TestAWorkloadBuildDefaults(t *testing.T) {
	if empty := (ProcessBuildSpec{}); empty.EffectiveStrategy() != BuildStrategyDockerfile {
		t.Errorf("strategy = %s, want dockerfile", empty.EffectiveStrategy())
	}
	declared := ProcessBuildSpec{Strategy: BuildStrategyBuildpacks, DockerfilePath: "docker/prod.Dockerfile"}
	if declared.EffectiveStrategy() != BuildStrategyBuildpacks {
		t.Errorf("a declared build was overridden: %+v", declared)
	}
}

// Only a service is addressed, and only a worker or a service is a Deployment.
func TestWhatEachWorkloadShapeIs(t *testing.T) {
	for _, tc := range []struct {
		processType           ProcessType
		addressed, continuous bool
	}{
		{ProcessWorker, false, true},
		{ProcessService, true, true},
		{ProcessCron, false, false},
	} {
		process := ProcessSpec{Type: tc.processType}
		if process.Addressed() != tc.addressed {
			t.Errorf("%s addressed = %v, want %v", tc.processType, process.Addressed(), tc.addressed)
		}
		if process.LongRunning() != tc.continuous {
			t.Errorf("%s long running = %v, want %v", tc.processType, process.LongRunning(), tc.continuous)
		}
	}
}
