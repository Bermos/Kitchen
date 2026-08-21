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
