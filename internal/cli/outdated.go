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

	"github.com/blang/semver/v4"

	"github.com/Bermos/Kitchen/internal/version"
)

// Telling somebody their `kitchen` is behind the installation it just talked
// to.
//
// One tag versions the chart, both images and this binary, so the two numbers
// are directly comparable and "older" is a fact rather than a guess. What it
// means is the ordinary thing: the platform has grown routes, flags and output
// fields this binary has never heard of, and the first sign of that is usually
// a command failing in a way that reads like a bug in the platform.
//
// Three properties, and they are the reasons this is shaped the way it is:
//
//   - **It costs no request.** Every API response carries version.Header, so
//     the number arrives on the back of the call the command was making
//     anyway. Nothing is polled, nothing is fetched at startup, and a command
//     that talks to no platform compares nothing.
//   - **It is never the answer.** It goes to stderr through the printer, which
//     under --json makes it a `warning` event on the stream warnings already
//     use. stdout still carries the answer and nothing else, so a script that
//     pipes this CLI into jq does not learn about it and does not have to.
//   - **It never turns a working command into a failing one.** It is a
//     sentence after the fact, the exit status is untouched, and anything it
//     cannot work out — a `dev` build, a platform too old to say, a number
//     neither side can parse — it says nothing about at all.

// noVersionCheck turns the whole thing off, for somebody who has decided they
// are staying on this version and does not want to be told again.
const noVersionCheck = "KITCHEN_NO_VERSION_CHECK"

// notePlatformVersion records what the platform last said it was running. It
// is the client's `noticed` callback, so it is called once per response and
// keeps the newest answer: a command that talks to two installations — `login`
// against one while a link file names another — should be measured against the
// one it ended up working with.
func (r *Runtime) notePlatformVersion(reported string) {
	r.platformVersion = strings.TrimSpace(reported)
}

// warnIfBehind says so, once, at the end of a command line, if this binary is
// older than the installation it was talking to. It is called from Execute for
// every command whether it succeeded or failed, since a client that is behind
// is a likely reason for the failure.
func (r *Runtime) warnIfBehind() {
	if truthy(r.env(noVersionCheck)) {
		return
	}
	if !behind(version.Version, r.platformVersion) {
		return
	}
	r.printer().warn("kitchen %s is older than the installation it is talking to, which is on %s. "+
		"Commands may be missing routes, flags and fields this release has never seen — update with "+
		"`go install github.com/Bermos/Kitchen/cmd/kitchen@v%s` (or set %s to stop being told).",
		version.Version, r.platformVersion, r.platformVersion, noVersionCheck)
}

// behind reports whether this binary's release is older than the platform's.
//
// It is deliberately silent rather than approximate about everything it cannot
// answer, because the only cost of saying nothing is a missing nudge, and the
// cost of saying the wrong thing is somebody chasing a version skew that is
// not there:
//
//   - **A build with no release** — "dev", or anything else that is not a
//     SemVer — compares against nothing. That is every `go build` in this
//     working copy, and telling a maintainer their working copy is behind the
//     cluster they are testing against would be noise on every command.
//   - **A platform that said nothing**, which is one running a release from
//     before the header existed. There is no number to compare.
//   - **A binary that is ahead**, which is a client built from a branch, or an
//     installation that has not been upgraded yet. It is a real skew and it is
//     not this warning's: the fix is on the platform, and nobody holding the
//     CLI can apply it.
//
// Prerelease ordering is SemVer's own, so 0.16.0-rc.1 is behind 0.16.0 and
// ahead of 0.15.0.
func behind(mine, platform string) bool {
	if mine == "" || platform == "" {
		return false
	}
	client, err := semver.ParseTolerant(mine)
	if err != nil {
		return false
	}
	installation, err := semver.ParseTolerant(platform)
	if err != nil {
		return false
	}
	return client.LT(installation)
}
