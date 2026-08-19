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

package audit

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// chain builds a sound run of n records, so that every test below starts from
// a log that verifies and breaks exactly one thing about it.
func chain(t *testing.T, n int) []clickhouse.AuditRecord {
	t.Helper()
	stamp := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	records := make([]clickhouse.AuditRecord, 0, n)
	previous := clickhouse.AuditRecord{}
	for i := range n {
		sealed := Seal(clickhouse.AuditRecord{
			Timestamp: stamp.Add(time.Duration(i) * time.Minute),
			Actor:     "grace@example.com",
			ActorKind: clickhouse.ActorUser,
			Operation: clickhouse.AuditTransition,
			Kind:      KindBuild,
			Name:      "shop-bld-1",
			Reason:    "the build job was created",
		}, previous)
		records = append(records, sealed)
		previous = sealed
	}
	return records
}

func TestSealStartsTheChainAtOne(t *testing.T) {
	sealed := Seal(clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, clickhouse.AuditRecord{})
	if sealed.Sequence != 1 {
		t.Errorf("first record has sequence %d, want 1", sealed.Sequence)
	}
	if sealed.PrevHash != GenesisHash {
		t.Errorf("first record links to %q, want the genesis hash", sealed.PrevHash)
	}
	if sealed.Hash == "" {
		t.Error("first record was sealed without a hash")
	}
}

// The hash is over the record as it will be read back, not as it was handed
// in: a timestamp with more precision than the column holds must not change
// the answer, or every record would fail verification after a round trip.
func TestChainHashIgnoresPrecisionTheColumnCannotHold(t *testing.T) {
	base := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	coarse := Seal(clickhouse.AuditRecord{Timestamp: base, Name: "shop"}, clickhouse.AuditRecord{})
	fine := Seal(clickhouse.AuditRecord{
		Timestamp: base.Add(400 * time.Microsecond),
		Name:      "shop",
	}, clickhouse.AuditRecord{})
	if coarse.Hash != fine.Hash {
		t.Error("sub-millisecond precision changed the hash, so a record read back would never verify")
	}
}

// Fields are hashed length-prefixed so that content cannot impersonate a
// field boundary. Two records that differ only in where the split falls must
// not hash the same.
func TestChainHashDistinguishesFieldBoundaries(t *testing.T) {
	left := Seal(clickhouse.AuditRecord{Actor: "ab", Reason: "c"}, clickhouse.AuditRecord{})
	right := Seal(clickhouse.AuditRecord{Actor: "a", Reason: "bc"}, clickhouse.AuditRecord{})
	if left.Hash == right.Hash {
		t.Error("two records with different field boundaries hash the same")
	}
}

func TestVerifyAcceptsASoundChain(t *testing.T) {
	result := Verify(chain(t, 5), clickhouse.AuditRecord{})
	if !result.Intact {
		t.Fatalf("a sound chain reported %d findings: %+v", len(result.Findings), result.Findings)
	}
	if result.From != 1 || result.To != 5 || result.Checked != 5 {
		t.Errorf("verified %d..%d over %d records, want 1..5 over 5", result.From, result.To, result.Checked)
	}
}

func TestVerifyDetectsAnEditedRecord(t *testing.T) {
	records := chain(t, 5)
	records[2].Actor = "mallory@example.com"

	result := Verify(records, clickhouse.AuditRecord{})
	if result.Intact {
		t.Fatal("an edited record verified as intact")
	}
	if len(result.Findings) != 1 {
		t.Fatalf("one edit produced %d findings, want 1: %+v", len(result.Findings), result.Findings)
	}
	finding := result.Findings[0]
	if finding.Break != BreakMutated || finding.Sequence != 3 {
		t.Errorf("reported %s at %d, want %s at 3", finding.Break, finding.Sequence, BreakMutated)
	}
}

func TestVerifyDetectsADeletedRecord(t *testing.T) {
	records := chain(t, 5)
	// Records 1, 2, 4, 5: what a DELETE of one row leaves behind.
	records = append(records[:2:2], records[3:]...)

	result := Verify(records, clickhouse.AuditRecord{})
	if result.Intact {
		t.Fatal("a chain with a record removed verified as intact")
	}
	breaks := map[Break]int{}
	for _, finding := range result.Findings {
		breaks[finding.Break]++
	}
	if breaks[BreakMissing] != 1 {
		t.Errorf("reported %d gaps, want 1: %+v", breaks[BreakMissing], result.Findings)
	}
	// The record after the gap still points at the one that was removed, so
	// the deletion is visible twice over — which is what makes it hard to
	// cover up by renumbering.
	if breaks[BreakUnlinked] != 1 {
		t.Errorf("reported %d unlinked records, want 1: %+v", breaks[BreakUnlinked], result.Findings)
	}
}

