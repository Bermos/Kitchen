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

// The audit log's storage half: the row, the append, and the two reads —
// a filtered page for the audit view, and an ordered scan for the chain
// verifier. The chain itself (what is hashed, and what a broken link means)
// lives in internal/audit, because hashing is not storage.
//
// It is not EventsTable. The activity feed is an account written for a person
// scanning what happened lately: best-effort, prose-shaped, and dropped
// silently when the store is down. This is evidence — every row is chained to
// the one before it, an append that fails fails the operation that caused it,
// and nothing rewrites a row once written. Two tables because they answer to
// two different standards, not because they hold different facts.

// Audit operations, as written into the `operation` column. They describe what
// happened to the object, not which client library call made it happen.
const (
	// AuditCreate is the first time the platform saw the object.
	AuditCreate = "create"
	// AuditUpdate is a change to an object that already existed — the
	// catch-all for spec edits that are not a phase move.
	AuditUpdate = "update"
	// AuditTransition is a move between two states of the object's own
	// lifecycle: a Build going Running, an Environment taking a new Release.
	AuditTransition = "transition"
	// AuditDelete is the object going away, recorded while it still exists
	// so that what was deleted is in the record rather than inferred.
	AuditDelete = "delete"
)

// Actor kinds. An actor is always one of these two and never "the operator":
// a transition the platform decided on its own is still attributable, to the
// named controller that decided it.
const (
	// ActorUser is a human, identified by whatever the identity provider
	// says they are — an email where the token carries one, the subject
	// otherwise.
	ActorUser = "user"
	// ActorService is a platform component, named as
	// `system:controller/<name>`.
	ActorService = "service"
)

// DefaultAuditLimit is how many records an audit query returns when it does
// not ask for a number, and MaxAuditLimit the ceiling on what it may ask for.
// Both are lower than the activity feed's: a record is wide, and the reads
// that matter are narrow ones about a single object.
const (
	DefaultAuditLimit = 100
	MaxAuditLimit     = 1000
)

// AuditRecord is one entry of the tamper-evident log: who moved what, from
// which state to which, and where it sits in the chain.
//
// Sequence and the two hashes are the chain; everything above them is the
// fact being recorded. Both are stored because the chain has to be verifiable
// from the table alone, with no knowledge of how it was built.
type AuditRecord struct {
	// Sequence is the record's position in the chain, from 1. It is dense:
	// a gap is a deletion, not a skipped number.
	Sequence int64 `json:"sequence"`

	Timestamp time.Time `json:"timestamp"`

	// Actor is the platform identity behind the change and ActorKind says
	// which sort it is (ActorUser or ActorService).
	Actor     string `json:"actor"`
	ActorKind string `json:"actorKind"`

	// Correlation ties every record that came out of one cause together —
	// a push that built, released and promoted is one correlation id across
	// three objects.
	Correlation string `json:"correlation,omitempty"`

	// Operation is what happened: AuditCreate, AuditUpdate, AuditTransition
	// or AuditDelete.
	Operation string `json:"operation"`

	// The object the record is about. UID is carried because names are
	// reused: a Project deleted and created again under the same name is two
	// objects, and only the UID says so.
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	UID       string `json:"uid,omitempty"`

	// Project the object belongs to, where it belongs to one. It is the
	// filter an application team's own audit view is built on.
	Project string `json:"project,omitempty"`

	// FromState and ToState are the transition. A create has no from-state
	// and a delete no to-state.
	FromState string `json:"fromState,omitempty"`
	ToState   string `json:"toState,omitempty"`

	// Reason is the one-line account of why, in the vocabulary a person
	// reads rather than the reconciler's.
	Reason string `json:"reason,omitempty"`

	// Details is a JSON object carrying whatever else the transition is not
	// fully described without — the image a release froze, the fields a
	// patch changed. It is opaque to this package and hashed verbatim.
	Details string `json:"details,omitempty"`

	// PrevHash is the hash of the record at Sequence-1, and Hash this
	// record's own. The first record's PrevHash is GenesisHash.
	PrevHash string `json:"prevHash"`
	Hash     string `json:"hash"`
}

// auditRow is the wire shape, both directions. The timestamp is a string for
// the same reason logRow's is (see timestampAlias).
type auditRow struct {
	Sequence    string `json:"seq"`
	Timestamp   string `json:"ts"`
	Actor       string `json:"actor"`
	ActorKind   string `json:"actor_kind"`
	Correlation string `json:"correlation"`
	Operation   string `json:"operation"`
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace"`
	Name        string `json:"name"`
	UID         string `json:"uid"`
	Project     string `json:"project"`
	FromState   string `json:"from_state"`
	ToState     string `json:"to_state"`
	Reason      string `json:"reason"`
	Details     string `json:"details"`
	PrevHash    string `json:"prev_hash"`
	Hash        string `json:"hash"`
}

// auditColumns is the SELECT list every read of the table uses, aliased to the
// JSON names auditRow decodes. UInt64 is read as a string because JSON cannot
// hold one exactly; nothing else here needs converting.
const auditColumns = `
    toString(sequence) AS seq,
    formatDateTime(timestamp, '%Y-%m-%dT%H:%i:%S.%fZ', 'UTC') AS ts,
    actor, actor_kind, correlation, operation,
    kind, namespace, name, uid, project,
    from_state, to_state, reason, details, prev_hash, hash`

