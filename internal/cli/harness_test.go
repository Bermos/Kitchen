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

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// The harness: a fake platform, a temporary home, and a way of running a
// command line against both.
//
// Every test in this package runs the CLI the way somebody would — through
// Execute, with an argument list — rather than calling the function behind a
// command. That is deliberate: the flags, the JSON envelope and the exit codes
// are the contract this CLI publishes in `kitchen schema`, and a test that
// called past them would not be testing the thing that is promised.

// platform is a stand-in for a Kitchen installation: the handful of endpoints
// the CLI calls, answering whatever a test set on it.
type platform struct {
	mutex sync.Mutex

	server *httptest.Server
	issuer *httptest.Server

	// requests records every call, so a test can assert what was sent as well
	// as what came back.
	requests []recorded

	// The answers. A nil entry answers 404.
	me           *account
	projects     []project
	project      *project
	builds       []build
	releases     []release
	environments []environment
	logLines     []logLine

	// buildPhases is walked one entry per GET of a build, so a test can play
	// a build through Queued, Running and Succeeded.
	buildPhases []string
	phaseReads  int

	// refuse answers every API call with this status and message when set.
	refuseStatus  int
	refuseMessage string
}

// recorded is one request the fake platform saw.
type recorded struct {
	Method string
	Path   string
	Query  string
	Body   string
}

func newPlatform(t *testing.T) *platform {
	t.Helper()
	p := &platform{
		me: &account{Subject: "user_01H8X", Email: "anna@example.com", PlatformRole: "member"},
	}

	p.issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/token" {
			http.NotFound(w, req)
			return
		}
		if req.Header.Get("x-api-key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(errorBody{Error: "no key"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "exchanged-token"})
	}))
	t.Cleanup(p.issuer.Close)

	p.server = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	return p
}

func (p *platform) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	p.mutex.Lock()
	p.requests = append(p.requests, recorded{
		Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Body: string(body),
	})
	p.mutex.Unlock()

	if req.URL.Path == configPath {
		writeAnswer(w, http.StatusOK, platformConfig{
			Issuer: p.issuer.URL, ClientID: "kitchen-ui", APIURL: p.server.URL, Version: "test",
		})
		return
	}
	if p.refuseStatus != 0 {
		writeAnswer(w, p.refuseStatus, errorBody{Error: p.refuseMessage})
		return
	}

	path := strings.TrimPrefix(req.URL.Path, apiPrefix)
	switch {
	case path == "/me":
		writeAnswer(w, http.StatusOK, p.me)
	case path == "/projects" && req.Method == http.MethodGet:
		writeAnswer(w, http.StatusOK, list[project]{Items: p.projects})
	case strings.HasSuffix(path, "/builds") && req.Method == http.MethodPost:
		p.startBuild(w, body)
	case strings.HasSuffix(path, "/builds"):
		writeAnswer(w, http.StatusOK, list[build]{Items: p.builds})
	case strings.HasSuffix(path, "/releases"):
		writeAnswer(w, http.StatusOK, list[release]{Items: p.releases})
	case strings.HasSuffix(path, "/environments"):
		writeAnswer(w, http.StatusOK, list[environment]{Items: p.environments})
	case strings.HasSuffix(path, "/env") && req.Method == http.MethodPatch:
		p.patchEnv(w, body)
	case strings.HasPrefix(path, "/projects/"):
		p.answerProject(w)
	case strings.HasSuffix(path, "/logs"):
		p.answerLogs(w, req)
	case strings.HasPrefix(path, "/builds/"):
		p.answerBuild(w)
	case strings.HasPrefix(path, "/environments/") && req.Method == http.MethodPatch:
		p.moveEnvironment(w, body)
	case strings.HasPrefix(path, "/environments/"):
		p.answerEnvironment(w, strings.TrimPrefix(path, "/environments/"))
	default:
		writeAnswer(w, http.StatusNotFound, errorBody{Error: "no such endpoint: " + path})
	}
}

func (p *platform) startBuild(w http.ResponseWriter, body []byte) {
	asked := map[string]string{}
	_ = json.Unmarshal(body, &asked)
	started := build{
		Name: "shop-bld-abc123def456-xk2p9", Project: "shop", Phase: "Queued",
		Git: revision{SHA: asked["sha"], Branch: asked["branch"]},
	}
	p.mutex.Lock()
	p.builds = append([]build{started}, p.builds...)
	p.mutex.Unlock()
	writeAnswer(w, http.StatusCreated, started)
}

func (p *platform) answerBuild(w http.ResponseWriter) {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if len(p.builds) == 0 {
		writeAnswer(w, http.StatusNotFound, errorBody{Error: "no such build"})
		return
	}
	current := p.builds[0]
	if p.phaseReads < len(p.buildPhases) {
		current.Phase = p.buildPhases[p.phaseReads]
		p.phaseReads++
		p.builds[0] = current
	}
	writeAnswer(w, http.StatusOK, current)
}

