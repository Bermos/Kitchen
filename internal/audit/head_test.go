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
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

const headNamespace = "kitchen-system"

func headRecorder(t *testing.T, objects ...client.Object) *Recorder {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return &Recorder{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(),
		Namespace: headNamespace,
	}
}

// emptyChain is what the table answers when nothing has been appended.
func emptyChain(context.Context) (clickhouse.AuditRecord, error) {
	return clickhouse.AuditRecord{}, nil
}

func headConfig(t *testing.T, r *Recorder) *corev1.ConfigMap {
	t.Helper()
	config := &corev1.ConfigMap{}
	if err := r.Client.Get(context.Background(), client.ObjectKey{
		Namespace: headNamespace, Name: HeadName,
	}, config); err != nil {
		t.Fatal(err)
	}
	return config
}

func TestClaimNumbersTheChainAndAdvancesTheHead(t *testing.T) {
	recorder := headRecorder(t)

	first, previous, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, emptyChain)
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 || first.PrevHash != GenesisHash {
		t.Errorf("the first claim produced %d linked to %s, want 1 linked to the genesis hash",
			first.Sequence, first.PrevHash)
	}
	if previous.Sequence != 0 {
		t.Errorf("the first claim displaced record %d, want none", previous.Sequence)
	}

	second, previous, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "blog"}, emptyChain)
	if err != nil {
		t.Fatal(err)
	}
	if second.Sequence != 2 || second.PrevHash != first.Hash {
		t.Errorf("the second claim produced %d linked to %s, want 2 linked to %s",
			second.Sequence, second.PrevHash, first.Hash)
	}
	if previous.Hash != first.Hash {
		t.Errorf("the second claim displaced %s, want the first record", previous.Hash)
	}

	config := headConfig(t, recorder)
	if config.Data[headKeySequence] != "2" || config.Data[headKeyHash] != second.Hash {
		t.Errorf("the head object says %v, want sequence 2 at %s", config.Data, second.Hash)
	}
}

// A head object that is not there is not necessarily an empty chain: it is
// also an upgrade from before it was kept, or one somebody deleted. Seeding it
// from zero on top of an existing log is the one mistake that turns a sound
// chain into a broken one.
func TestClaimSeedsTheHeadFromTheTableRatherThanFromZero(t *testing.T) {
	recorder := headRecorder(t)
	existing := Seal(clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, clickhouse.AuditRecord{})
	existing.Sequence = 412

	sealed, _, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindBuild, Name: "shop-bld-1"},
		func(context.Context) (clickhouse.AuditRecord, error) { return existing, nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Sequence != 413 {
		t.Errorf("the chain restarted at %d on top of a log that reached 412", sealed.Sequence)
	}
	if sealed.PrevHash != existing.Hash {
		t.Errorf("the first record after re-seeding links to %s, want the table's last record", sealed.PrevHash)
	}
}

func TestClaimReportsAStoreItCannotReadTheHeadFrom(t *testing.T) {
	recorder := headRecorder(t)

	_, _, err := recorder.claim(context.Background(), clickhouse.AuditRecord{Name: "shop"},
		func(context.Context) (clickhouse.AuditRecord, error) {
			return clickhouse.AuditRecord{}, errors.New("the store is unreachable")
		},
	)
	if err == nil {
		t.Fatal("a head that could not be read was treated as an empty chain")
	}
}

// A failed insert gives its number back, so a store that was briefly
// unreachable costs a retry rather than a permanent gap.
func TestReleaseRollsTheHeadBack(t *testing.T) {
	recorder := headRecorder(t)

	sealed, previous, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, emptyChain)
	if err != nil {
		t.Fatal(err)
	}
	recorder.release(context.Background(), sealed, previous)

	config := headConfig(t, recorder)
	if config.Data[headKeySequence] != "0" {
		t.Errorf("the head is at %s after a rollback, want back at 0", config.Data[headKeySequence])
	}

	// The number comes back around, rather than being burned.
	again, _, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, emptyChain)
	if err != nil {
		t.Fatal(err)
	}
	if again.Sequence != 1 {
		t.Errorf("the retry took sequence %d, want the released 1", again.Sequence)
	}
}

