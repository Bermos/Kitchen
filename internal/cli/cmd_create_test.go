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
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The create command, which is the one that runs before there is anything to
// run it against. What is checked here is the order it does things in — the
// preflight before the create, the link after it — and the two ways it can
// refuse without a terminal.

const (
	gitConnection      = "github"
	registryConnection = "kitchen"
)

// twoConnections is a platform offering one of each capability, so nothing has
// to be chosen.
func twoConnections() []connection {
	return []connection{
		{Name: gitConnection, Provider: "github", Capabilities: []string{capabilityGitSource}, Ready: true},
		{Name: registryConnection, Provider: "registry", Capabilities: []string{capabilityImageStore}, Ready: true},
	}
}

func TestCreateProjectRunsThePreflightFirst(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{
		Detected: true, Framework: "Next.js", Strategy: "buildpacks", Port: 3000, Ref: "main",
	}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}

	detects := h.platform.sent("POST", "/connections/"+gitConnection+"/detect")
	if len(detects) != 1 {
		t.Fatalf("expected one preflight, got %d", len(detects))
	}
	asked := detectTarget{}
	if err := json.Unmarshal([]byte(detects[0].Body), &asked); err != nil {
		t.Fatalf("preflight body: %v", err)
	}
	if asked.Repo != "acme/shop" {
		t.Errorf("preflight asked about %q", asked.Repo)
	}

	answer := projectCreated{}
	h.answer(&answer)
	if answer.Project.Name != testProject {
		t.Errorf("created %q", answer.Project.Name)
	}
	if answer.Detection == nil || answer.Detection.Framework != "Next.js" {
		t.Errorf("the verdict is not in the answer: %+v", answer.Detection)
	}
	if answer.Path == "" {
		t.Error("nothing was linked")
	}
	if link, _, err := findLink(h.work); err != nil || link == nil || link.Project != testProject {
		t.Errorf("the working directory is not linked: %+v %v", link, err)
	}
}

func TestCreateProjectSendsTheBuildContext(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true, Framework: "Go", Strategy: "buildpacks"}

	if code := h.run("projects", "create", testProject, "--repo", "acme/mono",
		"--root-directory", "apps/shop", "--dockerfile", "Dockerfile.web",
		"--dockerfile-target", "web",
		"--production-branch", "trunk", "--previews",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}

	creates := h.platform.sent("POST", "/projects")
	if len(creates) != 1 {
		t.Fatalf("expected one create, got %d", len(creates))
	}
	sent := newProject{}
	if err := json.Unmarshal([]byte(creates[0].Body), &sent); err != nil {
		t.Fatalf("create body: %v", err)
	}
	if sent.RootDirectory != "apps/shop" || sent.DockerfilePath != "Dockerfile.web" {
		t.Errorf("the build context did not travel with the project: %+v", sent)
	}
	// The stage travels with it too, and for a sharper reason: the first
	// build starts with the project, and a build of the wrong stage succeeds.
	if sent.DockerfileTarget != "web" {
		t.Errorf("the Dockerfile target did not travel with the project: %+v", sent)
	}
	if sent.ProductionBranch != "trunk" {
		t.Errorf("production branch %q", sent.ProductionBranch)
	}
	if sent.Previews == nil || !*sent.Previews {
		t.Errorf("previews %v", sent.Previews)
	}
}

// Previews left alone is the platform's default, not off: a pointer that is
// never set is a field the request does not carry.
func TestCreateProjectLeavesPreviewsToThePlatform(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true, Framework: "Go"}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if body := h.platform.sent("POST", "/projects")[0].Body; strings.Contains(body, "previews") {
		t.Errorf("the request decided previews for the platform: %s", body)
	}
}

// A repository nothing was recognised in is a question, and a question with no
// terminal to answer it is a failure naming the flag that would have.
func TestCreateProjectRefusesAnUnrecognisedRepositoryWithoutYes(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: false, Message: "no framework recognised"}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code == 0 {
		t.Fatal("created a project nothing was recognised in without being asked")
	}
	if hint := h.failure().Hint; !strings.Contains(hint, "--yes") {
		t.Errorf("the failure does not name --yes: %q", hint)
	}
	if creates := h.platform.sent("POST", "/projects"); len(creates) != 0 {
		t.Errorf("the project was created anyway: %+v", creates)
	}
}

