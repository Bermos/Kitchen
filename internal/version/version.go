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
// the manager and the preview gate — report it, and the dashboard shows it.
package version

// Version is the release, as a bare SemVer string with no leading "v":
// "0.2.0" on a published image, whatever `git describe` says on a local build,
// and "dev" when nothing set it.
//
// The linker fills it in, so a release bumps no source file: see LDFLAGS in
// the Makefile and the VERSION build argument in the Dockerfile, which the
// publish workflow passes the version release-please decided on. The image
// build context has no .git in it, which is why the value has to be handed in
// rather than worked out here.
var Version = "dev"
