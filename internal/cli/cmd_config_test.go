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

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `kitchen config check` is the one command here that reaches nothing at all:
// no platform, no credential, no network. That is what makes it usable in a
// pre-commit hook, and it is the property worth a test of its own — a version
// of it that quietly needed a token would be useless in the place it exists
// for.

func writeConfig(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestConfigCheckListsWhatTheFileTakesOver(t *testing.T) {
	h := newHarness(t)
	// No credential and no reachable platform: the check must not want one.
	delete(h.env, "KITCHEN_TOKEN")
	h.env["KITCHEN_API"] = "https://nothing.invalid"
	writeConfig(t, h.work, "kitchen.json", `{
	  "build": {"strategy": "dockerfile"},
	  "runtime": {"port": 3000},
	  "env": {"NODE_ENV": "production"}
	}`)

	if code := h.run("config", "check", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	answer := configCheck{}
	h.answer(&answer)
	if answer.Path != "kitchen.json" {
		t.Errorf("path = %q", answer.Path)
	}
	want := "build.strategy env.NODE_ENV runtime.port"
	if got := strings.Join(answer.Declares, " "); got != want {
		t.Errorf("declares = %q, want %q", got, want)
	}
}

func TestConfigCheckReadsAPathInAMonorepo(t *testing.T) {
	h := newHarness(t)
	writeConfig(t, h.work, filepath.Join("apps", "web", "kitchen.json"), `{"runtime": {"port": 4321}}`)

	if code := h.run("config", "check", "apps/web/kitchen.json", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	answer := configCheck{}
	h.answer(&answer)
	if len(answer.Declares) != 1 || answer.Declares[0] != "runtime.port" {
		t.Errorf("declares = %v", answer.Declares)
	}
}

// A valid file that declares nothing is a success with an empty list, not an
// absent one: a caller reading `.declares[]` gets no rows rather than an error
// about a null.
func TestConfigCheckAnswersAnEmptyListForAFileThatDeclaresNothing(t *testing.T) {
	h := newHarness(t)
	writeConfig(t, h.work, "kitchen.json", `{}`)

	if code := h.run("config", "check", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if got := strings.TrimSpace(h.stdout.String()); !strings.Contains(got, `"declares":[]`) {
		t.Errorf("stdout = %s, want an empty list", got)
	}
}

func TestConfigCheckRefusesTheFileABuildWouldRefuse(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		mentions string
	}{
		{"a key nothing recognises", `{"buildCommand": "npm run build"}`, "does not recognise"},
		{"the root directory", `{"build": {"rootDirectory": "apps/web"}}`, "cannot be set here"},
		{
			"a credential",
			`{"env": {"DATABASE_URL": {"secretRef": {"name": "db", "key": "url"}}}}`,
			"cannot take its value from secretRef",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			writeConfig(t, h.work, "kitchen.json", tc.body)

			if code := h.run("config", "check", "--json"); code != exitFailed {
				t.Fatalf("exit %d, want %d — stdout: %s", code, exitFailed, h.stdout.String())
			}
			if said := h.stdout.String(); !strings.Contains(said, tc.mentions) {
				t.Errorf("the failure does not say %q:\n%s", tc.mentions, said)
			}
		})
	}
}

// No file is not a failure of the platform's, and it is not exit 1 either: a
// project configured entirely in the dashboard is the ordinary case, and a
// hook that ran this over every repository should be able to tell "there is
// nothing to check" from "what is here is wrong".
func TestConfigCheckSaysWhenThereIsNoFile(t *testing.T) {
	h := newHarness(t)

	if code := h.run("config", "check", "--json"); code != exitNotFound {
		t.Fatalf("exit %d, want %d", code, exitNotFound)
	}
}

func TestConfigSchemaPrintsWhereTheSchemaIs(t *testing.T) {
	h := newHarness(t)

	if code := h.run("config", "schema", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	answer := configSchemaURL{}
	h.answer(&answer)
	if !strings.HasSuffix(answer.Schema, "kitchen.schema.json") {
		t.Errorf("schema = %q", answer.Schema)
	}
}
