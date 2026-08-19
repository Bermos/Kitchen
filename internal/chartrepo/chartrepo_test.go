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

package chartrepo

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newRegistry serves the read half of a registry over TLS, the way a real one
// behaves: an unauthenticated request is challenged, and the token endpoint
// hands out a token for a public repository without asking who is asking.
func newRegistry(t *testing.T, tags []string) (*Client, *int) {
	t.Helper()

	listings := 0
	mux := http.NewServeMux()
	server := httptest.NewTLSServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/token", func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Query().Get("scope") == "" {
			t.Errorf("the token request named no scope: %s", req.URL)
		}
		_, _ = fmt.Fprint(w, `{"token": "issued"}`)
	})
	mux.HandleFunc("/v2/bermos/charts/kitchen/tags/list", func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer issued" {
			w.Header().Set("Www-Authenticate", fmt.Sprintf(
				`Bearer realm="%s/token",service="registry",scope="repository:bermos/charts/kitchen:pull"`,
				server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		listings++
		_, _ = fmt.Fprintf(w, `{"name": "bermos/charts/kitchen", "tags": [%s]}`,
			`"`+strings.Join(tags, `", "`)+`"`)
	})

	client, err := New("oci://" + strings.TrimPrefix(server.URL, "https://") + "/bermos/charts/kitchen")
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()
	return client, &listings
}

func TestListingPublishedVersions(t *testing.T) {
	client, _ := newRegistry(t, []string{"0.1.4", "0.2.0", "latest", "0.10.0", "0.3.0-rc.1"})

	listing, err := client.Versions(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	got := make([]string, 0, len(listing.Versions))
	for _, version := range listing.Versions {
		got = append(got, version.String())
	}
	want := "0.1.4 0.2.0 0.3.0-rc.1 0.10.0"
	if strings.Join(got, " ") != want {
		t.Fatalf("want %q sorted as versions rather than as strings, got %q", want, strings.Join(got, " "))
	}
}

func TestTheNewestStableVersionWins(t *testing.T) {
	client, _ := newRegistry(t, []string{"0.2.0", "0.3.0-rc.1"})

	listing, err := client.Versions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	latest, ok := Latest(listing.Versions)
	if !ok || latest.String() != "0.2.0" {
		t.Fatalf("a release candidate is not what a latest-version button should offer, got %v (%t)", latest, ok)
	}
}

func TestTheListingIsCached(t *testing.T) {
	client, listings := newRegistry(t, []string{"0.2.0"})

	for range 3 {
		if _, err := client.Versions(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if *listings != 1 {
		t.Fatalf("want the registry asked once, it was asked %d times", *listings)
	}
}

func TestARefreshAsksAgainWithoutWaitingForTheCache(t *testing.T) {
	client, listings := newRegistry(t, []string{"0.2.0"})

	if _, err := client.Versions(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Backdated past the floor but nowhere near the hour the cache would
	// otherwise hold the answer for: that gap is what this exists to close.
	client.refreshed = time.Now().Add(-time.Minute)

	listing, err := client.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if *listings != 2 {
		t.Fatalf("want the registry asked again, it was asked %d times in total", *listings)
	}
	if time.Since(listing.CheckedAt) > time.Second {
		t.Fatalf("want the answer stamped with the read that took it, got %s", listing.CheckedAt)
	}
}

func TestRefreshesAreFloored(t *testing.T) {
	client, listings := newRegistry(t, []string{"0.2.0"})

	for range 5 {
		if _, err := client.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// Held down, the control must not turn into a way to have the registry
	// rate-limit this installation: a refused listing is cached as an error
	// for five minutes, which is worse than the staleness it was skipping.
	if *listings != 1 {
		t.Fatalf("want the registry asked once inside the floor, it was asked %d times", *listings)
	}
}

func TestAFailedListingIsAnErrorRatherThanAnEmptyAnswer(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such repository", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	client, err := New("oci://" + strings.TrimPrefix(server.URL, "https://") + "/bermos/charts/kitchen")
	if err != nil {
		t.Fatal(err)
	}
	client.http = server.Client()

	listing, err := client.Versions(context.Background())
	if err == nil {
		t.Fatalf("want the failure reported, got %v", listing.Versions)
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("want the registry's answer in the message, got %q", err)
	}
}

func TestOnlyOCIReferencesAreUnderstood(t *testing.T) {
	if _, err := New("https://charts.example.com/kitchen"); err == nil {
		t.Fatal("a classic chart repository should be refused rather than half-supported")
	}
	if _, err := New("oci://ghcr.io"); err == nil {
		t.Fatal("a registry with no repository names no chart")
	}
}