func (p *platform) answerProject(w http.ResponseWriter) {
	if p.project == nil {
		writeAnswer(w, http.StatusNotFound, errorBody{Error: "no such project"})
		return
	}
	writeAnswer(w, http.StatusOK, p.project)
}

func (p *platform) answerEnvironment(w http.ResponseWriter, name string) {
	name = strings.TrimSuffix(name, "/logs")
	for _, e := range p.environments {
		if e.Name == name {
			writeAnswer(w, http.StatusOK, e)
			return
		}
	}
	writeAnswer(w, http.StatusNotFound, errorBody{Error: "no such environment"})
}

func (p *platform) moveEnvironment(w http.ResponseWriter, body []byte) {
	asked := map[string]string{}
	_ = json.Unmarshal(body, &asked)
	if len(p.environments) == 0 {
		writeAnswer(w, http.StatusNotFound, errorBody{Error: "no such environment"})
		return
	}
	p.environments[0].Release = asked["release"]
	writeAnswer(w, http.StatusOK, p.environments[0])
}

func (p *platform) patchEnv(w http.ResponseWriter, body []byte) {
	asked := struct {
		Env []envVarWrite `json:"env"`
	}{}
	_ = json.Unmarshal(body, &asked)

	updated := project{Name: "shop", Role: "developer"}
	for _, variable := range asked.Env {
		updated.Env = append(updated.Env, envVar{
			Name:       variable.Name,
			Set:        variable.Value == nil || *variable.Value != "",
			PreviewSet: variable.PreviewValue != nil && *variable.PreviewValue != "",
			FromSecret: variable.FromSecret,
			FromClaim:  variable.FromClaim,
		})
	}
	writeAnswer(w, http.StatusOK, updated)
}

// answerLogs answers the bounded page, or the stream when the caller asked for
// one — the same negotiation the real API does on Accept.
func (p *platform) answerLogs(w http.ResponseWriter, req *http.Request) {
	if !strings.Contains(req.Header.Get("accept"), eventStream) {
		writeAnswer(w, http.StatusOK, list[logLine]{Items: p.logLines})
		return
	}

	w.Header().Set("Content-Type", eventStream)
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	_, _ = io.WriteString(w, ": keepalive\n\n")
	for _, line := range p.logLines {
		payload, _ := json.Marshal(line)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	}
	if flusher != nil {
		flusher.Flush()
	}
	// The stream stays open the way the real one does, until the client goes
	// away — which is what the follow's own cancellation has to cope with.
	<-req.Context().Done()
}

func writeAnswer(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

// sent reports the requests the platform saw, for a test that cares what was
// asked rather than what came back.
func (p *platform) sent(method, suffix string) []recorded {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	var found []recorded
	for _, request := range p.requests {
		if request.Method == method && strings.HasSuffix(request.Path, suffix) {
			found = append(found, request)
		}
	}
	return found
}

// harness runs command lines against a fake platform with a temporary home.
type harness struct {
	t        *testing.T
	platform *platform
	home     string
	work     string
	env      map[string]string
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	stdin    io.Reader
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	p := newPlatform(t)
	return &harness{
		t:        t,
		platform: p,
		home:     t.TempDir(),
		work:     t.TempDir(),
		env: map[string]string{
			"KITCHEN_API":   p.server.URL,
			"KITCHEN_TOKEN": "a-token",
		},
		stdin: strings.NewReader(""),
	}
}

// run executes one command line and answers its exit status. stdout and
// stderr are reset first, so a test that runs two commands reads the second
// one's output rather than both.
func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()

	runtime := &Runtime{
		Stdin:      h.stdin,
		Stdout:     &h.stdout,
		Stderr:     &h.stderr,
		WorkingDir: h.work,
		Getenv: func(name string) string {
			if name == "KITCHEN_CONFIG_HOME" {
				return h.home
			}
			return h.env[name]
		},
	}
	return Execute(context.Background(), runtime, args)
}

// answer decodes stdout as one JSON document.
func (h *harness) answer(into any) {
	h.t.Helper()
	if err := json.Unmarshal(h.stdout.Bytes(), into); err != nil {
		h.t.Fatalf("stdout is not one JSON document: %v\n%s", err, h.stdout.String())
	}
}

// lines decodes stdout as NDJSON, one object per line.
func (h *harness) lines() []map[string]any {
	h.t.Helper()
	out := []map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(h.stdout.String()), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		object := map[string]any{}
		if err := json.Unmarshal([]byte(line), &object); err != nil {
			h.t.Fatalf("stdout line is not JSON: %v\n%s", err, line)
		}
		out = append(out, object)
	}
	return out
}

// failure decodes the error envelope every failing command answers with.
func (h *harness) failure() *failure {
	h.t.Helper()
	envelope := struct {
		Error *failure `json:"error"`
	}{}
	h.answer(&envelope)
	if envelope.Error == nil {
		h.t.Fatalf("no error envelope on stdout:\n%s", h.stdout.String())
	}
	return envelope.Error
}
