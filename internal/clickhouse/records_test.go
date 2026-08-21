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
	"strings"
	"testing"
	"time"
)

// A signed record is an envelope kept whole: the row carries it verbatim next
// to the subject digest that ties it back to what it describes.

func TestInsertSignedRecordWritesEveryColumn(t *testing.T) {
	store := newFakeLogStore(t)
	err := store.client(t).InsertSignedRecord(context.Background(), SignedRecord{
		ID:        "1e8b2c3d-1111-2222-3333-444444444444",
		Timestamp: time.Date(2026, 8, 21, 9, 0, 0, 0, time.UTC),
		Type:      "https://kitchen.bermos.dev/attestation/data-class/v1",
		Subject:   "sha256:" + strings.Repeat("d", 64),
		Project:   "shop",
		Envelope:  `{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[]}`,
	})
	if err != nil {
		t.Fatalf("InsertSignedRecord: %v", err)
	}

	_, payload, found := strings.Cut(store.query, "FORMAT JSONEachRow\n")
	if !found {
		t.Fatalf("the insert is not a JSONEachRow statement: %s", store.query)
	}
	row := map[string]any{}
	if err := json.Unmarshal([]byte(payload), &row); err != nil {
		t.Fatalf("the inserted row is not JSON: %v (%s)", err, payload)
	}
	for _, column := range []string{"id", "timestamp", "type", "subject", "project", "envelope"} {
		if _, ok := row[column]; !ok {
			t.Errorf("the row misses the %s column: %s", column, payload)
		}
	}
	if row["envelope"] != `{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[]}` {
		t.Fatalf("the envelope must be kept verbatim, got %v", row["envelope"])
	}
	if !strings.Contains(store.query, SignedRecordsTable) {
		t.Fatalf("the insert does not name the table: %s", store.query)
	}
}

// The table keeps everything: retention is a deliberate non-feature here — a
// signed statement is kept as long as anything might cite it.
func TestSignedRecordsTableCarriesNoTTL(t *testing.T) {
	ddl := createSignedRecordsTable("kitchen")
	if strings.Contains(ddl, "TTL") {
		t.Fatalf("signed_records must carry no TTL: %s", ddl)
	}
	if !strings.Contains(ddl, "ORDER BY (subject, type, timestamp)") {
		t.Fatalf("the ordering key must lead with the subject: %s", ddl)
	}
}
