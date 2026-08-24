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

// Signed records: DSSE envelopes the platform minted about things that have
// no OCI registry to live in. An artifact's evidence is attached to its
// digest and the registry is its store; a resource claim's data-class
// declaration, or a recertification cycle's closing artefact (#139), has no
// digest anywhere — so the envelope itself is kept here, whole, and the
// subject digest inside it is what ties it back to the thing it describes.
//
// Like the audit log and the decision register, these rows are evidence:
// the write is waited on, rows are never rewritten, and the table carries no
// TTL at all — a signed statement is kept as long as anything might cite it,
// and there are few enough of them that retention is not a disk question.

// SignedRecordsTable holds one row per envelope, by the platform's own id.
const SignedRecordsTable = "signed_records"

// SignedRecord is one kept envelope: when it was minted, what kind of
// statement it is (the in-toto predicate type), whose identity it describes
// (the statement's subject digest), which project it belongs to, and the
// DSSE envelope verbatim.
type SignedRecord struct {
	// ID is the record's own identity, a UUID minted when the statement is.
	ID string `json:"id"`

	Timestamp time.Time `json:"timestamp"`

	// Type is the statement's predicate type URI — what kind of claim this
	// is, in the same vocabulary every attestation uses.
	Type string `json:"type"`

	// Subject is the statement's subject digest (`sha256:<hex>`), which for a
	// resource claim is its identity digest rather than any image's.
	Subject string `json:"subject"`

	// Project scopes the record the way every store row is scoped: members
	// read their projects' records, project-less rows are the operator's.
	Project string `json:"project,omitempty"`

	// Envelope is the DSSE envelope as JSON, verbatim — the payload bytes
	// inside it are what the signature covers, so nothing here is ever
	// re-encoded.
	Envelope string `json:"envelope"`
}

// EnsureRecordsSchema creates the signed-records table. It sits beside
// EnsurePolicySchema — the compliance reconcile runs both — and takes no
// retention, because the table has no TTL on purpose: see the package note.
func (c *Client) EnsureRecordsSchema(ctx context.Context) error {
	return c.Exec(ctx, createSignedRecordsTable(c.cfg.Database))
}

// InsertSignedRecord appends one envelope. Waited on, like the audit and
// decision inserts: a record the store refused is one the caller knows it
// could not keep, and says so.
func (c *Client) InsertSignedRecord(ctx context.Context, record SignedRecord) error {
	timestamp := record.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	row, err := json.Marshal(map[string]any{
		"id":        record.ID,
		"timestamp": timestamp.UTC().Format("2006-01-02 15:04:05.000"),
		"type":      record.Type,
		"subject":   record.Subject,
		"project":   record.Project,
		"envelope":  record.Envelope,
	})
	if err != nil {
		return err
	}
	statement := fmt.Sprintf("INSERT INTO %s.%s FORMAT JSONEachRow\n%s",
		quoteIdentifier(c.cfg.Database), quoteIdentifier(SignedRecordsTable), row)
	return c.Exec(ctx, statement)
}

