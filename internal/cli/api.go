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
	"fmt"
	"net/http"
	"net/url"
	"strings"
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

// projectSecret is one of a project's own secrets — the credentials Kitchen
// did not mint. Never its value: the API answers a name and the reference an
// environment variable reads it by, and nothing else exists to answer.
type projectSecret struct {
	Name string `json:"name"`
	// Reference is the `fromSecret` that reads this secret, answered by the
	// platform so that nobody has to know the name of the object it keeps
	// them in.
	Reference keyRef `json:"reference"`
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
	// Unreadable is the repository itself not having been read: it is not
	// there, or the connection's credential cannot see it. It is the one
	// verdict that is not about the build context, and the one a corrected
	// --root-directory will not change.
	Unreadable bool   `json:"unreadable,omitempty"`
	Message    string `json:"message,omitempty"`
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
	Name              string   `json:"name"`
	Project           string   `json:"project"`
	Phase             string   `json:"phase,omitempty"`
	Git               revision `json:"git"`
	DetectedFramework string   `json:"detectedFramework,omitempty"`
	// Config is the kitchen.json this commit carried, when it carried one.
	Config      *repoConfig `json:"config,omitempty"`
	Image       string      `json:"image,omitempty"`
	StartedAt   *time.Time  `json:"startedAt,omitempty"`
	CompletedAt *time.Time  `json:"completedAt,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
	Conditions  []condition `json:"conditions,omitempty"`
	Artifact    *artifact   `json:"artifact,omitempty"`
	Cache       *buildCache `json:"cache,omitempty"`
	Gates       []gate      `json:"gates,omitempty"`
	Source      *source     `json:"source,omitempty"`
	// Failure is why a failed build failed: which container stopped it, how
	// it exited, and the last of what it printed. Absent on every build that
	// did not fail.
	Failure *buildFailure `json:"failure,omitempty"`
}

// repoConfig is the commit's own kitchen.json: where it was read from, and
// every setting it took over from the project. It answers what was declared
// rather than what it was declared as — the values are already in the
// release's snapshot, and a second copy here is a second thing to disagree.
type repoConfig struct {
	Path     string   `json:"path"`
	Declares []string `json:"declares"`
}

// buildFailure is a failed build's own account of itself.
//
// The Job behind a build reports "Job has reached the specified backoff
// limit", which is the same sentence for every build that ever failed. This is
// what the platform read off the pod before it was collected, and it is the
// difference between a list of failures and a list of one failure repeated.
type buildFailure struct {
	Container string   `json:"container,omitempty"`
	ExitCode  *int32   `json:"exitCode,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Message   string   `json:"message,omitempty"`
	Log       []string `json:"log,omitempty"`
}

// why is a failed build in one line, bounded so that a table stays a table.
//
// A build that failed before it ever had a pod — an unsupported strategy, a
// commit refused for want of review — has no container to name, and the Ready
// condition the reconciler left is the answer instead.
func (b build) why() string {
	if b.Phase == "Running" {
		// The one case where this column is not about a failure. A build
		// whose Job has never created a pod reports Running for as long as
		// anybody leaves it there, and the reason is nowhere a developer can
		// reach — so the reconciler puts it on a Stalled condition and this
		// is where it is read.
		return firstLine(b.stalledReason(), buildWhyWidth)
	}
	if b.Phase != "Failed" {
		return ""
	}
	line := ""
	switch {
	case b.Failure != nil && b.Failure.Message != "":
		line = b.Failure.Message
	case b.Failure != nil && b.Failure.Container != "" && b.Failure.ExitCode != nil:
		line = fmt.Sprintf("%s exited %d", b.Failure.Container, *b.Failure.ExitCode)
	case b.Failure != nil && b.Failure.Container != "":
		line = b.Failure.Container + " did not run"
	default:
		for _, c := range b.Conditions {
			if c.Type == "Ready" && c.Status == "False" {
				line = c.Message
				break
			}
		}
	}
	return firstLine(line, buildWhyWidth)
}

