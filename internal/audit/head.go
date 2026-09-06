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
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The chain's head, and how a sequence number is claimed.
//
// A hash chain needs its appends serialized: the next hash is a function of
// the last one, so two appenders that both read head N produce two records
// numbered N+1, each of which verifies and neither of which is the log. An
// in-process mutex is not enough, because the manager's REST API answers on
// every replica while its reconcilers run only on the leader.
//
// So the head is claimed through a ConfigMap, and the API server's own
// optimistic concurrency does the serializing: read the head, compute the next
// record, write the head back at the resourceVersion it was read at. Exactly
// one writer wins a contested round and the losers retry against the new head.
// This is a Kubernetes API round trip per audit record, which is affordable
// precisely because audit records are human-scale — deploys, edits, promotions,
// not requests.
//
// Keeping the head outside the table has a second effect worth having. It is
// an anchor: a log truncated from the end still rehashes perfectly, so the
// chain cannot see its own tail being cut, but the head object says where the
// tail should have been.
//
// # The anchor has to survive its own removal (#428)
//
// An anchor that answers "sequence 0" when it is not there is worth nothing:
// deleting the object and truncating the table then agree, and the platform
// reports a sound chain. Three things together are what make the anchor mean
// something:
//
//   - Absence is an answer of its own. Head reports (Anchor, error) and an
//     Anchor that is not Present is not an Anchor at sequence 0; the verifier
//     turns it into a finding rather than into an empty field.
//   - The anchor exists from the moment the platform starts keeping a log,
//     not from the first append — EnsureAnchor is called by the compliance
//     reconcile. So "no anchor and no records" is *never* a fresh
//     installation once the platform has reconciled one, and "empty because
//     nothing was written" and "empty because somebody emptied it" are
//     different answers.
//   - Adopting the table's own last record is a privileged event and is
//     recorded as one, inside the chain. An installation upgrading from
//     before the head existed adopts once and says so; an attacker who
//     deletes the anchor and truncates the tail produces a second adoption
//     record, and cannot remove the first without breaking the chain.
const (
	// HeadName is the ConfigMap the head lives in, in the platform namespace.
	HeadName = "kitchen-audit-head"

	headKeySequence = "sequence"
	headKeyHash     = "hash"

	// headKeyOrigin, headKeyEstablished and headKeyAdoptedFrom are how the
	// head object came to exist, kept because "this chain has always been
	// anchored" and "this chain's numbering was taken from the table once,
	// on this date, at this sequence" are different claims and only one of
	// them is worth much.
	headKeyOrigin      = "origin"
	headKeyEstablished = "establishedAt"
	headKeyAdoptedFrom = "adoptedFrom"

	// headKeyAdoptionRecorded is false between the head being adopted and
	// the record saying so landing in the log. It is a key rather than a
	// process variable because the append can fail, and an adoption nothing
	// recorded is the laundering step this whole mechanism exists to
	// prevent.
	headKeyAdoptionRecorded = "adoptionRecorded"

	// headAdoptionDone is the value that key takes once the record has
	// landed. Anything else — including the object having no such key, which
	// is a head that was never adopted — is not "done".
	headAdoptionDone = "true"

	// headClaimAttempts bounds the optimistic retry. Contention here is two
	// replicas recording at the same instant, which resolves in one round;
	// a handful of attempts covers a burst without turning a wedged API
	// server into an unbounded loop.
	headClaimAttempts = 5
)

// AnchorOrigin says how the head object came to exist. It is stored in the
// object and published, because an anchor that was copied out of the table it
// is supposed to bound is a weaker claim than one that has been there since
// the chain started, and a reader is entitled to know which they have.
type AnchorOrigin string

const (
	// OriginGenesis is an anchor created before anything was appended: the
	// chain and its anchor start together, which is the strong case.
	OriginGenesis AnchorOrigin = "genesis"

	// OriginAdopted is an anchor seeded from the log's own last record. It
	// bounds everything appended after it and nothing before it, and the
	// chain carries a record saying exactly when that line falls.
	OriginAdopted AnchorOrigin = "adopted"

	// OriginUnknown is a head object written before this was recorded — an
	// installation upgrading across #428. It anchors; it just cannot say
	// how it came about.
	OriginUnknown AnchorOrigin = "unknown"
)

