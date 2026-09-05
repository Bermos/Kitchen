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

package repoconfig

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// The one value that shows up in a fixture, a merge and an assertion, spelled
// once so the linter is not counting occurrences of a word.
const production = "production"

// loggerInfo is the one configuration file body these cases write, spelled
// once for the same reason.
const loggerInfo = "logger: info\n"

// repo is a repository with a fixed set of files, which is everything Read
// needs to be exercised: what it does with the bytes is Parse's, and what it
// does with the absence of them is the case worth having a double for.
type repo struct {
	files map[string][]byte
	err   error
}

func (r repo) ListDir(context.Context, string, string, string) ([]gitprovider.DirEntry, error) {
	return nil, gitprovider.ErrFileNotFound
}

func (r repo) ReadFile(_ context.Context, _, _, path string) ([]byte, error) {
	if r.err != nil {
		return nil, r.err
	}
	if content, found := r.files[path]; found {
		return content, nil
	}
	return nil, gitprovider.ErrFileNotFound
}

func TestReadFindsNoFile(t *testing.T) {
	config, err := Read(context.Background(), repo{}, Target{Repo: "acme/shop", Ref: "abc123"})
	if err != nil {
		t.Fatalf("a repository without a %s is not a failure: %v", FileName, err)
	}
	if config != nil {
		t.Fatalf("expected no configuration, got %+v", config)
	}
}

