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
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

const (
	testNamespace = "kitchen-system"
	// testBuild is the build the fixtures start with: the one a rebuild
	// repeats and the one whose logs are read.
	testBuild = "shop-bld-abc123def456"
)

// issuer is a stand-in for the platform's identity provider: it serves the
// discovery document and the JWKS the API validates against, and it signs
// tokens with Ed25519 — the algorithm better-auth's jwt plugin defaults to.
type issuer struct {
	server  *httptest.Server
	private ed25519.PrivateKey
	keyID   string
}

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	iss := &issuer{private: private, keyID: "test-key"}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                iss.url(),
			"authorization_endpoint":                iss.url() + "/oauth2/authorize",
			"token_endpoint":                        iss.url() + "/oauth2/token",
			"jwks_uri":                              iss.url() + "/jwks",
			"id_token_signing_alg_values_supported": []string{"EdDSA"},
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "OKP",
			"crv": "Ed25519",
			"alg": "EdDSA",
			"use": "sig",
			"kid": iss.keyID,
			"x":   base64.RawURLEncoding.EncodeToString(public),
		}}})
	})

	iss.server = httptest.NewServer(mux)
	t.Cleanup(iss.server.Close)
	return iss
}

func (i *issuer) url() string {
	if i.server == nil {
		return ""
	}
	return i.server.URL
}

// sign mints a token. Everything a caller might get wrong — the audience, the
// expiry, the signing key — is a parameter, because those are exactly the
// cases the middleware exists to catch.
func (i *issuer) sign(t *testing.T, claims map[string]any, key ed25519.PrivateKey) string {
	t.Helper()
	if key == nil {
		key = i.private
	}
	encode := func(value any) string {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	signingInput := encode(map[string]string{"alg": "EdDSA", "typ": "JWT", "kid": i.keyID}) + "." + encode(claims)
	signature := ed25519.Sign(key, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// token is the everyday case: a valid platform token for the API.
func (i *issuer) token(t *testing.T) string {
	t.Helper()
	return i.sign(t, map[string]any{
		"sub":   "user_1",
		"email": "grace@example.com",
		"iss":   i.url(),
		"aud":   i.url(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"exp":   time.Now().Add(time.Hour).Unix(),
	}, nil)
}

// fixtures is one project with a build, a release and a live production
// environment: enough for every read, a rebuild and a rollback.
func fixtures() []runtime.Object {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.GitSourceSpec{
				ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: "gh"},
				Repo:             "acme/shop",
				ProductionBranch: "main",
			},
			Registry: kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
			},
			Previews: kitchenv1alpha1.PreviewsSpec{Enabled: true},
		},
		Status: kitchenv1alpha1.ProjectStatus{
			LatestBuildRef: &kitchenv1alpha1.LocalObjectReference{Name: testBuild},
		},
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:              testBuild,
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Hour)),
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git: kitchenv1alpha1.GitRevision{
				SHA:     "abc123def456789",
				Branch:  "main",
				Message: "ship it",
				Author:  "grace",
			},
		},
		Status: kitchenv1alpha1.BuildStatus{Phase: kitchenv1alpha1.BuildSucceeded},
	}
	release := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-1", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: testBuild},
			Image:      "registry.example.com/shop@sha256:1111",
		},
	}
	previous := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-rel-0", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "shop-bld-000000000000"},
			Image:      "registry.example.com/shop@sha256:0000",
		},
	}
	other := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: "blog-rel-0", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "blog"},
			BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "blog-bld-000000000000"},
			Image:      "registry.example.com/blog@sha256:2222",
		},
	}
	environment := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-production", Namespace: testNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.EnvironmentProduction,
			ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "shop-rel-1"},
		},
		Status: kitchenv1alpha1.EnvironmentStatus{
			Phase: kitchenv1alpha1.EnvironmentLive,
			URL:   "https://shop.apps.example.com",
		},
	}
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "github",
			CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "gh-credentials"},
		},
		Status: kitchenv1alpha1.ConnectionStatus{
			Capabilities: []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityGitSource},
		},
	}
	registry := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "dockerRegistry",
			CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "registry-credentials"},
		},
		Status: kitchenv1alpha1.ConnectionStatus{
			Capabilities: []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityImageStore},
		},
	}
	domain := &kitchenv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-com", Namespace: testNamespace},
		Spec: kitchenv1alpha1.DomainSpec{
			Hostname:       "shop.example.com",
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: "shop-production"},
		},
	}
	claim := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-db", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "neon"},
			Type:          "postgres",
		},
	}
	return []runtime.Object{project, build, release, previous, other, environment, connection, registry, domain, claim}
}

