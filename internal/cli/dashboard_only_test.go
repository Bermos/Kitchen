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
	"strings"
	"testing"
)

// What the platform commands say when the credential in hand cannot run them
// (#208).
//
// The API's refusal is correct and stays the message: it names the operation
// and the role it wanted. What it cannot know is that *no* credential this CLI
// can store would have worked, so a caller who reads only the 403 goes looking
// for a better key and there is not one. These pin the sentence that says so,
// and the exit status underneath it, which does not move: this is a permission
// failure and a script branching on 4 must keep getting 4.

// refusedAsAMember answers every call the way the API refuses a project key.
func refusedAsAMember(h *harness, message string) {
	h.platform.refuseStatus = 403
	h.platform.refuseMessage = message
}

func TestPlatformCommandRefusalNamesTheDashboard(t *testing.T) {
	for _, want := range []struct {
		command []string
		screen  string
		path    string
		refusal string
	}{
		{
			command: []string{"retention", "--json"},
			screen:  "Platform → Settings, under Retention",
			path:    "/platform/settings",
			refusal: "reading the platform's retention needs the operator role; you are a member",
		},
		{
			command: []string{"backup", "--json"},
			screen:  "Platform → Backup",
			path:    "/platform/backup",
			refusal: "exporting the platform's state needs the operator role; you are a member",
		},
		{
			command: []string{"access", "identities", "--json"},
			screen:  "Platform → Audit, under Access recertification",
			path:    "/platform/audit",
			refusal: "reading who holds what on the platform needs the operator role; you are a member",
		},
		{
			command: []string{"audit-pack", "--project", "shop",
				"--from", "2026-01-01", "--to", "2026-04-01", "--json"},
			screen:  "Platform → Audit, under Audit pack",
			path:    "/platform/audit",
			refusal: "exporting a project's audit pack needs the operator role; you are a member",
		},
	} {
		t.Run(want.command[0], func(t *testing.T) {
			h := newHarness(t)
			refusedAsAMember(h, want.refusal)

			// The exit status is the one a permission failure has always had.
			if code := h.run(want.command...); code != exitForbidden {
				t.Fatalf("exit %d, want %d: %s", code, exitForbidden, h.stdout.String())
			}
			refused := h.failure()
			if refused.Code != codeForbidden {
				t.Errorf("code %q, want %q", refused.Code, codeForbidden)
			}
			if refused.Status != 403 {
				t.Errorf("status %d, want 403", refused.Status)
			}
			// The API's own sentence survives: it names the operation.
			if refused.Message != want.refusal {
				t.Errorf("message %q, want the API's own %q", refused.Message, want.refusal)
			}
			// And the half the API cannot know: why no key would have worked,
			// and where the operation does exist.
			if !strings.Contains(refused.Hint, want.screen) {
				t.Errorf("the hint does not name the screen %q: %q", want.screen, refused.Hint)
			}
			if !strings.Contains(refused.Hint, want.path) {
				t.Errorf("the hint does not name the path %q: %q", want.path, refused.Hint)
			}
			if !strings.Contains(refused.Hint, "a role on one project") ||
				!strings.Contains(refused.Hint, "operator role") {
				t.Errorf("the hint does not give the reason: %q", refused.Hint)
			}
		})
	}
}

// On a terminal the same sentence is what a person reads, under the API's.
func TestPlatformCommandRefusalReachesAPersonToo(t *testing.T) {
	h := newHarness(t)
	refusedAsAMember(h, "reading the platform's retention needs the operator role; you are a member")

	if code := h.run("retention"); code != exitForbidden {
		t.Fatalf("exit %d, want %d", code, exitForbidden)
	}
	printed := h.stderr.String()
	if !strings.Contains(printed, "needs the operator role") {
		t.Errorf("the API's sentence is missing: %s", printed)
	}
	if !strings.Contains(printed, "Platform → Settings, under Retention (/platform/settings)") {
		t.Errorf("the screen is missing: %s", printed)
	}
	if !strings.Contains(h.stdout.String(), "") || h.stdout.Len() != 0 {
		t.Errorf("a failure wrote to stdout in text mode: %s", h.stdout.String())
	}
}

// It is the refusal that is explained, not everything that goes wrong. A
// platform command that fails for another reason says what went wrong and
// nothing about the dashboard — a note that appears on every failure is one
// nobody reads on the one that needs it.
func TestOnlyTheRefusalNamesTheDashboard(t *testing.T) {
	h := newHarness(t)
	h.platform.refuseStatus = 503
	h.platform.refuseMessage = "the telemetry store is not installed"

	if code := h.run("retention", "--json"); code != exitUnavailable {
		t.Fatalf("exit %d, want %d: %s", code, exitUnavailable, h.stdout.String())
	}
	if hint := h.failure().Hint; strings.Contains(hint, "dashboard") {
		t.Errorf("an unavailable store was explained as a permission problem: %q", hint)
	}
}

// And a project command's 403 is left alone: a developer refused a write on
// their own project needs a role, not a screen.
func TestAProjectCommandsRefusalIsLeftAlone(t *testing.T) {
	h := newHarness(t)
	refusedAsAMember(h, "changing a project's environment variables needs developer; you are a viewer")

	if code := h.run("env", "set", "PORT=8080", "--project", "shop", "--json"); code != exitForbidden {
		t.Fatalf("exit %d, want %d: %s", code, exitForbidden, h.stdout.String())
	}
	if hint := h.failure().Hint; strings.Contains(hint, "dashboard") {
		t.Errorf("a project refusal was answered with the platform's statement: %q", hint)
	}
}

// The schema carries the statement per command, which is the half a machine
// reads: a caller can tell before running anything that this one will not work
// with the credential it holds, and say where it does.
func TestSchemaPublishesTheDashboardOnlyStatement(t *testing.T) {
	document := tree(t)

	wanted := map[string]string{
		"kitchen retention":         "/platform/settings",
		"kitchen backup":            "/platform/backup",
		"kitchen backup list":       "/platform/backup",
		"kitchen backup run":        "/platform/backup",
		"kitchen audit-pack":        "/platform/audit",
		"kitchen access":            "/platform/audit",
		"kitchen access identities": "/platform/audit",
		"kitchen access reviews":    "/platform/audit",
		"kitchen access show":       "/platform/audit",
	}
	found := map[string]string{}
	for _, command := range document.Commands {
		if command.Needs.Platform == nil {
			continue
		}
		found[command.Path] = command.Needs.Platform.Path
		if command.Needs.Platform.Why != dashboardOnlyReason {
			t.Errorf("%s gives its own reason: %q", command.Path, command.Needs.Platform.Why)
		}
	}
	for path, screen := range wanted {
		if found[path] != screen {
			t.Errorf("%s publishes %q, want %q", path, found[path], screen)
		}
	}
	for path := range found {
		if _, expected := wanted[path]; !expected {
			t.Errorf("%s says it is the dashboard's and this test did not expect it — if that is "+
				"right, add it here and to docs/CLI.md", path)
		}
	}
}

// The paragraph is one text with one width, so a help page does not have one
// line running off the side of the terminal.
func TestTheNoteIsWrapped(t *testing.T) {
	note := onlyInTheDashboard("Platform → Audit, under Access recertification", "/platform/audit").note()
	for _, line := range strings.Split(note, "\n") {
		if len([]rune(line)) > helpWidth {
			t.Errorf("a line of %d runes: %q", len([]rune(line)), line)
		}
	}
}
