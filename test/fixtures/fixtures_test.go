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
	"encoding/json"
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

	// buildpacksNode is the fixture behind the buildpacks end-to-end case:
	// the one build in the tree that runs the Cloud Native Buildpacks
	// lifecycle rather than BuildKit.
	buildpacksNode = "buildpacks-node"

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

// The buildpacks fixture. What the expensive half proves is that the
// lifecycle's five phases build and push an image between them; what these
// hold is everything about the fixture that would make that job fail for a
// reason of its own — a directory the dockerfile strategy would have claimed,
// a manifest with no start script, a port the workflow asks on and the
// application does not serve.

// TestBuildpacksFixtureHasNoDockerfile is the whole reason this fixture
// exists. `auto` resolves to the container strategy the moment a Dockerfile
// appears beside the manifest, so a Dockerfile added here would quietly turn
// the one case that runs the lifecycle into a second BuildKit build.
func TestBuildpacksFixtureHasNoDockerfile(t *testing.T) {
	entries, err := os.ReadDir(buildpacksNode)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), "Dockerfile") {
			t.Fatal("the fixture has a Dockerfile: the buildpacks case would build with BuildKit")
		}
	}
}

// TestBuildpacksFixtureConfigParses holds it to the parser the operator runs,
// and to the two things the workflow asserts against.
func TestBuildpacksFixtureConfigParses(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(buildpacksNode, "kitchen.json"))
	if err != nil {
		t.Fatalf("reading the fixture's kitchen.json: %v", err)
	}
	config, err := repoconfig.Parse(raw)
	if err != nil {
		t.Fatalf("the fixture's kitchen.json is not one the platform accepts: %v", err)
	}
	if config == nil || config.Build == nil {
		t.Fatal("the fixture declares no build")
	}
	if config.Build.Strategy != kitchenv1alpha1.BuildStrategyBuildpacks {
		t.Errorf("the fixture builds with %q; the case is about the lifecycle",
			config.Build.Strategy)
	}
	if config.Runtime == nil || config.Runtime.Port == nil || *config.Runtime.Port != 8080 {
		t.Error("the application must serve 8080: the workflow asks it there, and $PORT is " +
			"what the platform tells a buildpacks-built image to listen on")
	}
}

// TestBuildpacksFixtureStarts holds the one thing the Node buildpack needs
// from a repository with no dependencies: a start script, which is what the
// image's entry point ends up running. Without it the lifecycle builds an
// image that starts nothing, and the case fails as a rollout that never
// becomes ready rather than as a fixture with a missing line.
func TestBuildpacksFixtureStarts(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(buildpacksNode, "package.json"))
	if err != nil {
		t.Fatalf("reading the fixture's package.json: %v", err)
	}
	manifest := struct {
		Scripts      map[string]string `json:"scripts"`
		Dependencies map[string]string `json:"dependencies"`
	}{}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the fixture's package.json does not parse: %v", err)
	}
	if manifest.Scripts["start"] == "" {
		t.Error("the fixture declares no start script, so the built image runs nothing")
	}
	// A dependency would mean a registry fetch inside the build pod, which is
	// a second thing that can fail in a case that is about the lifecycle.
	if len(manifest.Dependencies) > 0 {
		t.Errorf("the fixture declares dependencies %v: the case would depend on an "+
			"npm registry it has nothing to say about", manifest.Dependencies)
	}
	if _, err := os.Stat(filepath.Join(buildpacksNode, "server.js")); err != nil {
		t.Errorf("the start script runs server.js and it is not there: %v", err)
	}
}
