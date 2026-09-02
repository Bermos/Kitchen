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
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// What a project's Dockerfile target means, in one place, beside what its
// root directory means — because the same four readers have to agree about it
// and one of them cannot use it at all.
//
// **The target is a stage of the project's Dockerfile**, and the image the
// build ships is that stage's. Empty is the file's last stage, which is what
// a build with nothing to say about it has always shipped and still does.
//
// It exists because the last stage is often not the runtime: one file that
// also builds a test image or a toolchain ends on whichever of them was
// written last, and shipping that one is a **successful build of the wrong
// thing** — no error, a green pipeline, and the discovery at runtime. So the
// two ways of getting it wrong are both refused rather than resolved:
//
//   - A target the Dockerfile does not declare fails the build. BuildKit
//     refuses an unknown `--target` itself; the build reconciler recognises
//     that refusal and says which stages the file does have.
//   - A target on a build that resolves to buildpacks fails the build too.
//     The CNB lifecycle has no notion of a stage — it produces one image from
//     one application directory — so a target it was handed could only be
//     ignored, and an ignored target is the same wrong image again.
//
// Stages is what makes the first of those answerable before a build: the
// preflight reads the Dockerfile and lists the names it declares, so the
// dashboard can offer them rather than leave somebody to type one and find
// out several minutes later.

// stageName is what BuildKit will accept as a stage name: a letter, then
// letters, digits, dots, dashes and underscores. Anything else is refused
// where it is written rather than at the far end of a build — the dockerfile
// frontend would reject the Dockerfile itself, so a name it cannot hold could
// never match a stage that exists.
var stageName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]*$`)

// StageNameRule is the sentence every refusal of a target uses, so the API,
// the repository file and the CRD all describe the same rule the same way.
const StageNameRule = "a stage name starts with a letter and holds letters, digits, dots, dashes and underscores"

// NormalizeTarget spells a project's Dockerfile target the way a build spells
// it: no surrounding whitespace, and empty when the project named none.
//
// Case is left alone. A Dockerfile's stage names are matched
// case-insensitively by the frontend, and lowercasing here would make what
// the platform reports back differ from what somebody wrote.
func NormalizeTarget(target string) string {
	return strings.TrimSpace(target)
}

// ValidTarget reports whether a target is a name a Dockerfile stage could
// have. An empty target is valid: it is the file's last stage.
func ValidTarget(target string) bool {
	target = NormalizeTarget(target)
	if target == "" {
		return true
	}
	return len(target) <= 128 && stageName.MatchString(target)
}

// HasStage reports whether a target names one of the stages a Dockerfile
// declares, matching the way the dockerfile frontend does: without regard to
// case. An empty target is the last stage, which every Dockerfile has.
//
// A file whose stages could not be read answers false for nothing: callers
// pass the stages they actually read, and an empty list means "not known"
// rather than "none", which is why nothing here refuses on that basis.
func HasStage(stages []string, target string) bool {
	target = NormalizeTarget(target)
	if target == "" {
		return true
	}
	for _, stage := range stages {
		if strings.EqualFold(stage, target) {
			return true
		}
	}
	return false
}

// Stages are the named stages a Dockerfile declares, in the order the file
// declares them: every `FROM … AS <name>`, and nothing else.
//
// It is a reader of one instruction rather than a Dockerfile parser. What it
// is for is telling somebody which names they may choose from, so the failure
// mode that matters is naming a stage that is not there — and a file whose
// last stage is unnamed is the ordinary case, which is why an unnamed stage
// contributes nothing here rather than an index.
//
// Line continuations are joined because a `FROM` can carry a `--platform`
// onto a second line, and comments are dropped because a commented-out stage
// is not a stage.
func Stages(dockerfile []byte) []string {
	stages := []string{}
	for _, line := range logicalLines(string(dockerfile)) {
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.EqualFold(fields[0], "FROM") {
			continue
		}
		if !strings.EqualFold(fields[len(fields)-2], "AS") {
			continue
		}
		name := fields[len(fields)-1]
		if stageName.MatchString(name) {
			stages = append(stages, name)
		}
	}
	return stages
}

// logicalLines is the Dockerfile's lines with continuations joined and
// comments removed, which is as much of the file's syntax as reading its
// stage names needs.
func logicalLines(content string) []string {
	lines := []string{}
	current := strings.Builder{}
	for _, raw := range strings.Split(content, "\n") {
		line := strings.TrimRight(raw, "\r")
		if current.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if trimmed := strings.TrimRight(line, " \t"); strings.HasSuffix(trimmed, `\`) {
			current.WriteString(strings.TrimSuffix(trimmed, `\`))
			current.WriteString(" ")
			continue
		}
		current.WriteString(line)
		lines = append(lines, current.String())
		current.Reset()
	}
	if current.Len() > 0 {
		lines = append(lines, current.String())
	}
	return lines
}

// DockerfileStages reads the project's Dockerfile at a commit and returns the
// stages it declares. It is what the preflight adds to a repository it has
// already listed: one more read, and only when the file is where the project
// says it is.
//
// A file that cannot be read is not a failure — the answer is simply "not
// known", which is how a caller that could not read it and a file with no
// named stages are told apart: the error is returned, and the list is nil.
func DockerfileStages(
	ctx context.Context,
	reader gitprovider.SourceReader,
	target Target,
) ([]string, error) {
	dockerfile := NormalizeDockerfile(target.DockerfilePath)
	if LeavesRoot(dockerfile) {
		// Not this project's file: the builder is handed the build root and
		// nothing above it. Detection says the same about its presence.
		return nil, fmt.Errorf("%w: %s is outside the build root", ErrNotRecognised, dockerfile)
	}
	content, err := reader.ReadFile(ctx, target.Repo, target.Ref, path.Join(target.RootDirectory, dockerfile))
	switch {
	case errors.Is(err, gitprovider.ErrFileNotFound):
		return nil, fmt.Errorf("%w: %s", ErrNotRecognised, dockerfile)
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrSourceUnreadable, err)
	}
	return Stages(content), nil
}