// Anchor is where the chain ends according to the object outside the table.
//
// Present is the field that matters and it is why this is a struct rather than
// an int64: an anchor that is not there used to answer 0, which reads
// downstream as "the chain ends at the beginning" — no gap, nothing wrong —
// and is precisely the answer an emptied table agrees with.
type Anchor struct {
	// Present is false when there is no anchor to check anything against,
	// either because the object is gone or because it could never be read.
	Present bool
	// Sequence and Hash are the end of the chain as the object holds it.
	// They mean nothing unless Present.
	Sequence int64
	Hash     string
	// Origin, EstablishedAt and AdoptedFrom are the anchor's own provenance.
	Origin        AnchorOrigin
	EstablishedAt string
	AdoptedFrom   int64
	// Unreadable explains an anchor that could not be read at all, as
	// opposed to one that was read and found absent. Both leave the run with
	// nothing to check against; a reader still has to be able to tell a
	// deleted anchor from a cluster that did not answer.
	Unreadable string
}

// Absence words why there is nothing to check against. A deleted anchor and a
// cluster that did not answer are the same gap and very different events, and
// a reader deciding whether to page somebody needs to know which.
func (a Anchor) Absence() string {
	if a.Unreadable != "" {
		return a.Unreadable
	}
	return "the head object outside the table is not there"
}

// claim allocates the next sequence number for a record and seals it,
// advancing the head as it goes.
//
// It returns the sealed record and the head it displaced, because the caller
// needs the latter to put the head back if the insert that follows does not
// land.
func (r *Recorder) claim(
	ctx context.Context,
	record clickhouse.AuditRecord,
	tableHead func(context.Context) (clickhouse.AuditRecord, error),
) (clickhouse.AuditRecord, clickhouse.AuditRecord, error) {
	var lastErr error
	for attempt := 0; attempt < headClaimAttempts; attempt++ {
		head, config, err := r.readHead(ctx, tableHead)
		if err != nil {
			return clickhouse.AuditRecord{}, clickhouse.AuditRecord{}, err
		}
		// The head only ever moves forward, and a claim against one that has
		// gone backwards under a running platform would renumber records the
		// log already holds. That is the anchor being wound back — by hand,
		// or by the object being deleted and re-seeded from a table somebody
		// has since truncated — and the honest answer is to stop appending
		// and say so, not to carry on from a number nobody can account for.
		// Failing here fails the write that caused it, which is the contract
		// Record already has: an unrecorded change is not one the platform
		// makes.
		if head.Sequence < r.floor {
			return clickhouse.AuditRecord{}, clickhouse.AuditRecord{}, fmt.Errorf(
				"the audit chain's head has gone backwards, from %d to %d: the anchor in %s/%s has been "+
					"wound back or replaced, and appending here would renumber records the log already "+
					"holds. Nothing is appended until somebody accounts for it",
				r.floor, head.Sequence, r.Namespace, HeadName)
		}
		sealed := Seal(record, head)
		writeHead(config, sealed)
		switch err := r.Client.Update(ctx, config); {
		case err == nil:
			// The floor moves only on a number this replica actually took.
			// Moving it on the read would make another replica's rollback
			// (release, below) look like the anchor going backwards, and a
			// spurious refusal here fails a write that had nothing wrong
			// with it.
			r.floor = sealed.Sequence
			return sealed, head, nil
		case apierrors.IsConflict(err):
			// Somebody else claimed this number in the moment between the
			// read and the write. Their record is the one at that sequence;
			// this one takes the next.
			lastErr = err
		default:
			return clickhouse.AuditRecord{}, clickhouse.AuditRecord{}, err
		}
	}
	return clickhouse.AuditRecord{}, clickhouse.AuditRecord{}, fmt.Errorf(
		"the audit chain's head stayed contended over %d attempts: %w", headClaimAttempts, lastErr)
}

