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
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"time"
)

// The shapes the API answers with, as this CLI reads them.
//
// They are a deliberate subset: the fields the commands render and the fields
// `--json` is worth publishing, in the API's own names so that a reader who
// has docs/API.md open is reading about the same thing. They are *not* a
// second contract — `kitchen schema` derives its `shapes` section from these
// structs by reflection, so what the CLI says it answers and what it actually
// answers cannot drift apart, and an unknown field on the wire is ignored
// rather than being an error, which is what lets a newer platform answer an
// older CLI.

// list is how every collection on this API answers: an object with `items`,
// rather than a bare array, so a cursor can be added later without breaking
// anything that reads one.
type list[T any] struct {
	Items []T `json:"items"`
}

// account is `GET /me`: the caller described to themselves.
type account struct {
	Subject      string `json:"subject"`
	Email        string `json:"email,omitempty"`
	Name         string `json:"name,omitempty"`
	PlatformRole string `json:"platformRole"`
}

// condition is one of the platform's conditions, in Kubernetes' own shape.
type condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime"`
}

// keyRef names one key of a Secret or of a claim's binding.
type keyRef struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}

// envVar is one of a project's environment variables — never its value. The
// API reports whether a variable has one, not what it is, which is why
// `kitchen env list` can print the whole list and reveal nothing.
type envVar struct {
	Name       string  `json:"name"`
	Set        bool    `json:"set"`
	PreviewSet bool    `json:"previewSet"`
	FromSecret *keyRef `json:"fromSecret,omitempty"`
	FromClaim  *keyRef `json:"fromClaim,omitempty"`
}

// project is `GET /projects/{name}`.
type project struct {
	Name                  string      `json:"name"`
	Role                  string      `json:"role"`
	Repo                  string      `json:"repo"`
	Connection            string      `json:"connection"`
	Registry              string      `json:"registry"`
	ProductionBranch      string      `json:"productionBranch"`
	Previews              bool        `json:"previews"`
	PreviewsProtected     bool        `json:"previewsProtected"`
	BuildStrategy         string      `json:"buildStrategy,omitempty"`
	Env                   []envVar    `json:"env,omitempty"`
	Port                  int32       `json:"port,omitempty"`
	Replicas              *int32      `json:"replicas,omitempty"`
	CPU                   string      `json:"cpu,omitempty"`
	Memory                string      `json:"memory,omitempty"`
	ProductionEnvironment string      `json:"productionEnvironment,omitempty"`
	LatestBuild           string      `json:"latestBuild,omitempty"`
	CreatedAt             time.Time   `json:"createdAt"`
	Conditions            []condition `json:"conditions,omitempty"`
}

// revision is the commit a build was of.
type revision struct {
	SHA         string `json:"sha"`
	Branch      string `json:"branch"`
	Message     string `json:"message,omitempty"`
	Author      string `json:"author,omitempty"`
	PullRequest *int32 `json:"pullRequest,omitempty"`
}

// artifact is what a build produced, by content.
type artifact struct {
	Repository string     `json:"repository,omitempty"`
	Digest     string     `json:"digest,omitempty"`
	Attested   bool       `json:"attested"`
	AttestedAt *time.Time `json:"attestedAt,omitempty"`
	KeyID      string     `json:"keyID,omitempty"`
	Message    string     `json:"message,omitempty"`
}

// buildCache is what the layer cache did for a build: a cold build had nothing
// to reuse, which is the difference between one that was slow and one that
// regressed.
type buildCache struct {
	Enabled bool   `json:"enabled"`
	Warm    bool   `json:"warm"`
	Ref     string `json:"ref,omitempty"`
	Mode    string `json:"mode,omitempty"`
	Message string `json:"message,omitempty"`
}

// build is `GET /builds/{name}`. Phase is one of Queued, Running, Succeeded,
// Failed or Cancelled.
type build struct {
	Name              string      `json:"name"`
	Project           string      `json:"project"`
	Phase             string      `json:"phase,omitempty"`
	Git               revision    `json:"git"`
	DetectedFramework string      `json:"detectedFramework,omitempty"`
	Image             string      `json:"image,omitempty"`
	StartedAt         *time.Time  `json:"startedAt,omitempty"`
	CompletedAt       *time.Time  `json:"completedAt,omitempty"`
	CreatedAt         time.Time   `json:"createdAt"`
	Conditions        []condition `json:"conditions,omitempty"`
	Artifact          *artifact   `json:"artifact,omitempty"`
	Cache             *buildCache `json:"cache,omitempty"`
}

