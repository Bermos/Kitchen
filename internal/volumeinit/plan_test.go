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

package volumeinit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The one property this program lives or dies on is that running it twice is
// the same as running it once. It runs on every start of every pod of every
// deploy, and a step that clobbered a volume on the second one would be worse
// than no step at all — which is what the issue says in as many words.

// seedInto writes the platform's copy of a file where the program reads it
// from — the directory the operator mounts at SeedDir in the real container.
func seedInto(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDirectoriesAreCreatedOnceAndThenLeftAlone(t *testing.T) {
	volume := t.TempDir()
	plan := Plan{Volumes: []Volume{{
		Claim:     "config",
		MountPath: volume,
		Directories: []Directory{
			{Path: "data"},
			{Path: "custom/deep", Mode: "0750"},
		},
	}}}

	if step, err := Run(plan, ""); err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	info, err := os.Stat(filepath.Join(volume, "custom", "deep"))
	if err != nil {
		t.Fatalf("the declared directory was not created: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o750 {
		t.Errorf("the directory was created %04o, and the step asked for 0750", got)
	}
	// The parent a step did not name gets the platform's own default rather
	// than the mode declared for the child.
	parent, err := os.Stat(filepath.Join(volume, "custom"))
	if err != nil {
		t.Fatal(err)
	}
	if got := parent.Mode().Perm(); got != DefaultDirectoryMode.Perm() {
		t.Errorf("the parent was created %04o, and nothing declared a mode for it", got)
	}

	// What the application then does to its own volume, which a second run
	// must not undo.
	if err := os.Chmod(filepath.Join(volume, "custom", "deep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(volume, "data", "state.db"), []byte("rows"), 0o600); err != nil {
		t.Fatal(err)
	}

	if step, err := Run(plan, ""); err != nil {
		t.Fatalf("the second run failed, and every deploy is a second run: %s: %v", step, err)
	}
	again, err := os.Stat(filepath.Join(volume, "custom", "deep"))
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Mode().Perm(); got != 0o700 {
		t.Errorf("the second run reset the mode to %04o: a directory that is already there is the "+
			"application's, mode included", got)
	}
	if _, err := os.Stat(filepath.Join(volume, "data", "state.db")); err != nil {
		t.Errorf("the second run disturbed what the application had written: %v", err)
	}
}

func TestASeedIsWrittenOnceAndNeverOverAnApplicationsOwnFile(t *testing.T) {
	volume, seeds := t.TempDir(), t.TempDir()
	seedInto(t, seeds, "configuration", "logger: info\n")

	plan := Plan{Volumes: []Volume{{
		Claim:     "config",
		MountPath: volume,
		Seeds:     []Seed{{File: "configuration", Path: "conf/configuration.yaml", Mode: "0640"}},
	}}}

	if step, err := Run(plan, seeds); err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	at := filepath.Join(volume, "conf", "configuration.yaml")
	content, err := os.ReadFile(at) //nolint:gosec // a path this test built
	if err != nil {
		t.Fatalf("the seed did not land: %v", err)
	}
	if string(content) != "logger: info\n" {
		t.Errorf("the seed wrote %q", content)
	}
	info, err := os.Stat(at)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("the seed was written %04o, and the step asked for 0640", got)
	}

	// The application rewrites its own configuration, which is the whole
	// reason the file is seeded rather than mounted.
	if err := os.WriteFile(at, []byte("logger: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if step, err := Run(plan, seeds); err != nil {
		t.Fatalf("%s: %v", step, err)
	}
	content, err = os.ReadFile(at) //nolint:gosec // a path this test built
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "logger: debug\n" {
		t.Fatalf("the second run clobbered what the application wrote: %q", content)
	}
}

func TestAFailedStepSaysWhichStepAndWhy(t *testing.T) {
	volume := t.TempDir()
	// A file where the plan wants a directory: the platform will not remove
	// what the application put there, so it says so instead.
	if err := os.WriteFile(filepath.Join(volume, "data"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	plan := Plan{Volumes: []Volume{{
		Claim: "config", MountPath: volume,
		Directories: []Directory{{Path: "data"}},
	}}}

	step, err := Run(plan, "")
	if err == nil {
		t.Fatal("a directory that cannot be created has to fail: a workload started against a volume " +
			"nothing prepared is the failure this feature exists to end")
	}
	named := step.String()
	if !strings.Contains(named, `directory "data"`) || !strings.Contains(named, `volume "config"`) {
		t.Errorf("the failed step is reported as %q, which names neither the step nor the volume", named)
	}
}

func TestASeedThePlatformDidNotPlaceIsNamed(t *testing.T) {
	volume := t.TempDir()
	plan := Plan{Volumes: []Volume{{
		Claim: "config", MountPath: volume,
		Seeds: []Seed{{File: "nowhere", Path: "app.yaml"}},
	}}}
	step, err := Run(plan, t.TempDir())
	if err == nil {
		t.Fatal("a seed whose source is missing has to fail rather than write an empty file")
	}
	if !strings.Contains(err.Error(), "nowhere") {
		t.Errorf("the failure does not name the file: %v", err)
	}
	if !strings.Contains(step.What, "seed") {
		t.Errorf("the failed step is %q", step.What)
	}
}

func TestAPlanIsRequiredAndUnknownFieldsAreRefused(t *testing.T) {
	if _, err := Parse(""); err == nil {
		t.Error("an empty plan has to be refused: this container is started only with one to run")
	}
	if _, err := Parse(`{"volumes":[{"claim":"c","mountPath":"/x","command":["sh"]}]}`); err == nil {
		t.Error("a plan carrying a field this version does not understand has to be refused, " +
			"not silently half-run — and `command` is exactly the field that must never appear")
	}
	plan, err := Parse(`{"volumes":[{"claim":"c","mountPath":"/x","directories":[{"path":"d"}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Volumes) != 1 || plan.Volumes[0].Directories[0].Path != "d" {
		t.Errorf("the plan did not round-trip: %+v", plan)
	}
}