// stubLogs stands in for the telemetry store.
type stubLogs struct {
	lines      []clickhouse.LogLine
	last       clickhouse.LogQuery
	lastFilter clickhouse.LogFilter
	filterErr  error
}

func (s *stubLogs) SearchLogs(_ context.Context, query clickhouse.LogQuery) ([]clickhouse.LogLine, error) {
	s.last = query
	return s.lines, nil
}

func (s *stubLogs) FilterLogs(_ context.Context, filter clickhouse.LogFilter) ([]clickhouse.LogLine, error) {
	s.lastFilter = filter
	if s.filterErr != nil {
		return nil, s.filterErr
	}
	return s.lines, nil
}

type harness struct {
	server  *Server
	handler http.Handler
	issuer  *issuer
	logs    *stubLogs
}

// do sends a request carrying a valid token unless one is passed explicitly.
func (h *harness) do(t *testing.T, method, path, body string, token ...string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body == "" {
		req.ContentLength = 0
	}
	req.Header.Set("Content-Type", "application/json")
	bearer := ""
	if len(token) > 0 {
		bearer = token[0]
	} else {
		bearer = h.issuer.token(t)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, req)
	return recorder
}

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()
	var body T
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable response %q: %v", recorder.Body.String(), err)
	}
	return body
}

func newHarness(t *testing.T, kitchen *kitchenv1alpha1.Kitchen, objs ...runtime.Object) *harness {
	t.Helper()
	iss := newIssuer(t)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	if kitchen == nil {
		kitchen = &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: controller.KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				Auth:       kitchenv1alpha1.AuthSpec{Enabled: true, Host: iss.url()},
			},
		}
	}

	objects := append([]runtime.Object{kitchen}, objs...)
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithRuntimeObjects(objects...).
		WithStatusSubresource(&kitchenv1alpha1.Build{}, &kitchenv1alpha1.Environment{}).
		Build()

	logs := &stubLogs{}
	server := &Server{Client: c, Namespace: testNamespace}
	server.logStore = func(context.Context) (logReader, error) { return logs, nil }

	return &harness{server: server, handler: server.Handler(), issuer: iss, logs: logs}
}

// routes is every endpoint the API serves, used to prove that none of them
// answers an anonymous caller.
var routes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v1/projects"},
	{http.MethodPost, "/api/v1/projects"},
	{http.MethodGet, "/api/v1/projects/shop"},
	{http.MethodGet, "/api/v1/projects/shop/builds"},
	{http.MethodPost, "/api/v1/projects/shop/builds"},
	{http.MethodGet, "/api/v1/projects/shop/releases"},
	{http.MethodGet, "/api/v1/projects/shop/environments"},
	{http.MethodGet, "/api/v1/builds"},
	{http.MethodGet, "/api/v1/builds/shop-bld-abc123def456"},
	{http.MethodGet, "/api/v1/builds/shop-bld-abc123def456/logs"},
	{http.MethodGet, "/api/v1/releases"},
	{http.MethodGet, "/api/v1/releases/shop-rel-1"},
	{http.MethodGet, "/api/v1/environments"},
	{http.MethodGet, "/api/v1/environments/shop-production"},
	{http.MethodPatch, "/api/v1/environments/shop-production"},
	{http.MethodGet, "/api/v1/environments/shop-production/logs"},
	{http.MethodGet, "/api/v1/logs"},
	{http.MethodGet, "/api/v1/settings"},
	{http.MethodPatch, "/api/v1/settings"},
	{http.MethodGet, "/api/v1/connections"},
	{http.MethodGet, "/api/v1/connections/gh"},
	{http.MethodGet, "/api/v1/domains"},
	{http.MethodGet, "/api/v1/domains/shop-com"},
	{http.MethodGet, "/api/v1/claims"},
	{http.MethodGet, "/api/v1/claims/shop-db"},
	{http.MethodGet, "/api/v1/nonsense"},
}

