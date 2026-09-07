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

	"github.com/spf13/cobra"
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

// declaringCommand is a command that declares itself the dashboard's, built
// here rather than taken from the tree.
//
// **No command in the CLI declares it any more**: #349 built the credential
// that runs the four families #208 found, and their declarations came off with
// it. What is left is the mechanism, and the rule that decides when a command
// owes a declaration — TestDashboardOnlyMatchesTheAPIsTable, which reads the
// API's own table. So these test the mechanism against a command made for the
// purpose: a test that could only run against a real declarer would have been
// deleted along with the last one, taking the guard with it.
func declaringCommand(screen, path string) *cobra.Command {
	cmd := &cobra.Command{Use: "example"}
	return describe(cmd, meta{
		Output: output{Mode: outputNone},
		Needs:  needs{Auth: true, Platform: onlyInTheDashboard(screen, path)},
	})
}

// The refusal a declaring command gets: the API's own sentence, the exit
// status a permission failure has always had, and the half the API cannot know
// — that no credential this CLI can store would have worked, and where the
// operation does exist.
func TestPlatformCommandRefusalNamesTheDashboard(t *testing.T) {
	screen, path := "Platform → Credentials", "/platform/credentials"
	cmd := declaringCommand(screen, path)
	refused := &failure{
		Code:    codeForbidden,
		Status:  403,
		Message: "issuing a platform credential needs the operator role; you are a member",
	}

	explained := dashboardOnlyRefusal(cmd, refused)
	// The code — and so the exit status — does not move: a script branching on
	// 4 keeps getting 4, because this is a permission failure and nothing else.
	if explained.Code != codeForbidden || explained.Status != 403 {
		t.Fatalf("the refusal changed shape: %+v", explained)
	}
	if explained.Message != refused.Message {
		t.Errorf("message %q, want the API's own %q", explained.Message, refused.Message)
	}
	if !strings.Contains(explained.Hint, screen) || !strings.Contains(explained.Hint, path) {
		t.Errorf("the hint does not name the screen or the path: %q", explained.Hint)
	}
	if !strings.Contains(explained.Hint, "operator role") ||
		!strings.Contains(explained.Hint, "platform credential") {
		t.Errorf("the hint does not give the reason: %q", explained.Hint)
	}
}

// It is the refusal that is explained, and only that. A failure of another
// kind says what went wrong and nothing about the dashboard: a note that
// appears on every failure is one nobody reads on the one that means it.
func TestOnlyAForbiddenIsExplained(t *testing.T) {
	cmd := declaringCommand("Platform → Credentials", "/platform/credentials")
	for _, f := range []*failure{
		{Code: codeUnavailable, Status: 503, Message: "the telemetry store did not answer"},
		{Code: codeNotFound, Status: 404, Message: "no such project"},
	} {
		if hint := dashboardOnlyRefusal(cmd, f).Hint; hint != "" {
			t.Errorf("%s was explained as a permission problem: %q", f.Code, hint)
		}
	}
}

// And a command that does not declare it is left alone, whatever it was
// refused for: a developer refused a write on their own project needs a role,
// not a screen.
func TestACommandThatDoesNotDeclareItIsLeftAlone(t *testing.T) {
	cmd := describe(&cobra.Command{Use: "example"}, meta{
		Output: output{Mode: outputNone},
		Needs:  needs{Auth: true},
	})
	refused := &failure{
		Code:    codeForbidden,
		Status:  403,
		Message: "changing a project's environment variables needs developer; you are a viewer",
	}
	if hint := dashboardOnlyRefusal(cmd, refused).Hint; hint != "" {
		t.Errorf("a project refusal was answered with the platform's statement: %q", hint)
	}
}

// Nothing declares it today, and the schema says so. A command that starts to
// is caught by TestDashboardOnlyMatchesTheAPIsTable, which reads the API's own
// table rather than a list kept here — this is the other direction: if one
// appears, docs/CLI.md has to grow the section that explains it.
func TestSchemaPublishesTheDashboardOnlyStatement(t *testing.T) {
	document := tree(t)

	for _, command := range document.Commands {
		if command.Needs.Platform == nil {
			continue
		}
		if command.Needs.Platform.Why != dashboardOnlyReason {
			t.Errorf("%s gives its own reason: %q", command.Path, command.Needs.Platform.Why)
		}
		t.Errorf("%s says it is the dashboard's, and nothing does since #349 — if that is right, "+
			"give it a section in docs/CLI.md and say so here", command.Path)
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
