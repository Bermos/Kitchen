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

// Package databasetest fakes the slice of the Neon API the database
// provisioner speaks, over httptest. It exists as a package rather than a
// _test.go file because the database package's unit tests and the
// ResourceClaim reconciler's envtests drive the same fake.
package databasetest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// NeonRegion is where the fake claims to place every project — what
// Provision reports as the instance's Region.
const NeonRegion = "aws-eu-central-1"

// NeonRetention is the history retention the fake reports for every project,
// which is what the recovery window is computed from: seven days, a paid
// plan's answer.
const NeonRetention = 7 * 24 * 60 * 60

// NeonProject is one fake Neon project with its branches, keyed by branch ID.
type NeonProject struct {
	ID       string
	Name     string
	Branches map[string]*NeonBranch
	// retentionSet distinguishes "no retention" from "not configured", so
	// SetRetention(0) reports zero rather than the default.
	retentionSet bool
	// Retention is the project's history retention in seconds, as
	// `history_retention_seconds`. Zero means the fake reports
	// NeonRetention; SetRetention makes a project keep none, which is the
	// window a claim offers no recovery over.
	Retention int64
}

// NeonBranch is one fake branch. Its role password is derived from the ID
// (see Password), so tests can assert bindings without bookkeeping.
type NeonBranch struct {
	ID      string
	Name    string
	Default bool
	// ParentTimestamp is what the branch was created at, as the request
	// spelled it — empty for an ordinary branch of the parent's present.
	// It is the one field a point-in-time recovery adds, so it is the one
	// the tests assert reached the API.
	ParentTimestamp string
}

// Password is the fake's deterministic role password for a branch.
func (b *NeonBranch) Password() string { return "pw-" + b.ID }

// Host is the fake's deterministic endpoint host for a branch.
func (b *NeonBranch) Host() string { return b.ID + ".neon.example.com" }

// NeonServer is an in-memory Neon API. Every mutation the provisioner makes
// is observable through it, which is what the reconciler tests assert on.
type NeonServer struct {
	mu       sync.Mutex
	server   *httptest.Server
	projects map[string]*NeonProject
	nextID   int
	failWith string
	lastAuth string
}

// NewNeonServer starts the fake; Close it when done.
func NewNeonServer() *NeonServer {
	s := &NeonServer{projects: map[string]*NeonProject{}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /projects", s.listProjects)
	mux.HandleFunc("GET /projects/{project}", s.getProject)
	mux.HandleFunc("POST /projects", s.createProject)
	mux.HandleFunc("DELETE /projects/{project}", s.deleteProject)
	mux.HandleFunc("GET /projects/{project}/branches", s.listBranches)
	mux.HandleFunc("POST /projects/{project}/branches", s.createBranch)
	mux.HandleFunc("DELETE /projects/{project}/branches/{branch}", s.deleteBranch)
	mux.HandleFunc("GET /projects/{project}/branches/{branch}/databases", s.branchDatabases)
	mux.HandleFunc("GET /projects/{project}/branches/{branch}/endpoints", s.branchEndpoints)
	mux.HandleFunc("GET /projects/{project}/branches/{branch}/roles/{role}/reveal_password", s.revealPassword)

	s.server = httptest.NewServer(s.gate(mux))
	return s
}

// URL is the fake API's base URL, in place of https://console.neon.tech/api/v2.
func (s *NeonServer) URL() string { return s.server.URL }

// Close shuts the fake down.
func (s *NeonServer) Close() { s.server.Close() }

// FailWith makes every following request answer 500 with the message, until
// called with "".
func (s *NeonServer) FailWith(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failWith = message
}

// LastAuthorization is the Authorization header of the most recent request.
func (s *NeonServer) LastAuthorization() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAuth
}

// AddProject seeds a project the provisioner did not create — a database
// left behind by a claim that has since been deleted, which is what a
// retained resource looks like from the provider's side.
func (s *NeonServer) AddProject(name string) *NeonProject {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	project := &NeonProject{
		ID:       fmt.Sprintf("proj-%d", s.nextID),
		Name:     name,
		Branches: map[string]*NeonBranch{},
	}
	s.nextID++
	main := &NeonBranch{ID: fmt.Sprintf("br-%d", s.nextID), Name: "main", Default: true}
	project.Branches[main.ID] = main
	s.projects[project.ID] = project
	return project
}

// SetRetention sets a project's history retention in seconds. Zero seconds
// is a project with no history at all, which is what a claim with an empty
// recovery window is reading.
func (s *NeonServer) SetRetention(projectID string, seconds int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if project, ok := s.projects[projectID]; ok {
		project.Retention = seconds
		project.retentionSet = true
	}
}

// ProjectNamed returns a snapshot of the project with that name, or nil.
func (s *NeonServer) ProjectNamed(name string) *NeonProject {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, project := range s.projects {
		if project.Name != name {
			continue
		}
		snapshot := &NeonProject{
			ID: project.ID, Name: project.Name,
			Retention: project.retention(), retentionSet: true,
			Branches: map[string]*NeonBranch{},
		}
		for id, branch := range project.Branches {
			copied := *branch
			snapshot.Branches[id] = &copied
		}
		return snapshot
	}
	return nil
}

