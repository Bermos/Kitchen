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

// Asking for an acquisition (#308): the vendored equivalent of a rebuild.
//
// The route exists because the poll cannot be the only way in. Somebody who
// has just published upstream should not have to wait out an interval, and a
// vendor's own pipeline knows the digest it published and would rather say so
// than be discovered.

const (
	acquisitionsPath = "/api/v1/projects/" + otherProject + "/acquisitions"
	vendoredBody     = `{"name":"` + otherProject +
		`","image":{"repository":"ghcr.io/acme/thing","tag":"stable"}}`
)

// vendoredHarness is the fixtures plus a second project with no repository,
// created over the route that creates one.
func vendoredHarness(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t, nil, fixtures()...)
	if recorder := h.do(t, http.MethodPost, "/api/v1/projects", vendoredBody); recorder.Code != http.StatusCreated {
		t.Fatalf("creating the vendored project: %d %s", recorder.Code, recorder.Body.String())
	}
	return h
}

// acquisitionOf is the Build the route created, found by the one property
// that tells it from the fixtures' own: it names no commit.
func acquisitionOf(t *testing.T, h *harness, project string) *kitchenv1alpha1.Build {
	t.Helper()
	list := &kitchenv1alpha1.BuildList{}
	if err := h.server.Client.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	var found *kitchenv1alpha1.Build
	for i := range list.Items {
		build := &list.Items[i]
		if build.Spec.ProjectRef.Name == project && !build.FromRepository() {
			found = build
		}
	}
	if found == nil {
		t.Fatalf("no acquisition was created for %s", project)
	}
	return found
}

func TestAskingForAnAcquisition(t *testing.T) {
	h := vendoredHarness(t)

	recorder := h.do(t, http.MethodPost, acquisitionsPath, "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[buildView](t, recorder)
	if !strings.HasPrefix(view.Name, otherProject+"-acq-") {
		t.Fatalf("want a name that reads as an acquisition, got %q", view.Name)
	}

	build := acquisitionOf(t, h, otherProject)
	if build.Spec.Acquire == nil {
		t.Fatalf("the Build says nothing about what it acquires: %+v", build.Spec)
	}
	if build.Spec.Acquire.Reference != "ghcr.io/acme/thing:stable" {
		t.Fatalf("want the project's own reference followed, got %q", build.Spec.Acquire.Reference)
	}
	if build.Spec.Acquire.Digest != "" {
		t.Fatalf("an empty body takes whatever the reference names now, got %q", build.Spec.Acquire.Digest)
	}
	if build.Spec.Acquire.Trigger != kitchenv1alpha1.AcquisitionRequested {
		t.Fatalf("want the trigger to say somebody asked, got %q", build.Spec.Acquire.Trigger)
	}
	if build.FromRepository() {
		t.Fatal("nothing fakes a commit")
	}
}

func TestAskingForOneExactDigest(t *testing.T) {
	h := vendoredHarness(t)
	const digest = "sha256:" +
		"1111111111111111111111111111111111111111111111111111111111111111"

	recorder := h.do(t, http.MethodPost, acquisitionsPath, `{"digest":"`+digest+`"}`)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}

	build := acquisitionOf(t, h, otherProject)
	if build.Spec.Acquire.Digest != digest {
		t.Fatalf("the pin did not stick: %+v", build.Spec.Acquire)
	}
	if want := "ghcr.io/acme/thing@" + digest; build.Spec.Acquire.Reference != want {
		t.Fatalf("naming a digest is following that digest, want %q got %q",
			want, build.Spec.Acquire.Reference)
	}
}

func TestAnAcquisitionRefusesWhatItCannotTake(t *testing.T) {
	h := vendoredHarness(t)

	t.Run("a digest that is not one", func(t *testing.T) {
		recorder := h.do(t, http.MethodPost, acquisitionsPath, `{"digest":"1111"}`)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), "sixty-four hex digits") {
			t.Fatalf("the refusal should say what a digest is, got %s", recorder.Body.String())
		}
	})

	t.Run("a project built from a repository", func(t *testing.T) {
		recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/acquisitions", "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		for _, phrase := range []string{"nothing to acquire", "projects/" + feedProject + "/builds"} {
			if !strings.Contains(body, phrase) {
				t.Fatalf("the refusal should name what does move it, got %s", body)
			}
		}
	})
}

// The role, in the words every other refusal uses. It is admin's rather than a
// developer's because an acquisition takes a new artifact from a third party's
// registry onto this platform, which is a decision about where the software
// comes from and not about running its build again.
func TestAnAcquisitionNeedsAdmin(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleDeveloper)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/"+feedProject+"/acquisitions", "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	want := "you have developer on " + feedProject + "; acquiring an image needs admin"
	if got := errorOf(t, recorder.Body.String()); got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}
