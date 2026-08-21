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

// Event types, as written into the `type` column. The vocabulary is the
// activity feed's: what a person scanning "what happened recently" wants to
// read, not the reconcilers' internal state machine.
const (
	EventBuildSucceeded    = "build.succeeded"
	EventBuildFailed       = "build.failed"
	EventReleasePromoted   = "release.promoted"
	EventReleaseRolledBack = "release.rolledBack"
	EventReleasePruned     = "release.pruned"
	EventPreviewCreated    = "preview.created"
	EventPreviewRemoved    = "preview.removed"
	EventClaimBound        = "claim.bound"
	EventClaimFailed       = "claim.failed"
	EventClaimCreated      = "claim.created"
	EventClaimDeleted      = "claim.deleted"
	EventProjectCreated    = "project.created"
	EventProjectDeleted    = "project.deleted"
	EventDomainAttached    = "domain.attached"
	EventDomainRemoved     = "domain.removed"

	// Break-glass exceptions. Granting one is exactly the kind of thing a
	// person scanning "what happened recently" should trip over.
	EventExceptionGranted  = "exception.granted"
	EventExceptionResolved = "exception.resolved"

	// The platform's own upgrades. They name no project, environment or
	// release because they are about the installation itself — which is
	// precisely why they are worth reading in the same feed: an app that
	// changed behaviour at the same moment the platform did is a question the
	// feed can now answer.
	EventPlatformUpdated      = "platform.updated"
	EventPlatformUpdateFailed = "platform.updateFailed"
)

// DefaultEventLimit is how many entries a feed request returns when it does
// not ask for a number, and MaxEventLimit the ceiling on what it may ask for.
const (
	DefaultEventLimit = 50
	MaxEventLimit     = 500
)

// Event is one entry of the platform's activity: a release moving, a build
// finishing, a preview coming or going. The object fields name what the entry
// is about so a feed can link to it; empty ones are simply not involved.
type Event struct {
	Timestamp   time.Time `json:"timestamp"`
	Type        string    `json:"type"`
	Project     string    `json:"project,omitempty"`
	Environment string    `json:"environment,omitempty"`
	Build       string    `json:"build,omitempty"`
	Release     string    `json:"release,omitempty"`
	Claim       string    `json:"claim,omitempty"`
	Message     string    `json:"message"`
	// Actor is who caused it: an authenticated API caller by name, or
	// "operator" for things the reconcilers decided on their own.
	Actor string `json:"actor,omitempty"`
	// Value carries the one number some events have — a finished build's
	// duration in seconds.
	Value float64 `json:"value,omitempty"`
}

// eventRow is the wire shape, both directions: JSONEachRow in and out. The
// timestamp is a string for the same reason logRow's is (see timestampAlias).
type eventRow struct {
	Timestamp   string  `json:"ts"`
	Type        string  `json:"type"`
	Project     string  `json:"project"`
	Environment string  `json:"environment"`
	Build       string  `json:"build"`
	Release     string  `json:"release"`
	Claim       string  `json:"claim"`
	Message     string  `json:"message"`
	Actor       string  `json:"actor"`
	Value       float64 `json:"value"`
}

// InsertEvent writes one activity entry. A zero timestamp means "now".
func (c *Client) InsertEvent(ctx context.Context, event Event) error {
	timestamp := event.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	row, err := json.Marshal(map[string]any{
		"timestamp":   timestamp.UTC().Format("2006-01-02 15:04:05.000"),
		"type":        event.Type,
		"project":     event.Project,
		"environment": event.Environment,
		"build":       event.Build,
		"release":     event.Release,
		"claim":       event.Claim,
		"message":     event.Message,
		"actor":       event.Actor,
		"value":       event.Value,
	})
	if err != nil {
		return err
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(EventsTable), row)
	return c.Exec(ctx, statement)
}

// EventQuery selects activity entries, newest first. The zero value is valid
// and answers the whole feed: unlike logs, the events table is small enough
// that "everything recent" is a reasonable question.
type EventQuery struct {
	// Project narrows the feed to one project's activity.
	Project string
	// Since bounds the window. Zero is an open start.
	Since time.Time
	// Limit caps the number of entries, defaulting to DefaultEventLimit.
	Limit int
}

// QueryEvents reads the activity feed, newest first.
func (c *Client) QueryEvents(ctx context.Context, query EventQuery) ([]Event, error) {
	limit := query.Limit
	if limit < 1 {
		limit = DefaultEventLimit
	}
	if limit > MaxEventLimit {
		limit = MaxEventLimit
	}

	conditions := []string{"1 = 1"}
	params := map[string]string{"limit": strconv.Itoa(limit)}
	if query.Project != "" {
		conditions = append(conditions, "project = {project:String}")
		params["project"] = query.Project
	}
	if !query.Since.IsZero() {
		conditions = append(conditions, "timestamp >= parseDateTime64BestEffort({since:String}, 3, 'UTC')")
		params["since"] = query.Since.UTC().Format(time.RFC3339Nano)
	}

	statement := fmt.Sprintf(`SELECT
    formatDateTime(timestamp, '%%Y-%%m-%%dT%%H:%%i:%%S.%%fZ', 'UTC') AS %s,
    type, project, environment, build, release, claim, message, actor, value
FROM %s.%s
WHERE %s
ORDER BY timestamp DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`,
		timestampAlias, quoteIdentifier(c.cfg.Database), quoteIdentifier(EventsTable), strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}

	events := make([]Event, 0, limit)
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := eventRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable event row: %w", err)
		}
		timestamp, err := time.Parse("2006-01-02T15:04:05.999Z", row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("unreadable event timestamp %q: %w", row.Timestamp, err)
		}
		events = append(events, Event{
			Timestamp:   timestamp,
			Type:        row.Type,
			Project:     row.Project,
			Environment: row.Environment,
			Build:       row.Build,
			Release:     row.Release,
			Claim:       row.Claim,
			Message:     row.Message,
			Actor:       row.Actor,
			Value:       row.Value,
		})
	}
	return events, nil
}
