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

func TestHeadIsTheAnchorAndClaimsNothingWhenItIsNotThere(t *testing.T) {
	recorder := headRecorder(t)

	sequence, err := recorder.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 0 {
		t.Errorf("a missing head answered %d, want 0 — which claims nothing", sequence)
	}

	if _, _, err := recorder.claim(context.Background(),
		clickhouse.AuditRecord{Kind: KindProject, Name: "shop"}, emptyChain); err != nil {
		t.Fatal(err)
	}
	sequence, err = recorder.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if sequence != 1 {
		t.Errorf("the anchor reads %d after one append, want 1", sequence)
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
