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

// The cluster's own Warning events, kept.
//
// Kubernetes expires an event about an hour after it happens, which makes the
// API server's copy useless for the question people actually ask — "what
// happened at 03:00". The operator already watches these events one at a time
// for the component survey; recording them turns that watch into a history.
//
// This is not the activity feed. That table is the platform's story — releases
// moving, builds finishing — written by the reconcilers about things Kitchen
// did. This one is the cluster's, written about things that happened to it:
// FailedScheduling, FailedCreate, FailedMount, OOMKilling.

// DefaultK8sEventLimit is how many entries a query returns when it does not
// ask for a number, and MaxK8sEventLimit the ceiling.
const (
	DefaultK8sEventLimit = 100
	MaxK8sEventLimit     = 1000
)

// K8sEvent is one Warning the cluster raised, deduplicated the way Kubernetes
// deduplicates it: repeated occurrences are one event with a count, not a row
// each.
//
// Project and Environment are the operator's attribution from the involved
// object's namespace and labels, and are empty for platform and cluster
// objects — which is the interesting case as often as not, since the events
// that explain an install that never came up belong to no project.
type K8sEvent struct {
	Timestamp   time.Time `json:"timestamp"`
	Project     string    `json:"project,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Namespace   string    `json:"namespace,omitempty"`
	// Kind and Name are the involved object's: Pod, DaemonSet, PersistentVolumeClaim.
	Kind string `json:"kind,omitempty"`
	Name string `json:"name,omitempty"`
	// Reason is the machine-readable cause — FailedScheduling, FailedCreate —
	// and is what the events explorer facets on.
	Reason  string `json:"reason"`
	Message string `json:"message"`
	// Count is how many times the cluster reported this event before it was
	// recorded.
	Count uint32 `json:"count"`
	Node  string `json:"node,omitempty"`
}

// k8sEventRow is the wire shape coming back. The timestamp is a string under
// `ts`; see timestampAlias for why it is not called `timestamp`.
type k8sEventRow struct {
	Timestamp   string `json:"ts"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Namespace   string `json:"namespace"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Reason      string `json:"reason"`
	Message     string `json:"message"`
	Count       uint32 `json:"count"`
	Node        string `json:"node"`
}

// InsertK8sEvents writes a batch of cluster events. A zero timestamp means
// now, because an event the API server never stamped still happened when it
// was seen.
func (c *Client) InsertK8sEvents(ctx context.Context, events []K8sEvent) error {
	if len(events) == 0 {
		return nil
	}
	rows := make([]string, 0, len(events))
	for _, event := range events {
		timestamp := event.Timestamp
		if timestamp.IsZero() {
			timestamp = time.Now()
		}
		row, err := json.Marshal(map[string]any{
			"timestamp":   timestamp.UTC().Format("2006-01-02 15:04:05.000"),
			"project":     event.Project,
			"environment": event.Environment,
			"namespace":   event.Namespace,
			"kind":        event.Kind,
			"name":        event.Name,
			"reason":      event.Reason,
			"message":     event.Message,
			"count":       event.Count,
			"node":        event.Node,
		})
		if err != nil {
			return err
		}
		rows = append(rows, string(row))
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(K8sEventsTable), strings.Join(rows, "\n"))
	return c.Exec(ctx, statement)
}

// K8sEventQuery selects cluster events, newest first. The zero value answers
// the last hour cluster-wide, which is the operator's question; a project
// scope is the developer's.
type K8sEventQuery struct {
	// Project and Environment narrow to one application's events. An empty
	// Project is every event including the platform's own, which is not the
	// same as a project named "".
	Project     string
	Environment string
	// Namespace, Kind, Name and Reason are the explorer's facets, and the deep
	// link from a workload row is Kind plus Name.
	Namespace string
	Kind      string
	Name      string
	Reason    string
	// Search keeps events whose message contains it, case-insensitively.
	Search string
	// Since and Until bound the window. A zero Until means now; a zero Since
	// means an hour before Until.
	Since time.Time
	Until time.Time
	Limit int
}

// QueryK8sEvents reads the cluster's event history, newest first.
func (c *Client) QueryK8sEvents(ctx context.Context, query K8sEventQuery) ([]K8sEvent, error) {
	since, until, err := resolveWindow(query.Since, query.Until)
	if err != nil {
		return nil, err
	}
	limit := query.Limit
	if limit < 1 {
		limit = DefaultK8sEventLimit
	}
	if limit > MaxK8sEventLimit {
		limit = MaxK8sEventLimit
	}

	conditions := []string{
		"timestamp >= parseDateTime64BestEffort({since:String}, 3, 'UTC')",
		"timestamp <= parseDateTime64BestEffort({until:String}, 3, 'UTC')",
	}
	params := map[string]string{
		"since": since.Format(time.RFC3339Nano),
		"until": until.Format(time.RFC3339Nano),
		"limit": strconv.Itoa(limit),
	}
	filter := func(column, name, value string) {
		if value == "" {
			return
		}
		conditions = append(conditions, fmt.Sprintf("%s = {%s:String}", quoteIdentifier(column), name))
		params[name] = value
	}
	filter("project", "project", query.Project)
	filter("environment", "environment", query.Environment)
	filter("namespace", "namespace", query.Namespace)
	filter("kind", "kind", query.Kind)
	filter("name", "name", query.Name)
	filter("reason", "reason", query.Reason)
	if query.Search != "" {
		conditions = append(conditions, "positionCaseInsensitive(message, {search:String}) > 0")
		params["search"] = query.Search
	}

	statement := fmt.Sprintf(`SELECT
    formatDateTime(timestamp, '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS %s,
    project, environment, namespace, kind, name, reason, message, count, node
FROM %s.%s
WHERE %s
ORDER BY timestamp DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		timestampAlias, quoteIdentifier(c.cfg.Database), quoteIdentifier(K8sEventsTable),
		strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	events := make([]K8sEvent, 0, limit)
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := k8sEventRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable cluster event row: %w", err)
		}
		timestamp, err := time.Parse(otelTimestampLayout, row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("unreadable cluster event timestamp %q: %w", row.Timestamp, err)
		}
		events = append(events, K8sEvent{
			Timestamp:   timestamp,
			Project:     row.Project,
			Environment: row.Environment,
			Namespace:   row.Namespace,
			Kind:        row.Kind,
			Name:        row.Name,
			Reason:      row.Reason,
			Message:     row.Message,
			Count:       row.Count,
			Node:        row.Node,
		})
	}
	return events, nil
}