// release puts the head back after an append that did not land, so that a
// failed insert costs a retry rather than a permanent gap in the chain.
//
// It only rolls back a head that is still the one this claim wrote. Once
// something else has appended on top, the number is spent: a gap the verifier
// will report is the honest outcome, and better than rewriting a chain other
// records are already hanging off.
func (r *Recorder) release(ctx context.Context, claimed, previous clickhouse.AuditRecord) {
	config := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: r.Namespace, Name: HeadName}
	if err := r.reader().Get(ctx, key, config); err != nil {
		return
	}
	current := readHeadData(config)
	if current.Sequence != claimed.Sequence || current.Hash != claimed.Hash {
		return
	}
	writeHead(config, previous)
	if err := r.Client.Update(ctx, config); err != nil {
		return
	}
	// The number was given back, so the floor comes back with it: this
	// replica no longer claims to have taken it, and the next claim must not
	// read its own rollback as the anchor being wound back.
	r.floor = previous.Sequence
}

// readHead reads the head object, creating it when it is not there.
//
// A missing head object is not necessarily an empty chain: it is also an
// installation upgrading from before the head was kept here, or one where
// somebody deleted it. So it is seeded from the table's own last record rather
// than from zero — seeding from zero would restart the numbering on top of an
// existing log, which is the one mistake that turns a sound chain into a
// broken one.
//
// Seeding from the table is *adoption*, and the object records that it
// happened: which sequence it was taken from and when. It is the moment the
// platform stops being able to account for the numbering it inherited, and
// EnsureAnchor is what makes sure the chain says so out loud rather than the
// object saying so only to whoever thinks to read it (#428).
func (r *Recorder) readHead(
	ctx context.Context,
	tableHead func(context.Context) (clickhouse.AuditRecord, error),
) (clickhouse.AuditRecord, *corev1.ConfigMap, error) {
	config := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: r.Namespace, Name: HeadName}
	err := r.reader().Get(ctx, key, config)
	if err == nil {
		return readHeadData(config), config, nil
	}
	if !apierrors.IsNotFound(err) {
		return clickhouse.AuditRecord{}, nil, err
	}

	seed, err := tableHead(ctx)
	if err != nil {
		return clickhouse.AuditRecord{}, nil, fmt.Errorf("the audit chain's head could not be read back: %w", err)
	}
	config = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      HeadName,
			Namespace: r.Namespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kitchen"},
		},
		Data: map[string]string{
			headKeyOrigin:      string(OriginGenesis),
			headKeyEstablished: time.Now().UTC().Format(time.RFC3339),
		},
	}
	// A table with records in it and no anchor is an adoption, and the two
	// keys below are what makes it one that can be spoken about afterwards.
	// A table with nothing in it is a chain that starts here, with its
	// anchor, which is the case worth being able to name.
	if seed.Sequence > 0 {
		config.Data[headKeyOrigin] = string(OriginAdopted)
		config.Data[headKeyAdoptedFrom] = strconv.FormatInt(seed.Sequence, 10)
		config.Data[headKeyAdoptionRecorded] = "false"
	}
	writeHead(config, seed)
	if err := r.Client.Create(ctx, config); err != nil {
		if apierrors.IsAlreadyExists(err) {
			// Another replica seeded it first. Read theirs.
			if err := r.reader().Get(ctx, key, config); err != nil {
				return clickhouse.AuditRecord{}, nil, err
			}
			return readHeadData(config), config, nil
		}
		return clickhouse.AuditRecord{}, nil, err
	}
	return seed, config, nil
}

// readHeadData is the head as the object holds it. A sequence that will not
// parse reads as 0, which starts the chain over — deliberately loud rather
// than silently continuing from a number nobody can account for, because the
// verifier reports the result either way.
func readHeadData(config *corev1.ConfigMap) clickhouse.AuditRecord {
	sequence, _ := strconv.ParseInt(config.Data[headKeySequence], 10, 64)
	return clickhouse.AuditRecord{Sequence: sequence, Hash: config.Data[headKeyHash]}
}

