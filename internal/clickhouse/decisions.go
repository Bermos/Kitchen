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

package clickhouse

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The policy engine's storage half: every decision the engine made, with the
// two digests and the input that make it reproducible, and the bundles those
// digests name so replay outlives whatever ConfigMap a bundle came from.
//
// Like the audit log, and unlike everything telemetry-shaped in this package,
// these tables are evidence: writes are waited on, rows are never rewritten,
// and retention follows the audit knob rather than the telemetry one — see
// EnsurePolicySchema.

// PromotionDecisionsTable holds one row per policy evaluation the platform
// stored: promotions, scheduled rescans and replays. Eligibility previews are
// deliberately not here — a read that decides nothing stores nothing.
const PromotionDecisionsTable = "promotion_decisions"

// PolicyBundlesTable holds every policy bundle a stored decision has cited,
// keyed by digest, inserted on first use. It is what replay evaluates
// against: the ConfigMap a bundle came from can be edited or deleted, the
// bytes a decision cited cannot.
const PolicyBundlesTable = "policy_bundles"

// Decision limits mirror the audit ones, for the same reason: rows are wide,
// and the reads that matter are narrow.
const (
	DefaultDecisionLimit = 100
	MaxDecisionLimit     = 1000
)

// Decision is one stored policy evaluation: what was asked, what was
// answered, and everything needed to ask again.
type Decision struct {
	// ID is the decision's own identity, a UUID minted at evaluation.
	ID string `json:"id"`

	Timestamp time.Time `json:"timestamp"`

	// Kind is why the engine was asked: promotion, rescan or replay.
	Kind string `json:"kind"`

	// The pair the decision is about, and the artifact by content.
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Release     string `json:"release,omitempty"`
	Artifact    string `json:"artifact,omitempty"`

	// BundleDigest and InputDigest name the two halves of the evaluation;
	// DataSnapshot identifies the dataset the evidence was produced against.
	// Together with the stored Input they are the reproduction contract.
	BundleDigest string `json:"bundleDigest"`
	InputDigest  string `json:"inputDigest"`
	DataSnapshot string `json:"dataSnapshot,omitempty"`

	// Verdict is allowed, allowed-with-exception or blocked.
	Verdict string `json:"verdict"`

	// RulesFired is the fired rules as JSON — [{rule, message, waived,
	// exception}] — and Input the full canonical input. Both are opaque to
	// this package: the engine's encoding is the contract.
	RulesFired string `json:"rulesFired"`
	Input      string `json:"input"`

	// DecidedBy is who or what asked: a controller actor for the automatic
	// kinds, the caller for a replay.
	DecidedBy string `json:"decidedBy,omitempty"`
}

// decisionRow is the wire shape, both directions.
type decisionRow struct {
	ID           string `json:"id"`
	Timestamp    string `json:"ts"`
	Kind         string `json:"kind"`
	Project      string `json:"project"`
	Environment  string `json:"environment"`
	Release      string `json:"release"`
	Artifact     string `json:"artifact"`
	BundleDigest string `json:"bundle_digest"`
	InputDigest  string `json:"input_digest"`
	DataSnapshot string `json:"data_snapshot"`
	Verdict      string `json:"verdict"`
	RulesFired   string `json:"rules_fired"`
	Input        string `json:"input"`
	DecidedBy    string `json:"decided_by"`
}

// decisionColumns is the SELECT list every read of the table uses, aliased to
// the JSON names decisionRow decodes.
const decisionColumns = `
    id,
    formatDateTime(timestamp, '%Y-%m-%dT%H:%i:%S.%fZ', 'UTC') AS ts,
    kind, project, environment, release, artifact,
    bundle_digest, input_digest, data_snapshot,
    verdict, rules_fired, input, decided_by`

// InsertDecision appends one decision. Like the audit insert and unlike every
// telemetry write, it is waited on: a decision the store refused is a
// decision the platform knows it could not keep, and says so.
//
// It is idempotent on the id, like the bundle insert is on its digest: the
// promotion path derives a deterministic id from the promotion, so a requeue
// that re-records the same decision finds its row already present and adds
// nothing — the table is a plain MergeTree, and a second row under the same
// id would read back as two decisions.
func (c *Client) InsertDecision(ctx context.Context, decision Decision) error {
	existing, err := c.QueryWithParams(ctx, fmt.Sprintf(
		"SELECT 1 FROM %s.%s WHERE id = {id:String} LIMIT 1",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(PromotionDecisionsTable)),
		map[string]string{"id": decision.ID})
	if err != nil {
		return err
	}
	if strings.TrimSpace(existing) != "" {
		return nil
	}
	timestamp := decision.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	row, err := json.Marshal(map[string]any{
		"id":            decision.ID,
		"timestamp":     timestamp.UTC().Format("2006-01-02 15:04:05.000"),
		"kind":          decision.Kind,
		"project":       decision.Project,
		"environment":   decision.Environment,
		"release":       decision.Release,
		"artifact":      decision.Artifact,
		"bundle_digest": decision.BundleDigest,
		"input_digest":  decision.InputDigest,
		"data_snapshot": decision.DataSnapshot,
		"verdict":       decision.Verdict,
		"rules_fired":   decision.RulesFired,
		"input":         decision.Input,
		"decided_by":    decision.DecidedBy,
	})
	if err != nil {
		return err
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(PromotionDecisionsTable), row)
	return c.Exec(ctx, statement)
}

