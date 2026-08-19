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

// Package audit records what the platform did to its own objects, as an
// append-only chain that cannot be edited afterwards without saying so.
//
// It is not the activity feed. That is prose for a person catching up, written
// best-effort and dropped when the store is down. This is evidence: every
// record carries the hash of the one before it, an append that fails fails the
// transition that caused it, and the only supported way to change a record is
// to append another one.
//
// # What the chain does and does not prove
//
// Hash-chaining catches an editor who has the store but not the chain: a
// mutated row no longer hashes to its stored hash, a deleted row leaves a gap
// and orphans its successor's prev_hash, an inserted row has nowhere to link.
// It does not catch someone who can rewrite the whole tail — recomputing every
// hash from the edit onwards is exactly as cheap for an attacker as it was for
// the platform. What bounds that is an anchor kept outside the table: the head
// object the next sequence number is claimed through (see head.go) says where
// the chain ends, so a log truncated from the end is visible without reading
// the log at all. Anchoring further out — a transparency log, an
// operator-signed checkpoint — is the natural next step and is deliberately
// not in this first cut.
//
// # One appender at a time
//
// A chain needs its appends serialized, because the next hash is a function of
// the last one. Kitchen's are serialized through a head object in the cluster
// and the API server's own optimistic concurrency, so the guarantee holds
// however many replicas the manager runs — see head.go for the mechanism and
// what it costs.
//
// Recording happens on both sides of a change. Reconcilers record the
// transitions they observe, which catches a change made with kubectl behind
// the platform's back; the REST API records its own writes, which is the only
// way a deletion of an object that carries no finalizer leaves a record at
// all — after it, there is nothing left to observe. Both are appenders, and
// the reconcilers run on the leader while the API answers on every replica,
// which is exactly why the serialization cannot be a mutex.
package audit

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"strings"
	"time"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// GenesisHash is what the first record links to. A literal rather than an
// empty string, so that "this record starts the chain" and "whoever wrote this
// record left prev_hash blank" are different statements.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// hashTimeFormat is the timestamp as it is hashed. It is the format the record
// is *stored* in — millisecond precision, UTC — and not a finer one, because a
// verifier can only re-derive the hash from what it can read back. Hashing
// nanoseconds the column cannot hold would make every record fail
// verification.
const hashTimeFormat = "2006-01-02T15:04:05.000Z"

// ChainHash is the record's hash: SHA-256 over its previous hash and every
// field of its content, in a fixed order.
//
// Each field is written length-prefixed rather than delimited. A delimiter —
// any delimiter — lets one field's content impersonate the boundary before the
// next, so a reason of "x|admin@example.com" could be made to hash the same as
// a different record with a different actor. Length prefixes have no such
// reading.
func ChainHash(record clickhouse.AuditRecord) string {
	digest := sha256.New()
	writeField(digest, record.PrevHash)
	writeUint(digest, uint64(record.Sequence))
	writeField(digest, record.Timestamp.UTC().Format(hashTimeFormat))
	writeField(digest, record.Actor)
	writeField(digest, record.ActorKind)
	writeField(digest, record.Correlation)
	writeField(digest, record.Operation)
	writeField(digest, record.Kind)
	writeField(digest, record.Namespace)
	writeField(digest, record.Name)
	writeField(digest, record.UID)
	writeField(digest, record.Project)
	writeField(digest, record.FromState)
	writeField(digest, record.ToState)
	writeField(digest, record.Reason)
	writeField(digest, record.Details)
	return hex.EncodeToString(digest.Sum(nil))
}

func writeField(digest hash.Hash, value string) {
	writeUint(digest, uint64(len(value)))
	digest.Write([]byte(value))
}

func writeUint(digest hash.Hash, value uint64) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], value)
	digest.Write(buf[:])
}

// Seal fills in a record's chain fields, given the record it follows. A zero
// previous record starts a new chain at sequence 1.
//
// The timestamp is truncated to what the column stores, so that the hash is
// over the value a reader will read back rather than the one the caller
// happened to have.
func Seal(record clickhouse.AuditRecord, previous clickhouse.AuditRecord) clickhouse.AuditRecord {
	record.Sequence = previous.Sequence + 1
	record.PrevHash = previous.Hash
	if record.PrevHash == "" {
		record.PrevHash = GenesisHash
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now()
	}
	record.Timestamp = record.Timestamp.UTC().Truncate(time.Millisecond)
	record.Hash = ChainHash(record)
	return record
}

