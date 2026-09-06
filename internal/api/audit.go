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
	// Privileged and PrivilegeClass are lifted out of the details so a
	// reader does not have to parse an opaque JSON string to tell a waiver
	// from a redeploy. The details still carry them verbatim, because that
	// is what the chain covers — these two are a reading of the record, not
	// a second source for it.
	Privileged     bool   `json:"privileged,omitempty"`
	PrivilegeClass string `json:"privilegeClass,omitempty"`
	PrevHash       string `json:"prevHash"`
	Hash           string `json:"hash"`
}

func auditBody(record clickhouse.AuditRecord) auditRecordBody {
	class, privileged := audit.PrivilegeOf(record.Details)
	body := auditRecordBody{
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
	body.Privileged = privileged
	body.PrivilegeClass = string(class)
	return body
}

// openAuditLog answers the request itself when this installation keeps no
// audit log, and reports whether the caller may go on and read one. A false
// return means the response has been written, in the same shape openLogStore
// uses for a telemetry read on an installation with no store.
//
// The reads have to ask, because the log's table exists only when the answer is
// yes: EnsureAuditSchema is the compliance reconcile's, and an installation
// that turned the log off never gets one. Without this the store answers
// UNKNOWN_TABLE and a log nobody is keeping reads as a 500 about a failed
// query — which is #441, where the caller could not tell "this platform does
// not do that" from "the platform is broken".
func (s *Server) openAuditLog(w http.ResponseWriter, req *http.Request) bool {
	kitchen := kitchenFrom(req.Context())
	// The singleton travels with the request, so this costs nothing. Where it
	// somehow did not, the store is left to answer — it is about to be asked
	// anyway, and a claim about an installation nobody could read is worse
	// than a refusal from the store.
	if kitchen == nil || kitchen.Spec.Compliance.Audit.Enabled {
		return true
	}
	writeJSON(w, http.StatusServiceUnavailable, errorBody{
		Error: "this installation keeps no audit log: spec.compliance.audit is turned off, so no " +
			"transition has been recorded and there is nothing to read. GET /compliance reports " +
			"the same thing about attestation and the decision register",
	})
	return false
}

// writeAuditReadError answers a read of the audit log the store did not
// complete, saying which of the three things went wrong without handing over
// the store's diagnostic.
//
// The classification is the half of #441 that outlives the missing table: a
// caller who is told only "failed" cannot tell whether to retry, to fix the
// call, or to go and find somebody. So a store that did not answer and a log
// this installation has not created are `503` — both clear on their own, and
// both are what the CLI publishes as `unavailable` — while a statement
// ClickHouse judged and refused stays `500`, because that one is a fault in
// the platform's own query and nothing the caller does will change it.
func (s *Server) writeAuditReadError(w http.ResponseWriter, err error, what string) {
	s.log().Error(err, what+" failed")
	switch {
	case !clickhouse.Refused(err):
		writeJSON(w, http.StatusServiceUnavailable, errorBody{
			Error: what + " could not be made: the telemetry store did not answer. " +
				"The store rather than the request, so the same call is worth retrying",
		})
	case clickhouse.IsUnknownTable(err):
		writeJSON(w, http.StatusServiceUnavailable, errorBody{
			Error: "this installation has no audit log to read: the platform has not created the " +
				"log's table in the telemetry store. GET /compliance says whether the audit log is " +
				"being recorded, and the Kitchen object's compliance status says why it is not",
		})
	default:
		writeJSON(w, http.StatusInternalServerError, errorBody{
			Error: what + " failed; the operator's log has the store's diagnostic",
		})
	}
}

// listAuditRecords serves a page of the audit log, newest first.
//
// The filters are the four questions anyone asks of an audit log: what
// happened to this object, what did this person do, what happened in this
// window, and — the supervisor's question — what moved a control rather than
// a workload. They compose, so "which waivers did this person grant last
// quarter" is one request.
func (s *Server) listAuditRecords(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	privileged, err := boolParam(req, "privileged")
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	class := strings.TrimSpace(req.URL.Query().Get("privilegeClass"))
	if class != "" && !audit.Privilege(class).Valid() {
		badRequest(w, "privilegeClass %q is not a class of privileged act: one of %s",
			class, joinPrivileges())
		return
	}

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

	project := strings.TrimSpace(req.URL.Query().Get("project"))
	if !s.visibleProject(w, req, project) {
		return
	}

	if !s.openAuditLog(w, req) {
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
		Project:   project,
		Actor:     strings.TrimSpace(req.URL.Query().Get("actor")),

		Privileged:     privileged,
		PrivilegeClass: class,

		Since: since,
		Until: until,
		Limit: limit,
	})
	if err != nil {
		s.writeAuditReadError(w, err, "the audit log query")
		return
	}

	// Each record names the project it was about, so the log reads as "what
	// happened to my projects" for a member and as the whole log for an
	// operator. A record with no project is about the platform itself — the
	// settings, a connection, an upgrade — and is the operator's alone.
	scope := scopeFrom(ctx)
	body := make([]auditRecordBody, 0, len(records))
	for _, record := range records {
		if !scope.allows(record.Project) {
			continue
		}
		body = append(body, auditBody(record))
	}
	writeList(w, body)
}