func TestEveryEndpointRefusesAnAnonymousCaller(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, route := range routes {
		t.Run(route.method+" "+route.path, func(t *testing.T) {
			recorder := h.do(t, route.method, route.path, "", "")
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if auth := recorder.Header().Get("WWW-Authenticate"); !strings.HasPrefix(auth, "Bearer ") {
				t.Fatalf("want a Bearer challenge, got %q", auth)
			}
		})
	}
}

func TestTokensTheIssuerDidNotMintAreRefused(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	_, stranger, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}

	claims := func(overrides map[string]any) map[string]any {
		base := map[string]any{
			"sub": "user_1",
			"iss": h.issuer.url(),
			"aud": h.issuer.url(),
			"iat": time.Now().Add(-time.Minute).Unix(),
			"exp": time.Now().Add(time.Hour).Unix(),
		}
		for key, value := range overrides {
			base[key] = value
		}
		return base
	}

	for name, token := range map[string]string{
		"signed by a stranger": h.issuer.sign(t, claims(nil), stranger),
		"expired":              h.issuer.sign(t, claims(map[string]any{"exp": time.Now().Add(-time.Minute).Unix()}), nil),
		"from another issuer":  h.issuer.sign(t, claims(map[string]any{"iss": "https://evil.example.com"}), nil),
		"for another audience": h.issuer.sign(t, claims(map[string]any{"aud": "https://someone-else.example.com"}), nil),
		"without a subject":    h.issuer.sign(t, claims(map[string]any{"sub": ""}), nil),
		"not a token at all":   "surely-this-works",
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodGet, "/api/v1/projects", "", token)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("want 401, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestATokenForTheAPIsOwnURLIsAccepted(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	// What a client gets when it asks the issuer for a token for this API
	// by name (`resource=https://kitchen.apps.example.com`).
	token := h.issuer.sign(t, map[string]any{
		"sub": "user_1",
		"iss": h.issuer.url(),
		"aud": "https://kitchen.apps.example.com",
		"exp": time.Now().Add(time.Hour).Unix(),
	}, nil)

	recorder := h.do(t, http.MethodGet, "/api/v1/projects", "", token)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestAnInstallationWithoutAnIdentityProviderServesNothing(t *testing.T) {
	iss := newIssuer(t)
	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: controller.KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			BaseDomain: "apps.example.com",
			Auth:       kitchenv1alpha1.AuthSpec{Enabled: false, Host: iss.url()},
		},
	}
	h := newHarness(t, kitchen, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/projects", "", h.issuer.token(t))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestListingProjects(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/projects", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[listBody[projectView]](t, recorder)
	if len(body.Items) != 1 {
		t.Fatalf("want one project, got %d", len(body.Items))
	}
	project := body.Items[0]
	if project.Name != "shop" || project.Repo != "acme/shop" || project.Connection != "gh" {
		t.Fatalf("unexpected project: %+v", project)
	}
	if project.LatestBuild != testBuild {
		t.Fatalf("want the latest build, got %q", project.LatestBuild)
	}
}

func TestGettingSomethingThatIsNotThere(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/projects/nope", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCreatingAProject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"blog","repo":"acme/blog","connection":"gh","registry":"registry","productionBranch":"trunk","previews":false}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if view.Name != "blog" || view.Repo != "acme/blog" || view.ProductionBranch != "trunk" || view.Previews {
		t.Fatalf("the response does not echo the request: %+v", view)
	}

	stored := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "blog", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.Source.ConnectionRef.Name != "gh" || stored.Spec.Registry.ConnectionRef.Name != "registry" {
		t.Fatalf("the connections did not stick: %+v", stored.Spec)
	}
	if got := stored.Annotations["kitchen.bermos.dev/requested-by"]; got != "grace@example.com" {
		t.Fatalf("the creator should be recorded, got %q", got)
	}
}