// Break names one way a chain can be wrong.
type Break string

const (
	// BreakMutated is a record whose content no longer hashes to the hash
	// stored beside it: someone edited the row.
	BreakMutated Break = "mutated"
	// BreakMissing is a gap in the sequence: records were deleted.
	BreakMissing Break = "missing"
	// BreakUnlinked is a record whose prev_hash is not its predecessor's
	// hash: a record was inserted, replaced, or reordered.
	BreakUnlinked Break = "unlinked"
)

// Finding is one break, at one point in the chain.
type Finding struct {
	// Sequence is where the chain stopped being sound. For a gap it is the
	// first sequence number that should have been there and was not.
	Sequence int64 `json:"sequence"`
	Break    Break `json:"break"`
	// Detail says what was expected and what was found, in the terms
	// someone investigating would need to repeat the check by hand.
	Detail string `json:"detail"`
}

// Verification is the answer to "is this run of the log sound".
type Verification struct {
	// From and To are the sequence range actually checked. They are the
	// records' own numbers, not the range asked for: a scan that came back
	// short is reported as the range it covered.
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// Checked is how many records were read.
	Checked int `json:"checked"`
	// Intact is true when nothing was found.
	Intact bool `json:"intact"`
	// Findings are every break, in sequence order.
	Findings []Finding `json:"findings"`
}

// Verify walks a run of records in sequence order and reports every break.
//
// `previous` is the record the run is expected to follow — the zero value when
// the run starts at the beginning of the chain. Passing it is what makes a
// verification of records 500..600 meaningful: without it, a tail lifted whole
// out of another chain would verify.
//
// It reports every break rather than stopping at the first, because "the log
// was edited once in March" and "the log was rewritten" are different findings
// and an investigator needs to be able to tell them apart.
func Verify(records []clickhouse.AuditRecord, previous clickhouse.AuditRecord) Verification {
	result := Verification{Checked: len(records), Findings: []Finding{}}
	if len(records) == 0 {
		result.Intact = true
		return result
	}
	result.From = records[0].Sequence
	result.To = records[len(records)-1].Sequence

	expectedSequence := previous.Sequence + 1
	expectedPrevHash := previous.Hash
	if expectedPrevHash == "" {
		expectedPrevHash = GenesisHash
	}

	for _, record := range records {
		if record.Sequence != expectedSequence {
			result.Findings = append(result.Findings, Finding{
				Sequence: expectedSequence,
				Break:    BreakMissing,
				Detail: fmt.Sprintf("expected sequence %d, found %d — %d record(s) are not there",
					expectedSequence, record.Sequence, record.Sequence-expectedSequence),
			})
		}
		if !strings.EqualFold(record.PrevHash, expectedPrevHash) {
			result.Findings = append(result.Findings, Finding{
				Sequence: record.Sequence,
				Break:    BreakUnlinked,
				Detail: fmt.Sprintf("prev_hash is %s, the record before it hashes to %s",
					shortHash(record.PrevHash), shortHash(expectedPrevHash)),
			})
		}
		if recomputed := ChainHash(record); !strings.EqualFold(recomputed, record.Hash) {
			result.Findings = append(result.Findings, Finding{
				Sequence: record.Sequence,
				Break:    BreakMutated,
				Detail: fmt.Sprintf("stored hash is %s, the record's content hashes to %s",
					shortHash(record.Hash), shortHash(recomputed)),
			})
		}
		// Continue from what is actually there rather than from what should
		// have been: one edited record in the middle otherwise reports every
		// record after it as broken too, and the finding that matters gets
		// lost in the noise.
		expectedSequence = record.Sequence + 1
		expectedPrevHash = record.Hash
	}

	result.Intact = len(result.Findings) == 0
	return result
}

// shortHash keeps a finding readable. The full hash is in the record; what a
// finding needs is enough to tell two of them apart.
func shortHash(value string) string {
	if len(value) <= 12 {
		if value == "" {
			return "(empty)"
		}
		return value
	}
	return value[:12] + "…"
}