// anchorFrom is the head object read as the anchor a verifier checks against.
//
// A head with no origin recorded reads as OriginUnknown rather than as
// genesis: it is an installation upgrading across #428, and claiming its
// anchor has been there since the chain started would be inventing provenance
// nobody wrote down.
func anchorFrom(config *corev1.ConfigMap) Anchor {
	head := readHeadData(config)
	origin := AnchorOrigin(config.Data[headKeyOrigin])
	switch origin {
	case OriginGenesis, OriginAdopted:
	default:
		origin = OriginUnknown
	}
	adopted, _ := strconv.ParseInt(config.Data[headKeyAdoptedFrom], 10, 64)
	return Anchor{
		Present:       true,
		Sequence:      head.Sequence,
		Hash:          head.Hash,
		Origin:        origin,
		EstablishedAt: config.Data[headKeyEstablished],
		AdoptedFrom:   adopted,
	}
}

func writeHead(config *corev1.ConfigMap, record clickhouse.AuditRecord) {
	if config.Data == nil {
		config.Data = map[string]string{}
	}
	config.Data[headKeySequence] = strconv.FormatInt(record.Sequence, 10)
	config.Data[headKeyHash] = record.Hash
}

// Head reports where the chain ends, according to the object sequence numbers
// are claimed through.
//
// This is the anchor the package comment describes, and it is read straight
// from the cluster rather than from anything this process remembers: a
// verification that walks the table has to be checked against something the
// table did not produce.
//
// An anchor that is not there answers an Anchor that is not Present, and never
// an Anchor at sequence 0. That distinction is the whole of #428: zero was
// read downstream as "the chain ends at the beginning" — no gap, nothing to
// report — which is exactly what an emptied table agrees with.
func (r *Recorder) Head(ctx context.Context) (Anchor, error) {
	if r == nil {
		return Anchor{Unreadable: "this build records no audit log, so there is no anchor"}, nil
	}
	config := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: r.Namespace, Name: HeadName}
	if err := r.reader().Get(ctx, key, config); err != nil {
		if apierrors.IsNotFound(err) {
			return Anchor{}, nil
		}
		return Anchor{Unreadable: "the cluster did not answer for the anchor"}, err
	}
	return anchorFrom(config), nil
}

// markAdoptionRecorded flips the head object's adoptionRecorded key once the
// record saying the anchor was adopted is in the log.
//
// It re-reads the object rather than writing back the one the adoption was
// read from, because claiming the record's own sequence number has moved the
// head since. A conflict is retried; anything else is reported, because an
// adoption whose flag never lands is one that gets recorded a second time on
// the next process to notice, and a duplicate is confusing in a place where
// confusion is the whole cost.
func (r *Recorder) markAdoptionRecorded(ctx context.Context) error {
	key := types.NamespacedName{Namespace: r.Namespace, Name: HeadName}
	var lastErr error
	for attempt := 0; attempt < headClaimAttempts; attempt++ {
		config := &corev1.ConfigMap{}
		if err := r.reader().Get(ctx, key, config); err != nil {
			return err
		}
		if config.Data == nil {
			config.Data = map[string]string{}
		}
		config.Data[headKeyAdoptionRecorded] = headAdoptionDone
		switch err := r.Client.Update(ctx, config); {
		case err == nil:
			return nil
		case apierrors.IsConflict(err):
			lastErr = err
		default:
			return err
		}
	}
	return lastErr
}

// auditStore is the two things establishing an anchor needs of the log's
// store: where the table says the chain ends, and somewhere to append the
// record that says the anchor was taken from there. *clickhouse.Client is the
// implementation; naming the pair keeps this file testable without one.
type auditStore interface {
	AuditHead(ctx context.Context) (clickhouse.AuditRecord, error)
	InsertAuditRecord(ctx context.Context, record clickhouse.AuditRecord) error
}

