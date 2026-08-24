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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

func subject() *kitchenv1alpha1.Project {
	return &kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "uid-1"}}
}

// The classification reaches the stored record, both keys, and is readable
// back out of it. This is the acceptance criterion "privileged actions are
// separable from ordinary ones in the audit log", at the level the record is
// built.
func TestAPrivilegedTransitionCarriesItsClassIntoTheRecord(t *testing.T) {
	record, err := Transition{
		Object:     subject(),
		Kind:       KindProject,
		Operation:  clickhouse.AuditUpdate,
		Privileged: PrivilegeAccess,
		Reason:     "grace was given admin on shop",
		Details:    map[string]any{"member": "grace@example.com"},
	}.record()
	if err != nil {
		t.Fatalf("building the record: %v", err)
	}

	class, privileged := PrivilegeOf(record.Details)
	if !privileged || class != PrivilegeAccess {
		t.Fatalf("the record must read back as a privileged access record, got %q/%v", class, privileged)
	}
	// The caller's own details survive alongside the classification: the
	// recorder adds two keys, it does not replace the map.
	if got := record.Details; !strings.Contains(got, `"member":"grace@example.com"`) {
		t.Fatalf("the caller's details must survive the classification: %s", got)
	}
}

// An ordinary transition claims nothing. A reader that treated an unmarked
// record as privileged would make the filter useless in the one direction
// that matters.
func TestAnOrdinaryTransitionIsNotPrivileged(t *testing.T) {
	record, err := Transition{
		Object:    subject(),
		Kind:      KindProject,
		Operation: clickhouse.AuditUpdate,
		Reason:    "project shop settings changed",
		Details:   map[string]any{"fields": []string{"branch"}},
	}.record()
	if err != nil {
		t.Fatalf("building the record: %v", err)
	}
	if class, privileged := PrivilegeOf(record.Details); privileged || class != "" {
		t.Fatalf("an unclassified transition must read as ordinary, got %q/%v", class, privileged)
	}
}

// The classification is inside what the chain hashes. That is the whole
// argument for putting it in the details rather than in a column: a
// privileged marking added or removed after the fact breaks verification.
func TestTheClassificationIsCoveredByTheChain(t *testing.T) {
	record, err := Transition{
		Object:     subject(),
		Kind:       KindProject,
		Operation:  clickhouse.AuditUpdate,
		Privileged: PrivilegeAccess,
		Reason:     "grace was given admin on shop",
	}.record()
	if err != nil {
		t.Fatalf("building the record: %v", err)
	}
	sealed := Seal(record, clickhouse.AuditRecord{})

	// Somebody who can reach the row takes the marking off.
	stripped := sealed
	stripped.Details = `{"privilegedClass":"access"}`
	if result := Verify([]clickhouse.AuditRecord{stripped}, clickhouse.AuditRecord{}); result.Intact {
		t.Fatal("stripping the privileged marking must break the chain, and did not")
	}
	if result := Verify([]clickhouse.AuditRecord{sealed}, clickhouse.AuditRecord{}); !result.Intact {
		t.Fatalf("the untouched record must verify: %+v", result.Findings)
	}
}

// A record written before the vocabulary existed carries `privileged: true`
// and no class. It is still privileged: the boolean is what every stored
// record keys on and what the filter is built over.
func TestARecordWithNoClassIsStillPrivileged(t *testing.T) {
	class, privileged := PrivilegeOf(`{"privileged":true,"reason":"a waiver from an older Kitchen"}`)
	if !privileged {
		t.Fatal("a record marked privileged with no class must still read as privileged")
	}
	if class != "" {
		t.Fatalf("no class was written, so none may be reported, got %q", class)
	}
}

// Unreadable details claim nothing in either direction. Details is opaque to
// this package and hashed verbatim; guessing at it would be a claim the
// chain cannot back.
func TestUnreadableDetailsClaimNothing(t *testing.T) {
	for _, details := range []string{"", "not json", `"a string"`, `{"privileged":"yes"}`} {
		if class, privileged := PrivilegeOf(details); privileged || class != "" {
			t.Fatalf("details %q must claim nothing, got %q/%v", details, class, privileged)
		}
	}
}

func TestOnlyTheVocabularyIsAValidClass(t *testing.T) {
	for _, privilege := range Privileges() {
		if !privilege.Valid() {
			t.Fatalf("%q is published and must be valid", privilege)
		}
	}
	for _, bad := range []Privilege{"", "PRIVILEGED", "root", "break glass"} {
		if bad.Valid() {
			t.Fatalf("%q is not a class of privileged act", bad)
		}
	}
}
