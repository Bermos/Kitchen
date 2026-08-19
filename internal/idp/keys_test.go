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

package idp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// What the key client promises: the operator's credential goes on every call,
// the value comes back exactly once, and the two statuses that mean something
// — taken and missing — arrive as errors a caller can act on rather than as
// status codes it has to know about.

// The project and key these tests are about, and the operator's credential
// that has to reach the issuer on every call.
const (
	testProject    = "shop"
	testKeyName    = "nightly"
	testServiceKey = "service-key"
	testKeySubject = "user_ci"
)

// keyServer stands in for the identity provider's key endpoints.
type keyServer struct {
	*httptest.Server
	keys    []Key
	request struct {
		method  string
		apiKey  string
		project string
		name    string
		body    map[string]string
	}
	status int
}

func newKeyServer(t *testing.T, keys ...Key) *keyServer {
	t.Helper()
	server := &keyServer{keys: keys}
	mux := http.NewServeMux()
	mux.HandleFunc(KeysPath, func(w http.ResponseWriter, r *http.Request) {
		server.request.method = r.Method
		server.request.apiKey = r.Header.Get(serviceKeyHeader)
		server.request.project = r.URL.Query().Get("project")
		server.request.name = r.URL.Query().Get("name")
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &server.request.body)
		}
		if server.status != 0 {
			w.WriteHeader(server.status)
			_, _ = w.Write([]byte(`{"error":"nope"}`))
			return
		}
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(keysResponse{Keys: server.keys})
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(IssuedKey{
				Key: Key{
					Name:    server.request.body["name"],
					Project: server.request.body["project"],
					Subject: testKeySubject,
					Email:   server.request.body["project"] + "." + server.request.body["name"] + "@" + MachineAccountDomain,
					Prefix:  "abc123",
					Created: time.Unix(1, 0).UTC(),
				},
				Secret: "the-key-itself",
			})
		case http.MethodDelete:
			_ = json.NewEncoder(w).Encode(Key{Name: server.request.name, Subject: testKeySubject})
		}
	})
	server.Server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func (s *keyServer) client() *Client {
	return New(Config{Issuer: s.URL, BaseURL: s.URL, ServiceKey: testServiceKey})
}

func TestIssuingAKeyAnswersItsValueAndItsSubject(t *testing.T) {
	server := newKeyServer(t)

	issued, err := server.client().CreateKey(context.Background(), testProject, testKeyName)
	if err != nil {
		t.Fatal(err)
	}
	if issued.Secret != "the-key-itself" {
		t.Fatalf("want the key value, got %q", issued.Secret)
	}
	if issued.Subject != testKeySubject {
		t.Fatalf("want the machine account's subject, got %q", issued.Subject)
	}
	if server.request.apiKey != testServiceKey {
		t.Fatalf("the operator's credential must authenticate the call, got %q", server.request.apiKey)
	}
	if server.request.body["project"] != testProject || server.request.body["name"] != testKeyName {
		t.Fatalf("want the key named in the body, got %+v", server.request.body)
	}
}

func TestATakenKeyNameIsAConflictTheCallerCanRead(t *testing.T) {
	server := newKeyServer(t)
	server.status = http.StatusConflict

	if _, err := server.client().CreateKey(context.Background(), testProject, testKeyName); !errors.Is(err, ErrKeyExists) {
		t.Fatalf("want ErrKeyExists, got %v", err)
	}
}

func TestAnIssuerWithNoKeyEndpointsIsSaidSoRatherThanRetried(t *testing.T) {
	server := newKeyServer(t)
	server.status = http.StatusNotFound

	if _, err := server.client().Keys(context.Background(), testProject); !errors.Is(err, ErrNoKeyDirectory) {
		t.Fatalf("want ErrNoKeyDirectory, got %v", err)
	}
	if _, err := server.client().DeleteKey(context.Background(), testProject, testKeyName); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("want ErrKeyNotFound, got %v", err)
	}
}

func TestRevokingAKeyAddressesItByProjectAndName(t *testing.T) {
	server := newKeyServer(t)

	removed, err := server.client().DeleteKey(context.Background(), testProject, testKeyName)
	if err != nil {
		t.Fatal(err)
	}
	if removed.Subject != testKeySubject {
		t.Fatalf("the caller needs the subject to take the grant off the project, got %+v", removed)
	}
	if server.request.method != http.MethodDelete ||
		server.request.project != testProject || server.request.name != testKeyName {
		t.Fatalf("want a DELETE naming the key, got %+v", server.request)
	}
}

func TestListingKeysAsksAboutOneProject(t *testing.T) {
	server := newKeyServer(t, Key{Name: testKeyName, Project: testProject, Subject: testKeySubject})

	keys, err := server.client().Keys(context.Background(), testProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || keys[0].Name != testKeyName {
		t.Fatalf("want the project's one key, got %+v", keys)
	}
	if server.request.project != testProject {
		t.Fatalf("want the project named, got %q", server.request.project)
	}
}

// The convention that says an account is a machine's, which is a display rule
// and the only thing the operator reads out of the address.
func TestMachineAccountsAreRecognisedByTheirAddress(t *testing.T) {
	for _, c := range []struct {
		email            string
		machine          bool
		project, keyName string
	}{
		{testProject + "." + testKeyName + "@" + MachineAccountDomain, true, testProject, testKeyName},
		{"Shop.Nightly@" + MachineAccountDomain, true, testProject, testKeyName},
		{"anna@example.com", false, "", ""},
		{"", false, "", ""},
		// Under the domain but not written by this platform: nothing here can
		// say which project it belongs to, so it says nothing.
		{testKeyName + "@" + MachineAccountDomain, true, "", ""},
		{"a.b.c@" + MachineAccountDomain, true, "", ""},
	} {
		t.Run(c.email, func(t *testing.T) {
			if got := IsMachineAccount(c.email); got != c.machine {
				t.Fatalf("IsMachineAccount(%q) = %v", c.email, got)
			}
			project, name, ok := MachineIdentity(c.email)
			if ok != (c.project != "") {
				t.Fatalf("MachineIdentity(%q) resolved = %v", c.email, ok)
			}
			if project != c.project || name != c.keyName {
				t.Fatalf("MachineIdentity(%q) = %q, %q", c.email, project, name)
			}
		})
	}
}
