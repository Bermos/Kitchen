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
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two values repeated across these tests, named so a reader can see at a
// glance which string is the project under test and which is a variable's
// value.
const (
	testProject = "shop"
	testValue   = "debug"

	// testRefusal is the sentence the fake platform refuses with. The
	// assertions are about where it ends up rather than what it says.
	testRefusal = "adding a connection needs the operator role; you are a member"
)

func TestWhoamiAnswersTheAccountAsJSON(t *testing.T) {
	h := newHarness(t)

	if code := h.run("whoami", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	who := account{}
	h.answer(&who)
	if who.Email != "anna@example.com" || who.PlatformRole != "member" {
		t.Fatalf("unexpected account: %+v", who)
	}
	if h.stderr.Len() != 0 {
		t.Fatalf("stderr should be empty on success, got %q", h.stderr.String())
	}
}

// A refusal is the shape docs/API.md documents, and this CLI's own exit
// status: both halves matter, because a caller reads one of them.
func TestARefusalIsAnErrorEnvelopeAndAnExitCode(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		status  int
		message string
		code    string
		exit    int
	}{
		{"forbidden", 403, "you have viewer on shop; redeploying needs developer", codeForbidden, exitForbidden},
		{"not found", 404, "projects \"billing\" not found", codeNotFound, exitNotFound},
		{"conflict", 409, "the build already finished", codeConflict, exitConflict},
		{"no telemetry store", 503, "no telemetry store is installed", codeUnavailable, exitUnavailable},
		{"unauthenticated", 401, "no valid token", codeUnauthenticated, exitUnauthenticated},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			h := newHarness(t)
			h.platform.refuseStatus, h.platform.refuseMessage = testCase.status, testCase.message

			if code := h.run("projects", "--json"); code != testCase.exit {
				t.Fatalf("exit %d, wanted %d", code, testCase.exit)
			}
			refusal := h.failure()
			if refusal.Code != testCase.code {
				t.Fatalf("code %q, wanted %q", refusal.Code, testCase.code)
			}
			if !strings.Contains(refusal.Message, testCase.message) {
				t.Fatalf("the API's own sentence was lost: %q", refusal.Message)
			}
			if refusal.Status != testCase.status {
				t.Fatalf("status %d, wanted %d", refusal.Status, testCase.status)
			}
		})
	}
}

