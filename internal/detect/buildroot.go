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

package detect

import (
	"path"
	"strings"
)

// DefaultDockerfile is where a project's container build instructions are
// when the project has not said: the conventional name, at the build root.
const DefaultDockerfile = "Dockerfile"

// What a project's root directory means, in one place, because four things
// have to agree about it and two of them reach it by different routes.
//
// **The root directory is the build root**: the directory in the repository
// that is built, and the directory every path the project declares is
// relative to — its `kitchen.json`, its Dockerfile. Nothing above it is part
// of the build.
//
// The two build strategies reach that meaning differently, and neither
// pretends to be the other:
//
//   - BuildKit takes the commit as a git context and clones it itself, so the
//     root directory goes into the context reference (`#<sha>:<root>`). The
//     git source hands the frontend that directory as the whole context,
//     which is what makes `filename` — the Dockerfile — relative to it.
//   - The CNB lifecycle only ever builds a directory that is already on disk,
//     so the clone lands the repository in an init container and the
//     lifecycle is pointed inside it with `-app`.
//
// Detection is the third: it lists the build root through the provider's API
// and looks for the project's Dockerfile relative to it, so that the answer a
// preflight gives is the one the build will reach.
//
// The normalisation below is what stops those three disagreeing over spelling
// — `apps/shop`, `apps/shop/`, `./apps/shop` and `/apps/shop` are one
// directory, and `Dockerfile` and `./Dockerfile` are one file.

// NormalizeRoot spells a project's root directory the way a build spells it:
// no surrounding whitespace, no leading or trailing slash, cleaned, and empty
// for the repository itself. The CRD's default is ".", which every caller has
// to read as "no subdirectory at all" rather than as a path component.
func NormalizeRoot(root string) string {
	root = strings.Trim(strings.TrimSpace(root), "/")
	if root == "" {
		return ""
	}
	if cleaned := path.Clean(root); cleaned != "." {
		return cleaned
	}
	return ""
}

// NormalizeDockerfile spells a project's Dockerfile path the way a build
// spells it: cleaned, relative to the build root, and the conventional
// "Dockerfile" when the project named none.
//
// It deliberately leaves a path that reaches outside the build root alone
// rather than repairing it — see LeavesRoot, which is what refuses one.
func NormalizeDockerfile(dockerfile string) string {
	dockerfile = strings.TrimSpace(dockerfile)
	if dockerfile == "" {
		return DefaultDockerfile
	}
	return path.Clean(dockerfile)
}

// LeavesRoot reports whether a path a project declared reaches outside its
// build root — an absolute path, or one that climbs out with "..".
//
// Such a path is refused rather than resolved, because there is nothing above
// the build root to resolve it against: BuildKit is handed that directory as
// its entire context, and the lifecycle is pointed at it. A repository's own
// kitchen.json has always been refused one; this is the same rule for the
// same path set on the project.
func LeavesRoot(declared string) bool {
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return false
	}
	if path.IsAbs(declared) {
		return true
	}
	cleaned := path.Clean(declared)
	return cleaned == ".." || strings.HasPrefix(cleaned, "../")
}