// joinPrivileges words the vocabulary for a refusal, so that a mistyped
// class is answered with the list rather than with an empty page.
func joinPrivileges() string {
	names := make([]string, 0, len(audit.Privileges()))
	for _, privilege := range audit.Privileges() {
		names = append(names, string(privilege))
	}
	return strings.Join(names, ", ")
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
	// Findings are the breaks, in sequence order. The anchor's own two —
	// `truncated` and `unanchored` — are in here with the rest, because a
	// consumer that reads `intact` and the findings must not have to know
	// there is a second place to look.
	Findings []audit.Finding `json:"findings"`
	// AnchorPresent says whether there is an anchor at all: the head object
	// outside the table, which is the only thing that bounds a rewritten
	// tail. False is a finding, not an empty field.
	AnchorPresent bool `json:"anchorPresent"`
	// Anchor is where the chain ends according to that object. It is null
	// rather than 0 when there is no anchor — 0 is a real answer, meaning a
	// chain nothing has been appended to yet, and folding the two together
	// is what let a deleted anchor read as a sound log (#428).
	Anchor *int64 `json:"anchor"`
	// AnchorOrigin is how the anchor came to exist: `genesis` (it has been
	// there since before the first record), `adopted` (it was seeded from
	// the log's own last record, and `anchorAdoptedFrom` says where), or
	// `unknown` (a head written before the platform recorded this).
	AnchorOrigin string `json:"anchorOrigin,omitempty"`
	// AnchorAdoptedFrom is the sequence an adopted anchor was taken from.
	// Records at or below it are bounded by the hash chain alone.
	AnchorAdoptedFrom int64 `json:"anchorAdoptedFrom,omitempty"`
	// AnchorMessage explains an anchor that is not there, in the terms
	// somebody deciding whether to page an operator needs: an object that
	// was deleted and a cluster that did not answer are the same gap and
	// very different events.
	AnchorMessage string `json:"anchorMessage,omitempty"`
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

	if !s.openAuditLog(w, req) {
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
			s.writeAuditReadError(w, err, "the audit chain verification")
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
		s.writeAuditReadError(w, err, "the audit chain verification")
		return
	}
	// The run is one page of a longer log when it came back full: the anchor
	// is ahead of it by construction there, and saying so would report every
	// first page of every chain as truncated.
	partial := len(records) >= limit
	anchor := s.auditAnchor(ctx)
	result := audit.Verify(records, previous).AgainstAnchor(anchor, partial)

	body := auditVerificationBody{
		From:          result.From,
		To:            result.To,
		Checked:       result.Checked,
		Intact:        result.Intact,
		Findings:      result.Findings,
		AnchorPresent: anchor.Present,
		AnchorOrigin:  string(anchor.Origin),
		Truncated:     partial,
	}
	if anchor.Present {
		sequence := anchor.Sequence
		body.Anchor = &sequence
		body.AnchorAdoptedFrom = anchor.AdoptedFrom
	} else {
		// Always worded, never left to the reader to word: an object
		// somebody deleted and a cluster that did not answer are the same
		// gap and very different events.
		body.AnchorMessage = anchor.Absence()
	}
	writeJSON(w, http.StatusOK, body)
}

// auditAnchor is where the chain ends according to the object outside it.
//
// Reading it from the cluster rather than from the table is the whole point: a
// tail cut off the log rehashes perfectly, so the only way to notice is to
// compare against something the log did not produce.
//
// Every failure used to fold into 0, and 0 is a claim: it is the answer for a
// chain nothing has been appended to, and it reads downstream as "no gap".
// A read that did not happen answers an absent anchor carrying the reason
// instead, and an absent anchor is a break (#428).
func (s *Server) auditAnchor(ctx context.Context) audit.Anchor {
	anchor, err := s.Audit.Head(ctx)
	if err != nil {
		s.log().Error(err, "the audit chain's anchor could not be read")
		if anchor.Unreadable == "" {
			anchor.Unreadable = "the anchor could not be read from the cluster"
		}
		return audit.Anchor{Unreadable: anchor.Unreadable}
	}
	return anchor
}