// Once something else has appended on top, the number is spent. Rewriting the
// head then would orphan a record that is already in the log, so the gap
// stands and the verifier reports it — which is the honest outcome.
func TestReleaseLeavesTheHeadAloneOnceSomethingElseHasAppended(t *testing.T) {
	recorder := headRecorder(t)

	lost, previous, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, emptyChain)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "blog"}, emptyChain)
	if err != nil {
		t.Fatal(err)
	}

	recorder.release(context.Background(), lost, previous)

	config := headConfig(t, recorder)
	if config.Data[headKeyHash] != next.Hash {
		t.Errorf("a late rollback rewound the head to %s, want it left at %s",
			config.Data[headKeyHash], next.Hash)
	}
}

// The anchor's absence is an answer of its own. It used to be 0, which is also
// the answer for a chain nothing has been appended to — so deleting the object
// and emptying the table agreed with each other, and the platform reported a
// sound chain (#428).
func TestHeadReportsAnAbsentAnchorRatherThanSequenceZero(t *testing.T) {
	recorder := headRecorder(t)

	anchor, err := recorder.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if anchor.Present {
		t.Errorf("a missing head answered a present anchor at %d", anchor.Sequence)
	}
	if anchor.Absence() == "" {
		t.Error("an absent anchor says nothing about why it is absent")
	}

	if _, _, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, emptyChain); err != nil {
		t.Fatal(err)
	}
	anchor, err = recorder.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !anchor.Present || anchor.Sequence != 1 {
		t.Errorf("the anchor reads %+v after one append, want a present anchor at 1", anchor)
	}
	// An anchor created before anything was appended, and one taken from a
	// table that already held records, are different claims. This chain and
	// its anchor started together.
	if anchor.Origin != OriginGenesis {
		t.Errorf("the anchor's origin is %q, want %q", anchor.Origin, OriginGenesis)
	}
}

// A head object written before the origin was recorded still anchors; it just
// cannot say how it came about, and inventing "genesis" for it would be
// claiming provenance nobody wrote down.
func TestAnAnchorFromBeforeThisAnswersAnUnknownOrigin(t *testing.T) {
	recorder := headRecorder(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: HeadName, Namespace: headNamespace},
		Data:       map[string]string{headKeySequence: "412", headKeyHash: strings.Repeat("a", 64)},
	})

	anchor, err := recorder.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !anchor.Present || anchor.Sequence != 412 {
		t.Fatalf("the anchor reads %+v, want a present anchor at 412", anchor)
	}
	if anchor.Origin != OriginUnknown {
		t.Errorf("the anchor's origin is %q, want %q", anchor.Origin, OriginUnknown)
	}
}

// stubStore is the log's store as the anchor's establishment needs it: where
// the table says the chain ends, and somewhere for the record that says the
// anchor was taken from there.
type stubStore struct {
	head      clickhouse.AuditRecord
	inserted  []clickhouse.AuditRecord
	insertErr error
}

func (s *stubStore) AuditHead(context.Context) (clickhouse.AuditRecord, error) {
	return s.head, nil
}

func (s *stubStore) InsertAuditRecord(_ context.Context, record clickhouse.AuditRecord) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	s.inserted = append(s.inserted, record)
	s.head = record
	return nil
}