// A command that needs a project and cannot find one has its own exit status,
// and says all three ways of supplying one.
func TestNoProjectIsItsOwnFailure(t *testing.T) {
	h := newHarness(t)

	if code := h.run("status", "--json"); code != exitNotLinked {
		t.Fatalf("exit %d, wanted %d", code, exitNotLinked)
	}
	refusal := h.failure()
	if refusal.Code != codeNotLinked {
		t.Fatalf("code %q", refusal.Code)
	}
	for _, wanted := range []string{"kitchen link", "--project", "KITCHEN_PROJECT"} {
		if !strings.Contains(refusal.Hint, wanted) {
			t.Fatalf("the hint does not mention %s: %q", wanted, refusal.Hint)
		}
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	h := newHarness(t)

	if code := h.run("teleport", "--json"); code != exitUsage {
		t.Fatalf("exit %d, wanted %d", code, exitUsage)
	}
	if refusal := h.failure(); refusal.Code != codeUsage {
		t.Fatalf("code %q", refusal.Code)
	}
}

func TestLinkWritesTheProjectAndTheOtherCommandsReadIt(t *testing.T) {
	h := newHarness(t)
	h.platform.project = &project{Name: testProject, Role: "developer", Repo: "acme/shop",
		ProductionBranch: "main", ProductionEnvironment: "shop-production"}

	if code := h.run("link", "--project", testProject, "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	written := linked{}
	h.answer(&written)
	if written.Project != testProject || written.Role != "developer" {
		t.Fatalf("unexpected answer: %+v", written)
	}

	path := filepath.Join(h.work, linkDir, linkFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the link file was not written: %v", err)
	}
	stored := link{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("the link file is not JSON: %v", err)
	}
	if stored.Project != testProject || stored.API != h.platform.server.URL {
		t.Fatalf("unexpected link file: %+v", stored)
	}

	// The whole point of the link: the next command needs no flags. Unset the
	// API too, so it is the file answering both questions.
	delete(h.env, "KITCHEN_API")
	h.platform.environments = []environment{{Name: "shop-production", Project: testProject, Phase: "Live"}}
	if code := h.run("status", "--json"); code != exitOK {
		t.Fatalf("status after link: exit %d, stderr: %s", code, h.stderr.String())
	}
	answer := projectStatus{}
	h.answer(&answer)
	if answer.Project.Name != testProject || len(answer.Environments) != 1 {
		t.Fatalf("unexpected status: %+v", answer)
	}
}

// The link is found from a subdirectory, which is where anybody actually
// stands when they type a command.
func TestTheLinkIsFoundFromASubdirectory(t *testing.T) {
	h := newHarness(t)
	h.platform.project = &project{Name: testProject, Role: "developer"}
	if code := h.run("link", "--project", testProject, "--json"); code != exitOK {
		t.Fatalf("link: exit %d", code)
	}

	nested := filepath.Join(h.work, "services", "api")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	h.work = nested

	if code := h.run("env", "list", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
}

// Interactive-only work must never block: with no terminal, a command that
// would have asked names the flag that answers it instead.
func TestAQuestionWithNobodyToAnswerItIsAUsageError(t *testing.T) {
	h := newHarness(t)
	h.platform.projects = []project{{Name: testProject, Role: "developer"}, {Name: "billing", Role: "viewer"}}

	if code := h.run("link", "--json"); code != exitUsage {
		t.Fatalf("exit %d, wanted %d", code, exitUsage)
	}
	refusal := h.failure()
	if !strings.Contains(refusal.Hint, "--project") {
		t.Fatalf("the hint does not name the flag: %q", refusal.Hint)
	}
	if !strings.Contains(refusal.Hint, testProject) || !strings.Contains(refusal.Hint, "billing") {
		t.Fatalf("the hint does not list what could have been chosen: %q", refusal.Hint)
	}
}

func TestLogsReadAPageAsNDJSON(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, ProductionEnvironment: "shop-production"}
	h.platform.logLines = []logLine{
		{Timestamp: time.Now(), Source: "runtime", Level: "info", Message: "listening on 8080"},
		{Timestamp: time.Now(), Source: "runtime", Level: "error", Message: "connection refused"},
	}

	if code := h.run("logs", "--limit", "50", "--search", "conn", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	lines := h.lines()
	if len(lines) != 2 {
		t.Fatalf("wanted two lines, got %d", len(lines))
	}
	if lines[1]["message"] != "connection refused" {
		t.Fatalf("unexpected line: %+v", lines[1])
	}

	// The flags have to reach the API as its own parameters, or the filtering
	// silently happens nowhere.
	requests := h.platform.sent("GET", "/logs")
	if len(requests) != 1 {
		t.Fatalf("wanted one read, got %d", len(requests))
	}
	if !strings.Contains(requests[0].Query, "limit=50") || !strings.Contains(requests[0].Query, "search=conn") {
		t.Fatalf("the flags did not become query parameters: %q", requests[0].Query)
	}
	if !strings.HasSuffix(requests[0].Path, "/environments/shop-production/logs") {
		t.Fatalf("the default target is not the production environment: %q", requests[0].Path)
	}
}

func TestLogsSinceAcceptsADurationAndATimestamp(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, ProductionEnvironment: "shop-production"}

	if code := h.run("logs", "--since", "15m", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if code := h.run("logs", "--since", "yesterday", "--json"); code != exitUsage {
		t.Fatalf("a word that is neither should be a usage error, got exit %d", code)
	}
}

func TestDeployFollowsTheBuildAndAnswersEvents(t *testing.T) {
	quicken(t)

	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.buildPhases = []string{"Running", "Succeeded"}
	h.platform.logLines = []logLine{{Timestamp: time.Now(), Source: "build", Message: "step 1/4"}}
	h.platform.releases = []release{{
		Name: "shop-rel-42", Project: testProject, Build: "shop-bld-abc123def456-xk2p9", Image: "reg/shop@sha256:…",
	}}
	h.platform.environments = []environment{{
		Name: "shop-production", Project: testProject, Release: "shop-rel-42",
		Phase: "Live", URL: "https://shop.example.com",
	}}

	if code := h.run("deploy", "--sha", "abc123def456", "--branch", "main", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	events := h.lines()
	seen := map[string]bool{}
	for _, event := range events {
		seen[event["type"].(string)] = true
	}
	for _, wanted := range []string{"build", "log", "release", "environment", "result"} {
		if !seen[wanted] {
			t.Fatalf("no %s event in the stream: %v", wanted, events)
		}
	}

	final := events[len(events)-1]
	if final["type"] != "result" {
		t.Fatalf("the stream does not end in a result: %v", final)
	}
	if final["ok"] != true || final["url"] != "https://shop.example.com" {
		t.Fatalf("unexpected result: %v", final)
	}

	// What was asked of the platform is half the contract.
	started := h.platform.sent("POST", "/builds")
	if len(started) != 1 {
		t.Fatalf("wanted one build to be started, got %d", len(started))
	}
	if !strings.Contains(started[0].Body, "abc123def456") || !strings.Contains(started[0].Body, "main") {
		t.Fatalf("the commit was not sent: %q", started[0].Body)
	}
}

// A build that fails is not a CLI failure, and it has an exit status of its
// own so that a pipeline can tell the two apart.
func TestAFailedBuildHasItsOwnExitStatus(t *testing.T) {
	quicken(t)

	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.buildPhases = []string{"Running", "Failed"}

	if code := h.run("deploy", "--sha", "abc123", "--json"); code != exitBuildFailed {
		t.Fatalf("exit %d, wanted %d", code, exitBuildFailed)
	}
	// The result event comes first, then the failure envelope: a caller
	// reading the stream sees what happened before it sees that it failed.
	lines := h.lines()
	result := lines[len(lines)-2]
	if result["type"] != "result" || result["ok"] != false {
		t.Fatalf("unexpected result event: %v", result)
	}
	if refusal := lines[len(lines)-1]; refusal["error"] == nil {
		t.Fatalf("no error envelope: %v", refusal)
	}
}

func TestDeployDetachedStartsTheBuildAndStops(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject

	if code := h.run("deploy", "--detach", "--sha", "abc123", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	started := build{}
	h.answer(&started)
	if started.Name == "" {
		t.Fatalf("no build in the answer: %s", h.stdout.String())
	}
	if reads := h.platform.sent("GET", "/logs"); len(reads) != 0 {
		t.Fatalf("--detach followed the logs anyway")
	}
}

func TestDeployOutsideACheckoutSaysWhichFlagIsMissing(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject

	if code := h.run("deploy", "--json"); code != exitUsage {
		t.Fatalf("exit %d, wanted %d", code, exitUsage)
	}
	refusal := h.failure()
	if !strings.Contains(refusal.Hint, "--sha") || !strings.Contains(refusal.Hint, "--rebuild") {
		t.Fatalf("the hint names neither way out: %q", refusal.Hint)
	}
}

func TestRollbackGoesBackOneWithoutBeingTold(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, ProductionEnvironment: "shop-production"}
	h.platform.environments = []environment{{
		Name: "shop-production", Project: testProject, Release: "shop-rel-42", Phase: "Live",
		History: []releaseHistory{{Release: "shop-rel-41", Reason: "promoted"}},
	}}

	if code := h.run("rollback", "--yes", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	moved := environment{}
	h.answer(&moved)
	if moved.Release != "shop-rel-41" {
		t.Fatalf("moved to %q, wanted the previous release", moved.Release)
	}
	patches := h.platform.sent("PATCH", "/environments/shop-production")
	if len(patches) != 1 || !strings.Contains(patches[0].Body, "shop-rel-41") {
		t.Fatalf("unexpected patch: %+v", patches)
	}
}

func TestRollbackWithNoHistorySaysSo(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, ProductionEnvironment: "shop-production"}
	h.platform.environments = []environment{{Name: "shop-production", Release: "shop-rel-1"}}

	if code := h.run("rollback", "--yes", "--json"); code != exitNotFound {
		t.Fatalf("exit %d, wanted %d", code, exitNotFound)
	}
}

// The one that carries the most weight: a partial change to the environment
// variables must not lose the values the CLI has never been allowed to read.
func TestEnvSetKeepsEveryOtherVariableWithoutReadingItsValue(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Env: []envVar{
		{Name: "DATABASE_URL", FromClaim: &keyRef{Name: "shop-db", Key: "url"}},
		{Name: "API_KEY", Set: true},
		{Name: "LOG_LEVEL", Set: true, PreviewSet: true},
	}}

	if code := h.run("env", "set", "LOG_LEVEL=debug", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	patches := h.platform.sent("PATCH", "/env")
	if len(patches) != 1 {
		t.Fatalf("wanted one write, got %d", len(patches))
	}
	sent := struct {
		Env []envVarWrite `json:"env"`
	}{}
	if err := json.Unmarshal([]byte(patches[0].Body), &sent); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	if len(sent.Env) != 3 {
		t.Fatalf("the whole list has to be sent back, got %d of 3: %s", len(sent.Env), patches[0].Body)
	}

	byName := map[string]envVarWrite{}
	for _, variable := range sent.Env {
		byName[variable.Name] = variable
	}
	if value := byName["LOG_LEVEL"].Value; value == nil || *value != testValue {
		t.Fatalf("the changed variable did not carry its value: %+v", byName["LOG_LEVEL"])
	}
	if byName["API_KEY"].Value != nil {
		t.Fatalf("an untouched variable carried a value: it would have had to be read back first")
	}
	if byName["DATABASE_URL"].FromClaim == nil {
		t.Fatalf("an untouched reference was dropped: %+v", byName["DATABASE_URL"])
	}
}

func TestEnvSetReadsAFileAndReferences(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject}

	path := filepath.Join(h.work, ".env")
	contents := "# a comment\n\nLOG_LEVEL=debug\nGREETING=\"hello world\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	code := h.run("env", "set", "--from-file", path,
		"--from-secret", "API_KEY=shop-api-key:key", "--json")
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	sent := struct {
		Env []envVarWrite `json:"env"`
	}{}
	patches := h.platform.sent("PATCH", "/env")
	if err := json.Unmarshal([]byte(patches[0].Body), &sent); err != nil {
		t.Fatal(err)
	}
	byName := map[string]envVarWrite{}
	for _, variable := range sent.Env {
		byName[variable.Name] = variable
	}
	if value := byName["GREETING"].Value; value == nil || *value != "hello world" {
		t.Fatalf("the quoted value was not read: %+v", byName["GREETING"])
	}
	if reference := byName["API_KEY"].FromSecret; reference == nil || reference.Key != "key" {
		t.Fatalf("the secret reference was not written: %+v", byName["API_KEY"])
	}
}

func TestEnvRemoveRefusesANameThatIsNotThere(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Env: []envVar{{Name: "LOG_LEVEL", Set: true}}}

	if code := h.run("env", "rm", "TYPO", "--yes", "--json"); code != exitNotFound {
		t.Fatalf("exit %d, wanted %d", code, exitNotFound)
	}
	if writes := h.platform.sent("PATCH", "/env"); len(writes) != 0 {
		t.Fatalf("a typo wrote the list anyway")
	}
}

// `kitchen api` is the reason a route with no command is still reachable.
func TestAPIPassesTheBodyThroughUnchanged(t *testing.T) {
	h := newHarness(t)

	if code := h.run("api", "GET", "/projects", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	answer := list[project]{}
	h.answer(&answer)

	// Written without the prefix, with it, and without the leading slash: all
	// three reach the same endpoint.
	for _, path := range []string{"/api/v1/projects", "projects"} {
		if code := h.run("api", "GET", path, "--json"); code != exitOK {
			t.Fatalf("%s: exit %d", path, code)
		}
	}
	if code := h.run("api", "SING", "/projects", "--json"); code != exitUsage {
		t.Fatalf("an invented method should be a usage error, got %d", code)
	}
	if code := h.run("api", "POST", "/projects", "--data", "not json", "--json"); code != exitUsage {
		t.Fatalf("a body that is not JSON should be a usage error, got %d", code)
	}
}

func TestAPIRelaysARefusalAndItsStatus(t *testing.T) {
	h := newHarness(t)
	h.platform.refuseStatus = 403
	h.platform.refuseMessage = testRefusal

	if code := h.run("api", "POST", "/connections", "--data", "{}", "--json"); code != exitForbidden {
		t.Fatalf("exit %d, wanted %d", code, exitForbidden)
	}
	if !strings.Contains(h.stdout.String(), "operator role") {
		t.Fatalf("the platform's own sentence was not printed: %s", h.stdout.String())
	}
}

func TestAPIRefusalIsOneDocumentUnderJSON(t *testing.T) {
	h := newHarness(t)
	h.platform.refuseStatus = 403
	h.platform.refuseMessage = testRefusal

	if code := h.run("api", "POST", "/connections", "--data", "{}", "--json"); code != exitForbidden {
		t.Fatalf("exit %d, wanted %d", code, exitForbidden)
	}

	// failure decodes the whole of stdout, so the refusal body printed
	// alongside the envelope is this failing rather than an assertion of its
	// own: two documents is what --json promises there will not be.
	refusal := h.failure()
	if refusal.Status != 403 {
		t.Fatalf("unexpected status: %+v", refusal)
	}
	if !strings.Contains(refusal.Message, "operator role") {
		t.Fatalf("the platform's own sentence is not in the envelope: %+v", refusal)
	}
}

func TestAPIIgnoreStatusPrintsTheRefusalBodyAndSucceeds(t *testing.T) {
	h := newHarness(t)
	h.platform.refuseStatus = 403
	h.platform.refuseMessage = testRefusal

	code := h.run("api", "POST", "/connections", "--data", "{}", "--ignore-status", "--json")
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	// Nothing is returned, so nothing is suppressed: the body is the answer,
	// and it is still one document.
	body := errorBody{}
	h.answer(&body)
	if !strings.Contains(body.Error, "operator role") {
		t.Fatalf("unexpected body: %s", h.stdout.String())
	}
}

func TestLoginStoresTheCredentialAndCheckWhoItIs(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "KITCHEN_TOKEN")

	code := h.run("login", "--api", h.platform.server.URL, "--api-key", "a-key-from-the-project", "--json")
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	raw, err := os.ReadFile(filepath.Join(h.home, credentialFile))
	if err != nil {
		t.Fatalf("no credential file: %v", err)
	}
	stored := credentials{}
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	current := stored.Installations[h.platform.server.URL]
	if current == nil || current.APIKey != "a-key-from-the-project" {
		t.Fatalf("the key was not stored: %+v", stored)
	}
	if current.Issuer != h.platform.issuer.URL {
		t.Fatalf("the issuer was not remembered: %+v", current)
	}
	if stored.Current != h.platform.server.URL {
		t.Fatalf("the installation did not become the current one: %+v", stored)
	}

	// The credential file is a credential.
	info, err := os.Stat(filepath.Join(h.home, credentialFile))
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the credential file is mode %o, wanted 600", mode)
	}

	// And the stored key is enough for the next command, with nothing in the
	// environment at all.
	delete(h.env, "KITCHEN_API")
	if code := h.run("whoami", "--json"); code != exitOK {
		t.Fatalf("whoami after login: exit %d, stderr: %s", code, h.stderr.String())
	}
}

