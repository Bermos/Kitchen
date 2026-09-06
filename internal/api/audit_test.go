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
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
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

// A read the store did not complete is answered by what went wrong, not by one
// word for all three. #441: every failure was a 500 `failed`, so the caller
// could not tell a platform whose audit log has no table from one whose store
// was briefly away from one whose query is broken — and the first two clear on
// their own while the third never will.
func TestListAuditRecordsClassifiesWhatTheStoreDidNotDo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
		says   string
	}{
		{
			// The reported fault: the log's table is not in the store,
			// because this installation has never created one.
			name: "no table",
			err: &clickhouse.QueryError{Status: "404 Not Found", Message: "Code: 60. DB::Exception: " +
				"Unknown table expression identifier 'kitchen.audit_log' in scope SELECT " +
				"toString(sequence) AS seq. (UNKNOWN_TABLE) (version 26.3.17.110 (official build))"},
			status: http.StatusServiceUnavailable,
			says:   "no audit log to read",
		},
		{
			name:   "store away",
			err:    errors.New("clickhouse at http://kitchen-clickhouse:8123/: dial tcp: connection refused"),
			status: http.StatusServiceUnavailable,
			says:   "did not answer",
		},
		{
			// A statement ClickHouse judged and refused is the platform's own
			// fault and stays a 500: retrying it will fail identically.
			name: "refused",
			err: &clickhouse.QueryError{Status: "400 Bad Request", Message: "Code: 47. DB::Exception: " +
				"Unknown expression identifier 'sequenc'. (UNKNOWN_IDENTIFIER)"},
			status: http.StatusInternalServerError,
			says:   "the operator's log has the store's diagnostic",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			h.logs.auditErr = tc.err

			response := h.do(t, http.MethodGet, "/api/v1/audit", "")
			if response.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", response.Code, tc.status, response.Body.String())
			}
			if !strings.Contains(response.Body.String(), tc.says) {
				t.Errorf("the answer does not say %q: %s", tc.says, response.Body.String())
			}
			// Whatever the classification, the store's own diagnostic is not
			// in it: the caller cannot act on a nested-aggregate complaint.
			if strings.Contains(response.Body.String(), "DB::Exception") {
				t.Errorf("the store's diagnostic reached the caller: %s", response.Body.String())
			}
		})
	}
}

// An installation that keeps no audit log says so, rather than reading a table
// that was never created and reporting the refusal as a failed query.
func TestTheAuditLogSaysSoWhenTheInstallationKeepsNone(t *testing.T) {
	h := newHarness(t, nil)
	kitchen := &kitchenv1alpha1.Kitchen{}
	key := types.NamespacedName{Name: controller.KitchenSingletonName}
	if err := h.server.Client.Get(context.Background(), key, kitchen); err != nil {
		t.Fatalf("reading the singleton: %v", err)
	}
	kitchen.Spec.Compliance.Audit.Enabled = false
	if err := h.server.Client.Update(context.Background(), kitchen); err != nil {
		t.Fatalf("turning the audit log off: %v", err)
	}
	// The store would answer records if it were asked, which is what makes
	// this about the configuration rather than about the store.
	h.logs.auditRecords = auditChain(2)

	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/verify"} {
		response := h.do(t, http.MethodGet, path, "")
		if response.Code != http.StatusServiceUnavailable {
			t.Errorf("%s answered %d, want 503 where no log is kept: %s",
				path, response.Code, response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "spec.compliance.audit") {
			t.Errorf("%s does not name the setting that turned it off: %s", path, response.Body.String())
		}
	}
	if h.logs.lastAudit.Limit != 0 {
		t.Errorf("the store was read for a log this installation does not keep: %+v", h.logs.lastAudit)
	}
}