func TestCreateProjectAcceptsAnUnrecognisedRepositoryWithYes(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: false}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop", "--yes",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if creates := h.platform.sent("POST", "/projects"); len(creates) != 1 {
		t.Errorf("expected one create, got %d", len(creates))
	}
}

// A Dockerfile is an answer in itself, so a repository with one is not a
// question however little else was recognised.
func TestCreateProjectDoesNotAskAboutADockerfile(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: false, Dockerfile: true}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

// A stage the Dockerfile does not declare is a question, not a refusal — the
// preflight read one commit, and the change about to be pushed may add it —
// but it is asked, because the alternative is a build several minutes later
// failing in BuildKit's words about an option nobody typed.
func TestCreateProjectAsksAboutAStageTheDockerfileDoesNotHave(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{
		Detected: true, Framework: "dockerfile", Strategy: "dockerfile",
		Dockerfile: true, Stages: []string{"deps", "build", "web"},
	}

	// Nothing to answer the question, so it is a failure naming the flag
	// rather than a wait — every question here has one.
	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--dockerfile-target", "runtime",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code == 0 {
		t.Fatalf("a stage the file does not declare was not questioned: %s", h.stderr.String())
	}
	if creates := h.platform.sent("POST", "/projects"); len(creates) != 0 {
		t.Errorf("the project was created anyway: %d creates", len(creates))
	}

	// And --yes answers it, because the person may know something the
	// preflight does not.
	h = newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{
		Detected: true, Framework: "dockerfile", Strategy: "dockerfile",
		Dockerfile: true, Stages: []string{"deps", "build", "web"},
	}
	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--dockerfile-target", "runtime", "--yes",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}

	// A stage the file does declare is no question at all.
	h = newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{
		Detected: true, Framework: "dockerfile", Strategy: "dockerfile",
		Dockerfile: true, Stages: []string{"deps", "build", "web"},
	}
	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--dockerfile-target", "web",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
}

// The preflight is advice. A platform that cannot give any is not a reason to
// refuse to create a project.
func TestCreateProjectSurvivesAnUnavailablePreflight(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = nil

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	answer := projectCreated{}
	h.answer(&answer)
	if answer.Detection != nil {
		t.Errorf("a verdict was reported that was never given: %+v", answer.Detection)
	}
	if answer.Project.Name != testProject {
		t.Errorf("created %q", answer.Project.Name)
	}
}

// Two candidates and no terminal is the CLI's one rule about prompts: it says
// which flag would have answered, and which answers it would have accepted.
func TestCreateProjectNamesTheConnectionsItWouldHaveOffered(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = append(twoConnections(),
		connection{Name: "gitlab", Capabilities: []string{capabilityGitSource}, Ready: true})
	h.platform.detected = &detection{Detected: true}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--registry", registryConnection, "--json"); code == 0 {
		t.Fatal("chose a connection with nobody to ask")
	}
	hint := h.failure().Hint
	if !strings.Contains(hint, "--connection") || !strings.Contains(hint, "gitlab") {
		t.Errorf("the failure names neither the flag nor the choices: %q", hint)
	}
}

// One candidate is not a choice, so it is not a question either.
func TestCreateProjectTakesTheOnlyConnectionThatCanDoTheJob(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if detects := h.platform.sent("POST", "/connections/"+gitConnection+"/detect"); len(detects) != 1 {
		t.Errorf("the git connection was not the one chosen: %+v", h.platform.requests)
	}
	sent := newProject{}
	if err := json.Unmarshal([]byte(h.platform.sent("POST", "/projects")[0].Body), &sent); err != nil {
		t.Fatalf("create body: %v", err)
	}
	if sent.Registry != registryConnection {
		t.Errorf("registry %q", sent.Registry)
	}
}

func TestCreateProjectWithNoRepositoryAnywhereSaysSo(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()

	if code := h.run("projects", "create", testProject, "--json"); code == 0 {
		t.Fatal("created a project of no repository")
	}
	if hint := h.failure().Hint; !strings.Contains(hint, "--repo") {
		t.Errorf("the failure does not name --repo: %q", hint)
	}
}

