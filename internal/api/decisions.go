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

package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/uuid"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
)

// The decision register's REST surface: what the policy engine decided, one
// decision whole, a replay that proves a stored decision reproduces, and the
// bundles an environment owner can require.
//
// Decisions live in the store, not the cluster, so the project-scoping the
// guard table does for cluster objects is done here instead: the list is
// scope-filtered the way the audit log is, and the single-decision reads
// resolve the decision's own project and answer a caller with no role on it
// the same not-found a missing decision gets. A decision about no project —
// there are none today, but a row is data — is the operator's, like every
// other project-less record.

// decisionBody is one stored decision as the dashboard and the CLI read it.
// The fired rules pass through verbatim — they are the engine's own encoding,
// [{rule, message, waived, exception}] — and the full input appears only on
// the single-decision read, where the caller asked to see one whole.
type decisionBody struct {
	ID           string          `json:"id"`
	Timestamp    time.Time       `json:"timestamp"`
	Kind         string          `json:"kind"`
	Project      string          `json:"project,omitempty"`
	Environment  string          `json:"environment,omitempty"`
	Release      string          `json:"release,omitempty"`
	Artifact     string          `json:"artifact,omitempty"`
	BundleDigest string          `json:"bundleDigest"`
	InputDigest  string          `json:"inputDigest"`
	DataSnapshot string          `json:"dataSnapshot,omitempty"`
	Verdict      string          `json:"verdict"`
	RulesFired   json.RawMessage `json:"rulesFired,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	DecidedBy    string          `json:"decidedBy,omitempty"`
}

func decisionView(decision clickhouse.Decision, whole bool) decisionBody {
	body := decisionBody{
		ID:           decision.ID,
		Timestamp:    decision.Timestamp,
		Kind:         decision.Kind,
		Project:      decision.Project,
		Environment:  decision.Environment,
		Release:      decision.Release,
		Artifact:     decision.Artifact,
		BundleDigest: decision.BundleDigest,
		InputDigest:  decision.InputDigest,
		DataSnapshot: decision.DataSnapshot,
		Verdict:      decision.Verdict,
		RulesFired:   rawIfValid(decision.RulesFired),
		DecidedBy:    decision.DecidedBy,
	}
	if whole {
		body.Input = rawIfValid(decision.Input)
	}
	return body
}

// rawIfValid passes stored JSON through verbatim, and refuses to let a
// corrupt column break the whole answer: an unreadable value is served as a
// JSON string of itself, visible rather than either hidden or fatal.
func rawIfValid(stored string) json.RawMessage {
	if stored == "" {
		return nil
	}
	if json.Valid([]byte(stored)) {
		return json.RawMessage(stored)
	}
	quoted, err := json.Marshal(stored)
	if err != nil {
		return nil
	}
	return quoted
}

// listDecisions serves a page of the decision register, newest first,
// filtered like the audit log: by the pair, the artifact, the outcome, the
// question, and a window — and always by what the caller may see.
func (s *Server) listDecisions(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	since, until, err := windowFrom(req)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	limit, err := intParam(req, "limit", clickhouse.DefaultDecisionLimit)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	project := strings.TrimSpace(req.URL.Query().Get("project"))
	if !s.visibleProject(w, req, project) {
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	decisions, err := store.QueryDecisions(ctx, clickhouse.DecisionQuery{
		Project:     project,
		Environment: strings.TrimSpace(req.URL.Query().Get("environment")),
		Release:     strings.TrimSpace(req.URL.Query().Get("release")),
		Verdict:     strings.TrimSpace(req.URL.Query().Get("verdict")),
		Kind:        strings.TrimSpace(req.URL.Query().Get("kind")),
		Since:       since,
		Until:       until,
		Limit:       limit,
	})
	if err != nil {
		s.writeStoreError(w, err, "the decision query")
		return
	}

	scope := scopeFrom(ctx)
	body := make([]decisionBody, 0, len(decisions))
	for _, decision := range decisions {
		if !scope.allows(decision.Project) {
			continue
		}
		body = append(body, decisionView(decision, false))
	}
	writeList(w, body)
}

// loadVisibleDecision reads one decision and applies the visibility rule the
// guard table cannot: no role on the decision's project reads as the same
// not-found a missing id gets. A nil return means the response is written.
func (s *Server) loadVisibleDecision(w http.ResponseWriter, req *http.Request) *clickhouse.Decision {
	ctx := req.Context()
	id := req.PathValue("id")

	store := s.openLogStore(w, req)
	if store == nil {
		return nil
	}
	decision, found, err := store.Decision(ctx, id)
	if err != nil {
		s.writeStoreError(w, err, "the decision query")
		return nil
	}
	if !found || !scopeFrom(ctx).allows(decision.Project) {
		s.writeError(w, apierrors.NewNotFound(
			schema.GroupResource{Group: kitchenv1alpha1.GroupVersion.Group, Resource: "decisions"}, id))
		return nil
	}
	return &decision
}

// getDecision serves one decision whole: the verdict, every fired rule, and
// the full input it can be replayed from.
func (s *Server) getDecision(w http.ResponseWriter, req *http.Request) {
	decision := s.loadVisibleDecision(w, req)
	if decision == nil {
		return
	}
	writeJSON(w, http.StatusOK, decisionView(*decision, true))
}

// replayBody is what a replay answers: both verdicts side by side, what fired
// on the re-evaluation, and the one bit the endpoint exists for.
type replayBody struct {
	Original struct {
		Verdict string `json:"verdict"`
	} `json:"original"`
	Replay struct {
		Verdict string          `json:"verdict"`
		Fired   json.RawMessage `json:"fired"`
	} `json:"replay"`
	Match    bool   `json:"match"`
	Decision string `json:"decision"`
}

// replayDecision re-evaluates a stored decision from its stored inputs — the
// exact bundle bytes and the exact input the original cited — and stores the
// outcome as a decision of kind replay, so the check itself has a record.
//
// Who may: a developer on the decision's project, or an operator. Not a
// viewer, deliberately: a replay writes a row and an audit record, and
// writing in a project's name is what developer means everywhere else on
// this API. The role is enforced here rather than in the table because the
// project lives on the stored row, which no table resolver can reach.
func (s *Server) replayDecision(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	decision := s.loadVisibleDecision(w, req)
	if decision == nil {
		return
	}

	caller, _ := CallerFrom(ctx)
	var project *kitchenv1alpha1.Project
	if decision.Project != "" {
		found := &kitchenv1alpha1.Project{}
		if err := s.get(ctx, decision.Project, found); err == nil {
			project = found
		}
	}
	role := access.ProjectRoleFor(caller.access(), kitchenFrom(ctx), project)
	if !role.AtLeast(access.ProjectDeveloper) {
		forbidden(w, fmt.Sprintf(
			"replaying a decision writes a decision of its own, which needs developer on %s; you have %s",
			decision.Project, role))
		return
	}

	// The stored input is the whole of what the engine may see, exactly as it
	// was seen the first time.
	input := policy.Input{}
	if err := json.Unmarshal([]byte(decision.Input), &input); err != nil {
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "decision " + decision.ID + " cannot be replayed: its stored input is unreadable: " + err.Error(),
		})
		return
	}

	bundle, err := s.storedBundle(w, req, decision.BundleDigest)
	if bundle == nil {
		if err != nil {
			writeJSON(w, http.StatusConflict, errorBody{
				Error: "decision " + decision.ID + " cannot be replayed: " + err.Error(),
			})
		}
		return
	}

	result, err := policy.Evaluate(ctx, bundle, input)
	if err != nil {
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "decision " + decision.ID + " could not be re-evaluated: " + err.Error(),
		})
		return
	}
	rulesFired, err := json.Marshal(result.Fired)
	if err != nil {
		s.writeError(w, err)
		return
	}

	replay := clickhouse.Decision{
		ID:        string(uuid.NewUUID()),
		Timestamp: time.Now().UTC(),
		Kind:      policy.KindReplay,
		Project:   decision.Project, Environment: decision.Environment,
		Release: decision.Release, Artifact: decision.Artifact,
		BundleDigest: decision.BundleDigest,
		InputDigest:  decision.InputDigest,
		DataSnapshot: decision.DataSnapshot,
		Verdict:      result.Verdict,
		RulesFired:   string(rulesFired),
		Input:        decision.Input,
		DecidedBy:    callerName(caller),
	}
	match := result.Verdict == decision.Verdict

	// The audit record before the row, like every write here. The object is
	// the decision itself — there may be nothing left in the cluster to name.
	about := &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{Name: decision.ID, Namespace: s.Namespace},
	}
	if !s.recorded(w, req, audit.Transition{
		Object:      about,
		Kind:        audit.KindPromotionDecision,
		Operation:   clickhouse.AuditCreate,
		To:          result.Verdict,
		Project:     decision.Project,
		Correlation: replay.ID,
		Reason:      "decision " + decision.ID + " replayed from its stored inputs",
		Details: map[string]any{
			"decisionID":      replay.ID,
			"replayOf":        decision.ID,
			"kind":            policy.KindReplay,
			"verdict":         result.Verdict,
			"originalVerdict": decision.Verdict,
			"match":           match,
			"bundleDigest":    decision.BundleDigest,
			"inputDigest":     decision.InputDigest,
		},
	}) {
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	if err := store.InsertDecision(ctx, replay); err != nil {
		s.writeStoreError(w, err, "the replay's decision insert")
		return
	}

	s.log().Info("decision replayed through the api",
		"decision", decision.ID, "replay", replay.ID, "match", match, "caller", callerName(caller))

	body := replayBody{Match: match, Decision: replay.ID}
	body.Original.Verdict = decision.Verdict
	body.Replay.Verdict = result.Verdict
	body.Replay.Fired = rulesFired
	writeJSON(w, http.StatusCreated, body)
}

// storedBundle loads the bundle a decision cited, by digest: from the store
// first — that is what outlives ConfigMaps — and falling back to the live
// sources, which is safe because a digest names content wherever it is found.
// A nil return with a nil error means the response is already written; a nil
// return with an error leaves the wording to the caller.
func (s *Server) storedBundle(
	w http.ResponseWriter, req *http.Request, digest string,
) (policy.Bundle, error) {
	store := s.openLogStore(w, req)
	if store == nil {
		return nil, nil
	}
	content, found, err := store.PolicyBundle(req.Context(), digest)
	if err != nil {
		s.writeStoreError(w, err, "the policy bundle read")
		return nil, nil
	}
	if found {
		bundle := policy.Bundle{}
		if err := json.Unmarshal([]byte(content), &bundle); err != nil {
			return nil, fmt.Errorf("the stored policy bundle %s is unreadable: %s", digest, err.Error())
		}
		return bundle, nil
	}
	resolver := &policy.Resolver{Client: s.Client, Namespace: s.Namespace}
	info, err := resolver.Resolve(req.Context(), digest)
	if err != nil {
		return nil, fmt.Errorf("the policy bundle %s is neither in the decision store nor still available: %s",
			digest, err.Error())
	}
	return info.Bundle, nil
}

// bundleBody is one available policy bundle: what an environment owner pins.
type bundleBody struct {
	Digest string   `json:"digest"`
	Source string   `json:"source"`
	Rules  []string `json:"rules"`
}

// listPolicyBundles serves the bundles currently available to require: the
// built-in one and every labelled ConfigMap, each with its digest and the
// rules it can fire. This is where an environment owner finds the digest a
// requirements PATCH pins.
func (s *Server) listPolicyBundles(w http.ResponseWriter, req *http.Request) {
	resolver := &policy.Resolver{Client: s.Client, Namespace: s.Namespace}
	infos, err := resolver.List(req.Context())
	if err != nil {
		s.writeError(w, err)
		return
	}
	body := make([]bundleBody, 0, len(infos))
	for _, info := range infos {
		body = append(body, bundleBody{Digest: info.Digest, Source: info.Source, Rules: info.Rules})
	}
	writeList(w, body)
}
