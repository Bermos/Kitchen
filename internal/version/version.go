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

// Package version carries the release this binary was built from. It is a
// package rather than a constant in main because both binaries in the image —
// the manager and the preview gate — report it, the dashboard shows it, and
// the CLI puts it in its user agent.
package version

import (
	"runtime/debug"
	"strings"
)

// dev is what the value is when nothing has said otherwise: no linker flag,
// and no module version recorded in the build.
const dev = "dev"

// Version is the release, as a bare SemVer string with no leading "v":
// "0.2.0" on a published image, whatever `git describe` says on a local build,
// and "dev" when nothing set it.
//
// The linker fills it in, so a release bumps no source file: see LDFLAGS in
// the Makefile and the VERSION build argument in the Dockerfile, which the
// publish workflow passes the version release-please decided on. The image
// build context has no .git in it, which is why the value has to be handed in
// rather than worked out here.
var Version = dev

// The CLI is the one binary that is not built by the Makefile on the way to
// somewhere else: `go install github.com/Bermos/Kitchen/cmd/kitchen@v0.15.0`
// is the documented way to get it (docs/CLI.md), and that passes no linker
// flags at all. The toolchain does record which module version it built,
// though, so the value is there to be read when the linker left nothing —
// without it every installed CLI reports "dev" and its user agent says so to
// the API.
func init() {
	Version = resolve(Version, debug.ReadBuildInfo)
}

// resolve answers what the binary should report, given the value the linker
// left behind and the build information the toolchain recorded. A linked value
// always wins: it is the version the release was cut as, and on a local build
// it is the more precise of the two.
func resolve(linked string, read func() (*debug.BuildInfo, bool)) string {
	if linked != dev {
		return linked
	}
	info, ok := read()
	if !ok || info == nil {
		return linked
	}
	// "(devel)" is what a build from a working directory records, and it says
	// nothing "dev" does not already say.
	built := strings.TrimPrefix(info.Main.Version, "v")
	if built == "" || built == "(devel)" {
		return linked
	}
	return built
}
