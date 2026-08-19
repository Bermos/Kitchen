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
	"fmt"
	"strconv"

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
const (
	// HeadName is the ConfigMap the head lives in, in the platform namespace.
	HeadName = "kitchen-audit-head"

	headKeySequence = "sequence"
	headKeyHash     = "hash"

	// headClaimAttempts bounds the optimistic retry. Contention here is two
	// replicas recording at the same instant, which resolves in one round;
	// a handful of attempts covers a burst without turning a wedged API
	// server into an unbounded loop.
	headClaimAttempts = 5
)

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
		sealed := Seal(record, head)
		writeHead(config, sealed)
		switch err := r.Client.Update(ctx, config); {
		case err == nil:
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
	_ = r.Client.Update(ctx, config)
}

// readHead reads the head object, creating it when it is not there.
//
// A missing head object is not necessarily an empty chain: it is also an
// installation upgrading from before the head was kept here, or one where
// somebody deleted it. So it is seeded from the table's own last record rather
// than from zero — seeding from zero would restart the numbering on top of an
// existing log, which is the one mistake that turns a sound chain into a
// broken one.
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
		Data: map[string]string{},
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
func (r *Recorder) Head(ctx context.Context) (int64, error) {
	if r == nil {
		return 0, nil
	}
	config := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: r.Namespace, Name: HeadName}
	if err := r.reader().Get(ctx, key, config); err != nil {
		if apierrors.IsNotFound(err) {
			// Nothing has been appended yet, or the object was removed. Zero
			// claims nothing, which is the right answer to both.
			return 0, nil
		}
		return 0, err
	}
	return readHeadData(config).Sequence, nil
}

// reader is where the head is read from: the API server directly, so that the
// version handed back is the one an update can be made against.
func (r *Recorder) reader() client.Reader {
	if r.Reader != nil {
		return r.Reader
	}
	return r.Client
}
