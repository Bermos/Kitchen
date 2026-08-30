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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The repository listing: the second field of the create-a-project form,
// answered from what the connection's stored credential can see so that the
// repository is chosen rather than spelled.

// fakeGitHubRepos serves /user/repos for one token and nothing else, which is
// the whole of what the listing asks GitHub for.
func fakeGitHubRepos(t *testing.T, goodToken, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/user/repos" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if req.Header.Get("Authorization") != "Bearer "+goodToken {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message": "Bad credentials"}`))
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// gitHubConnection is a stored github connection pointing at a test server,
// with the credential the platform would have written for it.
func gitHubConnection(name, apiURL, token string) []runtime.Object {
	secretName := connectionSecretPrefix + name
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "github",
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: secretName},
			Config:               &runtime.RawExtension{Raw: []byte(`{"apiUrl": "` + apiURL + `"}`)},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: testNamespace},
		Data:       map[string][]byte{gitTokenKey: []byte(token)},
	}
	return []runtime.Object{connection, secret}
}

func TestListingWhatAConnectionCanSee(t *testing.T) {
	github := fakeGitHubRepos(t, "ghp_stored", `[
		{"full_name": "acme/storefront", "default_branch": "main", "private": true, "description": "the shop"},
		{"full_name": "acme/blog", "default_branch": "trunk"}
	]`)
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stored")...)...)
	// Anybody who can create a project can fill in its repository field, so
	// the caller here holds no role on anything at all.
	h.demoteCaller(t)

	recorder := h.do(t, http.MethodGet, "/api/v1/connections/hub/repositories", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[connectionRepositoriesView](t, recorder)
	if !view.Supported || view.Provider != "github" {
		t.Fatalf("a github connection did not answer as enumerable: %+v", view)
	}
	if len(view.Items) != 2 {
		t.Fatalf("want both repositories, got %+v", view.Items)
	}
	// The two fields the form fills itself in from.
	if view.Items[0].FullName != "acme/storefront" || view.Items[0].DefaultBranch != "main" {
		t.Fatalf("the listing does not carry what the form needs: %+v", view.Items[0])
	}
	if view.Truncated {
		t.Fatalf("a short listing claimed it was cut short: %+v", view)
	}
	// The token was used to ask the question and stays where it was.
	if strings.Contains(recorder.Body.String(), "ghp_stored") {
		t.Fatalf("the listing leaks the credential: %s", recorder.Body.String())
	}
}

func TestListingRepositoriesOfAProviderThatHasNone(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	// The registry connection holds a docker credential and has no
	// repositories to offer: that is a form field to type into, not an error,
	// and the secret is never even read for one.
	recorder := h.do(t, http.MethodGet, "/api/v1/connections/registry/repositories", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[connectionRepositoriesView](t, recorder)
	if view.Supported || len(view.Items) != 0 {
		t.Fatalf("a registry connection offered repositories: %+v", view)
	}
	if !strings.Contains(view.Message, "dockerRegistry") || !strings.Contains(view.Message, "owner/name") {
		t.Fatalf("the answer does not say what to do instead: %+v", view)
	}
}

func TestListingRepositoriesOfAProviderWithoutEnumeration(t *testing.T) {
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "gitlab",
			CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: connectionSecretPrefix + "lab"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: connectionSecretPrefix + "lab", Namespace: testNamespace},
		Data:       map[string][]byte{gitTokenKey: []byte("glpat")},
	}
	h := newHarness(t, nil, append(fixtures(), connection, secret)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/connections/lab/repositories", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[connectionRepositoriesView](t, recorder)
	if view.Supported {
		t.Fatalf("gitlab answered as enumerable: %+v", view)
	}
	if !strings.Contains(view.Message, "cannot enumerate repositories") {
		t.Fatalf("the answer does not say how to proceed: %+v", view)
	}
}

func TestListingRepositoriesWhenTheProviderRefusesTheToken(t *testing.T) {
	github := fakeGitHubRepos(t, "ghp_current", `[]`)
	h := newHarness(t, nil, append(fixtures(), gitHubConnection("hub", github.URL, "ghp_stale")...)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/connections/hub/repositories", "")
	// The platform is fine; the provider would not answer. The form still
	// takes a typed name, so this is the gateway's failure and not a 500.
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "Bad credentials") {
		t.Fatalf("the refusal does not carry the provider's words: %q", got)
	}
}

func TestListingRepositoriesOfAConnectionThatIsNotThere(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/connections/ghost/repositories", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}
