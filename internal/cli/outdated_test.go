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

	"github.com/Bermos/Kitchen/internal/version"
)

// Three releases in ascending order. The tests below are all about which of
// two things is on which of them, so they are named for their order rather
// than for whichever side is holding them, and spelled once so an assertion
// cannot disagree with the setup that produced it.
const (
	older  = "0.15.0"
	newer  = "0.16.0"
	newest = "0.17.2"
)

func TestBehind(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mine     string
		platform string
		want     bool
	}{
		{name: "a minor behind", mine: older, platform: newer, want: true},
		{name: "a patch behind", mine: older, platform: "0.15.1", want: true},
		{name: "the same release", mine: older, platform: older},
		{name: "ahead of the installation", mine: newer, platform: older},
		{name: "a leading v on either side", mine: "v" + older, platform: "v" + newer, want: true},

		// A prerelease sorts where SemVer puts it: behind the release it is a
		// candidate for, ahead of the one before it.
		{name: "a candidate for the platform's release", mine: newer + "-rc.1", platform: newer, want: true},
		{name: "a candidate ahead of the platform", mine: newer + "-rc.1", platform: older},

		// Everything that cannot be compared says nothing rather than
		// guessing: the cost of a missing nudge is one missing sentence, and
		// the cost of a wrong one is somebody chasing a skew that is not there.
		{name: "a working copy build", mine: "dev", platform: newer},
		{name: "a platform too old to say", mine: older, platform: ""},
		{name: "a platform that said something else", mine: older, platform: "not-a-version"},
		{name: "neither side knows", mine: "dev", platform: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := behind(tc.mine, tc.platform); got != tc.want {
				t.Errorf("behind(%q, %q) = %v, want %v", tc.mine, tc.platform, got, tc.want)
			}
		})
	}
}

// stampVersion makes this binary report a release for the duration of a test.
// version.Version is a package variable the linker writes, and a test in a
// working copy would otherwise always be "dev" — which is exactly the value
// the check stays silent about, so nothing about the wiring could be seen.
func stampVersion(t *testing.T, release string) {
	t.Helper()
	was := version.Version
	version.Version = release
	t.Cleanup(func() { version.Version = was })
}

// TestTheCLISaysWhenItIsBehindTheInstallation is the wiring: the platform's
// release arrives on the response of a call the command was making anyway, and
// the warning lands on stderr without touching the answer or the exit status.
func TestTheCLISaysWhenItIsBehindTheInstallation(t *testing.T) {
	stampVersion(t, older)
	h := newHarness(t)
	h.platform.release = newest
	h.platform.projects = []project{{Name: "shop"}}

	if code := h.run("projects", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	// stdout is still the answer and only the answer, which is the promise a
	// script piping this into jq is relying on.
	var answered list[project]
	h.answer(&answered)
	if len(answered.Items) != 1 || answered.Items[0].Name != "shop" {
		t.Fatalf("the answer changed: %+v", answered)
	}

	warning := h.stderr.String()
	for _, want := range []string{older, newest, "older than the installation", noVersionCheck} {
		if !strings.Contains(warning, want) {
			t.Fatalf("the warning does not mention %q: %s", want, warning)
		}
	}
	// Under --json a warning is a `warning` event on stderr, which is the
	// shape every other one already uses.
	if !strings.Contains(warning, `"type":"warning"`) {
		t.Fatalf("the warning is not a JSON event: %s", warning)
	}

	// It costs no request of its own: the number came off the call the command
	// was already making.
	if extra := h.platform.sent("GET", configPath); len(extra) != 0 {
		t.Fatalf("the check fetched the configuration: %+v", extra)
	}
}

// The rest of the matrix, through the same wiring: who gets told and who does
// not.
func TestTheOutdatedWarningIsSaidOnlyWhenItIsTrue(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mine     string
		release  string
		suppress string
		want     bool
	}{
		{name: "behind the installation", mine: older, release: newest, want: true},
		{name: "on the installation's release", mine: newest, release: newest},
		{name: "ahead of the installation", mine: "0.18.0", release: newest},
		{name: "a platform too old to say", mine: older, release: ""},
		{name: "a working copy build", mine: "dev", release: newest},
		{
			name: "told to stop", mine: older, release: newest,
			suppress: "1",
		},
		{
			name: "told to stop, in words", mine: older, release: newest,
			suppress: "yes",
		},
		{
			// The variable answers the same yes/no everything else in this CLI
			// does, so a shell exporting an empty value is not a decision.
			name: "an empty value is not an answer", mine: older, release: newest,
			suppress: "", want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stampVersion(t, tc.mine)
			h := newHarness(t)
			h.platform.release = tc.release
			// Always set, so the empty case is a shell that exported the
			// variable with nothing in it rather than one that never set it.
			h.env[noVersionCheck] = tc.suppress

			if code := h.run("projects", "--json"); code != exitOK {
				t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
			}
			said := strings.Contains(h.stderr.String(), "older than the installation")
			if said != tc.want {
				t.Fatalf("warned = %v, want %v; stderr: %s", said, tc.want, h.stderr.String())
			}
		})
	}
}

// A command that failed is the more useful of the two moments to hear this: a
// route that is not there yet is what being behind feels like from the
// terminal, so the warning has to survive the failure path and leave the exit
// status alone.
func TestTheOutdatedWarningSurvivesAFailedCommand(t *testing.T) {
	stampVersion(t, older)
	h := newHarness(t)
	h.platform.release = newest
	h.platform.refuseStatus, h.platform.refuseMessage = 404, "no such route"

	code := h.run("projects", "--json")
	if code == exitOK {
		t.Fatalf("the command reported success: %s", h.stdout.String())
	}
	if !strings.Contains(h.stderr.String(), "older than the installation") {
		t.Fatalf("a failing command said nothing about the version: %s", h.stderr.String())
	}
	// The failure envelope is still the whole of stdout.
	var envelope map[string]*failure
	h.answer(&envelope)
	if envelope["error"] == nil {
		t.Fatalf("stdout is not the error envelope: %s", h.stdout.String())
	}
}

// Signing in reads /config.json before there is a credential for the
// installation at all, and that document carries the release in its body
// rather than in the header. It is the one moment somebody is most likely to
// be running a binary that predates the platform, so it must not be the one
// path that says nothing.
func TestLoginNoticesAnOlderCLI(t *testing.T) {
	stampVersion(t, older)
	h := newHarness(t)
	h.platform.release = newest
	delete(h.env, "KITCHEN_TOKEN")

	if code := h.run("login", "--api", h.platform.server.URL, "--api-key", "a-key", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "older than the installation") {
		t.Fatalf("login said nothing about the version: %s", h.stderr.String())
	}
}
