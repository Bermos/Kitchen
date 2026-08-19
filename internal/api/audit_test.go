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
	"net/http"
	"testing"
	"time"

	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// auditChain is a sound run of records, sealed the way the recorder seals
// them, so the endpoint tests break exactly one thing about a log that
// verifies.
func auditChain(n int) []clickhouse.AuditRecord {
	stamp := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	records := make([]clickhouse.AuditRecord, 0, n)
	previous := clickhouse.AuditRecord{}
	for i := range n {
		sealed := audit.Seal(clickhouse.AuditRecord{
			Timestamp: stamp.Add(time.Duration(i) * time.Minute),
			Actor:     testCaller,
			ActorKind: clickhouse.ActorUser,
			Operation: clickhouse.AuditCreate,
			Kind:      audit.KindProject,
			Name:      feedProject,
			Project:   feedProject,
			Reason:    "project shop created",
		}, previous)
		records = append(records, sealed)
		previous = sealed
	}
	return records
}

func TestListAuditRecordsPassesTheFiltersThrough(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.auditRecords = auditChain(2)

	response := h.do(t, http.MethodGet,
		"/api/v1/audit?kind=Project&name="+feedProject+"&actor="+testCaller+"&project="+feedProject+"&limit=25", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}

	query := h.logs.lastAudit
	if query.Kind != audit.KindProject || query.Name != feedProject ||
		query.Actor != testCaller || query.Project != feedProject || query.Limit != 25 {
		t.Errorf("the store was asked %+v, want every filter carried through", query)
	}

	body := decode[listBody[auditRecordBody]](t, response)
	if len(body.Items) != 2 {
		t.Fatalf("returned %d records, want 2", len(body.Items))
	}
	// The chain fields are part of the answer. An audit view that hid them
	// would be asking to be believed, which is the thing the chain exists to
	// avoid.
	if body.Items[0].Hash == "" || body.Items[0].PrevHash == "" {
		t.Error("a record came back without its chain fields")
	}
}

func TestVerifyAuditChainReportsASoundLog(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.auditRecords = auditChain(4)

	response := h.do(t, http.MethodGet, "/api/v1/audit/verify", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	body := decode[auditVerificationBody](t, response)
	if !body.Intact {
		t.Errorf("a sound log verified with findings: %+v", body.Findings)
	}
	if body.Checked != 4 || body.From != 1 || body.To != 4 {
		t.Errorf("checked %d records over %d..%d, want 4 over 1..4", body.Checked, body.From, body.To)
	}
}

func TestVerifyAuditChainReportsAnEditedRecord(t *testing.T) {
	h := newHarness(t, nil)
	records := auditChain(4)
	records[1].Actor = "mallory@example.com"
	h.logs.auditRecords = records

	response := h.do(t, http.MethodGet, "/api/v1/audit/verify", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	body := decode[auditVerificationBody](t, response)
	if body.Intact {
		t.Fatal("an edited log verified as intact")
	}
	if len(body.Findings) != 1 || body.Findings[0].Break != audit.BreakMutated {
		t.Errorf("findings %+v, want one mutated record", body.Findings)
	}
}

// A run that starts partway through has to be linked to the record before it,
// and that record has to be there.
func TestVerifyAuditChainRefusesARunWithNothingBeforeIt(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.auditRecords = auditChain(4)

	response := h.do(t, http.MethodGet, "/api/v1/audit/verify?from=9", "")
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status %d, want 400: %s", response.Code, response.Body.String())
	}
}

func TestVerifyAuditChainLinksARunToTheRecordBeforeIt(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.auditRecords = auditChain(6)

	response := h.do(t, http.MethodGet, "/api/v1/audit/verify?from=4", "")
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body.String())
	}
	body := decode[auditVerificationBody](t, response)
	if !body.Intact || body.From != 4 || body.To != 6 {
		t.Errorf("verified %d..%d intact=%v, want 4..6 intact: %+v", body.From, body.To, body.Intact, body.Findings)
	}
	// Record 3 has to have been read as well, or the run would have been
	// accepted on its own word.
	if h.logs.lastScanFrom != 4 {
		t.Errorf("the last scan started at %d, want the run itself at 4", h.logs.lastScanFrom)
	}
}

func TestAuditEndpointsRefuseAnonymousCallers(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/verify"} {
		response := h.do(t, http.MethodGet, path, "", "")
		if response.Code != http.StatusUnauthorized {
			t.Errorf("%s answered %d to an anonymous caller, want 401", path, response.Code)
		}
	}
}

func TestListAuditRecordsReportsAStoreFailure(t *testing.T) {
	h := newHarness(t, nil)
	h.logs.auditErr = &clickhouse.QueryError{Message: "Code: 60. DB::Exception: Table audit_log does not exist"}

	response := h.do(t, http.MethodGet, "/api/v1/audit", "")
	if response.Code != http.StatusInternalServerError {
		t.Errorf("status %d, want 500 when the store refuses: %s", response.Code, response.Body.String())
	}
}
