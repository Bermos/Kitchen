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

// Package fixtures holds the repositories the kind jobs build.
//
// The tests here are the cheap half of a check whose expensive half is twelve
// minutes on a cluster. Every one of them holds a property "Several workloads
// on kind" depends on and cannot discover until it has already spent a build:
// a fixture whose kitchen.json the parser refuses, or whose Dockerfile stopped
// declaring the stage the workflow asserts against, would fail that job with a
// message about the platform rather than about the fixture.
package fixtures_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/repoconfig"
)

const (
	// multiWorkload is the fixture behind the several-workloads end-to-end
	// case.
	multiWorkload = "multi-workload"

	// namedStage is the stage the unit's api workload asks for, and lastStage
	// the one the file ends on and so the one a build asking for nothing
	// ships. They are the whole of what the fourth case observes, and the
	// workflow spells them too — these are what keep the two in step.
	namedStage = "shipped"
	lastStage  = "final"
)

// TestMultiWorkloadConfigParses holds the fixture to the parser the operator
// runs. It is the same refusal a build would make, several minutes earlier.
func TestMultiWorkloadConfigParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(multiWorkload, "kitchen.json"))
	if err != nil {
		t.Fatalf("reading the fixture's kitchen.json: %v", err)
	}
	config, err := repoconfig.Parse(raw)
	if err != nil {
		t.Fatalf("the fixture's kitchen.json is not one the platform accepts: %v", err)
	}
	if config == nil {
		t.Fatal("the fixture declares nothing")
	}

	if config.Runtime == nil || config.Runtime.Port == nil || *config.Runtime.Port != 8080 {
		t.Error("the web process must listen on 8080: the workflow asks it for /stage.txt there")
	}

	byName := map[string]kitchenv1alpha1.ProcessSpec{}
	for _, process := range config.Processes {
		byName[process.Name] = process
	}

	api, ok := byName["api"]
	if !ok {
		t.Fatal("the fixture declares no api workload, so the commit produces one image")
	}
	if api.Type != kitchenv1alpha1.ProcessService {
		t.Errorf("the api workload is %q; it has to be a service to be addressed by a sibling", api.Type)
	}
	if api.Port != 9000 {
		t.Errorf("the api workload listens on %d; the workflow asks it for /stage.txt on 9000", api.Port)
	}
	if api.Build == nil {
		t.Fatal("the api workload declares no build of its own, so the commit produces one image")
	}
	// The whole of the dockerfileTarget case: the workload names a stage that
	// is not the file's last one, and the web process names none.
	if api.Build.DockerfileTarget != namedStage {
		t.Errorf("the api workload ships stage %q, not the %q the workflow asserts",
			api.Build.DockerfileTarget, namedStage)
	}
	if config.Build != nil && config.Build.DockerfileTarget != "" {
		t.Error("the unit must name no stage of its own: the web process is what proves " +
			"that a build with no target ships the file's last stage")
	}

	migrate, ok := byName["migrate"]
	if !ok {
		t.Fatal("the fixture declares no deploy task")
	}
	if migrate.Type != kitchenv1alpha1.ProcessTask {
		t.Errorf("the migrate workload is %q, not a task, so no deploy waits for it", migrate.Type)
	}
}

// stageDeclaration matches a Dockerfile stage the way BuildKit names one.
var stageDeclaration = regexp.MustCompile(`(?mi)^FROM\s+\S+\s+AS\s+(\S+)`)

// TestMultiWorkloadDockerfileStages holds the two properties the fourth case
// rests on: that both stages the workflow names exist, and that `final` is the
// file's last one — which is what makes "the web process runs final" a
// statement about the *default* rather than about a second target.
func TestMultiWorkloadDockerfileStages(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(multiWorkload, "Dockerfile"))
	if err != nil {
		t.Fatalf("reading the fixture's Dockerfile: %v", err)
	}
	declared := stageDeclaration.FindAllStringSubmatch(string(raw), -1)
	stages := make([]string, 0, len(declared))
	for _, match := range declared {
		stages = append(stages, strings.ToLower(match[1]))
	}
	if len(stages) == 0 {
		t.Fatal("the fixture's Dockerfile declares no named stages")
	}
	for _, stage := range []string{namedStage, lastStage} {
		if !slices.Contains(stages, stage) {
			t.Errorf("the Dockerfile declares no %q stage; it declares %v", stage, stages)
		}
	}
	if last := stages[len(stages)-1]; last != lastStage {
		t.Errorf("the file's last stage is %q, so a build naming no target ships that "+
			"rather than the %q the workflow asserts", last, lastStage)
	}
}
