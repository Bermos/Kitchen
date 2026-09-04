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

	"k8s.io/utils/ptr"
)

// `kitchen processes set` and `kitchen processes rm`: declaring a project's
// workloads from a terminal, which is the only route there is for a project
// with no repository to put a kitchen.json in (#310).
//
// The property every test here is about is the same one `kitchen env set` has:
// the route replaces the whole list, so the command reads it, changes the one
// workload it was asked about, and sends the rest back exactly as they came.

// sentProcesses reads the workload list off the one PATCH the command sent.
func sentProcesses(t *testing.T, h *harness) []processWrite {
	t.Helper()
	patches := h.platform.sent("PATCH", "/projects/"+testProject)
	if len(patches) != 1 {
		t.Fatalf("wanted one write, got %d", len(patches))
	}
	body := struct {
		Processes []processWrite `json:"processes"`
	}{}
	if err := json.Unmarshal([]byte(patches[0].Body), &body); err != nil {
		t.Fatalf("the body is not JSON: %v", err)
	}
	return body.Processes
}

func byWorkloadName(writes []processWrite) map[string]processWrite {
	byName := map[string]processWrite{}
	for _, write := range writes {
		byName[write.Name] = write
	}
	return byName
}

func TestProcessesSetChangesOneWorkloadAndKeepsTheRest(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Processes: []process{
		// A parked workload and one kept out of previews: the two things that
		// had to survive the round trip before this command could exist.
		{Name: "worker", Type: "worker", Command: []string{"node", "worker.js"}, Replicas: ptr.To(int32(0))},
		{Name: "api", Type: "service", Port: 8080, Replicas: ptr.To(int32(2)), Previews: ptr.To(false)},
		{Name: "nightly", Type: "cron", Schedule: "0 3 * * *", Timeout: "30m0s", ConcurrencyPolicy: "Forbid"},
	}}

	if code := h.run("processes", "set", "api", "--replicas", "3", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	byName := byWorkloadName(sentProcesses(t, h))
	if len(byName) != 3 {
		t.Fatalf("the whole list has to be sent back, got %d of 3", len(byName))
	}
	if got := ptr.Deref(byName["api"].Replicas, -1); got != 3 {
		t.Fatalf("the change did not go: %+v", byName["api"])
	}
	if byName["api"].Previews == nil || *byName["api"].Previews {
		t.Fatalf("an untouched declaration was dropped: %+v", byName["api"])
	}
	if got := ptr.Deref(byName["worker"].Replicas, -1); got != 0 {
		t.Fatalf("an untouched parked workload was turned back on: %+v", byName["worker"])
	}
	if strings.Join(byName["worker"].Command, " ") != "node worker.js" {
		t.Fatalf("an untouched command did not survive: %+v", byName["worker"])
	}
	if byName["nightly"].Schedule != "0 3 * * *" || byName["nightly"].Timeout != "30m0s" {
		t.Fatalf("an untouched scheduled job did not survive: %+v", byName["nightly"])
	}
}

func TestProcessesSetDeclaresAWorkloadTheProjectDoesNotHave(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject}

	code := h.run("processes", "set", "cache", "--type", "service", "--port", "6379",
		"--image", "docker.io/library/redis:7.4", "--image-connection", "registry",
		"--previews", "no", "--memory", "256Mi", "--json")
	if code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}

	cache := byWorkloadName(sentProcesses(t, h))["cache"]
	if cache.Type != "service" || cache.Port != 6379 || cache.Memory != "256Mi" {
		t.Fatalf("the workload did not go as asked: %+v", cache)
	}
	if cache.Image == nil || cache.Image.Repository != "docker.io/library/redis" || cache.Image.Tag != "7.4" {
		t.Fatalf("the reference was not split into a repository and a tag: %+v", cache.Image)
	}
	if cache.Image.Connection != "registry" {
		t.Fatalf("the pull credential did not go: %+v", cache.Image)
	}
	if cache.Previews == nil || *cache.Previews {
		t.Fatalf("the previews declaration did not go: %+v", cache.Previews)
	}
}

