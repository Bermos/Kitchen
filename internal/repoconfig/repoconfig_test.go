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
	}
	config, err := Parse([]byte(`{
	  "runtime": {"port": 3000},
	  "env": {"NODE_ENV": "production"},
	  "processes": [{"name": "new", "type": "worker"}]
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

	// A build that read no file freezes the project as it stands.
	unchanged, err := Snapshot(base, nil)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if unchanged.Runtime.Port != 8080 {
		t.Fatalf("snapshot = %+v, want the project's own", unchanged)
	}
}
