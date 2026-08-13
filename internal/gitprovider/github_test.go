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

func TestEnsureWebhookCreates(t *testing.T) {
	var created githubHook
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/acme/shop/hooks":
			if got := r.Header.Get("Authorization"); got != "Bearer tok" {
				t.Errorf("unexpected auth header %q", got)
			}
			_, _ = w.Write([]byte(`[]`))
		case "POST /repos/acme/shop/hooks":
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

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	id, err := gh.EnsureWebhook(context.Background(), "acme/shop", WebhookSpec{
		URL:    "https://kitchen.apps.example.com/webhooks/git/gh",
		Secret: "hunter2",
		Events: []string{"push", "pull_request"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "42" {
		t.Errorf("expected hook id 42, got %s", id)
	}
	if created.Config.Secret != "hunter2" || created.Config.ContentType != "json" {
		t.Errorf("unexpected hook config %+v", created.Config)
	}
	if len(created.Events) != 2 {
		t.Errorf("unexpected events %v", created.Events)
	}
}

func TestEnsureWebhookUpdatesExisting(t *testing.T) {
	patched := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /repos/acme/shop/hooks":
			_, _ = w.Write([]byte(`[{"id": 7, "config": {"url": "https://kitchen.apps.example.com/webhooks/git/gh"}}]`))
		case "PATCH /repos/acme/shop/hooks/7":
			patched = true
			_, _ = w.Write([]byte(`{"id": 7}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	id, err := gh.EnsureWebhook(context.Background(), "acme/shop", WebhookSpec{
		URL: "https://kitchen.apps.example.com/webhooks/git/gh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if id != "7" {
		t.Errorf("expected hook id 7, got %s", id)
	}
	if !patched {
		t.Error("expected existing hook to be updated")
	}
}

func TestDeleteWebhookToleratesMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	gh := &GitHub{APIURL: server.URL, Token: "tok"}
	if err := gh.DeleteWebhook(context.Background(), "acme/shop", "42"); err != nil {
		t.Fatalf("expected 404 to be tolerated, got %v", err)
	}
}