// A digest is the other half of the same flag, and a registry host with a port
// in it is the case a naive split gets wrong.
func TestProcessesSetReadsEveryShapeOfImageReference(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	for name, reference := range map[string]struct{ flag, repository, tag, digest string }{
		"a tag":             {"ghcr.io/acme/thing:1.2", "ghcr.io/acme/thing", "1.2", ""},
		"a digest":          {"ghcr.io/acme/thing@" + digest, "ghcr.io/acme/thing", "", digest},
		"a registry's port": {"registry.example.com:5000/acme/thing:1.2", "registry.example.com:5000/acme/thing", "1.2", ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.env["KITCHEN_PROJECT"] = testProject
			h.platform.project = &project{Name: testProject}

			code := h.run("processes", "set", "thing", "--type", "worker", "--image", reference.flag, "--json")
			if code != exitOK {
				t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
			}
			image := byWorkloadName(sentProcesses(t, h))["thing"].Image
			if image == nil || image.Repository != reference.repository ||
				image.Tag != reference.tag || image.Digest != reference.digest {
				t.Fatalf("%q did not split as expected: %+v", reference.flag, image)
			}
		})
	}
}

// A vendored image is pinned, and a reference with no version is refused here
// rather than by the platform: the CLI has everything it needs to say so.
func TestProcessesSetRefusesAnImageWithNoVersion(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject}

	if code := h.run("processes", "set", "thing", "--type", "worker",
		"--image", "ghcr.io/acme/thing", "--json"); code != exitUsage {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if len(h.platform.sent("PATCH", "/projects/"+testProject)) != 0 {
		t.Fatal("a refused command still wrote")
	}
}

// A workload is built here or published elsewhere, never both, so each flag
// clears the other rather than sending a pair the platform would refuse.
func TestProcessesSetSwapsABuildForAnImage(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Processes: []process{
		{Name: "api", Type: "service", Port: 8080, Build: &processBuild{Strategy: "auto", RootDirectory: "services/api"}},
	}}

	if code := h.run("processes", "set", "api", "--image", "ghcr.io/acme/api:1.2", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	api := byWorkloadName(sentProcesses(t, h))["api"]
	if api.Build != nil || api.Image == nil {
		t.Fatalf("naming an image should take the build away: %+v", api)
	}
}

func TestProcessesSetNeedsATypeForAWorkloadThatDoesNotExist(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject}

	if code := h.run("processes", "set", "worker", "--replicas", "2", "--json"); code != exitUsage {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String()+h.stderr.String(), "--type") {
		t.Fatalf("the refusal should name the flag that answers it: %s%s", h.stdout.String(), h.stderr.String())
	}
}

func TestProcessesRemoveTakesOneOffAndRefusesANameTheProjectDoesNotHave(t *testing.T) {
	h := newHarness(t)
	h.env["KITCHEN_PROJECT"] = testProject
	h.platform.project = &project{Name: testProject, Processes: []process{
		{Name: "worker", Type: "worker"},
		{Name: "nightly", Type: "cron", Schedule: "0 3 * * *"},
	}}

	if code := h.run("processes", "rm", "nightly", "--yes", "--json"); code != exitOK {
		t.Fatalf("exit %d, stderr: %s", code, h.stderr.String())
	}
	kept := sentProcesses(t, h)
	if len(kept) != 1 || kept[0].Name != "worker" {
		t.Fatalf("the wrong list was sent: %+v", kept)
	}

	// A typo is a failure rather than a silent success, and nothing is written.
	other := newHarness(t)
	other.env["KITCHEN_PROJECT"] = testProject
	other.platform.project = &project{Name: testProject, Processes: []process{{Name: "worker", Type: "worker"}}}
	if code := other.run("processes", "rm", "nightly", "--yes", "--json"); code != exitNotFound {
		t.Fatalf("exit %d, stderr: %s", code, other.stderr.String())
	}
	if len(other.platform.sent("PATCH", "/projects/"+testProject)) != 0 {
		t.Fatal("a refused removal still wrote")
	}
}