// DecisionQuery selects decisions. The zero value answers the newest page of
// everything.
type DecisionQuery struct {
	// Project, Environment and Release narrow to a pair or an artifact's
	// history; Verdict and Kind to an outcome or a question.
	Project     string
	Environment string
	Release     string
	Verdict     string
	Kind        string
	// Since and Until bound the window; both are open when zero.
	Since time.Time
	Until time.Time
	// Limit caps the page, defaulting to DefaultDecisionLimit.
	Limit int
}

// QueryDecisions reads a page of decisions, newest first.
func (c *Client) QueryDecisions(ctx context.Context, query DecisionQuery) ([]Decision, error) {
	limit := query.Limit
	if limit < 1 {
		limit = DefaultDecisionLimit
	}
	if limit > MaxDecisionLimit {
		limit = MaxDecisionLimit
	}

	conditions := []string{"1 = 1"}
	params := map[string]string{"limit": strconv.Itoa(limit)}
	// Ordered rather than ranged over a map, for the reason the audit query
	// is: the same filters must build the same statement every time.
	for _, filter := range []struct{ column, value string }{
		{"project", query.Project},
		{"environment", query.Environment},
		{"release", query.Release},
		{"verdict", query.Verdict},
		{"kind", query.Kind},
	} {
		if filter.value == "" {
			continue
		}
		conditions = append(conditions, fmt.Sprintf("%s = {%s:String}", filter.column, filter.column))
		params[filter.column] = filter.value
	}
	if !query.Since.IsZero() {
		conditions = append(conditions, "timestamp >= parseDateTime64BestEffort({since:String}, 3, 'UTC')")
		params["since"] = query.Since.UTC().Format(time.RFC3339Nano)
	}
	if !query.Until.IsZero() {
		conditions = append(conditions, "timestamp <= parseDateTime64BestEffort({until:String}, 3, 'UTC')")
		params["until"] = query.Until.UTC().Format(time.RFC3339Nano)
	}

	statement := fmt.Sprintf(`SELECT %s
FROM %s.%s
WHERE %s
ORDER BY timestamp DESC, id DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`, decisionColumns,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(PromotionDecisionsTable),
		strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}
	return decodeDecisionRows(body)
}

// Decision reads one decision by id. The second return says whether it was
// there — an absent decision is an ordinary answer, not an error.
func (c *Client) Decision(ctx context.Context, id string) (Decision, bool, error) {
	statement := fmt.Sprintf(`SELECT %s
FROM %s.%s
WHERE id = {id:String}
LIMIT 1
FORMAT JSONEachRow`, decisionColumns,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(PromotionDecisionsTable))

	body, err := c.QueryWithParams(ctx, statement, map[string]string{"id": id})
	if err != nil {
		return Decision{}, false, err
	}
	decisions, err := decodeDecisionRows(body)
	if err != nil || len(decisions) == 0 {
		return Decision{}, false, err
	}
	return decisions[0], true, nil
}

// InsertPolicyBundle records a bundle's content under its digest, once: a
// digest already present is left exactly as first seen, which is the point of
// content addressing — there is nothing new to say about the same bytes.
func (c *Client) InsertPolicyBundle(ctx context.Context, digest, content string) error {
	existing, err := c.QueryWithParams(ctx, fmt.Sprintf(
		"SELECT 1 FROM %s.%s WHERE digest = {digest:String} LIMIT 1",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(PolicyBundlesTable)),
		map[string]string{"digest": digest})
	if err != nil {
		return err
	}
	if strings.TrimSpace(existing) != "" {
		return nil
	}
	row, err := json.Marshal(map[string]any{
		"digest":     digest,
		"content":    content,
		"first_seen": time.Now().UTC().Format("2006-01-02 15:04:05.000"),
	})
	if err != nil {
		return err
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(PolicyBundlesTable), row)
	return c.Exec(ctx, statement)
}

// PolicyBundle reads a stored bundle's content by digest. The second return
// says whether the store holds it.
func (c *Client) PolicyBundle(ctx context.Context, digest string) (string, bool, error) {
	statement := fmt.Sprintf(`SELECT content
FROM %s.%s
WHERE digest = {digest:String}
LIMIT 1
FORMAT JSONEachRow`, quoteIdentifier(c.cfg.Database), quoteIdentifier(PolicyBundlesTable))

	body, err := c.QueryWithParams(ctx, statement, map[string]string{"digest": digest})
	if err != nil {
		return "", false, err
	}
	line := strings.TrimSpace(body)
	if line == "" {
		return "", false, nil
	}
	row := struct {
		Content string `json:"content"`
	}{}
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		return "", false, fmt.Errorf("unreadable policy bundle row: %w", err)
	}
	return row.Content, true, nil
}

func decodeDecisionRows(body string) ([]Decision, error) {
	decisions := make([]Decision, 0, 16)
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := decisionRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable decision row: %w", err)
		}
		timestamp, err := time.Parse("2006-01-02T15:04:05.999Z", row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("unreadable decision timestamp %q: %w", row.Timestamp, err)
		}
		decisions = append(decisions, Decision{
			ID:           row.ID,
			Timestamp:    timestamp,
			Kind:         row.Kind,
			Project:      row.Project,
			Environment:  row.Environment,
			Release:      row.Release,
			Artifact:     row.Artifact,
			BundleDigest: row.BundleDigest,
			InputDigest:  row.InputDigest,
			DataSnapshot: row.DataSnapshot,
			Verdict:      row.Verdict,
			RulesFired:   row.RulesFired,
			Input:        row.Input,
			DecidedBy:    row.DecidedBy,
		})
	}
	return decisions, nil
}
