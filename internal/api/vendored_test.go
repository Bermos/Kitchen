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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Creating and reading a project whose software this platform did not build
// (#307), over the same route that creates one from a repository.
//
// There is no second endpoint. A project's source is a union of one member,
// so it is one body with one of two keys in it — and the refusals below are
// what stops the other kind's settings from being sent along with it and
// reading back as settings that took.

func TestCreatingAProjectFromAVendoredImage(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"blog","image":{"repository":"ghcr.io/home-assistant/home-assistant",`+
			`"tag":"2026.9.1","connection":"registry"}}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if view.Image == nil {
		t.Fatalf("the response says nothing about where the software comes from: %+v", view)
	}
	if view.Image.Reference != "ghcr.io/home-assistant/home-assistant:2026.9.1" {
		t.Fatalf("the reference does not read back: %+v", view.Image)
	}
	if view.Previews {
		t.Fatalf("a project with no repository has no pull requests to preview: %+v", view)
	}
	if view.Repo != "" || view.Registry != "" {
		t.Fatalf("a repository's fields should be empty on a vendored project: %+v", view)
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), otherProject, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Source.HasRepository() {
		t.Fatalf("the project should have no repository: %+v", stored.Spec.Source)
	}
	if stored.Spec.Registry != nil {
		t.Fatalf("a project that builds nothing has nothing to push: %+v", stored.Spec.Registry)
	}
	if got := stored.Spec.Source.ImageSource().PullConnection(); got != "registry" {
		t.Fatalf("the pull credential did not stick, got %q", got)
	}
}

func TestCreatingAVendoredProjectRefusesARepositorysSettings(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	const image = `"image":{"repository":"ghcr.io/acme/thing","tag":"1"}`

	for name, body := range map[string]string{
		"a repository as well as an image": `{"name":"blog","repo":"acme/blog","connection":"gh",` +
			`"registry":"registry",` + image + `}`,
		"neither":               `{"name":"blog"}`,
		"a registry to push to": `{"name":"blog","registry":"registry",` + image + `}`,
		"a source connection":   `{"name":"blog","connection":"gh",` + image + `}`,
		"a production branch":   `{"name":"blog","productionBranch":"trunk",` + image + `}`,
		"previews":              `{"name":"blog","previews":true,` + image + `}`,
		"an image with no version": `{"name":"blog","image":` +
			`{"repository":"ghcr.io/acme/thing"}}`,
		"an image whose repository carries its own tag": `{"name":"blog","image":` +
			`{"repository":"ghcr.io/acme/thing:1","tag":"1"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/projects", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// The refusal is worded, not silent: a preview that never appears reads as a
// fault, so asking for one is answered with why there cannot be.
func TestAskingAVendoredProjectForPreviewsIsRefusedInWords(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	if recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"blog","image":{"repository":"ghcr.io/acme/thing","tag":"1"}}`); recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/"+otherProject, `{"previews":true}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, phrase := range []string{"no repository to open one against", "ghcr.io/acme/thing:1"} {
		if !strings.Contains(body, phrase) {
			t.Fatalf("the refusal should say why, got %s", body)
		}
	}

	recorder = h.do(t, http.MethodPatch, "/api/v1/projects/"+otherProject, `{"productionBranch":"trunk"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for a branch on a project with no repository, got %d: %s",
			recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "has no repository") {
		t.Fatalf("the refusal should say why, got %s", recorder.Body.String())
	}
}

// A workload of any project can name an image the platform did not build, and
// it travels in the process list the settings PATCH already replaces.
func TestSettingAWorkloadsVendoredImage(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"processes":[{"name":"cache","type":"service","port":6379,`+
			`"image":{"repository":"docker.io/library/redis","tag":"7.4","connection":"registry"}}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Spec.Processes) != 1 || stored.Spec.Processes[0].Image == nil {
		t.Fatalf("the workload's image did not stick: %+v", stored.Spec.Processes)
	}
	image := stored.Spec.Processes[0].Image
	if image.Reference() != "docker.io/library/redis:7.4" || image.PullConnection() != "registry" {
		t.Fatalf("the image did not round-trip: %+v", image)
	}

	view := decode[projectView](t, recorder)
	if len(view.Processes) != 1 || view.Processes[0].ImageSource == nil {
		t.Fatalf("the response should say where the workload's image comes from: %+v", view.Processes)
	}
	if view.Processes[0].ImageSource.Reference != "docker.io/library/redis:7.4" {
		t.Fatalf("the reference does not read back: %+v", view.Processes[0].ImageSource)
	}
}

func TestAWorkloadIsBuiltOrVendoredAndNotBoth(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/projects/shop",
		`{"processes":[{"name":"api","type":"service","port":8080,`+
			`"build":{"rootDirectory":"services/api"},`+
			`"image":{"repository":"docker.io/library/redis","tag":"7.4"}}]}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "never both") {
		t.Fatalf("the refusal should say what to remove, got %s", recorder.Body.String())
	}
}