// A genuinely fresh installation gets its anchor before its first record, and
// nothing is appended to say so: there is nothing to admit to. That is the
// case the whole mechanism turns on — an anchor that exists from the moment
// the platform keeps a log is an anchor whose absence means one thing.
func TestEnsureAnchorSeedsAFreshInstallationOnceAndSaysNothing(t *testing.T) {
	recorder := headRecorder(t)
	store := &stubStore{}

	for range 3 {
		recorder.anchored = false // as if a new process each time
		if err := recorder.establishAnchor(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}

	if len(store.inserted) != 0 {
		t.Errorf("a fresh installation recorded %d anchor record(s), want none: %+v",
			len(store.inserted), store.inserted)
	}
	config := headConfig(t, recorder)
	if config.Data[headKeyOrigin] != string(OriginGenesis) {
		t.Errorf("the anchor's origin is %q, want %q", config.Data[headKeyOrigin], OriginGenesis)
	}
	if config.Data[headKeySequence] != "0" {
		t.Errorf("a fresh anchor starts at %q, want 0", config.Data[headKeySequence])
	}

	// And the chain that follows verifies against it.
	sealed, _, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, store.AuditHead)
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := recorder.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result := Verify([]clickhouse.AuditRecord{sealed}, clickhouse.AuditRecord{}).AgainstAnchor(anchor, false)
	if !result.Intact {
		t.Errorf("a fresh installation's first record does not verify: %+v", result.Findings)
	}
}

// An installation whose anchor exists only in the table — one upgrading from
// before the head object was kept — is adopted once, and the adoption is a
// record in the chain rather than a silent re-seed. Doing it twice, or not
// recording it at all, is the laundering step #428 is about.
func TestEnsureAnchorAdoptsATableOnlyChainOnceAndRecordsIt(t *testing.T) {
	recorder := headRecorder(t)
	existing := Seal(clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, clickhouse.AuditRecord{})
	existing.Sequence = 412
	existing.Hash = ChainHash(existing)
	store := &stubStore{head: existing}

	for range 3 {
		recorder.anchored = false
		if err := recorder.establishAnchor(context.Background(), store); err != nil {
			t.Fatal(err)
		}
	}

	if len(store.inserted) != 1 {
		t.Fatalf("the adoption was recorded %d time(s), want exactly one: %+v",
			len(store.inserted), store.inserted)
	}
	record := store.inserted[0]
	if record.Kind != KindAuditAnchor {
		t.Errorf("the adoption was recorded as kind %q, want %q", record.Kind, KindAuditAnchor)
	}
	if record.Sequence != 413 || record.PrevHash != existing.Hash {
		t.Errorf("the adoption record is %d linked to %s, want 413 linked to the table's last record",
			record.Sequence, record.PrevHash)
	}
	// It is in the chain, not beside it: removing it later is a break the
	// verifier reports, which is what makes a second adoption impossible to
	// tidy away.
	if ChainHash(record) != record.Hash {
		t.Error("the adoption record is not sealed into the chain")
	}
	class, privileged := PrivilegeOf(record.Details)
	if !privileged || class != PrivilegeIntegrity {
		t.Errorf("the adoption is classified %q/%v, want an integrity act", class, privileged)
	}
	if !strings.Contains(record.Details, ChangeAuditAnchorAdopted) {
		t.Errorf("the adoption record does not name the change: %s", record.Details)
	}
	if !strings.Contains(record.Reason, "412") {
		t.Errorf("the adoption record does not say where the numbering was taken from: %s", record.Reason)
	}

	config := headConfig(t, recorder)
	if config.Data[headKeyOrigin] != string(OriginAdopted) {
		t.Errorf("the anchor's origin is %q, want %q", config.Data[headKeyOrigin], OriginAdopted)
	}
	if config.Data[headKeyAdoptedFrom] != "412" {
		t.Errorf("the anchor was adopted from %q, want 412", config.Data[headKeyAdoptedFrom])
	}
	if config.Data[headKeyAdoptionRecorded] != headAdoptionDone {
		t.Error("the head does not record that the adoption reached the log")
	}
	anchor, err := recorder.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if anchor.Origin != OriginAdopted || anchor.AdoptedFrom != 412 {
		t.Errorf("the published anchor is %+v, want an adopted one from 412", anchor)
	}
}