// EnsureAnchor makes sure this installation's chain has an anchor, and that
// an anchor taken from the table says so in the log.
//
// It is called by the compliance reconcile rather than only by the first
// append, and that ordering is the point. An anchor created lazily cannot
// tell a fresh installation from an emptied one — both are "no object, no
// records" — while an anchor that exists from the moment the platform starts
// keeping a log makes its absence mean one thing: somebody removed it.
//
// A nil Recorder, a log that is turned off and an installation with no store
// are all "there is no log to anchor", and none of them is an error.
func (r *Recorder) EnsureAnchor(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	store, err := r.resolve(ctx)
	if err != nil {
		return fmt.Errorf("the audit log could not be reached: %w", err)
	}
	if store == nil {
		return nil
	}
	return r.establishAnchor(ctx, store)
}

// establishAnchor is EnsureAnchor's body, for a caller that already holds r.mu
// and has resolved the store.
//
// The remembered flag is this process's, and it is only an optimization: the
// object's own adoptionRecorded key is what decides, so two replicas cannot
// both conclude the record still needs writing after one of them has written
// it.
func (r *Recorder) establishAnchor(ctx context.Context, store auditStore) error {
	if r.anchored {
		return nil
	}
	_, config, err := r.readHead(ctx, store.AuditHead)
	if err != nil {
		return err
	}
	if AnchorOrigin(config.Data[headKeyOrigin]) == OriginAdopted &&
		config.Data[headKeyAdoptionRecorded] != headAdoptionDone {
		if err := r.recordAdoption(ctx, store, config); err != nil {
			return err
		}
	}
	r.anchored = true
	return nil
}

// recordAdoption appends the record that says the chain's anchor was taken
// from the log's own last record.
//
// This is the one moment the platform admits it cannot account for the
// numbering it is continuing, and the admission belongs *inside* the chain
// rather than beside it: a record here is hash-linked like any other, so it
// cannot be removed without the verifier reporting the removal, and a second
// adoption — an anchor deleted and re-seeded from a truncated table — leaves a
// second record next to the first.
//
// It is classified as an integrity act for the same reason. On an upgrade from
// before the head existed nobody did anything wrong and the record is a
// migration note; on a live platform it is §11.4's case exactly, and the two
// are the same event seen from two sides.
func (r *Recorder) recordAdoption(
	ctx context.Context,
	store auditStore,
	config *corev1.ConfigMap,
) error {
	adopted, _ := strconv.ParseInt(config.Data[headKeyAdoptedFrom], 10, 64)
	details, err := json.Marshal(map[string]any{
		PrivilegedDetail:     true,
		PrivilegeClassDetail: string(PrivilegeIntegrity),
		"change":             ChangeAuditAnchorAdopted,
		"adoptedFrom":        adopted,
		"origin":             string(OriginAdopted),
	})
	if err != nil {
		return fmt.Errorf("the audit anchor's adoption record could not be encoded: %w", err)
	}

	record := clickhouse.AuditRecord{
		Actor:       ControllerActor("audit"),
		ActorKind:   clickhouse.ActorService,
		Correlation: HeadName,
		Operation:   clickhouse.AuditCreate,
		Kind:        KindAuditAnchor,
		Namespace:   r.Namespace,
		Name:        HeadName,
		ToState:     string(OriginAdopted),
		Reason: fmt.Sprintf(
			"the audit chain had no anchor, and one was adopted from the log's own last record, "+
				"sequence %d. Records up to %d are bounded by the chain alone; everything after is "+
				"bounded by the anchor as well", adopted, adopted),
		Details: string(details),
	}

	sealed, previous, err := r.claim(ctx, record, store.AuditHead)
	if err != nil {
		return err
	}
	if err := store.InsertAuditRecord(ctx, sealed); err != nil {
		r.release(context.WithoutCancel(ctx), sealed, previous)
		return fmt.Errorf("the audit anchor's adoption could not be recorded: %w", err)
	}
	r.sequence = sealed.Sequence
	return r.markAdoptionRecorded(ctx)
}

// reader is where the head is read from: the API server directly, so that the
// version handed back is the one an update can be made against.
func (r *Recorder) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}