func TestCreatingAProjectAppliesTheDefaults(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"blog","repo":"acme/blog","connection":"gh","registry":"registry"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if view.ProductionBranch != "main" || !view.Previews {
		t.Fatalf("want the main branch and previews on by default, got %+v", view)
	}
}

func TestCreatingAProjectThatAlreadyExists(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"shop","repo":"acme/shop","connection":"gh","registry":"registry"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestCreatingAProjectRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"no name":                              `{"repo":"acme/blog","connection":"gh","registry":"registry"}`,
		"a name that is not a DNS label":       `{"name":"Blog!","repo":"acme/blog","connection":"gh","registry":"registry"}`,
		"a name too long to derive names from": `{"name":"` + strings.Repeat("a", 47) + `","repo":"acme/blog","connection":"gh","registry":"registry"}`,
		"no repo":                              `{"name":"blog","connection":"gh","registry":"registry"}`,
		"a repo without an owner":              `{"name":"blog","repo":"blog","connection":"gh","registry":"registry"}`,
		"no connection":                        `{"name":"blog","repo":"acme/blog","registry":"registry"}`,
		"no registry":                          `{"name":"blog","repo":"acme/blog","connection":"gh"}`,
		"an unknown field":                     `{"name":"blog","repo":"acme/blog","connection":"gh","registry":"registry","branch":"main"}`,
		"not JSON":                             `{`,
		"an empty body":                        ``,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/projects", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			if err := h.server.get(context.Background(), "blog", &kitchenv1alpha1.Project{}); err == nil {
				t.Fatal("the project was created anyway")
			}
		})
	}
}

func TestCreatingAProjectChecksItsConnections(t *testing.T) {
	// A Connection the operator has not reconciled yet reports no
	// capabilities; the create flow accepts it and the Project's own
	// conditions take over from there.
	unreconciled := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "gitea",
			CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "fresh-credentials"},
		},
	}
	h := newHarness(t, nil, append(fixtures(), unreconciled)...)

	for name, tc := range map[string]struct {
		body string
		code int
		want string
	}{
		"a connection that does not exist": {
			body: `{"name":"blog","repo":"acme/blog","connection":"nope","registry":"registry"}`,
			code: http.StatusBadRequest,
			want: "does not exist: create the Connection first",
		},
		"a registry without the imageStore capability": {
			body: `{"name":"blog","repo":"acme/blog","connection":"gh","registry":"gh"}`,
			code: http.StatusBadRequest,
			want: "imageStore",
		},
		"a connection that has not reported capabilities yet": {
			body: `{"name":"blog","repo":"acme/blog","connection":"fresh","registry":"registry"}`,
			code: http.StatusCreated,
			want: `"name":"blog"`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/projects", tc.body)
			if recorder.Code != tc.code {
				t.Fatalf("want %d, got %d: %s", tc.code, recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), tc.want) {
				t.Fatalf("want %q in the response, got %s", tc.want, recorder.Body.String())
			}
		})
	}
}

func TestRebuildingWithoutABodyRepeatsTheLastCommit(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/shop/builds", "")
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	build := decode[buildView](t, recorder)
	if build.Git.SHA != "abc123def456789" || build.Git.Branch != "main" {
		t.Fatalf("want the last commit rebuilt, got %+v", build.Git)
	}
	if build.Name == testBuild {
		t.Fatal("a rebuild must be a new Build, not the one it repeats")
	}
	if !strings.HasPrefix(build.Name, testBuild+"-") {
		t.Fatalf("want a name that reads like the build it repeats, got %q", build.Name)
	}

	list := &kitchenv1alpha1.BuildList{}
	if err := h.server.Client.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 2 {
		t.Fatalf("want two builds after a rebuild, got %d", len(list.Items))
	}
}

