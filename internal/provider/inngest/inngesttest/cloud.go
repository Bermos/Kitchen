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

// Package inngesttest fakes the slice of Inngest Cloud's v2 API the inngest
// provisioner speaks, over httptest. It is a package rather than a _test.go
// file because the provider's unit tests and the ResourceClaim reconciler's
// envtests drive the same fake.
//
// It models what the docs say about keys: production and every custom
// environment have keys of their own, and every branch environment shares
// the account's one branch key pair
// (https://www.inngest.com/docs/platform/environments#configuring-branch-environments).
package inngesttest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
)

// The fake's fixed key material, for tests to assert bindings against
// without bookkeeping.
const (
	ProductionSigningKey = "signkey-prod-0123"
	ProductionEventKey   = "evkey-prod-0123"
	BranchSigningKey     = "signkey-branch-4567"
	BranchEventKey       = "evkey-branch-4567"
	// AccountEmail is who GET /account says the API key belongs to.
	AccountEmail = "ops@example.com"
)

// Env is one fake environment.
type Env struct {
	ID       string
	Name     string
	Type     string
	Archived bool
}

// App is one fake app in one environment.
type App struct {
	ID        string
	Method    string
	Functions int
}

type keyPair struct {
	signing, event []map[string]string
}

// CloudServer is an in-memory Inngest Cloud API. Every mutation the
// provisioner makes is observable through it.
type CloudServer struct {
	mu       sync.Mutex
	server   *httptest.Server
	envs     map[string]*Env
	keys     map[string]*keyPair // by environment name; branch envs share "branch"
	apps     map[string]map[string]*App
	nextID   int
	failWith string
	lastAuth string
	lastEnv  string
}

// NewCloudServer starts the fake with a production environment and the
// shared branch keys in place; Close it when done.
func NewCloudServer() *CloudServer {
	s := &CloudServer{
		envs: map[string]*Env{},
		keys: map[string]*keyPair{
			"production": {
				signing: []map[string]string{{"id": "sk-prod", "name": "", "key": ProductionSigningKey}},
				event:   []map[string]string{{"id": "ek-prod", "name": "Default", "key": ProductionEventKey}},
			},
			"branch": {
				signing: []map[string]string{{"id": "sk-branch", "name": "", "key": BranchSigningKey}},
				event:   []map[string]string{{"id": "ek-branch", "name": "Branch", "key": BranchEventKey}},
			},
		},
		apps: map[string]map[string]*App{},
	}
	s.envs["env-production"] = &Env{ID: "env-production", Name: "production", Type: "PRODUCTION"}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /account", s.account)
	mux.HandleFunc("GET /envs", s.listEnvs)
	mux.HandleFunc("POST /envs", s.createEnv)
	mux.HandleFunc("PATCH /envs/{id}", s.patchEnv)
	mux.HandleFunc("GET /keys/signing", s.signingKeys)
	mux.HandleFunc("GET /keys/events", s.eventKeys)
	mux.HandleFunc("GET /apps/{app}", s.app)

	s.server = httptest.NewServer(s.gate(mux))
	return s
}

// URL is the fake API's base URL, in place of https://api.inngest.com/v2.
func (s *CloudServer) URL() string { return s.server.URL }

// Close shuts the fake down.
func (s *CloudServer) Close() { s.server.Close() }

// FailWith makes every following request answer 500 with the message, until
// called with "".
func (s *CloudServer) FailWith(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith = message
}

// LastAuthorization is the Authorization header of the most recent request.
func (s *CloudServer) LastAuthorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuth
}

// LastEnvironment is the X-Inngest-Env header of the most recent request.
func (s *CloudServer) LastEnvironment() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastEnv
}

// AddEnvironment adds a custom (TEST) environment with keys of its own,
// the way one created in the Inngest dashboard has them.
func (s *CloudServer) AddEnvironment(name, signingKey, eventKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := fmt.Sprintf("env-%d", s.nextID)
	s.envs[id] = &Env{ID: id, Name: name, Type: "TEST"}
	s.keys[name] = &keyPair{
		signing: []map[string]string{{"id": "sk-" + id, "name": "", "key": signingKey}},
		event:   []map[string]string{{"id": "ek-" + id, "name": "Default", "key": eventKey}},
	}
}

// RemoveEventKeys leaves an environment with no event key at all — the
// state a freshly created custom environment can be in.
func (s *CloudServer) RemoveEventKeys(environment string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pair, ok := s.keys[environment]; ok {
		pair.event = nil
	}
}

// RegisterApp records an app as a worker's connection would, in the named
// environment.
func (s *CloudServer) RegisterApp(environment, appID, method string, functions int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.apps[environment] == nil {
		s.apps[environment] = map[string]*App{}
	}
	s.apps[environment][appID] = &App{ID: appID, Method: method, Functions: functions}
}

