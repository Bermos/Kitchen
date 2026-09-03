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
	"crypto/sha256"
	"encoding/hex"
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

	// secrets is what /projects/{name}/secrets answers, and what a write to
	// one appends to. Values are never in it: the fake answers the shape the
	// API answers, which carries none.
	secrets []projectSecret

	// configDiff is what /releases/{name}/config-diff answers with: the
	// comparison `kitchen rollback` prints before it asks. Nil answers 404,
	// which is a platform too old to serve it.
	configDiff *configDiff

	// buildPhases is walked one entry per GET of a build, so a test can play
	// a build through Queued, Running and Succeeded.
	buildPhases []string
	phaseReads  int

	// environmentSteps is walked one entry per read of a project's
	// environments, so a test can play a deploy through Deploying, a moment
	// of Degraded and Live. The last entry answers every read after it, and
	// an empty list means `environments` answers them all.
	environmentSteps [][]environment
	environmentReads int

	// backup is the archive POST /platform/backup answers with, and
	// backupFilename the name it suggests. A nil archive is refused the way
	// the API refuses a member.
	backup         []byte
	backupFilename string

	// auditPack is the document GET /projects/{name}/audit-pack answers with,
	// auditPackEnvelope the DSSE signature over it, and auditPackHTML the
	// reader's rendering. A nil pack is refused the way the API refuses
	// anybody who is not an operator.
	auditPack         []byte
	auditPackEnvelope []byte
	auditPackHTML     []byte

	// connections is what GET /connections answers, and detected what the
	// preflight makes of a repository. A nil detection is a platform whose
	// preflight is unavailable, which the create command carries on from.
	connections []connection
	detected    *detection

	// The decision register: what /decisions answers, and what a replay says.
	decisions []decision
	// The exception register: what /exceptions answers.
	exceptions []exception
	// The criticality mapping, both directions.
	criticality *criticalityMap
	dependents  *dependents
	replay      *decisionReplay

	// The pipeline: what the project's promotions list answers, and — when
	// set — the promotion an environment move becomes (the 202 an
	// environment with requirements answers).
	promotions      []promotion
	moveToPromotion *promotion

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
	if p.answerProjectSetup(w, req, path, body) {
		return
	}
	switch {
	case strings.HasSuffix(path, "/builds") && req.Method == http.MethodPost:
		p.startBuild(w, body)
	case strings.HasSuffix(path, "/builds"):
		writeAnswer(w, http.StatusOK, list[build]{Items: p.builds})
	case strings.HasSuffix(path, "/releases"):
		writeAnswer(w, http.StatusOK, list[release]{Items: p.releases})
	case strings.HasSuffix(path, "/config-diff"):
		if p.configDiff == nil {
			writeAnswer(w, http.StatusNotFound, errorBody{Error: "no such endpoint: " + path})
			return
		}
		writeAnswer(w, http.StatusOK, p.configDiff)
	case strings.HasSuffix(path, "/environments"):
		writeAnswer(w, http.StatusOK, list[environment]{Items: p.readEnvironments()})
	case strings.HasSuffix(path, "/promotions") && req.Method == http.MethodPost:
		p.createPromotion(w, body)
	case strings.HasSuffix(path, "/promotions"):
		writeAnswer(w, http.StatusOK, list[promotion]{Items: p.promotions})
	case strings.HasPrefix(path, "/promotions/"):
		p.answerPromotion(w, strings.TrimPrefix(path, "/promotions/"))
	case strings.HasSuffix(path, "/audit-pack"):
		p.answerAuditPack(w, req)
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
	case path == "/platform/backup" && req.Method == http.MethodPost:
		p.answerBackup(w)
	case strings.HasPrefix(path, "/decisions") || strings.HasPrefix(path, "/exceptions"):
		p.answerRegisters(w, req, path)
	case path == "/compliance/criticality":
		writeAnswer(w, http.StatusOK, p.criticality)
	case path == "/compliance/dependents":
		writeAnswer(w, http.StatusOK, p.dependents)
	case strings.HasPrefix(path, "/environments/"):
		p.answerEnvironment(w, strings.TrimPrefix(path, "/environments/"))
	default:
		writeAnswer(w, http.StatusNotFound, errorBody{Error: "no such endpoint: " + path})
	}
}

// answerProjectSetup carries the directory the create command walks — the
// caller, the project list, the connections and the repository preflight —
// so that serve stays a dispatch and not a catalogue.
func (p *platform) answerProjectSetup(w http.ResponseWriter, req *http.Request, path string, body []byte) bool {
	switch {
	case path == "/me":
		writeAnswer(w, http.StatusOK, p.me)
	case path == "/projects" && req.Method == http.MethodGet:
		writeAnswer(w, http.StatusOK, list[project]{Items: p.projects})
	case path == "/projects" && req.Method == http.MethodPost:
		p.createProject(w, body)
	case path == "/connections" && req.Method == http.MethodGet:
		writeAnswer(w, http.StatusOK, list[connection]{Items: p.connections})
	case strings.HasSuffix(path, "/detect") && req.Method == http.MethodPost:
		if p.detected == nil {
			writeAnswer(w, http.StatusBadGateway, errorBody{Error: "the provider is unreachable"})
			return true
		}
		writeAnswer(w, http.StatusOK, p.detected)
	default:
		return p.answerSecrets(w, req, path, body)
	}
	return true
}