// BranchNamed returns a snapshot of the branch with that name in the named
// project, or nil.
func (s *NeonServer) BranchNamed(projectName, branchName string) *NeonBranch {
	project := s.ProjectNamed(projectName)
	if project == nil {
		return nil
	}
	for _, branch := range project.Branches {
		if branch.Name == branchName {
			return branch
		}
	}
	return nil
}

// gate applies the failure switch and records the auth header before routing.
func (s *NeonServer) gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		s.lastAuth = req.Header.Get("Authorization")
		failWith := s.failWith
		s.mu.Unlock()
		if failWith != "" {
			http.Error(w, failWith, http.StatusInternalServerError)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func (s *NeonServer) listProjects(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	projects := []map[string]string{}
	for _, project := range s.projects {
		projects = append(projects, map[string]string{
			"id": project.ID, "name": project.Name, "region_id": NeonRegion,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"projects": projects})
}

// getProject is what the recovery window is read from: the project with its
// history retention.
func (s *NeonServer) getProject(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, ok := s.projects[req.PathValue("project")]
	if !ok {
		http.NotFound(w, req)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": map[string]any{
		"id": project.ID, "name": project.Name, "region_id": NeonRegion,
		"history_retention_seconds": project.retention(),
	}})
}

// retention is what the fake reports for a project: what SetRetention was
// given, or the default.
func (p *NeonProject) retention() int64 {
	if p.retentionSet {
		return p.Retention
	}
	return NeonRetention
}

func (s *NeonServer) createProject(w http.ResponseWriter, req *http.Request) {
	body := struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
	}{}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	project := &NeonProject{
		ID:       fmt.Sprintf("proj-%d", s.nextID),
		Name:     body.Project.Name,
		Branches: map[string]*NeonBranch{},
	}
	s.nextID++
	main := &NeonBranch{ID: fmt.Sprintf("br-%d", s.nextID), Name: "main", Default: true}
	project.Branches[main.ID] = main
	s.projects[project.ID] = project

	writeJSON(w, http.StatusCreated, map[string]any{
		"project": map[string]string{"id": project.ID, "name": project.Name, "region_id": NeonRegion},
		"branch":  map[string]any{"id": main.ID, "name": main.Name, "default": true},
	})
}

func (s *NeonServer) deleteProject(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := req.PathValue("project")
	if _, ok := s.projects[id]; !ok {
		http.NotFound(w, req)
		return
	}
	delete(s.projects, id)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *NeonServer) listBranches(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, ok := s.projects[req.PathValue("project")]
	if !ok {
		http.NotFound(w, req)
		return
	}
	branches := []map[string]any{}
	for _, branch := range project.Branches {
		branches = append(branches, map[string]any{
			"id": branch.ID, "name": branch.Name, "default": branch.Default,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": branches})
}

func (s *NeonServer) createBranch(w http.ResponseWriter, req *http.Request) {
	body := struct {
		Branch struct {
			Name            string `json:"name"`
			ParentTimestamp string `json:"parent_timestamp"`
		} `json:"branch"`
	}{}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	project, ok := s.projects[req.PathValue("project")]
	if !ok {
		http.NotFound(w, req)
		return
	}
	s.nextID++
	branch := &NeonBranch{
		ID: fmt.Sprintf("br-%d", s.nextID), Name: body.Branch.Name,
		ParentTimestamp: body.Branch.ParentTimestamp,
	}
	project.Branches[branch.ID] = branch
	writeJSON(w, http.StatusCreated, map[string]any{
		"branch": map[string]any{"id": branch.ID, "name": branch.Name, "default": false},
	})
}

func (s *NeonServer) deleteBranch(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, ok := s.projects[req.PathValue("project")]
	if !ok {
		http.NotFound(w, req)
		return
	}
	id := req.PathValue("branch")
	if _, ok := project.Branches[id]; !ok {
		http.NotFound(w, req)
		return
	}
	delete(project.Branches, id)
	writeJSON(w, http.StatusOK, map[string]any{})
}

func (s *NeonServer) branchDatabases(w http.ResponseWriter, req *http.Request) {
	if s.branch(w, req) == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"databases": []map[string]string{{"name": "neondb", "owner_name": "neondb_owner"}},
	})
}

func (s *NeonServer) branchEndpoints(w http.ResponseWriter, req *http.Request) {
	branch := s.branch(w, req)
	if branch == nil {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"endpoints": []map[string]string{{"host": branch.Host()}},
	})
}

func (s *NeonServer) revealPassword(w http.ResponseWriter, req *http.Request) {
	branch := s.branch(w, req)
	if branch == nil {
		return
	}
	if !strings.EqualFold(req.PathValue("role"), "neondb_owner") {
		http.NotFound(w, req)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"password": branch.Password()})
}

// branch resolves the request's project and branch, answering 404 itself when
// either is missing.
func (s *NeonServer) branch(w http.ResponseWriter, req *http.Request) *NeonBranch {
	s.mu.Lock()
	defer s.mu.Unlock()
	project, ok := s.projects[req.PathValue("project")]
	if !ok {
		http.NotFound(w, req)
		return nil
	}
	branch, ok := project.Branches[req.PathValue("branch")]
	if !ok {
		http.NotFound(w, req)
		return nil
	}
	return branch
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
