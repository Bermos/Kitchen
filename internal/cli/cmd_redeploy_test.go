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
	"strings"
	"testing"
)

// `kitchen redeploy` — the same commit, today's settings (#392).
//
// The command is thin on purpose: the platform decides whether there is
// anything to redeploy, so what is worth pinning here is that it asks the
// right endpoint about the right environment, that the question it puts to a
// person says what the change actually is, and that a refusal arrives as the
// exit code a script branches on rather than as prose.

// redeployFixtures is an environment running a release, and a platform willing
// to cut another from the same commit.
func redeployFixtures(h *harness) {
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, ProductionEnvironment: "shop-production"}
	h.platform.environments = []environment{{
		Name: "shop-production", Project: testProject, Release: "shop-rel-42", Phase: "Live",
	}}
	h.platform.redeployed = &redeployed{
		Environment: "shop-production", Project: testProject,
		Release: "shop-rel-42-cfg-3f7a91be", PreviousRelease: "shop-rel-42",
		Image:   "registry.example.com/shop@sha256:9d3f",
		Message: "shop-production is deploying shop-rel-42-cfg-3f7a91be",
	}
}

func TestRedeployAsksThePlatformForTheEnvironmentItIsOn(t *testing.T) {
	h := newHarness(t)
	redeployFixtures(h)

	if code := h.run("redeploy", "--yes", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	// The route the API serves, on the project's production environment,
	// which is what the command resolves when nobody names one.
	if asked := h.platform.sent("POST", "/environments/shop-production/redeploy"); len(asked) != 1 {
		t.Fatalf("unexpected calls: %+v", h.platform.requests)
	}

	answer := redeployed{}
	if err := json.Unmarshal(h.stdout.Bytes(), &answer); err != nil {
		t.Fatalf("--json did not put one document on stdout: %q", h.stdout.String())
	}
	if answer.Release != "shop-rel-42-cfg-3f7a91be" || answer.PreviousRelease != "shop-rel-42" {
		t.Fatalf("the answer is not the platform's: %+v", answer)
	}
}

// The question is the one thing a person may have wrong: "redeploy" reads like
// a synonym for "deploy", and the difference is the whole feature.
func TestRedeploySaysWhatChangesBeforeItAsks(t *testing.T) {
	h := newHarness(t)
	redeployFixtures(h)
	h.stdinTerminal = true
	h.stdin = strings.NewReader("y\n")

	if code := h.run("redeploy"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	asked := h.stderr.String()
	if !strings.Contains(asked, "the same commit as shop-rel-42") ||
		!strings.Contains(asked, "settings as they stand") {
		t.Fatalf("the prompt does not say what the change is: %q", asked)
	}
}

// Nothing blocks on a prompt: with no terminal to ask, the failure names the
// flag that would have answered it.
func TestRedeployWithNobodyToAskNamesTheFlag(t *testing.T) {
	h := newHarness(t)
	redeployFixtures(h)

	if code := h.run("redeploy"); code != exitUsage {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stderr.String(), "--yes") {
		t.Fatalf("the failure does not name the flag: %q", h.stderr.String())
	}
	if asked := h.platform.sent("POST", "/environments/shop-production/redeploy"); len(asked) != 0 {
		t.Fatalf("it wrote without being answered: %+v", asked)
	}
}

// A project that already declares what the running release froze is a `409`,
// and a `409` is the conflict exit code — the contract a script branches on.
func TestRedeployWithNothingToRedeployExitsConflict(t *testing.T) {
	h := newHarness(t)
	redeployFixtures(h)
	h.platform.redeployed = nil

	if code := h.run("redeploy", "--yes", "--json"); code != exitConflict {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	failed := struct {
		Error failure `json:"error"`
	}{}
	if err := json.Unmarshal(h.stdout.Bytes(), &failed); err != nil {
		t.Fatalf("--json did not put one error on stdout: %q", h.stdout.String())
	}
	if failed.Error.Code != codeConflict {
		t.Fatalf("the failure is not a conflict: %+v", failed.Error)
	}
}

// An environment that has never deployed anything is refused before anything
// is sent: there is no commit to redeploy, only one to build.
func TestRedeployRefusesAnEnvironmentRunningNothing(t *testing.T) {
	h := newHarness(t)
	redeployFixtures(h)
	h.platform.environments[0].Release = ""

	if code := h.run("redeploy", "--yes", "--json"); code != exitNotFound {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if asked := h.platform.sent("POST", "/environments/shop-production/redeploy"); len(asked) != 0 {
		t.Fatalf("it asked anyway: %+v", asked)
	}
}

// The endpoint the command says it calls is one the API serves. The whole tree
// is checked by TestEveryCallNamesARealAPIRoute; this is the one command that
// names the route this issue added, and it is worth saying so where a reader
// of the command will find it.
func TestRedeployNamesTheRedeployRoute(t *testing.T) {
	for _, command := range tree(t).Commands {
		if command.Path != "kitchen redeploy" {
			continue
		}
		for _, call := range command.Calls {
			if call == "POST /api/v1/environments/{name}/redeploy" {
				return
			}
		}
		t.Fatalf("kitchen redeploy does not name the route it calls: %+v", command.Calls)
	}
	t.Fatal("kitchen redeploy is not in the published schema")
}