func TestVerifyDetectsAnInsertedRecord(t *testing.T) {
	records := chain(t, 3)
	forged := Seal(clickhouse.AuditRecord{
		Timestamp: records[1].Timestamp,
		Actor:     "mallory@example.com",
		Kind:      KindProject,
		Name:      "shop",
	}, clickhouse.AuditRecord{Sequence: 1, Hash: records[0].Hash})
	// Slotted in as a second record 2, chained correctly to record 1 — which
	// is the best a forger with write access to the table can do.
	records = append([]clickhouse.AuditRecord{records[0], forged}, records[1:]...)

	result := Verify(records, clickhouse.AuditRecord{})
	if result.Intact {
		t.Fatal("a chain with a record inserted verified as intact")
	}
}

// A run pulled out of the middle of the log is only meaningful when it is
// checked against the record before it. Without that, a tail from another
// chain would verify.
func TestVerifyRefusesToAcceptATailFromAnotherChain(t *testing.T) {
	ours := chain(t, 4)
	theirs := chain(t, 4)
	theirs[0].Actor = "somebody-else@example.com"
	theirs[0] = Seal(theirs[0], clickhouse.AuditRecord{})
	for i := 1; i < len(theirs); i++ {
		theirs[i] = Seal(theirs[i], theirs[i-1])
	}

	// Records 2..4 of the other chain, offered as ours.
	result := Verify(theirs[1:], ours[0])
	if result.Intact {
		t.Fatal("a run lifted out of another chain verified against our head")
	}
}

func TestTransitionAttributesLifecycleWritesToTheRequester(t *testing.T) {
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "shop",
			Annotations: map[string]string{RequestedByAnnotation: "grace@example.com"},
		},
	}
	transition := Transition{
		Object:     project,
		Kind:       KindProject,
		Operation:  clickhouse.AuditCreate,
		Controller: "project",
	}
	record, err := transition.record()
	if err != nil {
		t.Fatal(err)
	}
	if record.Actor != "grace@example.com" || record.ActorKind != clickhouse.ActorUser {
		t.Errorf("a created project was attributed to %q (%s), want the requester as a user",
			record.Actor, record.ActorKind)
	}
}

// The annotation outlives the request that wrote it, so a phase the platform
// moved on its own must not be attributed to whoever created the object.
func TestTransitionAttributesPlatformDecisionsToTheController(t *testing.T) {
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "shop-bld-1",
			Annotations: map[string]string{RequestedByAnnotation: "grace@example.com"},
		},
	}
	record, err := Transition{
		Object:     build,
		Kind:       KindBuild,
		Controller: "build",
		From:       "Running",
		To:         "Succeeded",
	}.record()
	if err != nil {
		t.Fatal(err)
	}
	if record.Actor != "system:controller/build" || record.ActorKind != clickhouse.ActorService {
		t.Errorf("a build that finished on its own was attributed to %q (%s), want the controller",
			record.Actor, record.ActorKind)
	}
}

func TestTransitionRefusesARecordAboutNothing(t *testing.T) {
	if _, err := (Transition{Kind: KindBuild}).record(); err == nil {
		t.Error("a transition with no object was accepted")
	}
}

func TestTransitionEncodesDetailsDeterministically(t *testing.T) {
	build := &kitchenv1alpha1.Build{ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-1"}}
	transition := Transition{
		Object:     build,
		Kind:       KindBuild,
		Controller: "build",
		Details:    map[string]any{"commit": "abc123", "branch": "main", "attempt": 2},
	}
	first, err := transition.record()
	if err != nil {
		t.Fatal(err)
	}
	second, err := transition.record()
	if err != nil {
		t.Fatal(err)
	}
	if first.Details != second.Details {
		t.Errorf("details encoded two ways: %q then %q", first.Details, second.Details)
	}
	if ChainHash(Seal(first, clickhouse.AuditRecord{})) != ChainHash(Seal(second, clickhouse.AuditRecord{})) {
		t.Error("the same details produced two hashes, so a record could not be re-derived")
	}
}
