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

	// nuxtStock is the fixture behind the default-path case (#468): a stock
	// Nuxt application, a deploy task and a postgres claim, with no
	// Dockerfile, no strategy, no port, no start command and no build
	// environment of its own. Everything it needs to build and to run is
	// something the platform works out, which is exactly what makes it worth
	// a kind job — every one of those was missing at once, and the only way
	// to ship such an application was to write a Dockerfile.
	nuxtStock = "nuxt-stock"

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

// The stock-Nuxt fixture. What the expensive half proves is that the default
// path works end to end; what these hold is everything about the fixture that
// would make that job fail for a reason of its own — a file that would send
// the build down another strategy, a workaround that would make the case
// prove nothing, or a manifest missing what the assertions read.

// TestNuxtFixtureIsOnTheDefaultPath is the whole reason this fixture exists.
// A Dockerfile would send `auto` to BuildKit, and a `strategy` in the
// kitchen.json would answer the question the case exists to ask.
func TestNuxtFixtureIsOnTheDefaultPath(t *testing.T) {
	entries, err := os.ReadDir(nuxtStock)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), "Dockerfile") {
			t.Fatal("the fixture has a Dockerfile: `auto` would build it with BuildKit, " +
				"and the case is about the path a repository without one takes")
		}
		if strings.EqualFold(entry.Name(), "Procfile") {
			t.Fatal("the fixture has a Procfile: the lifecycle would declare a process " +
				"from it, and the case is about an image that declares none")
		}
	}

	raw, err := os.ReadFile(filepath.Join(nuxtStock, "kitchen.json"))
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
	if config.Build != nil && config.Build.Strategy != "" {
		t.Errorf("the fixture names strategy %q: `auto` reading the repository is what "+
			"the case is about", config.Build.Strategy)
	}
	if config.Runtime != nil {
		if config.Runtime.Port != nil {
			t.Error("the fixture names a port: the detected framework's is what the platform " +
				"is supposed to supply")
		}
		if len(config.Runtime.Command) > 0 {
			t.Errorf("the fixture names command %q: the detected framework's is what the "+
				"platform is supposed to supply (#440)", config.Runtime.Command)
		}
	}

	migrate := kitchenv1alpha1.ProcessSpec{}
	for _, process := range config.Processes {
		if process.Name == "migrate" {
			migrate = process
		}
	}
	if migrate.Name == "" {
		t.Fatal("the fixture declares no deploy task, so nothing proves a command reaches " +
			"the launcher on the default path")
	}
	if migrate.Type != kitchenv1alpha1.ProcessTask {
		t.Errorf("the migrate workload is %q, not a task, so no deploy waits for it", migrate.Type)
	}
	if len(migrate.Command) == 0 {
		t.Error("the deploy task names no command, so it would start the image's own process")
	}
}

// TestNuxtFixtureIsStock holds the fixture to being what the case claims it
// is: a repository that says nothing about how to build or start itself, with
// a build script for the buildpacks to run, a runtime version for them to
// pick, and no workaround anywhere for the four defects #468 is about.
func TestNuxtFixtureIsStock(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(nuxtStock, "package.json"))
	if err != nil {
		t.Fatalf("reading the fixture's package.json: %v", err)
	}
	manifest := struct {
		Scripts         map[string]string `json:"scripts"`
		Engines         map[string]string `json:"engines"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}{}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("the fixture's package.json does not parse: %v", err)
	}

	if manifest.Scripts["build"] == "" {
		t.Error("the fixture declares no build script, so BP_NODE_RUN_SCRIPTS would name " +
			"nothing and the assertion about it would pass vacuously")
	}
	// The defect in one line. Stock Nuxt has no start script, which is why
	// npm-start passes, node-start passes, the Procfile buildpack passes, and
	// the lifecycle exports an image with `processes: []`.
	if manifest.Scripts["start"] != "" {
		t.Errorf("the fixture declares a start script (%q): the `npm-start` buildpack would "+
			"give the image a process type, and the case would stop being about an image "+
			"that declares none", manifest.Scripts["start"])
	}
	if manifest.Engines["node"] == "" {
		t.Error("the fixture's manifest names no node version, so BP_NODE_VERSION would " +
			"not be set and the build would take whatever is newest that day")
	}
	if _, ok := manifest.DevDependencies["nuxt"]; !ok {
		t.Error("the fixture does not depend on nuxt, so detection would not recognise it")
	}
	// The deploy task and the server route both open the database, and
	// verifying the server is the whole of what the claim half asserts.
	if _, ok := manifest.Dependencies["pg"]; !ok {
		t.Error("the fixture does not depend on pg, so nothing in it connects to its claim")
	}
	for _, script := range manifest.Scripts {
		if strings.Contains(script, "NODE_OPTIONS") {
			t.Errorf("a script bakes NODE_OPTIONS in (%q): capping the build heap is the "+
				"platform's job, and the case exists to prove it does it", script)
		}
	}

	for _, name := range []string{"nuxt.config.ts", "app.vue", "migrate.mjs",
		filepath.Join("server", "api", "kitchen.get.ts")} {
		if _, err := os.Stat(filepath.Join(nuxtStock, name)); err != nil {
			t.Errorf("the fixture is missing %s: %v", name, err)
		}
	}
}