// EnvNamed returns a snapshot of the environment with that name, or nil.
func (s *CloudServer) EnvNamed(name string) *Env {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, env := range s.envs {
		if env.Name == name {
			copied := *env
			return &copied
		}
	}
	return nil
}

// Archive sets an environment's archived flag, as Inngest's auto-archive
// does three days after a branch's last deploy.
func (s *CloudServer) Archive(name string, archived bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, env := range s.envs {
		if env.Name == name {
			env.Archived = archived
		}
	}
}

// gate applies the failure switch and records the headers before routing.
func (s *CloudServer) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		s.lastAuth = req.Header.Get("Authorization")
		s.lastEnv = req.Header.Get("X-Inngest-Env")
		failWith := s.failWith
		s.mu.Unlock()
		if failWith != "" {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"errors": []map[string]string{{"code": "internal_error", "message": failWith}},
			})
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (s *CloudServer) account(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]string{"id": "acct-1", "name": "Example", "email": AccountEmail},
	})
}

func (s *CloudServer) listEnvs(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	envs := []map[string]any{}
	for _, env := range s.envs {
		envs = append(envs, envJSON(env))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": envs,
		"page": map[string]any{"cursor": nil, "hasMore": false, "limit": 250},
	})
}

func (s *CloudServer) createEnv(w http.ResponseWriter, req *http.Request) {
	body := struct {
		Name string `json:"name"`
	}{}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil || body.Name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"errors": []map[string]string{{"code": "missing_field", "message": "Environment name is required"}},
		})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, env := range s.envs {
		if env.Name == body.Name {
			writeJSON(w, http.StatusConflict, map[string]any{
				"errors": []map[string]string{{"code": "conflict", "message": "environment already exists"}},
			})
			return
		}
	}
	s.nextID++
	env := &Env{ID: fmt.Sprintf("env-%d", s.nextID), Name: body.Name, Type: "BRANCH"}
	s.envs[env.ID] = env
	writeJSON(w, http.StatusCreated, map[string]any{"data": envJSON(env)})
}

func (s *CloudServer) patchEnv(w http.ResponseWriter, req *http.Request) {
	body := struct {
		IsArchived *bool `json:"isArchived"`
	}{}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	env, ok := s.envs[req.PathValue("id")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"errors": []map[string]string{{"code": "not_found", "message": "environment not found"}},
		})
		return
	}
	if body.IsArchived != nil {
		env.Archived = *body.IsArchived
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": envJSON(env)})
}

// keysFor resolves the request's environment header to a key pair: the
// named environment's own, the shared branch pair for a branch environment,
// production's without a header, and nothing for an environment that does
// not exist.
func (s *CloudServer) keysFor(req *http.Request) (*keyPair, string, bool) {
	name := req.Header.Get("X-Inngest-Env")
	if name == "" {
		name = "production"
	}
	for _, env := range s.envs {
		if env.Name != name {
			continue
		}
		if env.Type == "BRANCH" {
			return s.keys["branch"], name, true
		}
		return s.keys[name], name, true
	}
	return nil, name, false
}

func (s *CloudServer) signingKeys(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pair, name, ok := s.keysFor(req)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"errors": []map[string]string{{"code": "not_found", "message": "environment not found"}},
		})
		return
	}
	writeKeys(w, pair.signing, name)
}

func (s *CloudServer) eventKeys(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pair, name, ok := s.keysFor(req)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"errors": []map[string]string{{"code": "not_found", "message": "environment not found"}},
		})
		return
	}
	writeKeys(w, pair.event, name)
}

func writeKeys(w http.ResponseWriter, keys []map[string]string, environment string) {
	data := []map[string]any{}
	for _, key := range keys {
		data = append(data, map[string]any{
			"id": key["id"], "name": key["name"], "environment": environment, "key": key["key"],
			"createdAt": "2026-01-01T00:00:00Z",
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": data,
		"page": map[string]any{"cursor": nil, "hasMore": false, "limit": 100},
	})
}

func (s *CloudServer) app(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := req.Header.Get("X-Inngest-Env")
	if name == "" {
		name = "production"
	}
	app, ok := s.apps[name][req.PathValue("app")]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"errors": []map[string]string{{"code": "not_found", "message": "app not found"}},
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data": map[string]any{
			"id": app.ID, "name": app.ID, "method": app.Method, "functionCount": app.Functions,
			"isArchived": false,
			"latestSync": map[string]any{
				"status": "success", "sdkLanguage": "typescript", "sdkVersion": "3.22.0",
			},
		},
	})
}

func envJSON(env *Env) map[string]any {
	return map[string]any{
		"id": env.ID, "name": env.Name, "type": env.Type, "isArchived": env.Archived,
		"createdAt": "2026-01-01T00:00:00Z",
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