// createSignedRecordsTable is the schema. The ordering key leads with the
// subject because "every declaration about this claim" is the read an audit
// pack makes; type and time narrow it. Nothing nullable, nothing defaulted —
// a column the writer forgot must not read back as a plausible empty string.
func createSignedRecordsTable(database string) string {
	return fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
(
    id        String,
    timestamp DateTime64(3, 'UTC'),
    type      LowCardinality(String),
    subject   String,
    project   LowCardinality(String),
    envelope  String,
    INDEX idx_project project TYPE bloom_filter(0.01) GRANULARITY 1
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(timestamp)
ORDER BY (subject, type, timestamp)`,
		quoteIdentifier(database), quoteIdentifier(SignedRecordsTable))
}

// Signed-record limits, mirroring the decision register's for the same
// reason: the envelope column is wide, and the reads that matter are narrow.
const (
	DefaultSignedRecordLimit = 100
	MaxSignedRecordLimit     = 1000
)

// SignedRecordQuery selects kept envelopes. The zero value answers the newest
// page of everything, and every field is an equality filter — the reads this
// table serves are "this claim's declarations", "every access review's
// artefact", "this project's evidence", and an audit pack asking for all
// three of them at once.
type SignedRecordQuery struct {
	// Subject narrows to one identity digest, Type to one predicate, and
	// Project to one project's records. A record with no project is about
	// the platform — a platform-scoped recertification cycle — and is
	// answered only when Project is empty.
	Subject string
	Type    string
	Project string
	// Since and Until bound the window; both are open when zero.
	Since time.Time
	Until time.Time
	// Limit caps the page, defaulting to DefaultSignedRecordLimit.
	Limit int
}

// signedRecordColumns is the SELECT list every read of the table uses, aliased
// to the JSON names signedRecordRow decodes. The timestamp is formatted rather
// than returned raw for the reason the decision register's is: the store's own
// rendering of a DateTime64 is not the one time.Parse reads.
const signedRecordColumns = `
    id,
    formatDateTime(timestamp, '%Y-%m-%dT%H:%i:%S.%fZ', 'UTC') AS ts,
    type, subject, project, envelope`

// signedRecordRow is the wire shape coming back.
type signedRecordRow struct {
	ID        string `json:"id"`
	Timestamp string `json:"ts"`
	Type      string `json:"type"`
	Subject   string `json:"subject"`
	Project   string `json:"project"`
	Envelope  string `json:"envelope"`
}

// QuerySignedRecords reads a page of kept envelopes, newest first.
//
// The order is (timestamp, id) descending rather than the table's own
// ordering key, because every caller wants "the most recent statements about
// this" and an audit pack sorts what it keeps for itself anyway — a stable
// total order is the export's problem, not the store's.
func (c *Client) QuerySignedRecords(ctx context.Context, query SignedRecordQuery) ([]SignedRecord, error) {
	limit := query.Limit
	if limit < 1 {
		limit = DefaultSignedRecordLimit
	}
	if limit > MaxSignedRecordLimit {
		limit = MaxSignedRecordLimit
	}

	conditions := []string{"1 = 1"}
	params := map[string]string{"limit": strconv.Itoa(limit)}
	// Ordered rather than ranged over a map, for the reason the audit and
	// decision queries are: the same filters must build the same statement
	// every time.
	for _, filter := range []struct{ column, value string }{
		{"subject", query.Subject},
		{"type", query.Type},
		{"project", query.Project},
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
		conditions = append(conditions, "timestamp < parseDateTime64BestEffort({until:String}, 3, 'UTC')")
		params["until"] = query.Until.UTC().Format(time.RFC3339Nano)
	}

	statement := fmt.Sprintf(`SELECT %s
FROM %s.%s
WHERE %s
ORDER BY timestamp DESC, id DESC
LIMIT {limit:UInt32}
FORMAT JSONEachRow`, signedRecordColumns,
		quoteIdentifier(c.cfg.Database), quoteIdentifier(SignedRecordsTable),
		strings.Join(conditions, " AND "))

	body, err := c.QueryWithParams(ctx, statement, params)
	if err != nil {
		return nil, err
	}
	return decodeSignedRecordRows(body)
}

// decodeSignedRecordRows reads the JSONEachRow answer.
func decodeSignedRecordRows(body string) ([]SignedRecord, error) {
	records := make([]SignedRecord, 0, 16)
	for _, raw := range strings.Split(body, "\n") {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		row := signedRecordRow{}
		if err := json.Unmarshal([]byte(raw), &row); err != nil {
			return nil, fmt.Errorf("unreadable signed record row: %w", err)
		}
		timestamp, err := time.Parse("2006-01-02T15:04:05.999Z", row.Timestamp)
		if err != nil {
			return nil, fmt.Errorf("unreadable signed record timestamp %q: %w", row.Timestamp, err)
		}
		records = append(records, SignedRecord{
			ID:        row.ID,
			Timestamp: timestamp,
			Type:      row.Type,
			Subject:   row.Subject,
			Project:   row.Project,
			Envelope:  row.Envelope,
		})
	}
	return records, nil
}
