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
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The audit log's REST surface: the writes this API records about itself, and
// the two reads — a filtered page of the log, and a verification of a run of
// the chain.
//
// Recording an API write here is not a duplicate of what the reconcilers do.
// A reconciler can only record what it can still see, and an object with no
// finalizer is gone by the time the reconciler is told: a deletion recorded
// here is the only record there will ever be of it.

// recorded appends one transition and reports whether the caller may go ahead.
//
// It is the whole of what a write handler has to do about the audit log, and
// it is shaped so that forgetting it is visible in review: the handler either
// goes through this and gets a bool back, or it writes to the cluster with
// nothing said about it.
//
// A refused append answers 503 rather than 500. The request was well formed and
// the platform is willing; what it cannot do is keep a record, and that is a
// condition that clears on its own — which is exactly what 503 means and what
// makes a client retry rather than give up.
func (s *Server) recorded(w http.ResponseWriter, req *http.Request, transition audit.Transition) bool {
	ctx := req.Context()
	if transition.Actor == "" {
		caller, _ := CallerFrom(ctx)
		transition.Actor = callerName(caller)
	}
	if err := s.Audit.Record(ctx, transition); err != nil {
		s.log().Error(err, "refusing a write the audit log could not record",
			"kind", transition.Kind, "name", transition.Object.GetName())
		writeJSON(w, http.StatusServiceUnavailable, errorBody{
			Error: "this change was not made: the platform could not record it in the audit log, " +
				"and an unrecorded change is not one it will make. Try again shortly.",
		})
		return false
	}
	return true
}

// auditRecordBody is one record as the dashboard reads it. It is the storage
// shape with the chain fields kept: an audit view that hid them would be
// asking to be trusted, and the point of the chain is that it does not have
// to be.
type auditRecordBody struct {
	Sequence    int64     `json:"sequence"`
	Timestamp   time.Time `json:"timestamp"`
	Actor       string    `json:"actor"`
	ActorKind   string    `json:"actorKind"`
	Correlation string    `json:"correlation,omitempty"`
	Operation   string    `json:"operation"`
	Kind        string    `json:"kind"`
	Name        string    `json:"name"`
	Project     string    `json:"project,omitempty"`
	FromState   string    `json:"fromState,omitempty"`
	ToState     string    `json:"toState,omitempty"`
	Reason      string    `json:"reason,omitempty"`
	Details     string    `json:"details,omitempty"`
	PrevHash    string    `json:"prevHash"`
	Hash        string    `json:"hash"`
}

func auditBody(record clickhouse.AuditRecord) auditRecordBody {
	return auditRecordBody{
		Sequence:    record.Sequence,
		Timestamp:   record.Timestamp,
		Actor:       record.Actor,
		ActorKind:   record.ActorKind,
		Correlation: record.Correlation,
		Operation:   record.Operation,
		Kind:        record.Kind,
		Name:        record.Name,
		Project:     record.Project,
		FromState:   record.FromState,
		ToState:     record.ToState,
		Reason:      record.Reason,
		Details:     record.Details,
		PrevHash:    record.PrevHash,
		Hash:        record.Hash,
	}
}

// listAuditRecords serves a page of the audit log, newest first.
//
// The filters are the three questions anyone asks of an audit log: what
// happened to this object, what did this person do, and what happened in this
// window. They compose, so "what did this person do to this project last
// Tuesday" is one request.
func (s *Server) listAuditRecords(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	since, err := timeParam(req, "since")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	until, err := timeParam(req, "until")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	limit, err := intParam(req, "limit", clickhouse.DefaultAuditLimit)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	records, err := store.QueryAuditRecords(ctx, clickhouse.AuditQuery{
		Kind:      strings.TrimSpace(req.URL.Query().Get("kind")),
		Namespace: strings.TrimSpace(req.URL.Query().Get("namespace")),
		Name:      strings.TrimSpace(req.URL.Query().Get("name")),
		Project:   strings.TrimSpace(req.URL.Query().Get("project")),
		Actor:     strings.TrimSpace(req.URL.Query().Get("actor")),
		Since:     since,
		Until:     until,
		Limit:     limit,
	})
	if err != nil {
		s.writeStoreError(w, err, "the audit log query")
		return
	}

	body := make([]auditRecordBody, 0, len(records))
	for _, record := range records {
		body = append(body, auditBody(record))
	}
	writeList(w, body)
}

// auditVerificationBody is the chain verifier's answer.
type auditVerificationBody struct {
	// From and To are the sequence range checked.
	From int64 `json:"from"`
	To   int64 `json:"to"`
	// Checked is how many records were read, and Intact whether all of them
	// were sound.
	Checked int  `json:"checked"`
	Intact  bool `json:"intact"`
	// Findings are the breaks, in sequence order.
	Findings []audit.Finding `json:"findings"`
	// Anchor is the sequence number the platform published on the Kitchen
	// object, outside the table. A run that verifies as intact but ends
	// below the anchor is a log cut short from the end — the one edit the
	// chain cannot see on its own, because a rewritten tail rehashes
	// perfectly.
	Anchor int64 `json:"anchor"`
	// Truncated says the run asked for was longer than one read returns, so
	// `to` is where to continue from rather than the end of the chain.
	Truncated bool `json:"truncated"`
}

// verifyAuditChain re-derives the hashes over a run of the log and reports
// every break.
//
// It reads the record before the run as well as the run itself. Without it a
// verification of records 500 onwards would accept a tail lifted out of some
// other chain, because every link inside the run would check out.
func (s *Server) verifyAuditChain(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	from, err := intParam(req, "from", 1)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if from < 1 {
		badRequest(w, "from must be a sequence number of 1 or more (got %d)", from)
		return
	}
	limit, err := intParam(req, "limit", clickhouse.MaxAuditLimit)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	previous := clickhouse.AuditRecord{}
	if from > 1 {
		preceding, err := store.ScanAuditRecords(ctx, int64(from)-1, 1)
		if err != nil {
			s.writeStoreError(w, err, "the audit chain verification")
			return
		}
		if len(preceding) == 0 || preceding[0].Sequence != int64(from)-1 {
			badRequest(w, "record %d is not in the log, so a run starting at %d cannot be linked to anything",
				from-1, from)
			return
		}
		previous = preceding[0]
	}

	records, err := store.ScanAuditRecords(ctx, int64(from), limit)
	if err != nil {
		s.writeStoreError(w, err, "the audit chain verification")
		return
	}
	result := audit.Verify(records, previous)

	body := auditVerificationBody{
		From:      result.From,
		To:        result.To,
		Checked:   result.Checked,
		Intact:    result.Intact,
		Findings:  result.Findings,
		Anchor:    s.auditAnchor(ctx),
		Truncated: len(records) >= limit,
	}
	writeJSON(w, http.StatusOK, body)
}

// auditAnchor is where the chain ends according to the object outside it.
//
// Reading it from the cluster rather than from the table is the whole point: a
// tail cut off the log rehashes perfectly, so the only way to notice is to
// compare against something the log did not produce. A head that cannot be
// read answers 0, which claims nothing rather than claiming soundness.
func (s *Server) auditAnchor(ctx context.Context) int64 {
	sequence, err := s.Audit.Head(ctx)
	if err != nil {
		return 0
	}
	return sequence
}