// InsertAuditRecord appends one record. Unlike every other write in this
// package it is expected to be waited on: the caller is holding a state
// transition it must not make unless this returns nil.
func (c *Client) InsertAuditRecord(ctx context.Context, record AuditRecord) error {
	timestamp := record.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	row, err := json.Marshal(map[string]any{
		"sequence":    record.Sequence,
		"timestamp":   timestamp.UTC().Format("2006-01-02 15:04:05.000"),
		"actor":       record.Actor,
		"actor_kind":  record.ActorKind,
		"correlation": record.Correlation,
		"operation":   record.Operation,
		"kind":        record.Kind,
		"namespace":   record.Namespace,
		"name":        record.Name,
		"uid":         record.UID,
		"project":     record.Project,
		"from_state":  record.FromState,
		"to_state":    record.ToState,
		"reason":      record.Reason,
		"details":     record.Details,
		"prev_hash":   record.PrevHash,
		"hash":        record.Hash,
	})
	if err != nil {
		return err
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(AuditTable), row)
	return c.Exec(ctx, statement)
}

// AuditHead reads the last record in the chain, which is what an appender
// needs to link the next one. A chain with no records yet answers a zero
// record and no error — that is a first append, not a fault.
func (c *Client) AuditHead(ctx context.Context) (AuditRecord, error) {
	statement := fmt.Sprintf(`SELECT %s
FROM %s.%s
ORDER BY sequence DESC
LIMIT 1
FORMAT JSONEachRow`, auditColumns, quoteIdentifier(c.cfg.Database), quoteIdentifier(AuditTable))

	body, err := c.Query(ctx, statement)
	if err != nil {
		return AuditRecord{}, err
	}
	records, err := decodeAuditRows(body)
	if err != nil {
		return AuditRecord{}, err
	}
	if len(records) == 0 {
		return AuditRecord{}, nil
	}
	return records[0], nil
}

// AuditQuery selects records. The zero value answers the newest page of the
// whole log.
type AuditQuery struct {
	// Kind, Namespace and Name narrow to one object, or to every object of
	// a kind when only Kind is given.
	Kind      string
	Namespace string
	Name      string
	// Project narrows to one project's objects.
	Project string
	// Actor narrows to what one identity did.
	Actor string
	// Since and Until bound the window; both are open when zero.
	Since time.Time
	Until time.Time
	// Limit caps the page, defaulting to DefaultAuditLimit.
	Limit int
}

// QueryAuditRecords reads a page of the log, newest first.
func (c *Client) QueryAuditRecords(ctx context.Context, query AuditQuery) ([]AuditRecord, error) {
	limit := query.Limit
	if limit < 1 {
		limit = DefaultAuditLimit
	}
	if limit > MaxAuditLimit {
		limit = MaxAuditLimit
	}

	conditions := []string{"1 = 1"}
	params := map[string]string{"limit": strconv.Itoa(limit)}
	// Ordered rather than ranged over a map: the same filters have to build
	// the same statement every time, or the query cache and the tests that
	// read the SQL both see a different query for the same question.
	for _, filter := range []struct{ column, value string }{
		{"kind", query.Kind},
		{"namespace", query.Namespace},
		{"name", query.Name},
		{"project", query.Project},
		{"actor", query.Actor},
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
ORDER BY sequence DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`, auditColumns,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(AuditTable), strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}
	return decodeAuditRows(body)
}

// ScanAuditRecords reads a contiguous run of the chain in sequence order,
// which is the only order it can be verified in. The bounds are sequence
// numbers rather than timestamps deliberately: a verifier that took a time
// window could not tell a record deleted at the edge of the window from one
// that was never in it.
//
// `from` is inclusive and 0 means the start of the chain; `limit` bounds the
// run so that verifying a year of records is a series of bounded reads rather
// than one that has to fit in memory.
func (c *Client) ScanAuditRecords(ctx context.Context, from int64, limit int) ([]AuditRecord, error) {
	if limit < 1 {
		limit = DefaultAuditLimit
	}
	if limit > MaxAuditLimit {
		limit = MaxAuditLimit
	}
	if from < 0 {
		from = 0
	}

	statement := fmt.Sprintf(`SELECT %s
FROM %s.%s
WHERE sequence >= {from:UInt64}
ORDER BY sequence ASC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`, auditColumns, quoteIdentifier(c.cfg.Database), quoteIdentifier(AuditTable))

	body, err := c.QueryWithParams(ctx, statement, map[string]string{
		"from":  strconv.FormatInt(from, 10),
		"limit": strconv.Itoa(limit),
	})
	if err != nil {
		return nil, err
	}
	return decodeAuditRows(body)
}

func decodeAuditRows(body string) ([]AuditRecord, error) {
	records := make([]AuditRecord, 0, 16)
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := auditRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable audit row: %w", err)
		}
		sequence, err := strconv.ParseInt(row.Sequence, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unreadable audit sequence %q: %w", row.Sequence, err)
		}
		timestamp, err := time.Parse("2006-01-02T15:04:05.999Z", row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("unreadable audit timestamp %q: %w", row.Timestamp, err)
		}
		records = append(records, AuditRecord{
			Sequence:    sequence,
			Timestamp:   timestamp,
			Actor:       row.Actor,
			ActorKind:   row.ActorKind,
			Correlation: row.Correlation,
			Operation:   row.Operation,
			Kind:        row.Kind,
			Namespace:   row.Namespace,
			Name:        row.Name,
			UID:         row.UID,
			Project:     row.Project,
			FromState:   row.FromState,
			ToState:     row.ToState,
			Reason:      row.Reason,
			Details:     row.Details,
			PrevHash:    row.PrevHash,
			Hash:        row.Hash,
		})
	}
	return records, nil
}
