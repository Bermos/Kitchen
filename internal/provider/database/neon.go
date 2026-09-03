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

package database

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// DefaultNeonAPIURL is Neon's public API. Overridable through the
// Connection config's "apiUrl" field, which is what tests point at httptest.
const DefaultNeonAPIURL = "https://console.neon.tech/api/v2"

// neonPort is the port every Neon endpoint serves Postgres on; the API hands
// out hosts, not ports.
const neonPort = "5432"

// Neon implements Provisioner against the Neon HTTP API: one Neon project
// per instance, Neon's copy-on-write branches as branches.
type Neon struct {
	APIURL string
	Token  string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

type neonProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// RegionID is where Neon placed the project — the actual placement, which
	// is what the claim's residency records.
	RegionID string `json:"region_id"`
}

type neonBranch struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Default and Primary are the two spellings Neon has used for the branch
	// a project starts with; either marks it.
	Default bool `json:"default"`
	Primary bool `json:"primary"`
}

// Provision creates the Neon project of the given name, or finds it again
// when a previous reconcile created it but its ID was lost before it could be
// recorded. Either way the binding is read back through the same describe
// calls, so the two paths cannot drift.
//
// The instance is declared production: a claim's Neon project IS the
// database the application runs against, not a copy of anything. The region
// is Neon's own answer to where it put the project.
func (n *Neon) Provision(ctx context.Context, res naming.Resource) (Instance, error) {
	// Neon has nowhere to record a project — its API takes a name and no
	// tags — so here the name is the whole of the record, which is exactly
	// why it carries the project.
	name, err := naming.Resolve(ctx, res, naming.Provider{Kind: "Neon project", Lookup: n.owner})
	if err != nil {
		return Instance{}, err
	}

	existing, err := n.findProject(ctx, name)
	if err != nil {
		return Instance{}, err
	}
	if existing != nil {
		branch, err := n.defaultBranch(ctx, existing.ID)
		if err != nil {
			return Instance{}, err
		}
		binding, err := n.branchBinding(ctx, existing.ID, branch.ID)
		if err != nil {
			return Instance{}, err
		}
		return Instance{
			ID: existing.ID, Name: name, Binding: binding,
			Provenance: ProvenanceProduction, Region: existing.RegionID,
		}, nil
	}

	created := struct {
		Project neonProject `json:"project"`
		Branch  neonBranch  `json:"branch"`
	}{}
	body := map[string]any{"project": map[string]any{"name": name}}
	if err := n.do(ctx, http.MethodPost, "/projects", body, &created); err != nil {
		return Instance{}, err
	}
	binding, err := n.branchBinding(ctx, created.Project.ID, created.Branch.ID)
	if err != nil {
		return Instance{}, err
	}
	return Instance{
		ID: created.Project.ID, Name: name, Binding: binding,
		Provenance: ProvenanceProduction, Region: created.Project.RegionID,
	}, nil
}

// owner answers naming.Lookup. A Neon project carries no label, so it is
// found or it is not: the project a name belongs to is the name itself.
func (n *Neon) owner(ctx context.Context, name string) (naming.Owner, error) {
	found, err := n.findProject(ctx, name)
	if err != nil {
		return naming.Owner{}, err
	}
	return naming.Owner{Found: found != nil}, nil
}

// Deprovision deletes the Neon project with its data; already gone is fine.
func (n *Neon) Deprovision(ctx context.Context, instanceID string) error {
	err := n.do(ctx, http.MethodDelete, "/projects/"+instanceID, nil, nil)
	if err != nil && !isNeonNotFound(err) {
		return err
	}
	return nil
}

// CreateBranch creates a copy-on-write branch with its own read-write
// endpoint, or finds the branch of that name a previous reconcile created.
//
// The branch is declared production, and that declaration is the honest one:
// a Neon branch is a copy-on-write view of the parent's data at branch time —
// every row of the production database, under a preview's address. Cheap to
// make does not make it not production-derived; a provisioner that masks or
// synthesizes on the way to a branch is where masked/synthetic declarations
// come from, and Neon does neither.
func (n *Neon) CreateBranch(ctx context.Context, instanceID, name string) (Branch, error) {
	branches, err := n.listBranches(ctx, instanceID)
	if err != nil {
		return Branch{}, err
	}
	for _, existing := range branches {
		if existing.Name != name {
			continue
		}
		binding, err := n.branchBinding(ctx, instanceID, existing.ID)
		if err != nil {
			return Branch{}, err
		}
		return Branch{ID: existing.ID, Binding: binding, Provenance: ProvenanceProduction}, nil
	}

	created := struct {
		Branch neonBranch `json:"branch"`
	}{}
	body := map[string]any{
		"branch":    map[string]any{"name": name},
		"endpoints": []map[string]any{{"type": "read_write"}},
	}
	if err := n.do(ctx, http.MethodPost, "/projects/"+instanceID+"/branches", body, &created); err != nil {
		return Branch{}, err
	}
	binding, err := n.branchBinding(ctx, instanceID, created.Branch.ID)
	if err != nil {
		return Branch{}, err
	}
	return Branch{ID: created.Branch.ID, Binding: binding, Provenance: ProvenanceProduction}, nil
}

