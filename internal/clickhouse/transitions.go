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
	"strings"
	"time"
)

// The signal catalogue's history: what opened, what resolved, and when.
//
// This package deliberately knows nothing about the catalogue. A transition is
// a row here — an identity, a state, a severity and the sentences a person
// reads — and the rules that produce it, the diffing that decides when one
// happened and the audiences it is delivered to are all internal/signals'.
// Keeping the arrow one way is what lets the loop that writes these and the
// API that reads them share a store without either of them importing the
// other.

// SignalTransition is one row of the history.
type SignalTransition struct {
	At    time.Time `json:"at"`
	State string    `json:"state"`

	// Signal, Fingerprint and Audience are the identity. A transition is
	// keyed on (fingerprint, audience): one condition delivered to two
	// audiences is two rows, acknowledged and silenced separately.
	Signal      string `json:"signal"`
	Fingerprint string `json:"fingerprint"`
	Audience    string `json:"audience"`

	// Tier is what this delivery's audience is meant to do about the
	// condition — `page`, `ticket` or `log` — as the rule declared it for
	// that audience when the row was written. It is on the row for the same
	// reason Version is: a history reinterpreted against a catalogue that
	// has since moved is a history that changes what it said.
	Tier string `json:"tier,omitempty"`

	// Version is the rule's own version when the row was written.
	Version int `json:"version"`

	Severity string `json:"severity"`

	// Scope is the finding's scope kind, and the five fields under it are
	// whichever of the subject's names that kind sets.
	Scope       string `json:"scope"`
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	Node        string `json:"node,omitempty"`
	Name        string `json:"name,omitempty"`

	Title    string `json:"title"`
	Detail   string `json:"detail"`
	Evidence string `json:"evidence"`

	// Since is what the round could prove about the condition's age, and
	// OpenedAt when this platform first saw it.
	Since    time.Time `json:"since"`
	OpenedAt time.Time `json:"openedAt"`
}

// InsertSignalTransitions writes a round's changes. A round with nothing to
// say writes nothing at all, which is the normal case: the table records
// changes, not observations.
func (c *Client) InsertSignalTransitions(ctx context.Context, transitions []SignalTransition) error {
	if len(transitions) == 0 {
		return nil
	}
	rows := make([]string, 0, len(transitions))
	for _, transition := range transitions {
		at := transition.At
		if at.IsZero() {
			at = time.Now()
		}
		openedAt := transition.OpenedAt
		if openedAt.IsZero() {
			openedAt = at
		}
		since := transition.Since
		if since.IsZero() {
			since = openedAt
		}
		row, err := json.Marshal(map[string]any{
			"timestamp":   at.UTC().Format(storeTimestampLayout),
			"state":       transition.State,
			"signal":      transition.Signal,
			"version":     transition.Version,
			"fingerprint": transition.Fingerprint,
			"audience":    transition.Audience,
			"tier":        transition.Tier,
			"severity":    transition.Severity,
			"scope":       transition.Scope,
			"project":     transition.Project,
			"environment": transition.Environment,
			"namespace":   transition.Namespace,
			"node":        transition.Node,
			"name":        transition.Name,
			"title":       transition.Title,
			"detail":      transition.Detail,
			"evidence":    transition.Evidence,
			"since":       since.UTC().Format(storeTimestampLayout),
			"opened_at":   openedAt.UTC().Format(storeTimestampLayout),
		})
		if err != nil {
			return err
		}
		rows = append(rows, string(row))
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(SignalTransitionsTable), strings.Join(rows, "\n"))
	return c.Exec(ctx, statement)
}

// storeTimestampLayout is how this package writes a DateTime64(3) literal.
const storeTimestampLayout = "2006-01-02 15:04:05.000"

// OpenSignalTransitions is every condition the history says is still open: for
// each (fingerprint, audience), the newest row, kept when it is an opening.
//
// The grouping is the whole query. A condition that opened, resolved and
// opened again has three rows and one state, and the state is whichever row is
// newest — so this is an argMax per key rather than a scan for openings that
// have no closing beside them.
func (c *Client) OpenSignalTransitions(ctx context.Context) ([]SignalTransition, error) {
	statement := fmt.Sprintf(`SELECT
    formatDateTime(argMax(timestamp, timestamp), '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS %s,
    fingerprint,
    audience,
    argMax(tier, timestamp) AS tier,
    argMax(signal, timestamp) AS signal,
    argMax(version, timestamp) AS version,
    argMax(severity, timestamp) AS severity,
    argMax(scope, timestamp) AS scope,
    project,
    environment,
    argMax(namespace, timestamp) AS namespace,
    argMax(node, timestamp) AS node,
    argMax(name, timestamp) AS name,
    argMax(title, timestamp) AS title,
    argMax(detail, timestamp) AS detail,
    argMax(evidence, timestamp) AS evidence,
    formatDateTime(argMax(since, timestamp), '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS since_ts,
    formatDateTime(argMax(opened_at, timestamp), '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS opened_ts
FROM %s.%s
GROUP BY project, environment, fingerprint, audience
HAVING argMax(state, timestamp) = 'open'
ORDER BY fingerprint, audience
FORMAT JSONEachRow`,
		timestampAlias, quoteIdentifier(c.cfg.Database), quoteIdentifier(SignalTransitionsTable))

	body, err := c.Query(ctx, statement)
	if err != nil {
		return nil, err
	}
	return parseSignalTransitions(body)
}

// signalTransitionRow is the wire shape. The three timestamps come back as
// strings under aliases, for the reason timestampAlias gives: a column and an
// alias of the same name is a query that reads perfectly and answers something
// else.
type signalTransitionRow struct {
	At          string `json:"ts"`
	Fingerprint string `json:"fingerprint"`
	Audience    string `json:"audience"`
	Tier        string `json:"tier"`
	Signal      string `json:"signal"`
	Version     int    `json:"version"`
	Severity    string `json:"severity"`
	Scope       string `json:"scope"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Namespace   string `json:"namespace"`
	Node        string `json:"node"`
	Name        string `json:"name"`
	Title       string `json:"title"`
	Detail      string `json:"detail"`
	Evidence    string `json:"evidence"`
	Since       string `json:"since_ts"`
	OpenedAt    string `json:"opened_ts"`
}

func parseSignalTransitions(body string) ([]SignalTransition, error) {
	transitions := make([]SignalTransition, 0, 8)
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := signalTransitionRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable signal transition row: %w", err)
		}
		at, err := time.Parse(otelTimestampLayout, row.At)
		if err != nil {
			return nil, fmt.Errorf("unreadable signal transition timestamp %q: %w", row.At, err)
		}
		transition := SignalTransition{
			At:          at,
			State:       "open",
			Signal:      row.Signal,
			Fingerprint: row.Fingerprint,
			Audience:    row.Audience,
			Tier:        row.Tier,
			Version:     row.Version,
			Severity:    row.Severity,
			Scope:       row.Scope,
			Project:     row.Project,
			Environment: row.Environment,
			Namespace:   row.Namespace,
			Node:        row.Node,
			Name:        row.Name,
			Title:       row.Title,
			Detail:      row.Detail,
			Evidence:    row.Evidence,
		}
		if since, err := time.Parse(otelTimestampLayout, row.Since); err == nil {
			transition.Since = since
		}
		if opened, err := time.Parse(otelTimestampLayout, row.OpenedAt); err == nil {
			transition.OpenedAt = opened
		}
		transitions = append(transitions, transition)
	}
	return transitions, nil
}
