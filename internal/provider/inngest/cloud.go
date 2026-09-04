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

package inngest

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

// DefaultCloudAPIURL is Inngest Cloud's management API, v2 —
// https://api-docs.inngest.com/authentication. Overridable through the
// Connection config's "apiUrl" field, which is what tests point at httptest.
const DefaultCloudAPIURL = "https://api.inngest.com/v2"

// envHeader is how the v2 API is told which environment a request is about;
// without it, the production environment's.
const envHeader = "X-Inngest-Env"

// Cloud implements Provisioner against Inngest Cloud's v2 REST API with an
// API key (https://www.inngest.com/docs/platform/api-keys): the account's
// keys are read per environment, and a preview's environment is created and
// archived through /envs.
type Cloud struct {
	APIURL string
	Token  string
	// HTTPClient defaults to http.DefaultClient.
	HTTPClient *http.Client
}

// cloudEnv is one environment as /envs answers it.
type cloudEnv struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	IsArchived bool   `json:"isArchived"`
}

// cloudKey is one event key or signing key; the two endpoints answer the
// same shape.
type cloudKey struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
	Key         string `json:"key"`
}

// cloudPage is the pagination every list carries.
type cloudPage struct {
	Cursor  string `json:"cursor"`
	HasMore bool   `json:"hasMore"`
}

// Provision reads the binding of the claim's production environment. It
// creates nothing — the app registers itself when its worker first connects,
// and the keys are the environment's own — so the claim's naming.Resource is
// nothing to it: there is no object here for the platform to name.
//
// A mode other than connect is refused, and so is an environment with no
// event key — the API cannot mint one, and the message says where to.
func (c *Cloud) Provision(ctx context.Context, _ naming.Resource, req Requirements) (Instance, error) {
	if req.Mode != "" && req.Mode != ModeConnect {
		return Instance{}, fmt.Errorf("%w: mode %q is not provisioned through Inngest Cloud — only connect is, "+
			"where the worker dials out to Inngest. In serve mode Inngest calls the application over HTTP from "+
			"the internet, which a protected preview answers with a login page; set the mode to connect, or "+
			"claim through a %s connection, whose server is in this cluster and may be served",
			ErrUnsatisfiable, req.Mode, ProviderSelfHosted)
	}
	environment := req.Environment
	if environment == "" {
		environment = DefaultEnvironment
	}
	binding, err := c.binding(ctx, environment, req.App)
	if err != nil {
		return Instance{}, err
	}
	// Production's keys select their own environment; INNGEST_ENV stays
	// empty so that the SDK sends no environment header it does not need.
	binding.Env = ""
	return Instance{
		ID:          req.App,
		Environment: environment,
		Binding:     binding,
		Reason:      "KeysRead",
		Message:     fmt.Sprintf("binding read for app %s in Inngest environment %s", req.App, environment),
	}, nil
}

// CreateBranch finds the branch environment of the given name — unarchiving
// it if Inngest's auto-archive got there first — or creates it, and reads the
// binding that selects it: the account's shared branch keys plus INNGEST_ENV.
func (c *Cloud) CreateBranch(ctx context.Context, _, name string, _ Requirements) (Branch, error) {
	env, err := c.findEnv(ctx, name)
	if err != nil {
		return Branch{}, err
	}
	switch {
	case env == nil:
		created := struct {
			Data cloudEnv `json:"data"`
		}{}
		err := c.do(ctx, http.MethodPost, "/envs", "", map[string]any{"name": name}, &created)
		if err != nil && !isCloudStatus(err, http.StatusConflict) {
			return Branch{}, err
		}
		if err == nil {
			env = &created.Data
			break
		}
		// 409: created between the list and the create. Find it again.
		if env, err = c.findEnv(ctx, name); err != nil {
			return Branch{}, err
		}
		if env == nil {
			return Branch{}, fmt.Errorf("inngest environment %q was refused as already existing, and is not listed", name)
		}
	case env.IsArchived:
		// Inngest archives a branch environment three days after its last
		// deploy; a preview that is still open gets it back.
		if err := c.setArchived(ctx, env.ID, false); err != nil {
			return Branch{}, err
		}
	}
	binding, err := c.binding(ctx, name, "")
	if err != nil {
		return Branch{}, err
	}
	binding.Env = name
	return Branch{ID: env.ID, Binding: binding}, nil
}

// DeleteBranch archives the environment. Archiving is what the API offers
// and it is the right verb: it stops the environment's functions triggering
// and deletes nothing, so a preview reopened later finds its history.
func (c *Cloud) DeleteBranch(ctx context.Context, _, branchID string) error {
	err := c.setArchived(ctx, branchID, true)
	if err != nil && !isCloudStatus(err, http.StatusNotFound) {
		return err
	}
	return nil
}