// stalledReason is what a running build says when it is not moving, and
// nothing when it is.
func (b build) stalledReason() string {
	for _, c := range b.Conditions {
		if c.Type == "Stalled" && c.Status == "True" {
			return c.Message
		}
	}
	return ""
}

// buildWhyWidth is how much of a failure a table column may take. The whole of
// it is on `kitchen builds --json`, and on the build's own page; this is the
// column that has to sit next to four others.
const buildWhyWidth = 56

// firstLine is one line of somebody else's text, bounded.
func firstLine(s string, width int) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > width {
		return s[:width-1] + "…"
	}
	return s
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

// vexStatement is one OpenVEX statement about the artifact, as the API joins
// it: what was asserted, who asserted it, who submitted it, and what the
// platform can establish about it. `justified`, `expired` and `verified` are
// facts rather than a verdict — whether a statement actually suppresses
// anything is the target environment's bundle's question.
type vexStatement struct {
	Vulnerability   string   `json:"vulnerability"`
	Status          string   `json:"status"`
	Justification   string   `json:"justification,omitempty"`
	Products        []string `json:"products,omitempty"`
	Justified       bool     `json:"justified"`
	Author          string   `json:"author,omitempty"`
	SubmittedBy     string   `json:"submittedBy,omitempty"`
	DocumentID      string   `json:"documentID,omitempty"`
	Timestamp       string   `json:"timestamp,omitempty"`
	ExpiresAt       string   `json:"expiresAt,omitempty"`
	Expired         bool     `json:"expired"`
	Verified        bool     `json:"verified"`
	StatusNotes     string   `json:"statusNotes,omitempty"`
	ImpactStatement string   `json:"impactStatement,omitempty"`
	ActionStatement string   `json:"actionStatement,omitempty"`
}

// vexFinding is one finding from the artifact's newest vulnerability scan,
// beside the statement covering it. A suppressed finding is still a finding
// and still listed: never silently applied.
type vexFinding struct {
	Vulnerability string        `json:"vulnerability"`
	Severity      string        `json:"severity,omitempty"`
	Package       string        `json:"package,omitempty"`
	Version       string        `json:"version,omitempty"`
	FixedIn       string        `json:"fixedIn,omitempty"`
	VEX           *vexStatement `json:"vex,omitempty"`
}

// vexAnswer is what GET /builds/{name}/vex returns.
type vexAnswer struct {
	Subject      string         `json:"subject"`
	Verification string         `json:"verification"`
	Statements   []vexStatement `json:"statements"`
	Findings     []vexFinding   `json:"findings"`
	Caveat       string         `json:"caveat,omitempty"`
}

// vexSubmission carries the OpenVEX document itself, as the exact bytes its
// author wrote: the platform signs those bytes, and re-encoding somebody's
// assertion into a shape of this CLI's choosing would be the CLI editing it.
type vexSubmission struct {
	Document json.RawMessage `json:"document"`
}

// vexAccepted is what the API answers a submission with.
type vexAccepted struct {
	DocumentID      string   `json:"documentID,omitempty"`
	PredicateType   string   `json:"predicateType"`
	Manifest        string   `json:"manifest"`
	Subject         string   `json:"subject"`
	Author          string   `json:"author"`
	SubmittedBy     string   `json:"submittedBy"`
	Statements      int      `json:"statements"`
	Vulnerabilities []string `json:"vulnerabilities"`
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

// driftRule is one rule standing in the way of a deployed release, and
// whether it was already standing there when the release was promoted.
type driftRule struct {
	Rule    string `json:"rule"`
	Message string `json:"message,omitempty"`
	// Since is "rescan" for a rule that started failing after promotion, and
	// "promotion" for one that fired then too and was waived by an exception
	// which has since run out.
	Since string `json:"since"`
	// Exception is the grant waiving this rule in the evaluation the row
	// reports; WaivedAtPromotion is the grant that waived it when the release
	// was promoted. They are two questions, and a rule firing unwaived now
	// answers only the second.
	Exception         string `json:"exception,omitempty"`
	WaivedAtPromotion string `json:"waivedAtPromotion,omitempty"`
}

// driftItem is one deployed (environment, release) pair as the drift view
// answers it.
type driftItem struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Release     string `json:"release"`
	Artifact    string `json:"artifact,omitempty"`
	// Status is compliant, waived, newly-failing, waived-at-promotion or
	// not-evaluated.
	Status       string     `json:"status"`
	Verdict      string     `json:"verdict,omitempty"`
	ScannedAt    *time.Time `json:"scannedAt,omitempty"`
	DataSnapshot string     `json:"dataSnapshot,omitempty"`
	Findings     int32      `json:"findings,omitempty"`
	DecisionID   string     `json:"decisionID,omitempty"`
	// ScanFailed is why the most recent scan attempt did not run, where it did
	// not. A row carrying it is answering with something older than the
	// failure, whatever its verdict says.
	ScanFailed      string      `json:"scanFailed,omitempty"`
	PromotedVerdict string      `json:"promotedVerdict,omitempty"`
	PromotedAt      *time.Time  `json:"promotedAt,omitempty"`
	Rules           []driftRule `json:"rules,omitempty"`
	Message         string      `json:"message,omitempty"`
}