// terminal reports whether a build has stopped moving, whichever way it went.
func (b build) terminal() bool {
	switch b.Phase {
	case phaseSucceeded, phaseFailed, phaseCancelled:
		return true
	default:
		return false
	}
}

// release is `GET /releases/{name}`: an immutable snapshot of an image and the
// configuration it runs with, which is what makes a rollback a pointer move.
type release struct {
	Name         string    `json:"name"`
	Project      string    `json:"project"`
	Build        string    `json:"build"`
	Image        string    `json:"image"`
	Environments []string  `json:"environments,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

// preview is the pull request a preview environment belongs to.
type preview struct {
	PullRequest int32  `json:"pullRequest"`
	Branch      string `json:"branch"`
}

// releaseHistory is one stint of a release being current on an environment:
// what it was, when it held it, how it stopped and who moved it. It is what
// `kitchen rollback` reads to know what "the previous release" means.
type releaseHistory struct {
	Release string    `json:"release"`
	From    time.Time `json:"from"`
	To      time.Time `json:"to"`
	Reason  string    `json:"reason"`
	By      string    `json:"by,omitempty"`
}

// environment is `GET /environments/{name}`. Phase is one of Pending,
// Deploying, Live, Degraded or Terminating.
type environment struct {
	Name            string           `json:"name"`
	Project         string           `json:"project"`
	Type            string           `json:"type"`
	Release         string           `json:"release"`
	ObservedRelease string           `json:"observedRelease,omitempty"`
	Phase           string           `json:"phase,omitempty"`
	URL             string           `json:"url,omitempty"`
	Preview         *preview         `json:"preview,omitempty"`
	History         []releaseHistory `json:"history,omitempty"`
	CreatedAt       time.Time        `json:"createdAt"`
	Conditions      []condition      `json:"conditions,omitempty"`
}

// logLine is one line out of the telemetry store, from a build or from
// something running.
type logLine struct {
	Timestamp   time.Time         `json:"timestamp"`
	Source      string            `json:"source"`
	Project     string            `json:"project,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Build       string            `json:"build,omitempty"`
	Pod         string            `json:"pod,omitempty"`
	Container   string            `json:"container,omitempty"`
	Stream      string            `json:"stream,omitempty"`
	Level       string            `json:"level,omitempty"`
	Message     string            `json:"message"`
	TraceID     string            `json:"traceId,omitempty"`
	SpanID      string            `json:"spanId,omitempty"`
	Fields      map[string]string `json:"fields,omitempty"`
}

// The phase words, spelled once. They are the platform's, from
// api/v1alpha1 — a CLI that invented its own would be a second vocabulary for
// the same states.
const (
	phaseSucceeded = "Succeeded"
	phaseFailed    = "Failed"
	phaseCancelled = "Cancelled"
	phaseLive      = "Live"
	phaseDegraded  = "Degraded"
)

// envVarWrite is one variable on the way *in*, which is the only direction a
// value ever travels.
//
// Value and PreviewValue are pointers because the API distinguishes three
// things a request can say about a value and a plain string can only say two:
// a field left out keeps the value the variable already has (so the CLI can
// send the whole list back without ever having read one), an empty string
// clears it, and a string sets it.
type envVarWrite struct {
	Name         string  `json:"name"`
	Value        *string `json:"value,omitempty"`
	PreviewValue *string `json:"previewValue,omitempty"`
	FromSecret   *keyRef `json:"fromSecret,omitempty"`
	FromClaim    *keyRef `json:"fromClaim,omitempty"`
}

// decodeJSON reads one of the platform's payloads, failing in the CLI's own
// shape rather than in encoding/json's.
func decodeJSON(payload []byte, into any) error {
	if err := json.Unmarshal(payload, into); err != nil {
		return failf(codeFailed, "the platform sent something that is not the expected JSON: %v", err)
	}
	return nil
}

// The calls. One method per endpoint the CLI uses, so that the list of what
// this client touches is a list of methods rather than a search for string
// literals — and so `kitchen schema`'s `calls` for each command can be checked
// against something.

