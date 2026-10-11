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

package registrytoken

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func testAccounts(key *rsa.PrivateKey) *StaticAccounts {
	return &StaticAccounts{Key: key, Accounts: map[string]StaticAccount{
		"kitchen":      {Password: "platform", Grant: PlatformGrant()},
		"kitchen-read": {Password: "reader", Grant: ReadGrant()},
		"shop.push":    {Password: "shop-push", Grant: ProjectGrant("shop", true)},
		"shop.pull":    {Password: "shop-pull", Grant: ProjectGrant("shop", false)},
	}}
}

// verified decodes a token after checking its signature the way the
// registry does: RS256 against the public half of the key.
func verified(t *testing.T, key *rsa.PrivateKey, token string) claims {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("a JWT has three parts, this has %d", len(parts))
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], signature); err != nil {
		t.Fatalf("the token's signature does not verify: %v", err)
	}
	var header map[string]string
	raw, _ := base64.RawURLEncoding.DecodeString(parts[0])
	if err := json.Unmarshal(raw, &header); err != nil || header["alg"] != "RS256" {
		t.Fatalf("header %s", raw)
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

func ask(t *testing.T, h http.Handler, username, password string, scopes ...string) (int, claims) {
	t.Helper()
	query := url.Values{"service": {"registry.apps.example.com"}, "scope": scopes}
	req := httptest.NewRequest(http.MethodGet, "/kitchen/token?"+query.Encode(), nil)
	if username != "" {
		req.SetBasicAuth(username, password)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, claims{}
	}
	var body tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Token == "" || body.Token != body.AccessToken {
		t.Fatalf("the token is answered under both names clients read: %+v", body)
	}
	handler := h.(*Handler)
	key, _ := handler.Accounts.SigningKey(t.Context())
	return rec.Code, verified(t, key, body.Token)
}

func TestAProjectsAccountReachesItsOwnRepositoriesAndNoOther(t *testing.T) {
	h := &Handler{Accounts: testAccounts(testKey(t))}

	_, c := ask(t, h, "shop.push", "shop-push",
		"repository:shop:pull,push",
		"repository:shop/api:pull,push",
		"repository:shop-api:pull,push", // a project called shop-api, not shop's
		"repository:other:pull",         // another project
		"repository:shopping:pull",      // a name that merely starts the same
		"repository:shop/../other:pull", // not a repository name at all
		"registry:catalog:*",            // not granted to anybody here
	)
	want := []ResourceAccess{
		{Type: "repository", Name: "shop", Actions: []string{"pull", "push"}},
		{Type: "repository", Name: "shop/api", Actions: []string{"pull", "push"}},
	}
	if !reflect.DeepEqual(c.Access, want) {
		t.Errorf("access = %+v, want %+v", c.Access, want)
	}
	if c.Subject != "shop.push" || c.Audience != "registry.apps.example.com" {
		t.Errorf("sub %q aud %q", c.Subject, c.Audience)
	}
}

func TestAProjectsReadAccountCannotPush(t *testing.T) {
	h := &Handler{Accounts: testAccounts(testKey(t))}
	_, c := ask(t, h, "shop.pull", "shop-pull", "repository:shop/api:pull,push")
	want := []ResourceAccess{{Type: "repository", Name: "shop/api", Actions: []string{"pull"}}}
	if !reflect.DeepEqual(c.Access, want) {
		t.Errorf("access = %+v, want %+v", c.Access, want)
	}
}

func TestThePlatformsAccountsReachEverything(t *testing.T) {
	h := &Handler{Accounts: testAccounts(testKey(t))}
	_, c := ask(t, h, "kitchen", "platform", "repository:anyone/at-all:*")
	if len(c.Access) != 1 || !reflect.DeepEqual(c.Access[0].Actions, []string{"pull", "push", "delete"}) {
		t.Errorf("the platform's pushing account: %+v", c.Access)
	}
	_, c = ask(t, h, "kitchen-read", "reader", "repository:anyone:pull,push")
	if len(c.Access) != 1 || !reflect.DeepEqual(c.Access[0].Actions, []string{"pull"}) {
		t.Errorf("the platform's read account: %+v", c.Access)
	}
}

func TestLegacyRepositoriesArePullOnly(t *testing.T) {
	grant := Grant{
		Project: "shop", ProjectActions: []string{ActionPull, ActionPush},
		Legacy: map[string]bool{"shop-api": true},
	}
	got := accessFor(grant, []string{"repository:shop-api:pull,push", "repository:shop-web:pull"})
	want := []ResourceAccess{{Type: "repository", Name: "shop-api", Actions: []string{"pull"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("access = %+v, want %+v", got, want)
	}
}

func TestAWrongPasswordOrNoneIsRefused(t *testing.T) {
	h := &Handler{Accounts: testAccounts(testKey(t))}
	if code, _ := ask(t, h, "shop.push", "guess", "repository:shop:pull"); code != http.StatusUnauthorized {
		t.Errorf("a wrong password: %d", code)
	}
	if code, _ := ask(t, h, "", "", "repository:shop:pull"); code != http.StatusUnauthorized {
		t.Errorf("no credential: %d", code)
	}
	if code, _ := ask(t, h, "nobody.push", "x", "repository:nobody:pull"); code != http.StatusUnauthorized {
		t.Errorf("an account that does not exist: %d", code)
	}
}

// What `docker login` and the connection test ask: a token for no scope at
// all, which is the registry's way of checking the credential exists.
func TestALoginWithNoScopeGetsATokenForNothing(t *testing.T) {
	h := &Handler{Accounts: testAccounts(testKey(t))}
	code, c := ask(t, h, "shop.push", "shop-push")
	if code != http.StatusOK || len(c.Access) != 0 {
		t.Errorf("code %d access %+v", code, c.Access)
	}
}

// containerd tries the OAuth 2 password grant before the GET.
func TestThePasswordGrantIsAnsweredToo(t *testing.T) {
	key := testKey(t)
	h := &Handler{Accounts: testAccounts(key)}
	form := url.Values{
		"grant_type": {"password"}, "username": {"shop.push"}, "password": {"shop-push"},
		"service": {"registry.apps.example.com"}, "scope": {"repository:shop:pull"}, "client_id": {"containerd"},
	}
	req := httptest.NewRequest(http.MethodPost, "/kitchen/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	var body tokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	c := verified(t, key, body.AccessToken)
	if len(c.Access) != 1 || c.Access[0].Name != "shop" {
		t.Errorf("access %+v", c.Access)
	}

	form.Set("grant_type", "refresh_token")
	req = httptest.NewRequest(http.MethodPost, "/kitchen/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a refresh grant, when no refresh token is ever issued: %d", rec.Code)
	}
}

func TestATokenIsDatedForClockSkewAndExpires(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h := &Handler{Accounts: testAccounts(testKey(t)), Now: func() time.Time { return now }}
	_, c := ask(t, h, "shop.push", "shop-push", "repository:shop:pull")
	if c.IssuedAt >= now.Unix() {
		t.Errorf("a registry on a node whose clock is a little behind refuses a token issued in its future")
	}
	if c.ExpiresAt != now.Add(DefaultTTL).Unix() {
		t.Errorf("exp %d", c.ExpiresAt)
	}
}

func TestParseScope(t *testing.T) {
	for _, tc := range []struct {
		scope   string
		name    string
		actions []string
		ok      bool
	}{
		{"repository:shop:pull", "shop", []string{"pull"}, true},
		{"repository:shop/api:pull,push", "shop/api", []string{"pull", "push"}, true},
		{"repository:shop:*", "shop", []string{"*"}, true},
		{"repository:Shop:pull", "", nil, false},
		{"repository::pull", "", nil, false},
		{"repository:shop", "", nil, false},
		{"registry:catalog:*", "", nil, false},
	} {
		name, actions, ok := parseScope(tc.scope)
		if name != tc.name || ok != tc.ok || !reflect.DeepEqual(actions, tc.actions) {
			t.Errorf("parseScope(%q) = %q %v %v", tc.scope, name, actions, ok)
		}
	}
}
