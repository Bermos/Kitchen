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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// The preflight the new project form runs: read the repository the way a
// build would, and say what the platform makes of it while the build context
// is still a form field.

// fakeGitHubContents serves the contents API for one repository from a map of
// directory path to listing, and one file path to its contents. Anything not
// in either is a 404, which is what a wrong root directory looks like.
//
// The repository itself answers too, because GitHub's 404 for a path inside a
// repository and its 404 for a repository the token may not see are the same
// answer, and the only thing that tells them apart is asking about the
// repository. A server that served contents and not the repository would be
// one no credential can read.
func fakeGitHubContents(t *testing.T, dirs map[string][]string, files map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		// What this connection offers, which is the set the preflight is
		// allowed to be asked about at all. acme/vanished is listed and is a
		// 404 everywhere else: that is a listing gone stale between the two
		// requests, and the one way a repository the connection offers is
		// still unreadable.
		if req.URL.Path == gitHubReposPath {
			_, _ = w.Write([]byte(`[{"full_name": "acme/shop", "default_branch": "trunk"},
				{"full_name": "acme/vanished", "default_branch": "trunk"}]`))
			return
		}
		// The repository itself, which is what a request that named no ref
		// resolves its default branch from. It is deliberately not "main":
		// a preflight that assumed the name would look right and be wrong.
		if req.URL.Path == "/repos/acme/shop" {
			_, _ = w.Write([]byte(`{"full_name": "acme/shop", "default_branch": "trunk"}`))
			return
		}
		path, ok := strings.CutPrefix(req.URL.Path, "/repos/acme/shop/contents")
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message": "Not Found"}`))
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
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("source", github.URL, "ghp_stored")...)...)

	// The repository root is nothing the platform recognises, which is the
	// answer somebody needs to see before they create the project.
	recorder := h.do(t, http.MethodPost, "/api/v1/connections/source/detect",
		`{"repo": "acme/shop", "ref": "main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[detectionView](t, recorder); view.Detected || view.Message == "" {
		t.Fatalf("an unrecognisable root answered as recognised: %+v", view)
	}

	// The same repository with the root directory corrected is a Dockerfile
	// build, and asking again is the whole of fixing it.
	recorder = h.do(t, http.MethodPost, "/api/v1/connections/source/detect",
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

func TestDetectingARepositoryTheConnectionCannotSee(t *testing.T) {
	// acme/vanished is a repository the connection lists and cannot read:
	// every request about it is a 404, which is what GitHub answers both for
	// a repository that does not exist and for one a token is not allowed to
	// know about. A listing that has gone stale since it was walked is what
	// that looks like from here.
	github := fakeGitHubContents(t, map[string][]string{"": {"go.mod"}}, nil)
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stored")...)...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/vanished", "ref": "main"}`)
	// It is the caller's to act on — a repository to correct, or a token to
	// widen — so it is an answer rather than a failure.
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[detectionView](t, recorder)
	if view.Detected || !view.Unreadable {
		t.Fatalf("a repository nothing could read was not reported as such: %+v", view)
	}
	// The message that used to be given here sent somebody to correct a root
	// directory that was already correct.
	if strings.Contains(view.Message, "directory") {
		t.Fatalf("an unreadable repository is still reported as a directory: %+v", view)
	}
	// It names the connection, because an installation may have several and
	// the fix is usually in the one that was asked.
	if !strings.Contains(view.Message, `"hub"`) || !strings.Contains(view.Message, "acme/vanished") {
		t.Fatalf("the message does not say what could not read what: %+v", view)
	}
}

// The preflight reads a repository's contents through the platform's own
// credential, so it answers about the repositories the connection offers and
// about nothing else. Without that, anybody who may create a project — which
// is anybody signed in, on their first day and holding no role — could read
// any repository the installation's token happens to reach by guessing its
// name.
func TestDetectingARepositoryTheConnectionDoesNotList(t *testing.T) {
	// The listing carries acme/storefront alone. This server serves nothing
	// else, so a preflight that read the repository anyway would answer 200
	// with "unreadable" rather than the refusal below.
	github := fakeGitHubRepos(t, "ghp_stored", `[{"full_name": "acme/storefront", "default_branch": "main"}]`)
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stored")...)...)
	h.demoteCaller(t)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/secrets", "ref": "main"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	got := errorOf(t, recorder.Body.String())
	// It names the connection — an installation may have several, and
	// another may well offer this repository — and the repository that was
	// asked about.
	if !strings.Contains(got, `"hub"`) || !strings.Contains(got, "acme/secrets") {
		t.Fatalf("the refusal does not say what was refused: %q", got)
	}
	if !strings.Contains(got, "does not list") {
		t.Fatalf("the refusal does not say why: %q", got)
	}
	// Nothing about the repository itself: whether it exists is what the
	// providers' own 404s are careful not to say, and a refusal that said it
	// would be the enumeration this route was closed to.
	if strings.Contains(got, "unreadable") || strings.Contains(got, "not found") {
		t.Fatalf("the refusal says something about the repository: %q", got)
	}
}

// A repository the listing does carry is read exactly as it was, and the
// name is matched the way the providers match it: acme/shop and Acme/Shop are
// one repository, and a name pasted with the wrong case is not a repository
// the connection does not offer.
func TestDetectingIgnoresTheCaseOfARepositoryName(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", "https://example.invalid", "ghp_stored")...)...)
	h.server.GitProviders = func(*kitchenv1alpha1.Connection, string) (gitprovider.Provider, error) {
		return listingProvider{repos: []string{"acme/shop"}}, nil
	}
	h.demoteCaller(t)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "Acme/Shop", "ref": "main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[detectionView](t, recorder); !view.Detected {
		t.Fatalf("a listed repository was not read: %+v", view)
	}

	// The repository next to it in the same organisation is not offered, and
	// nothing about it is read.
	recorder = h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/secrets", "ref": "main"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// A listing cut short at its cap is the one case a repository the credential