func TestReadUsesTheProjectsRootDirectory(t *testing.T) {
	source := repo{files: map[string][]byte{
		"apps/web/kitchen.json": []byte(`{"runtime": {"port": 4321}}`),
		// The one at the top of the repository belongs to another project
		// in the same monorepo, and must not be picked up by this one.
		"kitchen.json": []byte(`{"runtime": {"port": 9999}}`),
	}}
	config, err := Read(context.Background(), source, Target{
		Repo: "acme/monorepo", Ref: "abc123", RootDirectory: "apps/web",
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if config.Path != "apps/web/kitchen.json" {
		t.Fatalf("path = %q, want apps/web/kitchen.json", config.Path)
	}
	if got := *config.Runtime.Port; got != 4321 {
		t.Fatalf("port = %d, want the file beside the project", got)
	}
}

func TestReadWaitsForAnUnreadableRepository(t *testing.T) {
	source := repo{err: errors.New("502 from the provider")}
	_, err := Read(context.Background(), source, Target{Repo: "acme/shop", Ref: "abc123"})
	if !errors.Is(err, ErrSourceUnreadable) {
		t.Fatalf("err = %v, want it to be ErrSourceUnreadable so the build waits", err)
	}
}

func TestReadFailsAnInvalidFile(t *testing.T) {
	source := repo{files: map[string][]byte{"kitchen.json": []byte(`{"runtime": {"port": 0}}`)}}
	_, err := Read(context.Background(), source, Target{Repo: "acme/shop", Ref: "abcdef1234567890"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	// The message has to name the file and the commit, because the reader is
	// looking at neither when a build fails.
	for _, want := range []string{"kitchen.json", "abcdef1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
}

func TestParseWholeFile(t *testing.T) {
	config, err := Parse([]byte(`{
	  "$schema": "https://example.invalid/kitchen.schema.json",
	  "build": {"strategy": "buildpacks", "dockerfilePath": "docker/Dockerfile"},
	  "runtime": {
	    "port": 8000,
	    "replicas": 3,
	    "command": ["gunicorn"],
	    "args": ["app:app"],
	    "previewArgs": ["app:app", "--reload"],
	    "resources": {"cpu": "500m", "memory": "512Mi"},
	    "health": {"path": "/healthz", "periodSeconds": 5}
	  },
	  "env": {"NODE_ENV": "production", "FEATURE_X": "on"},
	  "previewEnv": {"NODE_ENV": "development"},
	  "processes": [
	    {"name": "worker", "type": "worker", "command": ["python"], "args": ["worker.py"]},
	    {"name": "nightly", "type": "cron", "schedule": "0 3 * * *", "command": ["python"], "args": ["cron.py"]}
	  ]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if config.Build.Strategy != kitchenv1alpha1.BuildStrategyBuildpacks {
		t.Errorf("strategy = %q", config.Build.Strategy)
	}
	if config.Build.DockerfilePath != "docker/Dockerfile" {
		t.Errorf("dockerfilePath = %q", config.Build.DockerfilePath)
	}
	if *config.Runtime.Port != 8000 || *config.Runtime.Replicas != 3 {
		t.Errorf("runtime = %+v", config.Runtime)
	}
	if config.Runtime.Health.Path != "/healthz" || config.Runtime.Health.PeriodSeconds != 5 {
		t.Errorf("health = %+v", config.Runtime.Health)
	}
	if config.Runtime.Resources.CPU != "500m" || config.Runtime.Resources.Memory != "512Mi" {
		t.Errorf("resources = %+v", config.Runtime.Resources)
	}

	// Variables come out in name order, so that two builds of one file record
	// the same thing and a diff of two builds is about the file.
	if len(config.Env) != 2 || config.Env[0].Name != "FEATURE_X" || config.Env[1].Name != "NODE_ENV" {
		t.Fatalf("env = %+v, want it sorted by name", config.Env)
	}
	if config.Env[1].Value != production || config.Env[1].PreviewValue != "development" {
		t.Errorf("NODE_ENV = %+v", config.Env[1])
	}
	if len(config.Processes) != 2 || config.Processes[1].Schedule != "0 3 * * *" {
		t.Errorf("processes = %+v", config.Processes)
	}
}

func TestParseEmptyFile(t *testing.T) {
	config, err := Parse([]byte(`{}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if fields := config.Declares(); len(fields) != 0 {
		t.Fatalf("an empty file declares %v", fields)
	}
}

// The refusals. Each one exists because the alternative is a deploy that
// quietly does not do what the file says, so each is checked for the sentence
// it gives as well as for failing.
func TestParseRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		file     string
		mentions string
	}{
		{
			name:     "a key nothing recognises",
			file:     `{"buildCommand": "npm run build"}`,
			mentions: "does not recognise",
		},
		{
			name:     "a key nothing recognises, nested",
			file:     `{"runtime": {"prot": 3000}}`,
			mentions: "does not recognise",
		},
		{
			name:     "the root directory, which is how the file was found",
			file:     `{"build": {"rootDirectory": "apps/web"}}`,
			mentions: "rootDirectory cannot be set here",
		},
		{
			// Refused here rather than at the build: a name no stage could
			// have will never match one, and BuildKit would say so several
			// minutes into a build in its own words.
			name:     "a Dockerfile target no stage could be called",
			file:     `{"build": {"dockerfileTarget": "web stage"}}`,
			mentions: "dockerfileTarget must name a stage",
		},
		{
			name:     "a variable pointing at a secret",
			file:     `{"env": {"DATABASE_URL": {"secretRef": {"name": "db", "key": "url"}}}}`,
			mentions: "cannot take its value from secretRef",
		},
		{
			name:     "a variable pointing at a claim",
			file:     `{"env": {"DATABASE_URL": {"fromResourceClaim": {"name": "db", "key": "url"}}}}`,
			mentions: "cannot take its value from fromResourceClaim",
		},
		{
			name:     "a variable that is not a string",
			file:     `{"env": {"REPLICAS": 3}}`,
			mentions: "must be a string",
		},
		{
			name:     "a variable name a shell cannot export",
			file:     `{"env": {"not-a-name": "x"}}`,
			mentions: "not a usable environment variable name",
		},
		{
			name:     "a preview value for a variable that has no value",
			file:     `{"previewEnv": {"NODE_ENV": "development"}}`,
			mentions: "which env does not declare",
		},
		{
			name:     "a strategy that is not one of the three",
			file:     `{"build": {"strategy": "nixpacks"}}`,
			mentions: "auto, dockerfile or buildpacks",
		},
		{
			name:     "a Dockerfile outside the project",
			file:     `{"build": {"dockerfilePath": "../../etc/Dockerfile"}}`,
			mentions: "cannot leave the project's root directory",
		},
		{
			name:     "an absolute Dockerfile path",
			file:     `{"build": {"dockerfilePath": "/Dockerfile"}}`,
			mentions: "must be relative",
		},
		{
			name:     "a port outside the range",
			file:     `{"runtime": {"port": 70000}}`,
			mentions: "between 1 and 65535",
		},
		{
			name:     "a port of zero, which is what leaving it out means",
			file:     `{"runtime": {"port": 0}}`,
			mentions: "leave it out",
		},
		{
			name:     "no replicas at all",
			file:     `{"runtime": {"replicas": 0}}`,
			mentions: "at least 1",
		},
		{
			name:     "a singleton that runs three",
			file:     `{"runtime": {"singleton": true, "replicas": 3}}`,
			mentions: "never run at once",
		},
		{
			name:     "a quantity that is not one",
			file:     `{"runtime": {"resources": {"memory": "512 megabytes"}}}`,
			mentions: "Kubernetes quantity",
		},
		{
			name:     "a health path that is not a path",
			file:     `{"runtime": {"health": {"path": "healthz"}}}`,
			mentions: "must start with /",
		},
		{
			name:     "a worker with a schedule",
			file:     `{"processes": [{"name": "w", "type": "worker", "schedule": "* * * * *"}]}`,
			mentions: "runs continuously",
		},
		{
			name:     "a cron process without one",
			file:     `{"processes": [{"name": "c", "type": "cron"}]}`,
			mentions: "needs a schedule",
		},
		{
			name:     "a process called web",
			file:     `{"processes": [{"name": "web", "type": "worker"}]}`,
			mentions: `cannot be called "web"`,
		},
		{
			name:     "two processes with one name",
			file:     `{"processes": [{"name": "w", "type": "worker"}, {"name": "w", "type": "worker"}]}`,
			mentions: "listed twice",
		},
		{
			name:     "a singleton worker that runs three",
			file:     `{"processes": [{"name": "w", "type": "worker", "singleton": true, "replicas": 3}]}`,
			mentions: "never run at once",
		},
		{
			name: "a singleton schedule",
			file: `{"processes": [{"name": "n", "type": "cron", "schedule": "0 3 * * *", ` +
				`"singleton": true}]}`,
			mentions: "concurrencyPolicy",
		},
		{
			name:     "something that is not JSON",
			file:     `{"runtime": }`,
			mentions: "not valid JSON",
		},
		{
			name:     "two documents",
			file:     `{} {}`,
			mentions: "more than one JSON document",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.file))
			if err == nil {
				t.Fatalf("parsed %s without complaint", tc.file)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("err = %v, want ErrInvalid so the build fails rather than waits", err)
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("message %q does not say %q", err, tc.mentions)
			}
		})
	}
}

func TestDeclares(t *testing.T) {
	config, err := Parse([]byte(`{
	  "build": {"strategy": "dockerfile"},
	  "runtime": {"port": 3000, "replicas": 2},
	  "env": {"NODE_ENV": "production"},
	  "processes": [{"name": "worker", "type": "worker", "command": ["node", "worker.js"]}]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := fmt.Sprint(config.Declares())
	want := "[build.strategy env.NODE_ENV processes runtime.port runtime.replicas]"
	if got != want {
		t.Fatalf("Declares() = %s, want %s", got, want)
	}
	if !config.DeclaresEnv("NODE_ENV") || config.DeclaresEnv("PORT") {
		t.Errorf("DeclaresEnv is wrong about what the file names")
	}
}

// The stage of the Dockerfile travels with the commit for the reason the
// Dockerfile path does: which stages a file has is a fact about the file, so a
// rebuild of an old commit builds the stage that commit asked for.
func TestTheDockerfileTargetTravelsWithTheCommit(t *testing.T) {
	if got := DockerfileTarget(nil, "web"); got != "web" {
		t.Errorf("DockerfileTarget(nil) = %q, want the project's", got)
	}

	config, err := Parse([]byte(`{"build": {"dockerfileTarget": "worker"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := DockerfileTarget(config, "web"); got != "worker" {
		t.Errorf("the file did not win: %q", got)
	}
	if got := fmt.Sprint(config.Declares()); got != "[build.dockerfileTarget]" {
		t.Errorf("Declares() = %s", got)
	}
}

func TestStrategyAndDockerfilePathFallBackToTheProject(t *testing.T) {
	if got := Strategy(nil, kitchenv1alpha1.BuildStrategyBuildpacks); got != kitchenv1alpha1.BuildStrategyBuildpacks {
		t.Errorf("Strategy(nil) = %q", got)
	}
	if got := DockerfilePath(nil, "Containerfile"); got != "Containerfile" {
		t.Errorf("DockerfilePath(nil) = %q", got)
	}

	config := &kitchenv1alpha1.RepoConfig{Build: &kitchenv1alpha1.RepoBuildConfig{
		Strategy:       kitchenv1alpha1.BuildStrategyDockerfile,
		DockerfilePath: "docker/Dockerfile",
	}}
	if got := Strategy(config, kitchenv1alpha1.BuildStrategyBuildpacks); got != kitchenv1alpha1.BuildStrategyDockerfile {
		t.Errorf("the file did not win: %q", got)
	}
	if got := DockerfilePath(config, "Containerfile"); got != "docker/Dockerfile" {
		t.Errorf("the file did not win: %q", got)
	}
}

func TestRuntimeOverlaysOnlyWhatTheFileNames(t *testing.T) {
	base := kitchenv1alpha1.RuntimeSpec{
		Port:     8080,
		Replicas: ptr.To(int32(4)),
		Command:  []string{"/app/server"},
		Health:   &kitchenv1alpha1.HealthSpec{Path: "/live"},
	}
	config, err := Parse([]byte(`{"runtime": {"port": 3000, "args": ["--verbose"]}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	merged, err := Runtime(base, config)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.Port != 3000 {
		t.Errorf("port = %d, want the file's", merged.Port)
	}
	if merged.Args[0] != "--verbose" {
		t.Errorf("args = %v", merged.Args)
	}
	// Everything the file said nothing about is still the project's.
	if *merged.Replicas != 4 || merged.Command[0] != "/app/server" || merged.Health.Path != "/live" {
		t.Errorf("the merge touched something the file did not name: %+v", merged)
	}
	// And the project's own object was not edited underneath it.
	if base.Port != 8080 {
		t.Errorf("the base runtime was mutated: %+v", base)
	}
}

// The commit that makes an image able to run read only is the commit that
// should say so, which is why the posture is a key of the file as well as a
// field of the settings.
func TestTheFileDeclaresTheSecurityPosture(t *testing.T) {
	config, err := Parse([]byte(`{"runtime": {"security": {
	  "runAsNonRoot": true, "runAsUser": 1001, "readOnlyRootFilesystem": true,
	  "fsGroup": 1001, "fsGroupChangePolicy": "OnRootMismatch",
	  "dropCapabilities": ["all"]
	}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	security := config.Runtime.Security
	if security == nil || !security.RunAsNonRoot || security.RunAsUser != 1001 {
		t.Fatalf("the posture did not survive the file: %+v", config.Runtime)
	}
	if !security.DropsAll() {
		t.Fatalf("the capability list is not the kernel's spelling: %v", security.DropCapabilities)
	}
	// The gid that owns the volume the image is given, which the repository
	// is as much the right place to say as the rest of the posture.
	if security.FSGroup != 1001 ||
		security.FSGroupChangePolicy != kitchenv1alpha1.FSGroupChangeOnRootMismatch {
		t.Fatalf("the volume group did not survive the file: %+v", security)
	}
	if !slices.Contains(config.Declares(), "runtime.security") {
		t.Errorf("the file does not report that it declared the posture: %v", config.Declares())
	}

	merged, err := Runtime(kitchenv1alpha1.RuntimeSpec{Port: 8080}, config)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if merged.Security == nil || !merged.Security.ReadOnlyRootFilesystem || merged.Port != 8080 {
		t.Fatalf("the file's posture did not reach the runtime: %+v", merged)
	}

	// A posture that asks for nothing is no posture: the platform's default
	// is what an absent block already means.
	empty, err := Parse([]byte(`{"runtime": {"security": {}}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if empty.Runtime.Security != nil {
		t.Errorf("an empty posture should be no posture: %+v", empty.Runtime.Security)
	}

	// A gid is a gid, and the file is refused rather than reaching a pod the
	// API server would not admit.
	if _, err := Parse([]byte(`{"runtime": {"security": {"fsGroup": -1}}}`)); err == nil {
		t.Error("a negative gid was accepted from the file")
	}
	// A change policy with no ownership to change is a setting that does
	// nothing, so it is refused where it is written.
	if _, err := Parse([]byte(
		`{"runtime": {"security": {"fsGroupChangePolicy": "OnRootMismatch"}}}`)); err == nil {
		t.Error("a change policy with no group was accepted from the file")
	}
}

// The workload that needs a different posture from its siblings is the one
// whose base image differs, and the commit that changes the base is the commit
// that should say so — which is why a workload's own posture is a key of the
// file as much as the project's is (#399).
func TestTheFileDeclaresAWorkloadsOwnPosture(t *testing.T) {
	config, err := Parse([]byte(`{
	  "runtime": {"security": {"runAsNonRoot": true, "runAsUser": 1000, "dropCapabilities": ["ALL"]}},
	  "processes": [
	    {"name": "sidecar", "type": "worker", "security": {"runAsUser": 65532}},
	    {"name": "worker", "type": "worker"}
	  ]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sidecar := kitchenv1alpha1.FindProcess(config.Processes, "sidecar")
	if sidecar == nil || sidecar.Security == nil || sidecar.Security.RunAsUser != 65532 {
		t.Fatalf("the workload's own posture did not survive the file: %+v", config.Processes)
	}
	if sidecar.Security.RunAsNonRoot {
		t.Fatalf("the workload's block is its own declaration, not the resolved posture: %+v", sidecar.Security)
	}
	if worker := kitchenv1alpha1.FindProcess(config.Processes, "worker"); worker == nil || worker.Security != nil {
		t.Fatalf("a workload that declared nothing must declare nothing: %+v", worker)
	}

	// What each of them runs under: the unit's, with the sidecar's own uid
	// written over it and everything else inherited.
	resolved := kitchenv1alpha1.ResolveSecurity(config.Runtime.Security, sidecar.Security)
	if resolved.RunAsUser != 65532 || !resolved.RunAsNonRoot || len(resolved.DropCapabilities) != 1 {
		t.Fatalf("the merge did not resolve the way the file reads: %+v", resolved)
	}

	// The same validator refuses the same things one level down, and names
	// the workload rather than the project.
	_, err = Parse([]byte(`{"processes": [
	  {"name": "worker", "type": "worker", "security": {"dropCapabilities": ["net-raw"]}}
	]}`))
	if err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("a bad capability on a workload was accepted or not attributed: %v", err)
	}
	if _, err := Parse([]byte(`{"processes": [
	  {"name": "worker", "type": "worker", "security": {}}
	]}`)); err != nil {
		t.Fatalf("an empty posture should be no posture rather than a refusal: %v", err)
	}
}

func TestRuntimeRefusesASingletonTheProjectRunsSeveralOf(t *testing.T) {
	base := kitchenv1alpha1.RuntimeSpec{Replicas: ptr.To(int32(3))}
	config, err := Parse([]byte(`{"runtime": {"singleton": true}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := Runtime(base, config); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want the contradiction refused", err)
	}
}

func TestEnvMergesByName(t *testing.T) {
	base := []kitchenv1alpha1.EnvVar{
		{Name: "KEPT", Value: "from the dashboard"},
		{Name: "NODE_ENV", Value: "development"},
	}
	config, err := Parse([]byte(`{"env": {"NODE_ENV": "production", "ADDED": "yes"}}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	merged, err := Env(base, config)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	byName := map[string]string{}
	for _, variable := range merged {
		byName[variable.Name] = variable.Value
	}
	if byName["KEPT"] != "from the dashboard" {
		t.Errorf("a variable the file did not name was lost: %+v", merged)
	}
	if byName["NODE_ENV"] != production {
		t.Errorf("the file did not win: %+v", merged)
	}
	if byName["ADDED"] != "yes" {
		t.Errorf("a variable only the file names was not added: %+v", merged)
	}
	if base[1].Value != "development" {
		t.Errorf("the project's own list was mutated: %+v", base)
	}
}

// The rule the whole design turns on: a preview builds a commit from a pull
// request, so a file that could take a bound variable's name could repoint a
// database URL by opening one.
func TestEnvRefusesToShadowACredential(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bound    kitchenv1alpha1.EnvVar
		mentions string
	}{
		{
			name: "a secret",
			bound: kitchenv1alpha1.EnvVar{Name: "DATABASE_URL", SecretRef: &kitchenv1alpha1.SecretKeySelector{
				Name: "shop-secrets", Key: "database-url",
			}},
			mentions: `the secret "shop-secrets"`,
		},
		{
			name: "a resource claim",
			bound: kitchenv1alpha1.EnvVar{Name: "DATABASE_URL", FromResourceClaim: &kitchenv1alpha1.ResourceClaimKeySelector{
				Name: "shop-db", Key: "url",
			}},
			mentions: `the resource claim "shop-db"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config, err := Parse([]byte(`{"env": {"DATABASE_URL": "postgres://somewhere-else"}}`))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			_, err = Env([]kitchenv1alpha1.EnvVar{tc.bound}, config)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want the shadowing refused", err)
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("message %q does not name where the value comes from (%s)", err, tc.mentions)
			}
		})
	}
}

func TestProcessesReplaceRatherThanMerge(t *testing.T) {
	base := []kitchenv1alpha1.ProcessSpec{
		{Name: "old-worker", Type: kitchenv1alpha1.ProcessWorker},
	}
	config, err := Parse([]byte(`{"processes": [{"name": "new-worker", "type": "worker"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	merged := Processes(base, config)
	if len(merged) != 1 || merged[0].Name != "new-worker" {
		t.Fatalf("processes = %+v, want the file's list alone", merged)
	}
	// A file that says nothing about processes leaves the project's.
	if merged := Processes(base, &kitchenv1alpha1.RepoConfig{}); len(merged) != 1 || merged[0].Name != "old-worker" {
		t.Fatalf("processes = %+v, want the project's kept", merged)
	}
}

func TestSnapshotIsWhatARollbackReplays(t *testing.T) {
	base := kitchenv1alpha1.ConfigSnapshot{
		Env:       []kitchenv1alpha1.EnvVar{{Name: "NODE_ENV", Value: "development"}},
		Runtime:   kitchenv1alpha1.RuntimeSpec{Port: 8080},
		Processes: []kitchenv1alpha1.ProcessSpec{{Name: "old", Type: kitchenv1alpha1.ProcessWorker}},
		Files: []kitchenv1alpha1.ConfigFile{
			{Name: "configuration", Path: "/config/app.yaml", Content: "logger: warn\n"},
		},
	}
	config, err := Parse([]byte(`{
	  "runtime": {"port": 3000},
	  "env": {"NODE_ENV": "production"},
	  "processes": [{"name": "new", "type": "worker"}],
	  "files": [{"name": "configuration", "path": "/config/app.yaml", "content": "logger: info\n"}]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	snapshot, err := Snapshot(base, config)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Runtime.Port != 3000 || snapshot.Env[0].Value != production || snapshot.Processes[0].Name != "new" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	// The file is frozen with the rest of it, which is what makes a rollback
	// restore the configuration file that release ran with.
	if len(snapshot.Files) != 1 || snapshot.Files[0].Content != loggerInfo {
		t.Fatalf("snapshot files = %+v", snapshot.Files)
	}

	// A build that read no file freezes the project as it stands.
	unchanged, err := Snapshot(base, nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if unchanged.Runtime.Port != 8080 || unchanged.Files[0].Content != "logger: warn\n" {
		t.Fatalf("snapshot = %+v, want the project's own", unchanged)
	}
}

// A configuration file declared in the repository (#311): what software the
// platform did not build is configured by, committed beside the code that
// runs it.
func TestFilesTravelWithTheCommit(t *testing.T) {
	config, err := Parse([]byte(`{
	  "processes": [{"name": "worker", "type": "worker", "command": ["node", "worker.js"]}],
	  "files": [
	    {"name": "configuration", "path": "/config/configuration.yaml", "content": "logger: info\n"},
	    {"name": "worker-conf", "path": "/etc/worker.toml", "content": "queue = \"jobs\"\n",
	     "workloads": ["worker"]}
	  ]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(config.Files) != 2 {
		t.Fatalf("files = %+v, want both", config.Files)
	}
	if config.Files[0].Content != loggerInfo || config.Files[0].Path != "/config/configuration.yaml" {
		t.Fatalf("the file did not survive the parse: %+v", config.Files[0])
	}
	if !config.Files[0].ReachesWorkload("worker") || config.Files[1].ReachesWorkload("web") {
		t.Fatalf("a file that named no workload reaches all of them, and one that named some reaches those")
	}
	if !slices.Contains(config.Declares(), "files.configuration") || !config.DeclaresFile("worker-conf") {
		t.Fatalf("the file does not say it declared its files: %v", config.Declares())
	}
}

func TestFilesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		file     string
		mentions string
	}{
		{
			// The whole reason a repository may not declare one: this file
			// is committed, so everything in it is public, and whether the
			// platform holds a credential is not a commit's to say.
			name:     "a secret file",
			file:     `{"files": [{"name": "app-ini", "path": "/data/app.ini", "secret": true}]}`,
			mentions: "sets secret",
		},
		{
			name:     "a file with no content",
			file:     `{"files": [{"name": "conf", "path": "/config/app.yaml"}]}`,
			mentions: "has no content",
		},
		{
			name:     "a path that is not absolute",
			file:     `{"files": [{"name": "conf", "path": "config/app.yaml", "content": "a"}]}`,
			mentions: "the path is absolute",
		},
		{
			name: "a workload the commit does not declare",
			file: `{"files": [{"name": "conf", "path": "/config/app.yaml", "content": "a",
			        "workloads": ["ghost"]}]}`,
			mentions: "which this project does not declare",
		},
		{
			name: "two files at one path",
			file: `{"files": [{"name": "conf", "path": "/config/app.yaml", "content": "a"},
			        {"name": "other", "path": "/config/app.yaml", "content": "b"}]}`,
			mentions: "one path is one file",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.file))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want the file refused", err)
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("message %q does not say what is wrong (%s)", err, tc.mentions)
			}
		})
	}
}

// Files merge by name, unlike the processes and like the variables — and for
// the variables' reason: a project may hold a secret file the repository is
// not allowed to declare, and a list that replaced would take it away.
func TestFilesMergeByName(t *testing.T) {
	base := []kitchenv1alpha1.ConfigFile{
		{Name: "configuration", Path: "/config/configuration.yaml", Content: "logger: warn\n"},
		{Name: "app-ini", Path: "/data/app.ini", Secret: true},
	}
	config, err := Parse([]byte(
		`{"files": [{"name": "configuration", "path": "/config/configuration.yaml", "content": "logger: info\n"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	merged, err := Files(base, config)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(merged) != 2 {
		t.Fatalf("files = %+v, want the secret file kept", merged)
	}
	if merged[0].Content != loggerInfo {
		t.Fatalf("the commit's content did not win: %+v", merged[0])
	}
	if !merged[1].Secret || merged[1].Name != "app-ini" {
		t.Fatalf("the project's secret file was dropped by a file that never mentioned it: %+v", merged)
	}

	// A file that says nothing about files leaves the project's.
	kept, err := Files(base, &kitchenv1alpha1.RepoConfig{})
	if err != nil || len(kept) != 2 {
		t.Fatalf("files = %+v, %v — want the project's kept", kept, err)
	}
}

func TestFilesRefuseToShadowASecretFile(t *testing.T) {
	base := []kitchenv1alpha1.ConfigFile{{Name: "app-ini", Path: "/data/app.ini", Secret: true}}
	config, err := Parse([]byte(
		`{"files": [{"name": "app-ini", "path": "/data/app.ini", "content": "[server]\nSECRET_KEY = x\n"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = Files(base, config)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want the shadowing refused", err)
	}
	if !strings.Contains(err.Error(), "holds as a secret") {
		t.Errorf("message %q does not say why", err)
	}
}

// A commit declares the volumes it needs, and nothing about how they are
// made. The declaration is what the build holds against the project's
// claims, so what has to survive the parse is the claim's name, the process
// and the mount path — and the two opinions the file may hold about the
// claim it names.
func TestVolumesTravelWithTheCommit(t *testing.T) {
	config, err := Parse([]byte(`{
	  "volumes": [
	    {"name": "config", "process": "web", "mountPath": "/config"},
	    {"name": "media", "process": "web", "mountPath": "/media",
	     "source": "bind", "accessMode": "ReadOnlyMany"}
	  ]
	}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(config.Volumes) != 2 {
		t.Fatalf("volumes = %+v, want both", config.Volumes)
	}
	if config.Volumes[0].Source != "" || config.Volumes[0].AccessMode != "" {
		t.Errorf("a declaration that held no opinion gained one: %+v", config.Volumes[0])
	}
	if config.Volumes[1].Source != kitchenv1alpha1.VolumeBind ||
		config.Volumes[1].AccessMode != "ReadOnlyMany" || config.Volumes[1].MountPath != "/media" {
		t.Errorf("the declaration did not survive the parse: %+v", config.Volumes[1])
	}
	if !slices.Contains(config.Declares(), "volumes.media") || !config.DeclaresVolume("config") {
		t.Errorf("the file does not say it declared its volumes: %v", config.Declares())
	}
}

// The one thing this file may not do is ask for storage. Each refusal names
// the field, because "unknown field" would be a true answer that explains
// nothing about why a committed file may not choose a disk.
func TestVolumesRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		file     string
		mentions string
	}{
		{
			name:     "asking for a size",
			file:     `{"volumes": [{"name": "d", "process": "web", "mountPath": "/d", "size": "10Gi"}]}`,
			mentions: "sets size",
		},
		{
			name:     "asking for a class",
			file:     `{"volumes": [{"name": "d", "process": "web", "mountPath": "/d", "storageClass": "fast"}]}`,
			mentions: "sets storageClass",
		},
		{
			// The whole reason: a file anybody who can open a pull request
			// may write must not be able to mount somebody's export into
			// its own preview.
			name: "naming somebody's existing volume",
			file: `{"volumes": [{"name": "d", "process": "web", "mountPath": "/d",
			        "bind": {"persistentVolume": "nas"}}]}`,
			mentions: "sets bind",
		},
		{
			name:     "no claim named",
			file:     `{"volumes": [{"process": "web", "mountPath": "/d"}]}`,
			mentions: "names none",
		},
		{
			name:     "no process",
			file:     `{"volumes": [{"name": "d", "mountPath": "/d"}]}`,
			mentions: "no process",
		},
		{
			name:     "no mount path",
			file:     `{"volumes": [{"name": "d", "process": "web"}]}`,
			mentions: "no mountPath",
		},
		{
			name: "one claim declared twice",
			file: `{"volumes": [{"name": "d", "process": "web", "mountPath": "/d"},
			        {"name": "d", "process": "web", "mountPath": "/e"}]}`,
			mentions: "declared twice",
		},
		{
			name:     "a source that is neither",
			file:     `{"volumes": [{"name": "d", "process": "web", "mountPath": "/d", "source": "borrow"}]}`,
			mentions: "which is neither",
		},
		{
			name:     "an access mode that is none of the three",
			file:     `{"volumes": [{"name": "d", "process": "web", "mountPath": "/d", "accessMode": "WriteOnly"}]}`,
			mentions: "which is not one of",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.file))
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want the file refused", err)
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("message %q does not say what is wrong (%s)", err, tc.mentions)
			}
		})
	}
}