// answerSecrets is a project's own secrets: the list, the write, and the
// delete. It hangs off answerProjectSetup rather than off serve's switch for
// the reason that switch is split up at all — serve is a dispatch, and every
// pair of cases added to it directly is a branch nobody reading it needs.
func (p *platform) answerSecrets(w http.ResponseWriter, req *http.Request, path string, body []byte) bool {
	switch {
	case strings.HasSuffix(path, "/secrets") && req.Method == http.MethodGet:
		writeAnswer(w, http.StatusOK, list[projectSecret]{Items: p.secrets})
	case strings.Contains(path, "/secrets/") && req.Method == http.MethodPut:
		p.setSecret(w, path, body)
	case strings.Contains(path, "/secrets/") && req.Method == http.MethodDelete:
		p.deleteSecret(w, path)
	default:
		return false
	}
	return true
}

// answerBackup streams a real archive, built by the same package the operator
// builds one with. A fixture of bytes would prove the command wrote a file; a
// real archive proves it wrote one that reads back.
func (p *platform) answerBackup(w http.ResponseWriter) {
	if p.backup == nil {
		writeAnswer(w, http.StatusForbidden, errorBody{
			Error: "exporting the platform's state needs the operator role; you are a member"})
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+p.backupFilename+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(p.backup)
}

// answerAuditPack serves the three renderings the API serves, each with the
// digest header and the filename the API sets — because those two headers are
// what the command decides where to write and what to check against.
func (p *platform) answerAuditPack(w http.ResponseWriter, req *http.Request) {
	if p.auditPack == nil {
		writeAnswer(w, http.StatusForbidden, errorBody{
			Error: "exporting a project's audit pack needs the operator role; you are a member"})
		return
	}
	body, kind, extension := p.auditPack, "application/json", "json"
	switch req.URL.Query().Get("format") {
	case "dsse":
		if p.auditPackEnvelope == nil {
			writeAnswer(w, http.StatusConflict, errorBody{
				Error: "this platform holds no signing key"})
			return
		}
		body, extension = p.auditPackEnvelope, "dsse.json"
	case "html":
		body, kind, extension = p.auditPackHTML, "text/html; charset=utf-8", "html"
	}
	// The digest is always of the *pack*, whichever rendering is being
	// served: it identifies the document, not the response.
	sum := sha256.Sum256(p.auditPack)
	w.Header().Set("Content-Type", kind)
	w.Header().Set("X-Kitchen-Pack-Digest", "sha256:"+hex.EncodeToString(sum[:]))
	w.Header().Set("Content-Disposition",
		`attachment; filename="kitchen-audit-pack-shop-2026-01-01-2026-04-01.`+extension+`"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// createProject answers POST /projects the way the API does: the project as
// the platform now holds it, with the caller as its admin.
func (p *platform) createProject(w http.ResponseWriter, body []byte) {
	asked := newProject{}
	_ = json.Unmarshal(body, &asked)
	branch := asked.ProductionBranch
	if branch == "" {
		branch = "main"
	}
	created := project{
		Name: asked.Name, Repo: asked.Repo, ProductionBranch: branch, Role: "admin",
	}
	p.mutex.Lock()
	p.projects = append(p.projects, created)
	p.mutex.Unlock()
	writeAnswer(w, http.StatusCreated, created)
}

func (p *platform) answerDecision(w http.ResponseWriter, id string) {
	for _, d := range p.decisions {
		if d.ID == id {
			writeAnswer(w, http.StatusOK, d)
			return
		}
	}
	writeAnswer(w, http.StatusNotFound, errorBody{Error: "decisions.kitchen.bermos.dev \"" + id + "\" not found"})
}

// answerRegisters routes the decision and exception registers, split out of
// serve to keep its complexity within the linter's patience.
func (p *platform) answerRegisters(w http.ResponseWriter, req *http.Request, path string) {
	switch {
	case path == "/decisions":
		writeAnswer(w, http.StatusOK, list[decision]{Items: p.decisions})
	case strings.HasSuffix(path, "/replay") && req.Method == http.MethodPost:
		p.answerReplay(w)
	case strings.HasPrefix(path, "/decisions/"):
		p.answerDecision(w, strings.TrimPrefix(path, "/decisions/"))
	case path == "/exceptions":
		writeAnswer(w, http.StatusOK, list[exception]{Items: p.exceptions})
	default:
		p.answerException(w, strings.TrimPrefix(path, "/exceptions/"))
	}
}

func (p *platform) answerException(w http.ResponseWriter, name string) {
	for _, e := range p.exceptions {
		if e.Name == name {
			writeAnswer(w, http.StatusOK, e)
			return
		}
	}
	writeAnswer(w, http.StatusNotFound, errorBody{Error: "exceptions.kitchen.bermos.dev \"" + name + "\" not found"})
}

func (p *platform) answerReplay(w http.ResponseWriter) {
	if p.replay == nil {
		writeAnswer(w, http.StatusNotFound, errorBody{Error: "no such decision"})
		return
	}
	writeAnswer(w, http.StatusCreated, p.replay)
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

// readEnvironments answers one read of a project's environments, walking
// environmentSteps when a test set one.
func (p *platform) readEnvironments() []environment {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	if len(p.environmentSteps) == 0 {
		return p.environments
	}
	at := p.environmentReads
	if at > len(p.environmentSteps)-1 {
		at = len(p.environmentSteps) - 1
	}
	p.environmentReads++
	return p.environmentSteps[at]
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
	// An environment with requirements does not move on the spot: the real
	// API answers 202 with the promotion the move became.
	if p.moveToPromotion != nil {
		accepted := *p.moveToPromotion
		accepted.Release = asked["release"]
		writeAnswer(w, http.StatusAccepted, accepted)
		return
	}
	p.environments[0].Release = asked["release"]
	writeAnswer(w, http.StatusOK, p.environments[0])
}

func (p *platform) createPromotion(w http.ResponseWriter, body []byte) {
	asked := map[string]string{}
	_ = json.Unmarshal(body, &asked)
	if asked["environment"] == "" || asked["release"] == "" {
		writeAnswer(w, http.StatusBadRequest, errorBody{Error: "environment and release are both required"})
		return
	}
	accepted := promotion{
		Name: "shop-promo-1a2b3", Project: "shop",
		Environment: asked["environment"], Release: asked["release"],
		Reason: asked["reason"], RequestedBy: "anna@example.com",
		Trigger: "manual", Phase: "Pending",
	}
	p.mutex.Lock()
	p.promotions = append([]promotion{accepted}, p.promotions...)
	p.mutex.Unlock()
	writeAnswer(w, http.StatusCreated, accepted)
}

func (p *platform) answerPromotion(w http.ResponseWriter, name string) {
	for _, found := range p.promotions {
		if found.Name == name {
			writeAnswer(w, http.StatusOK, found)
			return
		}
	}
	writeAnswer(w, http.StatusNotFound, errorBody{Error: "promotions.kitchen.bermos.dev \"" + name + "\" not found"})
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
// setSecret is the write half of a project's own secrets: it keeps the name,
// answers the reference, and deliberately never stores or echoes the value —
// so a test that found a value in an answer found a bug in the CLI.
func (p *platform) setSecret(w http.ResponseWriter, path string, body []byte) {
	name := path[strings.LastIndex(path, "/secrets/")+len("/secrets/"):]
	sent := struct {
		Value string `json:"value"`
	}{}
	if err := json.Unmarshal(body, &sent); err != nil || sent.Value == "" {
		writeAnswer(w, http.StatusBadRequest, errorBody{Error: "value is required"})
		return
	}
	written := projectSecret{Name: name, Reference: keyRef{Name: "kitchen-project-secrets", Key: name}}
	p.mutex.Lock()
	replaced := false
	for i := range p.secrets {
		if p.secrets[i].Name == name {
			replaced = true
		}
	}
	if !replaced {
		p.secrets = append(p.secrets, written)
	}
	p.mutex.Unlock()
	status := http.StatusCreated
	if replaced {
		status = http.StatusOK
	}
	writeAnswer(w, status, written)
}

func (p *platform) deleteSecret(w http.ResponseWriter, path string) {
	name := path[strings.LastIndex(path, "/secrets/")+len("/secrets/"):]
	p.mutex.Lock()
	kept := p.secrets[:0]
	found := false
	for _, secret := range p.secrets {
		if secret.Name == name {
			found = true
			continue
		}
		kept = append(kept, secret)
	}
	p.secrets = kept
	p.mutex.Unlock()
	if !found {
		writeAnswer(w, http.StatusNotFound, errorBody{Error: "no such secret: " + name})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

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
	// stdinTerminal is whether the command believes somebody is there to
	// answer a prompt. Off by default, which is what makes every other test
	// in this package a non-interactive run.
	stdinTerminal bool
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
		Stdin:         h.stdin,
		Stdout:        &h.stdout,
		Stderr:        &h.stderr,
		WorkingDir:    h.work,
		StdinTerminal: h.stdinTerminal,
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