func (c *client) me(ctx context.Context) (*account, error) {
	answer := &account{}
	return answer, c.do(ctx, "reading who you are", http.MethodGet, "/me", nil, nil, answer)
}

func (c *client) projects(ctx context.Context) ([]project, error) {
	answer := &list[project]{}
	err := c.do(ctx, "listing projects", http.MethodGet, "/projects", nil, nil, answer)
	return answer.Items, err
}

func (c *client) project(ctx context.Context, name string) (*project, error) {
	answer := &project{}
	return answer, c.do(ctx, "reading the project "+name, http.MethodGet, "/projects/"+name, nil, nil, answer)
}

// setEnv replaces a project's whole variable list. The API keeps the value of
// any variable this body leaves a `value` off, which is what lets the CLI send
// the list back without ever having read a value.
func (c *client) setEnv(ctx context.Context, name string, env []envVarWrite) (*project, error) {
	answer := &project{}
	body := map[string]any{"env": env}
	return answer, c.do(ctx, "changing "+name+"'s environment variables",
		http.MethodPatch, "/projects/"+name+"/env", nil, body, answer)
}

func (c *client) projectBuilds(ctx context.Context, name string) ([]build, error) {
	answer := &list[build]{}
	err := c.do(ctx, "listing "+name+"'s builds", http.MethodGet, "/projects/"+name+"/builds", nil, nil, answer)
	return answer.Items, err
}

func (c *client) projectReleases(ctx context.Context, name string) ([]release, error) {
	answer := &list[release]{}
	err := c.do(ctx, "listing "+name+"'s releases", http.MethodGet, "/projects/"+name+"/releases", nil, nil, answer)
	return answer.Items, err
}

func (c *client) projectEnvironments(ctx context.Context, name string) ([]environment, error) {
	answer := &list[environment]{}
	err := c.do(ctx, "listing "+name+"'s environments",
		http.MethodGet, "/projects/"+name+"/environments", nil, nil, answer)
	return answer.Items, err
}

// startBuild is the deploy. An empty sha rebuilds whatever the project built
// last, which is the rerun-after-a-flake case.
func (c *client) startBuild(ctx context.Context, name, sha, branch string) (*build, error) {
	body := map[string]string{}
	if sha != "" {
		body["sha"] = sha
	}
	if branch != "" {
		body["branch"] = branch
	}
	answer := &build{}
	return answer, c.do(ctx, "starting a build of "+name,
		http.MethodPost, "/projects/"+name+"/builds", nil, body, answer)
}

func (c *client) build(ctx context.Context, name string) (*build, error) {
	answer := &build{}
	return answer, c.do(ctx, "reading the build "+name, http.MethodGet, "/builds/"+name, nil, nil, answer)
}

func (c *client) cancelBuild(ctx context.Context, name string) (*build, error) {
	answer := &build{}
	return answer, c.do(ctx, "cancelling the build "+name,
		http.MethodPost, "/builds/"+name+"/cancel", nil, nil, answer)
}

func (c *client) environment(ctx context.Context, name string) (*environment, error) {
	answer := &environment{}
	return answer, c.do(ctx, "reading the environment "+name,
		http.MethodGet, "/environments/"+name, nil, nil, answer)
}

// moveEnvironment points an environment at another release. Promotion and
// rollback are the same call; which one it is depends only on which way the
// release is.
func (c *client) moveEnvironment(ctx context.Context, name, to string) (*environment, error) {
	answer := &environment{}
	return answer, c.do(ctx, "moving "+name+" to "+to,
		http.MethodPatch, "/environments/"+name, nil, map[string]string{"release": to}, answer)
}

// logsPage reads a bounded page of logs, oldest first.
func (c *client) logsPage(ctx context.Context, path string, query url.Values) ([]logLine, error) {
	answer := &list[logLine]{}
	err := c.do(ctx, "reading logs", http.MethodGet, path, query, nil, answer)
	return answer.Items, err
}

// followLogs tails the same selection over Server-Sent Events: the current
// page first, then every line as it arrives.
func (c *client) followLogs(ctx context.Context, path string, query url.Values, onLine func(logLine) error) error {
	return c.stream(ctx, "following logs", path, query, func(payload []byte) error {
		line := logLine{}
		if err := decodeJSON(payload, &line); err != nil {
			return err
		}
		return onLine(line)
	})
}