// could read is refused anyway. It is said outright, because somebody told
// only that the connection does not list it would go looking for a fault in
// the token.
func TestDetectingARepositoryPastTheListingsCap(t *testing.T) {
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != gitHubReposPath {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Every page full, so the walk stops at its own cap and says it did.
		// The size is the provider's, and a page short of it would end the
		// walk before the cap was reached.
		const providerPageSize = 100
		page := make([]map[string]any, 0, providerPageSize)
		for i := range providerPageSize {
			page = append(page, map[string]any{
				"full_name": fmt.Sprintf("acme/repo-%s-%d", req.URL.Query().Get("page"), i), "default_branch": "main"})
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(github.Close)
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stored")...)...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/secrets", "ref": "main"}`)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("want 403, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "cut short") {
		t.Fatalf("the refusal does not say the listing was truncated: %q", got)
	}
}

// A provider with no listing behind it leaves nothing to check against, and
// nothing is narrowed: gitlab and gitea are read exactly as they were. That
// is the honest gap rather than a filter invented for them.
func TestDetectingOnAProviderThatCannotEnumerate(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", "https://example.invalid", "ghp_stored")...)...)
	h.server.GitProviders = func(*kitchenv1alpha1.Connection, string) (gitprovider.Provider, error) {
		return blindProvider{}, nil
	}
	h.demoteCaller(t)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/anything", "ref": "main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if view := decode[detectionView](t, recorder); !view.Detected {
		t.Fatalf("a provider with no listing did not read the repository: %+v", view)
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

// The ref is optional, and the default branch is what a caller that sent none
// gets read at — `kitchen projects create` takes its repository from the
// checkout's origin, so nothing has told it which branch production is.
func TestDetectingWithoutARefReadsTheDefaultBranch(t *testing.T) {
	github := fakeGitHubContents(t,
		map[string][]string{"": {"go.mod", "main.go"}}, nil)
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stored")...)...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect", `{"repo": "acme/shop"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[detectionView](t, recorder)
	// The branch it was read at is answered back, so a form that sent none
	// can show which branch the verdict is about.
	if view.Ref != "trunk" {
		t.Fatalf("the repository was read at %q, not its default branch: %+v", view.Ref, view)
	}
	if !view.Detected || view.Framework != "go" {
		t.Fatalf("the default branch was not detected on: %+v", view)
	}
}

// A repository the connection lists and cannot read is a 404 on the way to
// the default branch, and it is the caller's to fix: a token that has lost
// the repository since the listing was walked, or a repository that has gone.
func TestDetectingWithoutARefForARepositoryThatIsNotThere(t *testing.T) {
	github := fakeGitHubContents(t, map[string][]string{"": {"go.mod"}}, nil)
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stored")...)...)

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect", `{"repo": "acme/vanished"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[detectionView](t, recorder)
	if view.Detected || !strings.Contains(view.Message, "acme/vanished") {
		t.Fatalf("the answer does not name the repository it could not see: %+v", view)
	}
	// It is the repository that could not be read, which is what separates
	// this from every other "detected": false — none of the build-context
	// fields is what is wrong.
	if !view.Unreadable {
		t.Fatalf("a repository nothing could read was not reported as such: %+v", view)
	}
}

// blindProvider reads a repository and cannot say anything about the
// repository itself, which is what a provider implemented as a source of
// webhooks first looks like.
type blindProvider struct{}

func (blindProvider) EnsureWebhook(context.Context, string, gitprovider.WebhookSpec) (string, error) {
	return "", nil
}
func (blindProvider) DeleteWebhook(context.Context, string, string) error { return nil }
func (blindProvider) ListDir(context.Context, string, string, string) ([]gitprovider.DirEntry, error) {
	return []gitprovider.DirEntry{{Name: "go.mod"}}, nil
}
func (blindProvider) ReadFile(context.Context, string, string, string) ([]byte, error) {
	return nil, gitprovider.ErrFileNotFound
}

// listingProvider reads a repository the way blindProvider does and lists the
// repositories it was built with, which is what lets a test say what the
// listing check makes of a name without a provider's wire format in the way.
type listingProvider struct {
	blindProvider
	repos []string
}

func (p listingProvider) ListRepositories(context.Context) (gitprovider.RepositoryListing, error) {
	listing := gitprovider.RepositoryListing{}
	for _, repo := range p.repos {
		listing.Repositories = append(listing.Repositories,
			gitprovider.Repository{FullName: repo, DefaultBranch: "main"})
	}
	return listing, nil
}

// A provider that cannot work out a default branch is not a failure of the
// platform's — it is a question the caller has to answer, and the refusal says
// which field answers it.
func TestDetectingWithoutARefOnAProviderThatCannotResolveOne(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", "https://example.invalid", "ghp_stored")...)...)
	h.server.GitProviders = func(*kitchenv1alpha1.Connection, string) (gitprovider.Provider, error) {
		return blindProvider{}, nil
	}

	recorder := h.do(t, http.MethodPost, "/api/v1/connections/hub/detect", `{"repo": "acme/shop"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "ref") {
		t.Fatalf("the refusal does not name the field that answers it: %s", recorder.Body.String())
	}

	// The same provider answers as it always did once the branch is named.
	recorder = h.do(t, http.MethodPost, "/api/v1/connections/hub/detect",
		`{"repo": "acme/shop", "ref": "main"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
}
