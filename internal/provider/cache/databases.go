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

package cache

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The logical databases of a server the platform does not run are one finite
// pool shared by every claim through the Connection, so handing them out is a
// write somewhere everybody can see — not a hash, and not a number a
// provisioner works out again from the claim's name.
//
// A hash is what this was, and it is why: two claims hashing to one number
// share a keyspace, and neither is told. The record below is what makes an
// allocation a fact rather than a coincidence — one holder per database,
// read back on every reconcile, and refused when there is nothing left.

const (
	// LegacyDatabase is database 0, and it is not allocated to anybody.
	// Every binding made before the platform allocated databases at all
	// selected it, so a claim that was already bound keeps it — moving a
	// bound claim would hand the application an empty keyspace and leave its
	// data where nothing reads it — and no new claim is ever put there,
	// because a claim not yet reconciled since the upgrade is still holding
	// it without anything saying so.
	LegacyDatabase = 0

	// FirstAllocatableDatabase is where allocation starts, for the reason
	// above.
	FirstAllocatableDatabase = 1

	// databaseWriteAttempts is how many times an allocation re-reads and
	// tries again when the record moved underneath it. Two claims reconciled
	// at once is the case; anything past a couple of rounds is the record
	// being written by something else entirely, and the reconcile requeues.
	databaseWriteAttempts = 5
)

// DatabaseHolding is one logical database of one server and who holds it.
// An empty Holder is a database that has been handed out before and given
// back: it can be handed out again, and what a previous holder left in it is
// still there, because emptying a server the platform does not run is not
// the platform's to do.
type DatabaseHolding struct {
	Database int
	Holder   string
}

// DatabaseLedger is where the allocations of one external server are kept.
//
// It is one method rather than a read and a write because the two have to be
// one act: two claims allocating at the same moment must not both be told
// database 1. Update reads the record, hands it to decide, writes what comes
// back, and does the whole of it again when the record moved in between.
type DatabaseLedger interface {
	Update(ctx context.Context, decide DatabaseDecision) error
}

// DatabaseDecision is what an allocation does with the record it was given:
// answer the updated holdings and whether anything in them changed. A
// decision that changed nothing is not written.
type DatabaseDecision func(holdings []DatabaseHolding) (updated []DatabaseHolding, changed bool, err error)

// connectionLedger keeps the record on the Connection's own status, where an
// operator can read it and where it survives everything short of the
// Connection being deleted — at which point the claims through it are going
// with it.
type connectionLedger struct {
	client client.Client
	key    types.NamespacedName
}

// Update applies decide to the Connection's recorded holdings.
func (l *connectionLedger) Update(ctx context.Context, decide DatabaseDecision) error {
	var lastErr error
	for attempt := 0; attempt < databaseWriteAttempts; attempt++ {
		conn := &kitchenv1alpha1.Connection{}
		if err := l.client.Get(ctx, l.key, conn); err != nil {
			return err
		}
		updated, changed, err := decide(holdingsOf(conn))
		if err != nil {
			return err
		}
		if !changed {
			return nil
		}
		conn.Status.Cache = &kitchenv1alpha1.CacheConnectionStatus{Databases: recordOf(updated)}
		if err := l.client.Status().Update(ctx, conn); err != nil {
			if apierrors.IsConflict(err) {
				// Somebody allocated between the read and the write. Read
				// what they wrote and decide again against it, which is the
				// whole reason this is a loop.
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	// Nothing is wrong: something else is allocating at this connection and
	// this reconcile lost every round. ErrNotReady holds the claim Pending
	// and looks again, where a failure would record a transition that did
	// not happen and leave the claim red until something else woke it.
	return fmt.Errorf("%w: the logical databases of connection %s are being allocated by something else; this "+
		"claim takes one on the next pass (%v)", ErrNotReady, l.key.Name, lastErr)
}

// holdingsOf reads the record off a Connection.
func holdingsOf(conn *kitchenv1alpha1.Connection) []DatabaseHolding {
	if conn.Status.Cache == nil {
		return nil
	}
	holdings := make([]DatabaseHolding, 0, len(conn.Status.Cache.Databases))
	for _, entry := range conn.Status.Cache.Databases {
		holdings = append(holdings, DatabaseHolding{Database: entry.Database, Holder: entry.Holder})
	}
	return holdings
}

// recordOf is the record as the Connection's status carries it.
func recordOf(holdings []DatabaseHolding) []kitchenv1alpha1.CacheDatabase {
	record := make([]kitchenv1alpha1.CacheDatabase, 0, len(holdings))
	for _, holding := range holdings {
		record = append(record, kitchenv1alpha1.CacheDatabase{
			Database: holding.Database,
			Holder:   holding.Holder,
		})
	}
	return record
}
