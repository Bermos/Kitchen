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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The preflight the new project form runs: read the repository the way a
// build would, and say what the platform makes of it while the build context
// is still a form field.

// fakeGitHubContents serves the contents API for one repository from a map of
// directory path to listing, and one file path to its contents. Anything not
// in either is a 404, which is what a wrong root directory looks like.
func fakeGitHubContents(t *testing.T, dirs map[string][]string, files map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path, ok := strings.CutPrefix(req.URL.Path, "/repos/acme/shop/contents")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		path = strings.Trim(path, "/")

		// A file is asked for with the raw media type, which is the file
		// itself rather than a JSON envelope around it.
		if body, ok := files[path]; ok && req.Header.Get("Accept") == "application/vnd.github.raw" {
			_, _ = w.Write([]byte(body))
			return
		}
		names, ok := dirs[path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message": "Not Found"}`))
			return
		}
		listing := make([]map[string]any, 0, len(names))
		for _, name := range names {
			kind := "file"
			if strings.HasSuffix(name, "/") {
				kind, name = "dir", strings.TrimSuffix(name, "/")
			}
			listing = append(listing, map[string]any{"name": name, "type": kind})
		}
		_ = json.NewEncoder(w).Encode(listing)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestDetectingWhatARepositoryIs(t *testing.T) {
	github := fakeGitHubContents(t,
		map[string][]string{"": {"package.json", "src/", "vite.config.ts"}},
		map[string]string{"package.json": `{"dependencies": {"vite": "^5"}, "scripts": {"build": "vite build"}}`})
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stored")...)...)
	// Anybody who can create a project can run its preflight.
	h.demoteCaller(t)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/shop", "ref": "main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[detectionView](t, recorder)
	if !view.Detected || view.Framework != "vite" {
		t.Fatalf("a vite repository was not recognised: %+v", view)
	}
	if view.Strategy != "buildpacks" || view.Port != 8080 {
		t.Fatalf("the verdict does not say how it would be built: %+v", view)
	}
	if view.Dockerfile {
		t.Fatalf("a repository with no Dockerfile claimed one: %+v", view)
	}
	// The listing is shown so somebody who disagrees can see what it was
	// reached from.
	if len(view.Files) == 0 {
		t.Fatalf("the answer does not say what it looked at: %+v", view)
	}
	if strings.Contains(recorder.Body.String(), "ghp_stored") {
		t.Fatalf("the preflight leaks the credential: %s", recorder.Body.String())
	}
}

func TestDetectingHonoursTheBuildContext(t *testing.T) {
	github := fakeGitHubContents(t, map[string][]string{
		"":          {"README.md", "apps/"},
		"apps/shop": {"Dockerfile", "main.go"},
	}, nil)
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stored")...)...)

	// The repository root is nothing the platform recognises, which is the
	// answer somebody needs to see before they create the project.
	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/shop", "ref": "main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[detectionView](t, recorder); view.Detected || view.Message == "" {
		t.Fatalf("an unrecognisable root answered as recognised: %+v", view)
	}

	// The same repository with the root directory corrected is a Dockerfile
	// build, and asking again is the whole of fixing it.
	recorder = h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/shop", "ref": "main", "rootDirectory": "apps/shop"}`)
	view := decode[detectionView](t, recorder)
	if !view.Detected || view.Framework != "dockerfile" || !view.Dockerfile {
		t.Fatalf("the corrected build context was not recognised: %+v", view)
	}
	if view.RootDirectory != "apps/shop" {
		t.Fatalf("the answer is not about the directory it was asked about: %+v", view)
	}
}

func TestDetectingARootDirectoryThatIsNotThere(t *testing.T) {
	github := fakeGitHubContents(t, map[string][]string{"": {"go.mod"}}, nil)
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stored")...)...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/shop", "ref": "main", "rootDirectory": "apps/typo"}`)
	// A directory that is not there is the caller's to fix, so it is an
	// answer rather than a failure.
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[detectionView](t, recorder)
	if view.Detected || !strings.Contains(view.Message, "apps/typo") {
		t.Fatalf("the answer does not name the directory that is missing: %+v", view)
	}
}

func TestDetectingOnAConnectionWithNoRepositories(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/registry/detect",
		`{"repo": "acme/shop", "ref": "main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[detectionView](t, recorder); view.Detected || view.Message == "" {
		t.Fatalf("a registry connection detected a framework: %+v", view)
	}
}

func TestDetectingWithoutARepository(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/registry/detect", `{}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}