// An adoption whose record did not land is not an adoption. The head keeps
// saying so until the append succeeds, because a process that gave up here
// would leave exactly the silent re-seed this exists to prevent.
func TestAnAdoptionWhoseRecordDidNotLandIsTriedAgain(t *testing.T) {
	recorder := headRecorder(t)
	existing := Seal(clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, clickhouse.AuditRecord{})
	existing.Sequence = 412
	existing.Hash = ChainHash(existing)
	store := &stubStore{head: existing, insertErr: errors.New("the store is unreachable")}

	if err := recorder.establishAnchor(context.Background(), store); err == nil {
		t.Fatal("an adoption nothing recorded was reported as done")
	}
	if headConfig(t, recorder).Data[headKeyAdoptionRecorded] != "false" {
		t.Fatal("the head claims the adoption was recorded when the append failed")
	}

	store.insertErr = nil
	recorder.anchored = false
	if err := recorder.establishAnchor(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	if len(store.inserted) != 1 {
		t.Errorf("the retry recorded %d adoption(s), want one", len(store.inserted))
	}
	if headConfig(t, recorder).Data[headKeyAdoptionRecorded] != headAdoptionDone {
		t.Error("the head still does not record that the adoption reached the log")
	}
}

// The head going backwards because this replica put a number back is not the
// head going backwards. A failed insert costs a retry by design, and a refusal
// here would turn every briefly unreachable store into a wedged log.
func TestAReleasedNumberIsNotAHeadThatHasGoneBackwards(t *testing.T) {
	recorder := headRecorder(t)
	for range 2 {
		if _, _, err := recorder.claim(context.Background(),
			clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, emptyChain); err != nil {
			t.Fatal(err)
		}
	}

	sealed, previous, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "blog"}, emptyChain)
	if err != nil {
		t.Fatal(err)
	}
	// As Record does when the store refuses the insert.
	recorder.release(context.Background(), sealed, previous)
	if headConfig(t, recorder).Data[headKeySequence] != "2" {
		t.Fatalf("the number was not given back: %s", headConfig(t, recorder).Data[headKeySequence])
	}

	retried, _, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "blog"}, emptyChain)
	if err != nil {
		t.Fatalf("the retry after a released number was refused: %v", err)
	}
	if retried.Sequence != 3 {
		t.Errorf("the retry took sequence %d, want the number that was given back", retried.Sequence)
	}
}

// The head only ever moves forward. One that has gone backwards under a
// running platform — wound back by hand, or deleted and re-seeded from a table
// somebody has since truncated — would renumber records the log already holds,
// so the append fails and says so rather than carrying on.
func TestClaimRefusesAHeadThatHasGoneBackwards(t *testing.T) {
	recorder := headRecorder(t)
	for range 3 {
		if _, _, err := recorder.claim(context.Background(),
			clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, emptyChain); err != nil {
			t.Fatal(err)
		}
	}

	// Somebody winds the anchor back to where a truncated tail would put it.
	config := headConfig(t, recorder)
	config.Data[headKeySequence] = "1"
	if err := recorder.Client.Update(context.Background(), config); err != nil {
		t.Fatal(err)
	}

	_, _, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "blog"}, emptyChain)
	if err == nil {
		t.Fatal("a claim against a head that had gone backwards was allowed")
	}
	if !strings.Contains(err.Error(), "backwards") {
		t.Errorf("the refusal does not say what happened: %v", err)
	}
}

// The head is a plain ConfigMap, so it can be inspected — and edited — by
// anyone with access to the namespace. A head somebody wound back has to be
// visible rather than silently obeyed, which is what the verifier's anchor
// comparison is for; what must not happen is the recorder mistaking an
// unparseable value for something meaningful.
func TestReadHeadTreatsAnUnreadableSequenceAsZero(t *testing.T) {
	recorder := headRecorder(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: HeadName, Namespace: headNamespace},
		Data:       map[string]string{headKeySequence: "not a number", headKeyHash: "whatever"},
	})

	head, _, err := recorder.readHead(context.Background(), emptyChain)
	if err != nil {
		t.Fatal(err)
	}
	if head.Sequence != 0 {
		t.Errorf("an unreadable sequence became %d", head.Sequence)
	}
}
