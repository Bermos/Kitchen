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

	"github.com/spf13/cobra"
)

// The commands no credential this CLI can store is able to run, and what they
// say about themselves instead (#208).
//
// `kitchen login` stores one kind of credential: an API key, which is a
// machine account holding a role on **one project**. A handful of commands
// call nothing but the platform's own surface, which the API answers to the
// operator role alone — so those commands are in `--help`, in `kitchen
// schema` and in the documentation, and there is no credential that can run
// them. The bootstrap loop makes it worse rather than better: a key comes
// from a project's key panel, which is a browser.
//
// The decision on #208 is that they are **the dashboard's for now**, stated
// rather than left to be discovered: a platform-scoped key is the real answer
// and is designed together with what a CI key is, since both reshape what a
// key is. Until then, three things say the same sentence, and all three come
// from here so they cannot come to say three different things:
//
//   - `--help` and `kitchen schema`'s description, through the paragraph
//     describe appends to a command that declares itself this way;
//   - `kitchen schema`'s `needs.platform`, which is the machine-readable half
//     — screen, path and reason;
//   - the refusal itself, when the API answers 403: the exit status stays the
//     one a permission failure has always had, and the hint names the screen
//     and why the credential in hand cannot.
//
// Nothing here is a second list of command names. A command declares itself,
// and TestDashboardOnlyMatchesTheAPIsTable checks the declaration against
// `api.PolicyTable()` in both directions — a route that stops being the
// operator's, or one that becomes it, fails the CLI's tests.

// dashboardOnly is a command's statement that it is the dashboard's for now,
// and where in the dashboard that is.
type dashboardOnly struct {
	// Screen is the screen, as somebody would say it out loud: "Platform →
	// Backup".
	Screen string `json:"screen"`
	// Path is the dashboard route it lives at.
	Path string `json:"path"`
	// Why is the reason the credential in hand cannot run the command. It is
	// filled in from one constant rather than written per command.
	Why string `json:"why"`
}

// dashboardOnlyReason is that one constant: why a stored credential cannot,
// in the words the failure, the help and the schema all use.
const dashboardOnlyReason = "a project API key holds a role on one project, and this command calls " +
	"the platform's own surface, which needs the operator role — no credential `kitchen login` can " +
	"store holds one"

// onlyInTheDashboard is what a command declares in its metadata. Naming the
// screen is the whole of what a caller has to be told beyond the reason, so it
// is the only argument that varies.
func onlyInTheDashboard(screen, path string) *dashboardOnly {
	return &dashboardOnly{Screen: screen, Path: path, Why: dashboardOnlyReason}
}

// note is the paragraph describe appends to such a command's long description,
// which is what `--help` prints and what `kitchen schema` publishes as the
// command's description. It is wrapped like the rest of them: a paragraph
// assembled from a constant and a screen name has no natural line breaks, and
// one long line in the middle of a help page is the only thing on it that
// looks like a mistake.
func (d *dashboardOnly) note() string {
	return wrapped("This command is the dashboard's for now: "+dashboardOnlyReason+
		". Run it in the dashboard: "+d.Screen+" ("+d.Path+").") + "\n\n" +
		wrapped(`See docs/CLI.md, "The platform commands are the dashboard's for now", `+
			"for the decision and for what would change it.")
}

// helpWidth is what the long descriptions in this package are already written
// to.
const helpWidth = 76

// wrapped breaks a paragraph at helpWidth on spaces. A word longer than the
// width goes on its own line rather than being cut: a URL or a path is worth
// more whole than aligned.
func wrapped(paragraph string) string {
	words := strings.Fields(paragraph)
	if len(words) == 0 {
		return ""
	}
	lines, line := []string{}, words[0]
	for _, word := range words[1:] {
		if len([]rune(line))+1+len([]rune(word)) > helpWidth {
			lines = append(lines, line)
			line = word
			continue
		}
		line += " " + word
	}
	return strings.Join(append(lines, line), "\n")
}

// hint is what the refusal says under the API's own sentence.
func (d *dashboardOnly) hint() string {
	return dashboardOnlyReason + ". Do it in the dashboard, under " +
		d.Screen + " (" + d.Path + "); see docs/CLI.md"
}

// dashboardOnlyRefusal turns the API's bare refusal of one of these commands
// into the decision behind it.
//
// The code — and so the exit status — is untouched: a caller branching on 4
// still gets 4, because this is a permission failure and nothing else. The
// API's own sentence stays the message, for the reason every message from the
// API does: it was written to be read by whoever sent the request. What is
// added is the half the API cannot know, which is that no credential this CLI
// can hold would have worked, and where the operation does exist.
func dashboardOnlyRefusal(cmd *cobra.Command, f *failure) *failure {
	if f == nil || f.Code != codeForbidden || cmd == nil {
		return f
	}
	m, ok := metaOf(cmd)
	if !ok || m.Needs.Platform == nil {
		return f
	}
	return f.withHint(m.Needs.Platform.hint())
}