// DeleteBranch removes a branch with its data; already gone is fine.
func (n *Neon) DeleteBranch(ctx context.Context, instanceID, branchID string) error {
	err := n.do(ctx, http.MethodDelete, "/projects/"+instanceID+"/branches/"+branchID, nil, nil)
	if err != nil && !isNeonNotFound(err) {
		return err
	}
	return nil
}

func (n *Neon) findProject(ctx context.Context, name string) (*neonProject, error) {
	out := struct {
		Projects []neonProject `json:"projects"`
	}{}
	// 400 is the API's page ceiling. An installation with more Neon projects
	// than that under one account would need pagination here.
	if err := n.do(ctx, http.MethodGet, "/projects?limit=400", nil, &out); err != nil {
		return nil, err
	}
	for i := range out.Projects {
		if out.Projects[i].Name == name {
			return &out.Projects[i], nil
		}
	}
	return nil, nil
}

func (n *Neon) listBranches(ctx context.Context, projectID string) ([]neonBranch, error) {
	out := struct {
		Branches []neonBranch `json:"branches"`
	}{}
	if err := n.do(ctx, http.MethodGet, "/projects/"+projectID+"/branches", nil, &out); err != nil {
		return nil, err
	}
	return out.Branches, nil
}

func (n *Neon) defaultBranch(ctx context.Context, projectID string) (neonBranch, error) {
	branches, err := n.listBranches(ctx, projectID)
	if err != nil {
		return neonBranch{}, err
	}
	for _, branch := range branches {
		if branch.Default || branch.Primary {
			return branch, nil
		}
	}
	return neonBranch{}, fmt.Errorf("neon project %s has no default branch", projectID)
}

// branchBinding assembles the binding for one branch: its first database and
// that database's owner role, the branch endpoint's host, and the owner's
// password revealed through the API — creation responses carry the password
// too, but only once, and reading everything back the same way keeps
// provisioning restartable.
func (n *Neon) branchBinding(ctx context.Context, projectID, branchID string) (Binding, error) {
	prefix := "/projects/" + projectID + "/branches/" + branchID

	databases := struct {
		Databases []struct {
			Name  string `json:"name"`
			Owner string `json:"owner_name"`
		} `json:"databases"`
	}{}
	if err := n.do(ctx, http.MethodGet, prefix+"/databases", nil, &databases); err != nil {
		return Binding{}, err
	}
	if len(databases.Databases) == 0 {
		return Binding{}, fmt.Errorf("neon branch %s has no database", branchID)
	}
	name, owner := databases.Databases[0].Name, databases.Databases[0].Owner

	endpoints := struct {
		Endpoints []struct {
			Host string `json:"host"`
		} `json:"endpoints"`
	}{}
	if err := n.do(ctx, http.MethodGet, prefix+"/endpoints", nil, &endpoints); err != nil {
		return Binding{}, err
	}
	if len(endpoints.Endpoints) == 0 {
		return Binding{}, fmt.Errorf("neon branch %s has no endpoint", branchID)
	}
	host := endpoints.Endpoints[0].Host

	password := struct {
		Password string `json:"password"`
	}{}
	if err := n.do(ctx, http.MethodGet, prefix+"/roles/"+url.PathEscape(owner)+"/reveal_password", nil, &password); err != nil {
		return Binding{}, err
	}

	connection := url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword(owner, password.Password),
		Host:     host,
		Path:     "/" + name,
		RawQuery: "sslmode=require",
	}
	return Binding{
		URL:      connection.String(),
		Host:     host,
		Port:     neonPort,
		User:     owner,
		Password: password.Password,
		Database: name,
	}, nil
}

type neonError struct {
	status int
	body   string
}

// Error carries the API's own diagnostic; the request's credential is a
// header and never part of it.
func (e *neonError) Error() string {
	return fmt.Sprintf("neon API returned %d: %s", e.status, e.body)
}

func isNeonNotFound(err error) bool {
	neonErr, ok := err.(*neonError)
	return ok && neonErr.status == http.StatusNotFound
}

func (n *Neon) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, n.APIURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	httpClient := n.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &neonError{status: resp.StatusCode, body: string(snippet)}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