func TestLoginWithNoKeyAndNoTerminalNamesTheFlags(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "KITCHEN_TOKEN")

	if code := h.run("login", "--api", h.platform.server.URL, "--json"); code != exitUsage {
		t.Fatalf("exit %d, wanted %d", code, exitUsage)
	}
	if hint := h.failure().Hint; !strings.Contains(hint, "--api-key-stdin") {
		t.Fatalf("the hint does not name a way to pass the key: %q", hint)
	}
}

func TestLogoutForgetsTheInstallation(t *testing.T) {
	h := newHarness(t)
	delete(h.env, "KITCHEN_TOKEN")
	if code := h.run("login", "--api", h.platform.server.URL, "--api-key", "a-key", "--json"); code != exitOK {
		t.Fatalf("login: exit %d", code)
	}

	if code := h.run("logout", "--api", h.platform.server.URL, "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	gone := forgotten{}
	h.answer(&gone)
	if len(gone.Forgotten) != 1 {
		t.Fatalf("unexpected answer: %+v", gone)
	}

	if code := h.run("whoami", "--json"); code != exitUnauthenticated {
		t.Fatalf("after logout: exit %d, wanted %d", code, exitUnauthenticated)
	}
	if hint := h.failure().Hint; !strings.Contains(hint, "kitchen login") {
		t.Fatalf("the hint does not say how to sign in again: %q", hint)
	}
}

// Text output is for a person and JSON is for everything else; neither may
// leak into the other's stream.
func TestTextOutputGoesToStdoutWithoutEscapeSequences(t *testing.T) {
	h := newHarness(t)
	h.platform.projects = []project{{Name: testProject, Role: "developer", Repo: "acme/shop", ProductionBranch: "main"}}

	if code := h.run("projects"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	out := h.stdout.String()
	if !strings.Contains(out, testProject) || !strings.Contains(out, "NAME") {
		t.Fatalf("unexpected text output: %q", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("a pipe was painted with escape sequences: %q", out)
	}
}

// quicken shortens the deploy follow's own waits, so a test of the whole flow
// takes milliseconds rather than the seconds a person would wait.
func quicken(t *testing.T) {
	t.Helper()
	poll, grace, wait := buildPollInterval, logGrace, releaseWait
	buildPollInterval, logGrace, releaseWait = 5*time.Millisecond, 10*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { buildPollInterval, logGrace, releaseWait = poll, grace, wait })
}