func TestRebuildingANamedCommit(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/shop/builds",
		`{"sha":"fedcba9876543210","branch":"topic"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	build := decode[buildView](t, recorder)
	if build.Git.SHA != "fedcba9876543210" || build.Git.Branch != "topic" {
		t.Fatalf("unexpected revision: %+v", build.Git)
	}
}

func TestRebuildingAFreshCommitFallsBackToTheProductionBranch(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/shop/builds", `{"sha":"0123456789abcdef"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if build := decode[buildView](t, recorder); build.Git.Branch != "main" {
		t.Fatalf("want the production branch, got %q", build.Git.Branch)
	}
}

func TestRebuildingRejectsUnusableRequests(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, body := range map[string]string{
		"a sha that is not one": `{"sha":"abc"}`,
		"an unknown field":      `{"commit":"abc123def456789"}`,
		"not JSON":              `{`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPost, "/api/v1/projects/shop/builds", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestRebuildingAProjectThatHasNeverBuilt(t *testing.T) {
	fresh := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "blog", Namespace: testNamespace},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.GitSourceSpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
				Repo:          "acme/blog",
			},
			Registry: kitchenv1alpha1.RegistrySpec{
				ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), fresh)...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects/blog/builds", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "never been built") {
		t.Fatalf("the error should say why: %s", recorder.Body.String())
	}
}

func TestRollingAnEnvironmentBack(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/environments/shop-production", `{"release":"shop-rel-0"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if env := decode[environmentView](t, recorder); env.Release != "shop-rel-0" {
		t.Fatalf("want the older release, got %q", env.Release)
	}

	stored := &kitchenv1alpha1.Environment{}
	if err := h.server.get(context.Background(), "shop-production", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.ReleaseRef.Name != "shop-rel-0" {
		t.Fatalf("the rollback did not stick: %q", stored.Spec.ReleaseRef.Name)
	}
}