// The repository and the name both default from the checkout, which is what
// makes `kitchen projects create` in a repository a one-word command.
func TestCreateProjectTakesTheRepositoryFromTheCheckout(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true}
	initRepo(t, h.work, "git@github.com:acme/shop.git")

	if code := h.run("projects", "create", "--yes", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	sent := newProject{}
	if err := json.Unmarshal([]byte(h.platform.sent("POST", "/projects")[0].Body), &sent); err != nil {
		t.Fatalf("create body: %v", err)
	}
	if sent.Repo != "acme/shop" || sent.Name != testProject {
		t.Errorf("took %q and %q from the checkout", sent.Repo, sent.Name)
	}
}

func TestCreateProjectCanBeToldNotToLink(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop", "--link=false",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	answer := projectCreated{}
	h.answer(&answer)
	if answer.Path != "" {
		t.Errorf("wrote %q despite --link=false", answer.Path)
	}
	if _, err := os.Stat(filepath.Join(h.work, ".kitchen", "project.json")); !os.IsNotExist(err) {
		t.Errorf("a link was written anyway: %v", err)
	}
}

// The origin URL is read the two ways people clone.
func TestOriginRepoReadsBothURLForms(t *testing.T) {
	for _, that := range []struct {
		url  string
		want string
	}{
		{"git@github.com:acme/shop.git", "acme/shop"},
		{"https://github.com/acme/shop.git", "acme/shop"},
		{"https://github.com/acme/shop", "acme/shop"},
		{"ssh://git@github.com/acme/shop.git", "acme/shop"},
		{"/somewhere/local", "somewhere/local"},
	} {
		dir := t.TempDir()
		initRepo(t, dir, that.url)
		if got := originRepo(t.Context(), dir); got != that.want {
			t.Errorf("%s: got %q, want %q", that.url, got, that.want)
		}
	}
}

func TestOriginRepoWithoutAnOriginIsEmpty(t *testing.T) {
	if got := originRepo(t.Context(), t.TempDir()); got != "" {
		t.Errorf("got %q from a directory that is no checkout", got)
	}
}

// initRepo makes dir a git repository whose origin is url.
func initRepo(t *testing.T, dir, url string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"remote", "add", "origin", url},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v\n%s", args, err, out)
		}
	}
}

// The preflight resolves the default branch of a repository that named none,
// and that is the branch the project deploys from: a repository whose trunk is
// "trunk" would otherwise be created on the platform's default of "main" and
// have its first build look for a branch that is not there.
func TestCreateProjectTakesTheProductionBranchFromThePreflight(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true, Framework: "Go", Ref: "trunk"}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}

	// The preflight itself is asked without one, which is what makes the
	// platform resolve it.
	asked := detectTarget{}
	if err := json.Unmarshal([]byte(h.platform.sent("POST", "/connections/"+gitConnection+"/detect")[0].Body),
		&asked); err != nil {
		t.Fatalf("preflight body: %v", err)
	}
	if asked.Ref != "" {
		t.Errorf("the preflight named a branch nobody gave it: %q", asked.Ref)
	}

	sent := newProject{}
	if err := json.Unmarshal([]byte(h.platform.sent("POST", "/projects")[0].Body), &sent); err != nil {
		t.Fatalf("create body: %v", err)
	}
	if sent.ProductionBranch != "trunk" {
		t.Errorf("created on %q, not the branch the preflight read", sent.ProductionBranch)
	}
}

// A branch that was given wins: the preflight was asked about it in the first
// place.
func TestCreateProjectKeepsTheProductionBranchItWasGiven(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true, Ref: "trunk"}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--production-branch", "stable",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	sent := newProject{}
	if err := json.Unmarshal([]byte(h.platform.sent("POST", "/projects")[0].Body), &sent); err != nil {
		t.Fatalf("create body: %v", err)
	}
	if sent.ProductionBranch != "stable" {
		t.Errorf("created on %q rather than the branch it was given", sent.ProductionBranch)
	}
}

// A Dockerfile build has no port to report — the image decides its own — and
// "on port 0" reads as a port rather than as the absence of one.
func TestDescribingADetectionLeavesOutAPortThereIsNone(t *testing.T) {
	for verdict, want := range map[*detection]string{
		{Detected: true, Framework: "dockerfile", Strategy: "dockerfile"}:     "detected dockerfile, built with dockerfile",
		{Detected: true, Framework: "go", Strategy: "buildpacks", Port: 8080}: "detected go, built with buildpacks on port 8080",
		{Detected: false, Dockerfile: true}:                                   "no framework recognised, building the Dockerfile",
	} {
		if got := describeDetection(verdict); got != want {
			t.Errorf("described %+v as %q, want %q", verdict, got, want)
		}
	}
}

