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
	RequirePullRequest    bool        `json:"requirePullRequest"`
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

// connection is one of the platform's Connections as somebody choosing one
// sees it. `GET /connections` answers two shapes — the operator's, with a
// provider and conditions, and the picker's, with readiness — and this reads
// either: the fields the other shape does not carry simply arrive empty, which
// is what lets one command serve both roles.
type connection struct {
	Name         string   `json:"name"`
	Provider     string   `json:"provider,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
	Ready        bool     `json:"ready"`
}

// can reports whether a connection offers a capability. A connection that has
// reported none yet is not refused here, for the same reason the API does not
// refuse it: nothing has assessed it, which is not the same as it being wrong.
func (c connection) can(capability string) bool {
	if len(c.Capabilities) == 0 {
		return true
	}
	for _, offered := range c.Capabilities {
		if offered == capability {
			return true
		}
	}
	return false
}

// The two capabilities a project names, spelled as the API spells them.
const (
	capabilityGitSource  = "gitSource"
	capabilityImageStore = "imageStore"
)

// detection is what the platform makes of a repository before a project exists
// — `POST /connections/{name}/detect`, the same preflight the dashboard's
// new-project dialog runs.
//
// It is advice, not admission: `detected` false is a repository the platform
// has no framework for, which is a fine thing to create a project from if it
// has a Dockerfile or if the person knows something the detector does not.
type detection struct {
	Detected      bool     `json:"detected"`
	Framework     string   `json:"framework,omitempty"`
	Strategy      string   `json:"strategy,omitempty"`
	Port          int32    `json:"port,omitempty"`
	Ref           string   `json:"ref,omitempty"`
	RootDirectory string   `json:"rootDirectory,omitempty"`
	Dockerfile    bool     `json:"dockerfile"`
	Files         []string `json:"files,omitempty"`
	Message       string   `json:"message,omitempty"`
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
	Repository string             `json:"repository,omitempty"`
	Digest     string             `json:"digest,omitempty"`
	Attested   bool               `json:"attested"`
	AttestedAt *time.Time         `json:"attestedAt,omitempty"`
	KeyID      string             `json:"keyID,omitempty"`
	Evidence   []artifactEvidence `json:"evidence,omitempty"`
	Message    string             `json:"message,omitempty"`
}

// artifactEvidence is one attestation attached to an artifact, as the build
// reports it: enough to say what is there without asking the registry.
type artifactEvidence struct {
	PredicateType string `json:"predicateType"`
	Kind          string `json:"kind"`
	Source        string `json:"source,omitempty"`
	Manifest      string `json:"manifest,omitempty"`
}

// evidenceSet is `GET /builds/{name}/attestations`: the materialized evidence
// attached to a build's artifact, read out of the registry.
//
// Verified says whether signatures were checked at all. A set gathered with no
// keys is a listing, not a verification, and printing the two the same way
// would eventually have somebody treat one as the other.
type evidenceSet struct {
	Subject      string     `json:"subject"`
	Verified     bool       `json:"verified"`
	Attestations []evidence `json:"attestations"`
}

type evidence struct {
	PredicateType string   `json:"predicateType"`
	Verified      bool     `json:"verified"`
	KeyIDs        []string `json:"keyIDs,omitempty"`
	Digest        string   `json:"digest"`
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
	Gates             []gate      `json:"gates,omitempty"`
	Source            *source     `json:"source,omitempty"`
}

// source is how the commit reached the branch: through review, or not.
//
// Every field is the git provider's claim rather than the platform's
// observation, which is why Provider travels with them. Required says whether
// the project demanded review for this commit, so a build carrying none reads
// as "not asked for" rather than "asked for and missing".
type source struct {
	Provider        string     `json:"provider,omitempty"`
	PullRequest     int32      `json:"pullRequest,omitempty"`
	Title           string     `json:"title,omitempty"`
	Author          string     `json:"author,omitempty"`
	MergedBy        string     `json:"mergedBy,omitempty"`
	Approvers       []string   `json:"approvers,omitempty"`
	SelfApproved    bool       `json:"selfApproved"`
	Independent     bool       `json:"independent"`
	MachineIdentity string     `json:"machineIdentity,omitempty"`
	Required        bool       `json:"required"`
	CheckedAt       *time.Time `json:"checkedAt,omitempty"`
	Message         string     `json:"message,omitempty"`
}

// gateSubmission is a gate result produced somewhere else — usually the
// application's own CI, which ran the scanner minutes before Kitchen saw the
// commit.
type gateSubmission struct {
	Gate       string          `json:"gate"`
	Version    string          `json:"version,omitempty"`
	Format     string          `json:"format,omitempty"`
	FinishedAt *time.Time      `json:"finishedAt,omitempty"`
	Findings   json.RawMessage `json:"findings"`
}

// gateAccepted is what the API answers a submission with: where the evidence
// went and whose word it is recorded as.
type gateAccepted struct {
	Gate          string `json:"gate"`
	PredicateType string `json:"predicateType"`
	Manifest      string `json:"manifest"`
	ReportedBy    string `json:"reportedBy"`
	Subject       string `json:"subject"`
}

// gate is one quality gate's run over a build's artifact.
//
// Phase is Pending, Running, Completed, Failed or Skipped. `Completed` means
// the gate ran, whatever it found — a scanner reporting a hundred critical
// vulnerabilities has completed. `Failed` means it did not run, and there is no
// evidence either way. Nothing here says whether the findings were acceptable:
// that is a policy question about the environment being deployed to.
type gate struct {
	Name          string     `json:"name"`
	Phase         string     `json:"phase,omitempty"`
	Source        string     `json:"source,omitempty"`
	ReportedBy    string     `json:"reportedBy,omitempty"`
	PredicateType string     `json:"predicateType,omitempty"`
	Attested      bool       `json:"attested"`
	FinishedAt    *time.Time `json:"finishedAt,omitempty"`
	Message       string     `json:"message,omitempty"`
}

// firedRule is one policy rule that fired on a decision. A waived rule fired
// all the same — the exception changed the verdict, not the facts.
type firedRule struct {
	Rule      string `json:"rule"`
	Message   string `json:"message,omitempty"`
	Waived    bool   `json:"waived,omitempty"`
	Exception string `json:"exception,omitempty"`
}

// decision is one stored policy decision: `GET /decisions` summarizes it, and
// `GET /decisions/{id}` adds the full input it can be replayed from.
type decision struct {
	ID           string         `json:"id"`
	Timestamp    time.Time      `json:"timestamp"`
	Kind         string         `json:"kind"`
	Project      string         `json:"project,omitempty"`
	Environment  string         `json:"environment,omitempty"`
	Release      string         `json:"release,omitempty"`
	Artifact     string         `json:"artifact,omitempty"`
	BundleDigest string         `json:"bundleDigest"`
	InputDigest  string         `json:"inputDigest"`
	DataSnapshot string         `json:"dataSnapshot,omitempty"`
	Verdict      string         `json:"verdict"`
	RulesFired   []firedRule    `json:"rulesFired,omitempty"`
	Input        map[string]any `json:"input,omitempty"`
	DecidedBy    string         `json:"decidedBy,omitempty"`
}

// replayVerdict is the original decision's half of a replay answer.
type replayVerdict struct {
	Verdict string `json:"verdict"`
}

// replayOutcome is the re-evaluation's half: what the same bundle said about
// the same input this time, and what fired.
type replayOutcome struct {
	Verdict string      `json:"verdict"`
	Fired   []firedRule `json:"fired,omitempty"`
}

// decisionReplay is `POST /decisions/{id}/replay`: both verdicts side by
// side, and the one bit the endpoint exists for.
type decisionReplay struct {
	Original replayVerdict `json:"original"`
	Replay   replayOutcome `json:"replay"`
	Match    bool          `json:"match"`
	Decision string        `json:"decision"`
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

// detectTarget is the preflight's request: POST /connections/{name}/detect. It
// asks the question a project carries the answers to, at the one moment a wrong
// root directory is still free to correct.
type detectTarget struct {
	Repo           string `json:"repo"`
	Ref            string `json:"ref,omitempty"`
	RootDirectory  string `json:"rootDirectory,omitempty"`
	DockerfilePath string `json:"dockerfilePath,omitempty"`
}

// newProject is POST /projects. Previews is a pointer so that not passing
// --previews leaves the platform's default alone rather than turning them off.
type newProject struct {
	Name             string `json:"name"`
	Repo             string `json:"repo"`
	Connection       string `json:"connection"`
	Registry         string `json:"registry"`
	ProductionBranch string `json:"productionBranch,omitempty"`
	Previews         *bool  `json:"previews,omitempty"`
	RootDirectory    string `json:"rootDirectory,omitempty"`
	DockerfilePath   string `json:"dockerfilePath,omitempty"`
}

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

func (c *client) connections(ctx context.Context) ([]connection, error) {
	answer := &list[connection]{}
	err := c.do(ctx, "listing connections", http.MethodGet, "/connections", nil, nil, answer)
	return answer.Items, err
}

// detect asks what the platform makes of a repository, before there is a
// project to ask about. It is the create flow's preflight, and the answer is
// advice: a repository the platform has no framework for is still one a
// project can be created from.
func (c *client) detect(ctx context.Context, conn string, target detectTarget) (*detection, error) {
	answer := &detection{}
	err := c.do(ctx, "reading what "+target.Repo+" looks like",
		http.MethodPost, "/connections/"+conn+"/detect", nil, target, answer)
	return answer, err
}

// createProject is the create. The build context travels with it rather than
// after it, because creating a project starts a build straight away — a root
// directory corrected by a later PATCH is corrected one failed build too late.
func (c *client) createProject(ctx context.Context, body newProject) (*project, error) {
	answer := &project{}
	return answer, c.do(ctx, "creating the project "+body.Name,
		http.MethodPost, "/projects", nil, body, answer)
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

func (c *client) buildAttestations(ctx context.Context, name string) (*evidenceSet, error) {
	answer := &evidenceSet{}
	err := c.do(ctx, "reading "+name+"'s evidence",
		http.MethodGet, "/builds/"+name+"/attestations", nil, nil, answer)
	return answer, err
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

// submitGate ingests a gate result something else produced. The findings are
// sent as the exact bytes the tool wrote: re-encoding somebody's evidence into
// a shape of this CLI's choosing would be the CLI editing evidence.
func (c *client) submitGate(ctx context.Context, name string, body gateSubmission) (*gateAccepted, error) {
	answer := &gateAccepted{}
	err := c.do(ctx, "submitting a gate result for "+name,
		http.MethodPost, "/builds/"+name+"/gates", nil, body, answer)
	return answer, err
}

func (c *client) decisions(ctx context.Context, query url.Values) ([]decision, error) {
	answer := &list[decision]{}
	err := c.do(ctx, "listing decisions", http.MethodGet, "/decisions", query, nil, answer)
	return answer.Items, err
}

func (c *client) decision(ctx context.Context, id string) (*decision, error) {
	answer := &decision{}
	return answer, c.do(ctx, "reading the decision "+id, http.MethodGet, "/decisions/"+id, nil, nil, answer)
}

// replayDecision re-evaluates a stored decision from its stored inputs, which
// stores a decision of kind replay in its own right.
func (c *client) replayDecision(ctx context.Context, id string) (*decisionReplay, error) {
	answer := &decisionReplay{}
	return answer, c.do(ctx, "replaying the decision "+id,
		http.MethodPost, "/decisions/"+id+"/replay", nil, nil, answer)
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