func TestRollbackRefusesAReleaseFromAnotherProject(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/environments/shop-production", `{"release":"blog-rel-0"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := &kitchenv1alpha1.Environment{}
	if err := h.server.get(context.Background(), "shop-production", stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.ReleaseRef.Name != "shop-rel-1" {
		t.Fatalf("the environment was moved anyway: %q", stored.Spec.ReleaseRef.Name)
	}
}

func TestRollbackRefusesAReleaseThatDoesNotExist(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPatch, "/api/v1/environments/shop-production", `{"release":"shop-rel-99"}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestReadingABuildsLogs(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.logs.lines = []clickhouse.LogLine{{
		Timestamp: time.Now(),
		Source:    clickhouse.SourceBuild,
		Build:     testBuild,
		Message:   "#8 exporting layers done",
	}}

	recorder := h.do(t, http.MethodGet,
		"/api/v1/builds/shop-bld-abc123def456/logs?limit=50&search=exporting", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if body := decode[listBody[clickhouse.LogLine]](t, recorder); len(body.Items) != 1 {
		t.Fatalf("want one line, got %d", len(body.Items))
	}
	if h.logs.last.Build != testBuild || h.logs.last.Source != clickhouse.SourceBuild {
		t.Fatalf("the query was not scoped to the build: %+v", h.logs.last)
	}
	if h.logs.last.Limit != 50 || h.logs.last.Search != "exporting" {
		t.Fatalf("the query dropped its parameters: %+v", h.logs.last)
	}
}

func TestReadingAnEnvironmentsLogs(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/shop-production/logs", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if h.logs.last.Environment != "shop-production" || h.logs.last.Source != clickhouse.SourceRuntime {
		t.Fatalf("the query was not scoped to the environment: %+v", h.logs.last)
	}
	if h.logs.last.Project != "shop" {
		t.Fatalf("the query lost the project: %+v", h.logs.last)
	}
}

func TestLogsOfSomethingThatDoesNotExist(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/builds/nope/logs", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestLogsWithoutATelemetryStore(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	h.server.logStore = func(context.Context) (logReader, error) { return nil, errNoLogStore }
	h.handler = h.server.Handler()

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/shop-production/logs", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestLogsRejectNonsenseParameters(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for _, query := range []string{"?limit=0", "?limit=lots", "?since=yesterday"} {
		recorder := h.do(t, http.MethodGet, "/api/v1/environments/shop-production/logs"+query, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d: %s", query, recorder.Code, recorder.Body.String())
		}
	}
}

func TestTheRestOfTheCollections(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for path, want := range map[string]string{
		"/api/v1/builds":                              testBuild,
		"/api/v1/releases?project=shop":               "shop-rel-1",
		"/api/v1/environments":                        "shop-production",
		"/api/v1/connections":                         "github",
		"/api/v1/domains?environment=shop-production": "shop.example.com",
		"/api/v1/claims?project=shop":                 "postgres",
	} {
		recorder := h.do(t, http.MethodGet, path, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d: %s", path, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("%s: want %q in the response, got %s", path, want, recorder.Body.String())
		}
	}
}

func TestAConnectionNeverExposesItsCredentials(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/connections/gh", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "gh-credentials") {
		t.Fatalf("the response names the credentials secret: %s", recorder.Body.String())
	}
}

func TestTheIssuerIsDerivedFromTheKitchenObject(t *testing.T) {
	for name, spec := range map[string]struct {
		kitchen kitchenv1alpha1.KitchenSpec
		issuer  string
		audit   string
	}{
		"from the base domain": {
			kitchen: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				Auth:       kitchenv1alpha1.AuthSpec{Enabled: true},
			},
			issuer: "https://auth.apps.example.com",
			audit:  "https://kitchen.apps.example.com",
		},
		// Without TLS the Gateway only listens on HTTP, so an issuer
		// advertised as https is one nothing answers on: discovery, the JWKS
		// fetch and every redirect built from it would fail.
		"under the scheme the gateway serves": {
			kitchen: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeNone},
				Auth:       kitchenv1alpha1.AuthSpec{Enabled: true},
			},
			issuer: "http://auth.apps.example.com",
			audit:  "http://kitchen.apps.example.com",
		},
		"from an explicit host and external URL": {
			kitchen: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				API:        kitchenv1alpha1.APISpec{ExternalURL: "https://api.example.com/"},
				Auth:       kitchenv1alpha1.AuthSpec{Enabled: true, Host: "id.example.com"},
			},
			issuer: "https://id.example.com",
			audit:  "https://api.example.com",
		},
		// An external identity provider may be reached over a scheme of its
		// own, whatever the platform's edge does.
		"from a host that spells its scheme out": {
			kitchen: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeNone},
				API:        kitchenv1alpha1.APISpec{ExternalURL: "http://kitchen.example.com"},
				Auth:       kitchenv1alpha1.AuthSpec{Enabled: true, Host: "https://id.example.com"},
			},
			issuer: "https://id.example.com",
			audit:  "http://kitchen.example.com",
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := issuerFor(&kitchenv1alpha1.Kitchen{Spec: spec.kitchen})
			if err != nil {
				t.Fatal(err)
			}
			if cfg.issuer != spec.issuer {
				t.Fatalf("want issuer %q, got %q", spec.issuer, cfg.issuer)
			}
			if len(cfg.audiences) != 2 || cfg.audiences[1] != spec.audit {
				t.Fatalf("want the API's own URL accepted as an audience, got %v", cfg.audiences)
			}
		})
	}
}

func TestBuildNamePrefixStaysWithinTheNameLimit(t *testing.T) {
	prefix := buildNamePrefix(strings.Repeat("p", 63), "abcdef1234567890")
	if len(prefix) > 58 {
		t.Fatalf("a generated build name would not fit: %d characters", len(prefix))
	}
	if !strings.HasSuffix(prefix, "-") {
		t.Fatalf("a generateName prefix should end in a separator: %q", prefix)
	}
}