// The link is written on the same terms `kitchen link` writes one, and the
// four cases are these: a directory that was not linked, one already linked to
// this project, and one linked to another — with an answer and without one.
//
// The last is the one that cost somebody an afternoon: the link was replaced
// without a word, and the next `kitchen builds` was about a project the
// platform had never heard of.
func TestCreateProjectLinksADirectoryThatWasNotLinked(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	answer := projectCreated{}
	h.answer(&answer)
	if answer.Path == "" || answer.Replaced != "" {
		t.Errorf("an unlinked directory reported a replacement: %+v", answer)
	}
}

func TestCreateProjectDoesNotAskAboutALinkToItself(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true}
	if _, err := writeLink(h.work, &link{Project: testProject, API: h.platform.server.URL}); err != nil {
		t.Fatal(err)
	}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	answer := projectCreated{}
	h.answer(&answer)
	if answer.Replaced != "" {
		t.Errorf("replaced a link to the same project: %+v", answer)
	}
}

func TestCreateProjectRefusesToReplaceALinkWithoutYes(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true}
	if _, err := writeLink(h.work, &link{Project: "billing", API: h.platform.server.URL}); err != nil {
		t.Fatal(err)
	}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != exitUsage {
		t.Fatalf("exit %d, wanted %d: %s", code, exitUsage, h.stderr.String())
	}
	refusal := h.failure()
	if !strings.Contains(refusal.Error(), "billing") || !strings.Contains(refusal.Hint, "--yes") {
		t.Errorf("the refusal does not name the link or the flag that answers it: %+v", refusal)
	}
	// Asked before the project is written, so a refusal leaves nothing behind
	// — neither a project nor a link somebody has to put back.
	if creates := h.platform.sent("POST", "/projects"); len(creates) != 0 {
		t.Errorf("the project was created anyway: %+v", creates)
	}
	if existing, _, err := findLink(h.work); err != nil || existing == nil || existing.Project != "billing" {
		t.Errorf("the link was replaced anyway: %+v %v", existing, err)
	}
}

func TestCreateProjectSaysWhichLinkItReplaced(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true}
	if _, err := writeLink(h.work, &link{Project: "billing", API: h.platform.server.URL}); err != nil {
		t.Fatal(err)
	}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop", "--yes",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	answer := projectCreated{}
	h.answer(&answer)
	if answer.Replaced != "billing" {
		t.Errorf("the answer does not say what it replaced: %+v", answer)
	}
	if existing, _, err := findLink(h.work); err != nil || existing == nil || existing.Project != testProject {
		t.Errorf("the link was not replaced: %+v %v", existing, err)
	}

	// And the person reading text rather than JSON is told the same thing.
	h.platform.detected = &detection{Detected: true}
	if _, err := writeLink(h.work, &link{Project: "billing", API: h.platform.server.URL}); err != nil {
		t.Fatal(err)
	}
	if code := h.run("projects", "create", testProject, "--repo", "acme/shop", "--yes",
		"--connection", gitConnection, "--registry", registryConnection); code != exitOK {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	if out := h.stdout.String(); !strings.Contains(out, "replacing the link to billing") {
		t.Errorf("the output does not say what it replaced:\n%s", out)
	}
}

// A prompt somebody is there to answer is answered, and "no" is a cancel that
// leaves both the project and the link alone.
func TestCreateProjectAsksBeforeReplacingALink(t *testing.T) {
	h := newHarness(t)
	h.platform.connections = twoConnections()
	h.platform.detected = &detection{Detected: true}
	h.stdinTerminal = true
	h.stdin = strings.NewReader("n\n")
	if _, err := writeLink(h.work, &link{Project: "billing", API: h.platform.server.URL}); err != nil {
		t.Fatal(err)
	}

	if code := h.run("projects", "create", testProject, "--repo", "acme/shop",
		"--connection", gitConnection, "--registry", registryConnection, "--json"); code == exitOK {
		t.Fatal("replaced a link the answer refused")
	}
	if question := h.stderr.String(); !strings.Contains(question, "is linked to billing") {
		t.Errorf("the question does not say what is linked where: %q", question)
	}
	if creates := h.platform.sent("POST", "/projects"); len(creates) != 0 {
		t.Errorf("the project was created anyway: %+v", creates)
	}
}