// App reports on the app of that ID in the environment; an app no worker
// has connected with yet is Found: false rather than an error.
func (c *Cloud) App(ctx context.Context, environment, appID string) (App, error) {
	out := struct {
		Data struct {
			ID            string `json:"id"`
			Method        string `json:"method"`
			FunctionCount int    `json:"functionCount"`
			LatestSync    struct {
				Status      string `json:"status"`
				Error       string `json:"error"`
				SDKLanguage string `json:"sdkLanguage"`
				SDKVersion  string `json:"sdkVersion"`
			} `json:"latestSync"`
		} `json:"data"`
	}{}
	err := c.do(ctx, http.MethodGet, "/apps/"+url.PathEscape(appID), environment, nil, &out)
	if isCloudStatus(err, http.StatusNotFound) {
		return App{}, nil
	}
	if err != nil {
		return App{}, err
	}
	sdk := out.Data.LatestSync.SDKLanguage
	if sdk != "" && out.Data.LatestSync.SDKVersion != "" {
		sdk += " " + out.Data.LatestSync.SDKVersion
	}
	return App{
		Found:      true,
		Method:     out.Data.Method,
		Functions:  out.Data.FunctionCount,
		SyncStatus: out.Data.LatestSync.Status,
		SyncError:  out.Data.LatestSync.Error,
		SDK:        sdk,
	}, nil
}

// binding reads an environment's signing key and event key. The signing key
// is the environment's one; among event keys, one named after the app is
// preferred, then any — an environment usually has one.
func (c *Cloud) binding(ctx context.Context, environment, app string) (Binding, error) {
	signing, err := c.listKeys(ctx, "/keys/signing", environment)
	if err != nil {
		return Binding{}, err
	}
	if len(signing) == 0 {
		return Binding{}, fmt.Errorf("inngest environment %q has no signing key the API key can read: every "+
			"environment has one, so the API key is scoped to another environment (API keys are created and "+
			"scoped at https://app.inngest.com/settings/api-keys)", environment)
	}
	events, err := c.listKeys(ctx, "/keys/events", environment)
	if err != nil {
		return Binding{}, err
	}
	if len(events) == 0 {
		return Binding{}, fmt.Errorf("%w: inngest environment %q has no event key, and the Inngest API cannot "+
			"create one — create it in the Inngest dashboard (Manage → Event keys, "+
			"https://www.inngest.com/docs/events/creating-an-event-key) and the claim binds on its next "+
			"reconcile", ErrUnsatisfiable, environment)
	}
	event := events[0]
	for _, key := range events {
		if app != "" && key.Name == app {
			event = key
		}
	}
	return Binding{EventKey: event.Key, SigningKey: signing[0].Key}, nil
}

// listKeys walks one of the two key lists for an environment.
func (c *Cloud) listKeys(ctx context.Context, path, environment string) ([]cloudKey, error) {
	var keys []cloudKey
	cursor := ""
	for {
		out := struct {
			Data []cloudKey `json:"data"`
			Page cloudPage  `json:"page"`
		}{}
		query := "?limit=100"
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := c.do(ctx, http.MethodGet, path+query, environment, nil, &out); err != nil {
			return nil, err
		}
		keys = append(keys, out.Data...)
		if !out.Page.HasMore || out.Page.Cursor == "" {
			return keys, nil
		}
		cursor = out.Page.Cursor
	}
}

// findEnv walks /envs for the one of that name; nil when there is none.
func (c *Cloud) findEnv(ctx context.Context, name string) (*cloudEnv, error) {
	cursor := ""
	for {
		out := struct {
			Data []cloudEnv `json:"data"`
			Page cloudPage  `json:"page"`
		}{}
		query := "?limit=250"
		if cursor != "" {
			query += "&cursor=" + url.QueryEscape(cursor)
		}
		if err := c.do(ctx, http.MethodGet, "/envs"+query, "", nil, &out); err != nil {
			return nil, err
		}
		for i := range out.Data {
			if out.Data[i].Name == name {
				return &out.Data[i], nil
			}
		}
		if !out.Page.HasMore || out.Page.Cursor == "" {
			return nil, nil
		}
		cursor = out.Page.Cursor
	}
}

func (c *Cloud) setArchived(ctx context.Context, envID string, archived bool) error {
	return c.do(ctx, http.MethodPatch, "/envs/"+url.PathEscape(envID), "", map[string]any{"isArchived": archived}, nil)
}

type cloudError struct {
	status int
	body   string
}

// Error carries the API's own diagnostic; the request's credential is a
// header and never part of it.
func (e *cloudError) Error() string {
	return fmt.Sprintf("inngest API returned %d: %s", e.status, e.body)
}

func isCloudStatus(err error, status int) bool {
	cloudErr, ok := err.(*cloudError)
	return ok && cloudErr.status == status
}

// do makes one request. environment, when set, goes in the X-Inngest-Env
// header, which is how every per-environment endpoint is scoped.
func (c *Cloud) do(ctx context.Context, method, path, environment string, in, out any) error {
	var body io.Reader
	if in != nil {
		payload, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIURL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if environment != "" {
		req.Header.Set(envHeader, environment)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	httpClient := c.HTTPClient
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
		return &cloudError{status: resp.StatusCode, body: string(snippet)}
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