// drift is `GET /compliance/drift`: what is running right now that no longer
// meets its environment's bar.
type drift struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// Rescanning says whether the continuous re-evaluation pass is running at
	// all. An empty answer means two different things depending on it.
	Rescanning bool           `json:"rescanning"`
	Message    string         `json:"message,omitempty"`
	Drifting   int            `json:"drifting"`
	Items      []driftItem    `json:"items"`
	Counts     map[string]int `json:"counts,omitempty"`
}

// criticalityEnvironment is one environment under a designated function, with
// the designation that actually applies to it — its own, or its project's
// where production declares none, which `inherited` names.
type criticalityEnvironment struct {
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Criticality string   `json:"criticality"`
	RTO         string   `json:"rto,omitempty"`
	RPO         string   `json:"rpo,omitempty"`
	Inherited   []string `json:"inherited,omitempty"`
	URL         string   `json:"url,omitempty"`
	Release     string   `json:"release,omitempty"`
	Image       string   `json:"image,omitempty"`
	Domains     []string `json:"domains,omitempty"`
}

// criticalityClaim is one provisioned resource, with the third party behind it.
type criticalityClaim struct {
	Name       string `json:"name"`
	Type       string `json:"type"`
	Connection string `json:"connection,omitempty"`
	Provider   string `json:"provider,omitempty"`
	Phase      string `json:"phase,omitempty"`
	DataClass  string `json:"dataClass"`
	Residency  string `json:"residency"`
}

// criticalityConnection is one third-party relationship, and what for.
type criticalityConnection struct {
	Name     string   `json:"name"`
	Provider string   `json:"provider"`
	UsedFor  []string `json:"usedFor"`
}

// criticalityFunction is one designated function and everything supporting it.
type criticalityFunction struct {
	Project      string                   `json:"project"`
	Criticality  string                   `json:"criticality"`
	RTO          string                   `json:"rto,omitempty"`
	RPO          string                   `json:"rpo,omitempty"`
	Environments []criticalityEnvironment `json:"environments"`
	Claims       []criticalityClaim       `json:"claims"`
	Connections  []criticalityConnection  `json:"connections"`
	ThirdParties []string                 `json:"thirdParties"`
}

// criticalityMap is `GET /compliance/criticality`.
type criticalityMap struct {
	GeneratedAt time.Time             `json:"generatedAt"`
	Minimum     string                `json:"minimum,omitempty"`
	Functions   []criticalityFunction `json:"functions"`
	// Undesignated is how many visible projects nobody has designated — the
	// number that says whether a short map is a small estate or an unfinished
	// designation exercise.
	Undesignated int    `json:"undesignated"`
	Depth        string `json:"depth"`
}

// criticalityDependent is one environment that would be affected.
type criticalityDependent struct {
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Type        string   `json:"type"`
	Criticality string   `json:"criticality"`
	RTO         string   `json:"rto,omitempty"`
	RPO         string   `json:"rpo,omitempty"`
	Inherited   []string `json:"inherited,omitempty"`
	Through     []string `json:"through"`
}

