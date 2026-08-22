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

package gitprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

const (
	giteaHooksPath  = "/repos/acme/shop/hooks"
	giteaHookSecret = "hunter2"
)

func TestGiteaEnsureWebhookCreates(t *testing.T) {
	var created giteaHook
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET " + giteaHooksPath:
			_, _ = w.Write([]byte(`[]`))
		case "POST " + giteaHooksPath:
			if err := json.NewDecoder(r.Body).Decode(&created); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 42}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	id, err := gt.EnsureWebhook(context.Background(), "acme/shop", WebhookSpec{
		URL:    "https://kitchen.example.com/webhooks/git/gt",
		Secret: giteaHookSecret,
		Events: []string{"push", "pull_request"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "42" {
		t.Errorf("expected id 42, got %s", id)
	}
	if created.Config.Secret != giteaHookSecret || created.Config.ContentType != "json" {
		t.Errorf("unexpected hook config %+v", created.Config)
	}
}

func TestGiteaDeleteWebhookToleratesMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gt := &Gitea{APIURL: server.URL, Token: "tok"}
	if err := gt.DeleteWebhook(context.Background(), "acme/shop", "42"); err != nil {
		t.Fatalf("expected 404 to be tolerated, got %v", err)
	}
}
