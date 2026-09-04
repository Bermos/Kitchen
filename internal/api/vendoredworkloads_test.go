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

package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Declaring a unit that has no kitchen.json to declare it in (#310).
//
// A repository puts its workloads in a file at the build root of every commit.
// A project whose source is an image has no repository, so it has no file —
// and the settings route, the dashboard and `kitchen processes set` are not a
// fallback for it but the whole of how its unit is described.
//
// What the audit that produced this file found was not a missing route. It was
// that the list could not be *read back the way it was written*, which is a
// different way for an editor to be impossible: a client that reads the list to
// change one workload and sends the rest back was turning every parked workload
// on and dropping every previews declaration. So the tests here are as much
// about the read as about the write.

// vendoredProject is a project with no repository at all: the home-lab case,
// and the one that forces every question this file asks.
func vendoredProject(t *testing.T, h *harness) {
	t.Helper()
	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"`+otherProject+`","image":{"repository":"ghcr.io/home-assistant/home-assistant",`+
			`"tag":"2026.9.1"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestDeclaringAUnitWithNoRepositoryToDeclareItIn(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	vendoredProject(t, h)
	const path = "/api/v1/projects/" + otherProject

	// Adding. Every workload of such a project runs an image somebody else
	// published — there is nothing here to build one from — and the list is
	// otherwise the one a repository project declares.
	recorder := h.do(t, http.MethodPatch, path, `{"processes":[
		{"name":"broker","type":"service","port":1883,
		 "image":{"repository":"ghcr.io/acme/broker","tag":"2.0"}},
		{"name":"backup","type":"cron","schedule":"0 4 * * *","timeout":"20m",
		 "image":{"repository":"ghcr.io/acme/backup","digest":"sha256:`+strings.Repeat("a", 64)+`"}}
	]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if len(view.Processes) != 2 {
		t.Fatalf("the unit did not come back: %+v", view.Processes)
	}
	if view.Processes[0].ImageSource == nil ||
		view.Processes[0].ImageSource.Reference != "ghcr.io/acme/broker:2.0" {
		t.Fatalf("the workload's image does not read back: %+v", view.Processes[0])
	}

	// Editing one of them. The whole list travels, which is what an editor
	// sends after changing one row of it.
	recorder = h.do(t, http.MethodPatch, path, `{"processes":[
		{"name":"broker","type":"service","port":1883,"replicas":2,"memory":"256Mi",
		 "image":{"repository":"ghcr.io/acme/broker","tag":"2.0.20"}},
		{"name":"backup","type":"cron","schedule":"0 4 * * *","timeout":"20m",
		 "image":{"repository":"ghcr.io/acme/backup","digest":"sha256:`+strings.Repeat("a", 64)+`"}}
	]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), otherProject, stored); err != nil {
		t.Fatal(err)
	}
	broker := kitchenv1alpha1.FindProcess(stored.Spec.Processes, "broker")
	if broker == nil || broker.Image == nil || broker.Image.Tag != "2.0.20" || broker.ReplicaCount() != 2 {
		t.Fatalf("the edit did not stick: %+v", broker)
	}

	// Removing one. The list replaces, so a workload left out is a workload
	// taken off — which is how the editor's remove button works.
	recorder = h.do(t, http.MethodPatch, path, `{"processes":[
		{"name":"broker","type":"service","port":1883,"replicas":2,
		 "image":{"repository":"ghcr.io/acme/broker","tag":"2.0.20"}}
	]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if err := h.server.get(context.Background(), otherProject, stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Processes) != 1 {
		t.Fatalf("the removal did not take: %+v", stored.Spec.Processes)
	}
}

// A workload built from the repository needs a repository, and the refusal
// says which of the two answers is available here rather than naming a field.
func TestAProjectWithNoRepositoryCanDeclareNoWorkloadBuiltFromOne(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	vendoredProject(t, h)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+otherProject,
		`{"processes":[{"name":"api","type":"service","port":8080,`+
			`"build":{"rootDirectory":"services/api"}}]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "api") || !strings.Contains(body, "has no repository") {
		t.Fatalf("the refusal should name the workload and say why, got %s", body)
	}
}

// The four settings that say how a commit becomes an image are the
// repository's, and a project that builds nothing is refused them rather than
// storing a setting that reads back and does nothing.
func TestSettingsThatDescribeABuildAreRefusedOnAProjectThatBuildsNothing(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	vendoredProject(t, h)

	for name, body := range map[string]string{
		"a build strategy":   `{"buildStrategy":"dockerfile"}`,
		"a Dockerfile":       `{"dockerfilePath":"build/Dockerfile"}`,
		"a Dockerfile stage": `{"dockerfileTarget":"web"}`,
		"a root directory":   `{"rootDirectory":"apps/shop"}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+otherProject, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), "has no repository") {
				t.Fatalf("the refusal should say why, got %s", recorder.Body.String())
			}
		})
	}

	// The same four are ordinary on a project that does build.
	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"buildStrategy":"dockerfile","rootDirectory":"apps/shop"}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200 on the project with a repository, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The read that makes an editor possible: a workload the client did not touch
// has to go back exactly as it came, and these are the two fields that did not.
func TestAWorkloadListReadsBackTheWayItWasWritten(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"processes":[
		{"name":"parked","type":"worker","replicas":0},
		{"name":"api","type":"service","port":8080,"previews":false},
		{"name":"mailer","type":"worker","previews":true},
		{"name":"quiet","type":"worker"}
	]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	byName := map[string]processView{}
	for _, workload := range view.Processes {
		byName[workload.Name] = workload
	}

	// Zero is a count somebody chose — a workload declared and parked — and it
	// used to read back as absent, which every editor turned into one.
	if parked := byName["parked"]; parked.Replicas == nil || *parked.Replicas != 0 {
		t.Fatalf("a parked workload should read back parked: %+v", parked)
	}
	// A declaration of previews reads back as the declaration, and its absence
	// as an absence: `false` and "nothing said" are different states, and the
	// type's own default is what resolves the second.
	if api := byName["api"]; api.Previews == nil || *api.Previews {
		t.Fatalf("a service kept out of previews should say so: %+v", api)
	}
	if mailer := byName["mailer"]; mailer.Previews == nil || !*mailer.Previews {
		t.Fatalf("a worker opted into previews should say so: %+v", mailer)
	}
	if quiet := byName["quiet"]; quiet.Previews != nil {
		t.Fatalf("a workload that declared nothing should declare nothing: %+v", quiet.Previews)
	}

	// And the round trip itself: what came out, sent back, is what is stored.
	round := `{"processes":[
		{"name":"parked","type":"worker","replicas":0},
		{"name":"api","type":"service","port":8080,"previews":false},
		{"name":"mailer","type":"worker","previews":true},
		{"name":"quiet","type":"worker"}
	]}`
	if recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", round); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	parked := kitchenv1alpha1.FindProcess(stored.Spec.Processes, "parked")
	if parked == nil || ptr.Deref(parked.Replicas, 1) != 0 {
		t.Fatalf("the round trip un-parked the workload: %+v", parked)
	}
	if quiet := kitchenv1alpha1.FindProcess(stored.Spec.Processes, "quiet"); quiet == nil || quiet.Previews != nil {
		t.Fatalf("the round trip invented a previews declaration: %+v", quiet)
	}
}

// A validation failure names the workload it is about. The list is replaced
// wholesale, so "which of these four" is the whole of what the caller needs.
func TestAWorkloadRefusalNamesTheWorkload(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop", `{"processes":[
		{"name":"worker","type":"worker"},
		{"name":"nightly","type":"cron"}
	]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "nightly") {
		t.Fatalf("the refusal should name the workload, got %s", recorder.Body.String())
	}
}