// criticalitySubject names what a reverse query was asked about.
type criticalitySubject struct {
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Provider    string   `json:"provider,omitempty"`
	Connections []string `json:"connections,omitempty"`
}

// dependents is `GET /compliance/dependents`: what breaks without one third
// party.
type dependents struct {
	GeneratedAt time.Time              `json:"generatedAt"`
	Subject     criticalitySubject     `json:"subject"`
	Affected    []criticalityDependent `json:"affected"`
	Counts      map[string]int         `json:"counts"`
	TightestRTO string                 `json:"tightestRTO,omitempty"`
	Depth       string                 `json:"depth"`
}

// exception is one break-glass grant as the register serves it: who asked,
// who approved, what it waives, until when, and what became of it. Phase is
// judged against the clock server-side, so Expired here means expired now.
type exception struct {
	Name        string   `json:"name"`
	Project     string   `json:"project"`
	Environment string   `json:"environment"`
	Release     string   `json:"release,omitempty"`
	RuleIDs     []string `json:"ruleIDs"`
	Reason      string   `json:"reason"`
	RequestedBy string   `json:"requestedBy"`
	ApprovedBy  string   `json:"approvedBy"`
	IncidentRef string   `json:"incidentRef,omitempty"`

	ExpiresAt    time.Time `json:"expiresAt"`
	AutoRollback bool      `json:"autoRollback"`

	Phase      string     `json:"phase"`
	UsedBy     []string   `json:"usedBy,omitempty"`
	ResolvedBy string     `json:"resolvedBy,omitempty"`
	ResolvedAt *time.Time `json:"resolvedAt,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
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

// promotion is one request to move a release into an environment, with what
// became of it. Phase is Pending, Evaluating, Allowed, AllowedWithException,
// Blocked, Applied or Failed; a Blocked one names the unmet rules by their
// stable ids, and decisionID leads to the stored decision behind the verdict.
type promotion struct {
	Name        string     `json:"name"`
	Project     string     `json:"project"`
	Environment string     `json:"environment"`
	Release     string     `json:"release"`
	RequestedBy string     `json:"requestedBy"`
	Trigger     string     `json:"trigger"`
	Reason      string     `json:"reason,omitempty"`
	Phase       string     `json:"phase"`
	Verdict     string     `json:"verdict,omitempty"`
	DecisionID  string     `json:"decisionID,omitempty"`
	UnmetRules  []string   `json:"unmetRules,omitempty"`
	Message     string     `json:"message,omitempty"`
	EvaluatedAt *time.Time `json:"evaluatedAt,omitempty"`
	AppliedAt   *time.Time `json:"appliedAt,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
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

// configDiff is what a move between two releases would change: the answer to
// GET /releases/{name}/config-diff?against=. `Release` is where the
// environment is going, `Against` where it is now.
//
// **There are no values in it, and there cannot be.** The API never reads an
// environment variable's value back, so the comparison is made on the server
// and only its verdict travels — `changed`, never what it changed to. That is
// what lets `kitchen rollback` say what a move would do without the platform
// first handing a terminal every literal the project ever set.
type configDiff struct {
	Release   string           `json:"release"`
	Against   string           `json:"against"`
	Project   string           `json:"project"`
	Variables []variableChange `json:"variables"`
	Runtime   []fieldChange    `json:"runtime"`
	Processes []processChange  `json:"processes"`
}

// variableChange is one environment variable across the two snapshots.
// `Change` is one of added, removed, changed, unchanged; `Source` and
// `AgainstSource` say where the value comes from on each side — value, secret
// or claim — which is the change no comparison of values would explain.
type variableChange struct {
	Name          string  `json:"name"`
	Change        string  `json:"change"`
	Source        string  `json:"source,omitempty"`
	AgainstSource string  `json:"againstSource,omitempty"`
	Ref           *keyRef `json:"ref,omitempty"`
	AgainstRef    *keyRef `json:"againstRef,omitempty"`
	// PreviewOnly marks a change confined to the preview override: the two
	// releases agree about what every environment but a preview runs with.
	PreviewOnly bool `json:"previewOnly,omitempty"`
}

// fieldChange is one runtime field, with both values: a port, a replica count
// and a compute request are project settings a viewer already reads, so unlike
// a variable they are reported as themselves.
type fieldChange struct {
	Field   string `json:"field"`
	From    string `json:"from,omitempty"`
	To      string `json:"to,omitempty"`
	Changed bool   `json:"changed"`
}

// processChange is one worker or scheduled job across the two snapshots.
type processChange struct {
	Name     string `json:"name"`
	Change   string `json:"change"`
	Type     string `json:"type,omitempty"`
	Schedule string `json:"schedule,omitempty"`
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

// process is one of a project's workers or scheduled jobs, as one environment
// is running it.
// processBuild is one workload's own build: which directory of the repository
// it is, and how the image comes out of it.
type processBuild struct {
	Strategy       string `json:"strategy"`
	DockerfilePath string `json:"dockerfilePath,omitempty"`
	RootDirectory  string `json:"rootDirectory,omitempty"`
}

type process struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Command  []string `json:"command,omitempty"`
	Args     []string `json:"args,omitempty"`
	Schedule string   `json:"schedule,omitempty"`
	// ConcurrencyPolicy and Timeout are a scheduled job's; Replicas and
	// ReadyReplicas are a worker's declared and actual counts.
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty"`
	Timeout           string `json:"timeout,omitempty"`
	Replicas          int32  `json:"replicas,omitempty"`
	ReadyReplicas     int32  `json:"readyReplicas,omitempty"`
	// Singleton is a worker two of which must never run at once: its deploys
	// stop the old pod before starting the new one instead of overlapping the
	// two.
	Singleton bool   `json:"singleton,omitempty"`
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Workload  string `json:"workload,omitempty"`
	// Port is a service's listening port and Address is where it answers
	// inside the cluster — `http://<host>:<port>`, the same value its
	// siblings read as KITCHEN_SERVICE_<NAME>. Both are absent on a worker
	// and a scheduled job, which nothing addresses. Neither is a public URL:
	// a service is never published.
	Port    int32  `json:"port,omitempty"`
	Address string `json:"address,omitempty"`
	// Image is what this workload runs when that is not the release's own
	// image, and Build is the build it has of its own. Both are absent for a
	// workload that runs the project's image with another command.
	Image string        `json:"image,omitempty"`
	Build *processBuild `json:"build,omitempty"`
	// Suspended is a process this environment declares and does not run: a
	// preview whose process was not opted in. Reason says so in a sentence.
	Suspended bool   `json:"suspended,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Active    int32  `json:"active,omitempty"`
	// LastRun and LastFailure are a scheduled job's. The failure is kept until
	// a later failure replaces it, never until a success does.
	LastRun     *processRun `json:"lastRun,omitempty"`
	LastFailure *processRun `json:"lastFailure,omitempty"`
	// Healthy is the platform's own verdict — a worker with no ready replica,
	// a schedule whose last run failed — so that this CLI and the dashboard
	// cannot disagree about what red means.
	Healthy bool `json:"healthy"`
}

// processRun is one firing of a scheduled job.
type processRun struct {
	Name            string     `json:"name"`
	Phase           string     `json:"phase"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	FinishedAt      *time.Time `json:"finishedAt,omitempty"`
	DurationSeconds *float64   `json:"durationSeconds,omitempty"`
	Message         string     `json:"message,omitempty"`
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

// projectSecrets lists a project's own secrets by name. There is no read that
// answers a value, here or anywhere else on this API.
func (c *client) projectSecrets(ctx context.Context, project string) ([]projectSecret, error) {
	answer := &list[projectSecret]{}
	err := c.do(ctx, "listing "+project+"'s secrets", http.MethodGet,
		"/projects/"+project+"/secrets", nil, nil, answer)
	return answer.Items, err
}

// setProjectSecret sets a secret, or replaces the value of one already there.
// The value travels one way: the answer is the name and the reference.
func (c *client) setProjectSecret(ctx context.Context, project, name, value string) (*projectSecret, error) {
	answer := &projectSecret{}
	return answer, c.do(ctx, "setting the secret "+name+" on "+project, http.MethodPut,
		"/projects/"+project+"/secrets/"+url.PathEscape(name), nil,
		map[string]string{"value": value}, answer)
}

func (c *client) deleteProjectSecret(ctx context.Context, project, name string) error {
	return c.do(ctx, "deleting the secret "+name+" from "+project, http.MethodDelete,
		"/projects/"+project+"/secrets/"+url.PathEscape(name), nil, nil, nil)
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

func (c *client) environmentProcesses(ctx context.Context, name string) ([]process, error) {
	answer := &list[process]{}
	err := c.do(ctx, "listing "+name+"'s processes",
		http.MethodGet, "/environments/"+name+"/processes", nil, nil, answer)
	if answer.Items == nil {
		answer.Items = []process{}
	}
	return answer.Items, err
}

func (c *client) processRuns(ctx context.Context, environment, name string) ([]processRun, error) {
	answer := &list[processRun]{}
	err := c.do(ctx, "listing "+name+"'s runs",
		http.MethodGet, "/environments/"+environment+"/processes/"+name+"/runs", nil, nil, answer)
	if answer.Items == nil {
		answer.Items = []processRun{}
	}
	return answer.Items, err
}

// startProcessRun runs a scheduled job now. The body is empty on purpose:
// nothing about the run is the caller's to choose — it is a copy of what the
// schedule would have run.
func (c *client) startProcessRun(ctx context.Context, environment, name string) (*processRun, error) {
	answer := &processRun{}
	err := c.do(ctx, "running "+name,
		http.MethodPost, "/environments/"+environment+"/processes/"+name+"/runs", nil, nil, answer)
	return answer, err
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

// vex reads the artifact's exploitability assertions joined to the findings
// they modify.
func (c *client) vex(ctx context.Context, name string) (*vexAnswer, error) {
	answer := &vexAnswer{}
	return answer, c.do(ctx, "reading the VEX statements on "+name,
		http.MethodGet, "/builds/"+name+"/vex", nil, nil, answer)
}

// submitVEX attaches an OpenVEX document to the build's artifact.
func (c *client) submitVEX(ctx context.Context, name string, body vexSubmission) (*vexAccepted, error) {
	answer := &vexAccepted{}
	err := c.do(ctx, "submitting a VEX document for "+name,
		http.MethodPost, "/builds/"+name+"/vex", nil, body, answer)
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

// complianceDrift asks what is deployed and no longer compliant.
func (c *client) complianceDrift(ctx context.Context, query url.Values) (*drift, error) {
	answer := &drift{}
	return answer, c.do(ctx, "reading compliance drift",
		http.MethodGet, "/compliance/drift", query, nil, answer)
}

// criticalityMap asks what supports each designated function.
func (c *client) criticalityMap(ctx context.Context, query url.Values) (*criticalityMap, error) {
	answer := &criticalityMap{}
	return answer, c.do(ctx, "reading the criticality map",
		http.MethodGet, "/compliance/criticality", query, nil, answer)
}

// dependents asks what breaks without one connection or one third party.
func (c *client) dependents(ctx context.Context, query url.Values) (*dependents, error) {
	answer := &dependents{}
	return answer, c.do(ctx, "reading what depends on it",
		http.MethodGet, "/compliance/dependents", query, nil, answer)
}

// identity is one account's one role in one place, as the access survey
// reports it: what is held where, when that identity was last recorded doing
// something, and whether anything is still behind it.
type identity struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	Grant   string `json:"grant"`
	Role    string `json:"role"`

	LastActive *time.Time `json:"lastActive,omitempty"`
	Inactive   bool       `json:"inactive,omitempty"`
	Unknown    bool       `json:"unknown,omitempty"`
	Orphaned   bool       `json:"orphaned,omitempty"`
}

// name is the account as a person reads it: the address where there is one,
// the opaque subject otherwise.
func (i identity) name() string {
	if i.Email != "" {
		return i.Email
	}
	return i.Subject
}

// identitySurvey is who holds what on the platform, whole. DirectoryConsulted
// is load-bearing: false means nothing is claimed about whether a grant
// belongs to anybody, because the identity provider could not be asked.
type identitySurvey struct {
	GeneratedAt        time.Time  `json:"generatedAt"`
	InactivityDays     int32      `json:"inactivityDays"`
	DirectoryConsulted bool       `json:"directoryConsulted"`
	Identities         []identity `json:"identities"`
	Orphans            int        `json:"orphans"`
	Message            string     `json:"message,omitempty"`
}

// accessReviewEntry is one grant inside a recertification cycle, with what
// was decided about it and what became of that decision.
type accessReviewEntry struct {
	Subject string `json:"subject"`
	Email   string `json:"email,omitempty"`
	Grant   string `json:"grant"`
	Role    string `json:"role"`

	Orphaned bool `json:"orphaned,omitempty"`

	Decision   string `json:"decision,omitempty"`
	DecidedBy  string `json:"decidedBy,omitempty"`
	Note       string `json:"note,omitempty"`
	SelfReview bool   `json:"selfReview,omitempty"`

	Applied      bool   `json:"applied,omitempty"`
	ApplyMessage string `json:"applyMessage,omitempty"`
}

// accessReviewArtifact points at the signed record a closed cycle left in the
// store. Message is what to read when a cycle closed without one.
type accessReviewArtifact struct {
	RecordID string     `json:"recordID,omitempty"`
	Subject  string     `json:"subject,omitempty"`
	SignedAt *time.Time `json:"signedAt,omitempty"`
	Message  string     `json:"message,omitempty"`
}

// accessReview is one recertification cycle as the register serves it. Phase
// is judged against the clock server-side, so Overdue here means overdue now.
type accessReview struct {
	Name      string   `json:"name"`
	Scope     string   `json:"scope"`
	Project   string   `json:"project,omitempty"`
	Reviewers []string `json:"reviewers"`
	OpenedBy  string   `json:"openedBy"`
	Reason    string   `json:"reason,omitempty"`

	DueBy    time.Time  `json:"dueBy"`
	ClosedBy string     `json:"closedBy,omitempty"`
	ClosedAt *time.Time `json:"closedAt,omitempty"`

	Phase        string `json:"phase"`
	Pending      int32  `json:"pending"`
	Confirmed    int32  `json:"confirmed"`
	Revoked      int32  `json:"revoked"`
	SelfReviewed int32  `json:"selfReviewed"`
	Orphaned     int32  `json:"orphaned"`

	Entries  []accessReviewEntry   `json:"entries"`
	Artifact *accessReviewArtifact `json:"artifact,omitempty"`
}

func (c *client) identities(ctx context.Context) (*identitySurvey, error) {
	answer := &identitySurvey{}
	return answer, c.do(ctx, "reading who holds what",
		http.MethodGet, "/access/identities", nil, nil, answer)
}

func (c *client) accessReviews(ctx context.Context, query url.Values) ([]accessReview, error) {
	answer := &list[accessReview]{}
	err := c.do(ctx, "listing access recertifications", http.MethodGet, "/access/reviews", query, nil, answer)
	return answer.Items, err
}

func (c *client) accessReview(ctx context.Context, name string) (*accessReview, error) {
	answer := &accessReview{}
	return answer, c.do(ctx, "reading the access recertification "+name,
		http.MethodGet, "/access/reviews/"+name, nil, nil, answer)
}

// retentionClass is one class of what the platform keeps, as the retention
// route answers it: the rule in force, and how far back the class actually
// goes.
type retentionClass struct {
	Class       string     `json:"class"`
	Label       string     `json:"label"`
	Description string     `json:"description"`
	Days        int32      `json:"days"`
	Source      string     `json:"source"`
	Enforced    bool       `json:"enforced"`
	Rows        int64      `json:"rows,omitempty"`
	Oldest      *time.Time `json:"oldest,omitempty"`
	Expired     int64      `json:"expired,omitempty"`
	Removed     int64      `json:"removed,omitempty"`
	Message     string     `json:"message,omitempty"`
}

// retentionOverride is the written decision behind an audit retention under
// the documented floor.
type retentionOverride struct {
	Reason     string `json:"reason"`
	ApprovedBy string `json:"approvedBy"`
}

// retention is the whole model.
type retention struct {
	Classes              []retentionClass   `json:"classes"`
	AuditFloorDays       int32              `json:"auditFloorDays"`
	AuditFloorOverridden bool               `json:"auditFloorOverridden"`
	AuditFloorOverride   *retentionOverride `json:"auditFloorOverride,omitempty"`
	LastSweep            *time.Time         `json:"lastSweep,omitempty"`
	Message              string             `json:"message,omitempty"`
}

// platformRetention asks how long the platform keeps each class.
func (c *client) platformRetention(ctx context.Context) (*retention, error) {
	answer := &retention{}
	return answer, c.do(ctx, "reading the platform's retention",
		http.MethodGet, "/platform/retention", nil, nil, answer)
}

func (c *client) exceptions(ctx context.Context, query url.Values) ([]exception, error) {
	answer := &list[exception]{}
	err := c.do(ctx, "listing exceptions", http.MethodGet, "/exceptions", query, nil, answer)
	return answer.Items, err
}

func (c *client) exception(ctx context.Context, name string) (*exception, error) {
	answer := &exception{}
	return answer, c.do(ctx, "reading the exception "+name, http.MethodGet, "/exceptions/"+name, nil, nil, answer)
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

// releaseConfigDiff asks what a move to `release` would change about the
// configuration `against` is running with.
func (c *client) releaseConfigDiff(ctx context.Context, release, against string) (*configDiff, error) {
	answer := &configDiff{}
	return answer, c.do(ctx, "comparing "+release+" with "+against,
		http.MethodGet, "/releases/"+release+"/config-diff", url.Values{"against": {against}}, nil, answer)
}

// moveOutcome is what moving an environment came to: the environment when the
// move was made outright, or the promotion the move became when the
// environment declares requirements — the platform answers 202 with the
// promotion, and the policy engine decides from there. Exactly one is set.
type moveOutcome struct {
	Environment *environment
	Promotion   *promotion
}

// moveEnvironment points an environment at another release. Promotion and
// rollback are the same call; which one it is depends only on which way the
// release is — and whether it happens now or through a Promotion depends on
// whether the environment sets a bar.
func (c *client) moveEnvironment(ctx context.Context, name, to string) (*moveOutcome, error) {
	raw := json.RawMessage{}
	if err := c.do(ctx, "moving "+name+" to "+to,
		http.MethodPatch, "/environments/"+name, nil, map[string]string{"release": to}, &raw); err != nil {
		return nil, err
	}
	// The two answers are told apart by a field only one of them has:
	// a promotion carries its trigger, an environment never does.
	probe := struct {
		Trigger string `json:"trigger"`
	}{}
	_ = json.Unmarshal(raw, &probe)
	out := &moveOutcome{}
	if probe.Trigger != "" {
		out.Promotion = &promotion{}
		return out, decodeJSON(raw, out.Promotion)
	}
	out.Environment = &environment{}
	return out, decodeJSON(raw, out.Environment)
}

// promote asks for a release to land on an environment: the platform creates
// a Promotion, phase Pending, and the policy engine takes it from there.
func (c *client) promote(ctx context.Context, project, environment, release, reason string) (*promotion, error) {
	body := map[string]string{"environment": environment, "release": release}
	if reason != "" {
		body["reason"] = reason
	}
	answer := &promotion{}
	return answer, c.do(ctx, "promoting "+release+" to "+environment,
		http.MethodPost, "/projects/"+project+"/promotions", nil, body, answer)
}

func (c *client) projectPromotions(ctx context.Context, project string, query url.Values) ([]promotion, error) {
	answer := &list[promotion]{}
	err := c.do(ctx, "listing "+project+"'s promotions",
		http.MethodGet, "/projects/"+project+"/promotions", query, nil, answer)
	return answer.Items, err
}

func (c *client) promotion(ctx context.Context, name string) (*promotion, error) {
	answer := &promotion{}
	return answer, c.do(ctx, "reading the promotion "+name,
		http.MethodGet, "/promotions/"+name, nil, nil, answer)
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
