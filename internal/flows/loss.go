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

package flows

import (
	"sync"
	"time"
)

// Loss accounting, §3.2.
//
// Hubble reports what it dropped rather than hiding it: when a node's ring
// buffer overflows or a consumer falls behind, Relay sends a `LostEvent`
// notice in-stream saying how many events went missing. Nothing was counting
// them, which is the worst kind of wrong for a monitoring system — the numbers
// simply read low, and a quiet under-report is indistinguishable from a quiet
// hour. Counting them lets §7's `ingest.flows-lost` say "request counts
// under-report; N events lost in the last hour" instead.
//
// A stream that ended and was re-established is counted the same way. Nothing
// buffers on Kitchen's behalf while the follower is not connected, so a
// reconnect is a gap of exactly the same kind, only with no notice attached to
// say how large it was.
//
// The counts are kept per minute over a trailing hour rather than as a total
// since start, because that is the sentence the signal wants to say and a
// lifetime counter cannot answer it without the reader remembering what it
// read the time before — which is state a signal is defined not to have.

const (
	// lossBucket is the resolution the ledger keeps and lossBuckets how many
	// of them it keeps, so the two together are how far back it can answer.
	lossBucket  = time.Minute
	lossBuckets = 60

	// LossWindow is how far back Collector.Loss can look. A longer window is
	// answered with this one rather than refused: the caller asked for
	// everything, and everything is an hour.
	LossWindow = lossBucket * lossBuckets
)

// Loss is what the follower knows it did not observe over a trailing window.
type Loss struct {
	// Events is how many observations Hubble reported lost. It is what makes
	// the under-report quantifiable — every one of them is a flow, and some
	// fraction of them were requests.
	Events uint64 `json:"events"`
	// Notices is how many `LostEvent` messages carried those events, which
	// separates one burst from a stream that is losing events continuously.
	Notices uint64 `json:"notices"`
	// Reconnects is how many times the stream broke and had to be
	// re-established. Every one is a gap of unknown size.
	Reconnects uint64 `json:"reconnects"`
	// Window is the span the counts cover, so a reader never has to assume it.
	Window time.Duration `json:"window"`
	// Latest is when the follower last lost anything, and is zero when it
	// never has.
	Latest time.Time `json:"latest,omitempty"`
}

// Lossless answers whether the window holds nothing worth reporting, which is
// the ordinary case and the one a signal should say nothing about.
func (l Loss) Lossless() bool { return l.Events == 0 && l.Notices == 0 && l.Reconnects == 0 }

// lossLedger is the ring the counts live in. Its zero value is ready to use,
// which matters because the Collector is built as a struct literal by the
// manager wiring and has no constructor to run.
//
// It is the one part of the follower a second goroutine touches — the signals
// evaluator reads it while the stream is being consumed — so it is the one
// part that locks.
type lossLedger struct {
	mu      sync.Mutex
	buckets [lossBuckets]lossCount
	latest  time.Time
}

// lossCount is one minute's worth. The minute is stored beside the counts so
// that a bucket the ring has come back around to is recognised as stale rather
// than added to; that is what makes a fixed array a sliding window.
type lossCount struct {
	minute     int64
	events     uint64
	notices    uint64
	reconnects uint64
}

// lost records one notice from Relay.
func (l *lossLedger) lost(events uint64) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket := l.at(now)
	bucket.events += events
	bucket.notices++
	l.latest = now
}

// reconnected records a stream that ended and will be dialled again.
func (l *lossLedger) reconnected() {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.at(now).reconnects++
	l.latest = now
}

// at finds the bucket a moment belongs in, clearing it first if the ring has
// wrapped since it was last written. The caller holds the lock.
func (l *lossLedger) at(now time.Time) *lossCount {
	minute := now.Unix() / int64(lossBucket/time.Second)
	bucket := &l.buckets[minute%lossBuckets]
	if bucket.minute != minute {
		*bucket = lossCount{minute: minute}
	}
	return bucket
}

// snapshot totals the buckets inside a trailing window.
func (l *lossLedger) snapshot(window time.Duration) Loss {
	if window <= 0 || window > LossWindow {
		window = LossWindow
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	oldest := time.Now().Add(-window).Unix() / int64(lossBucket/time.Second)
	loss := Loss{Window: window, Latest: l.latest}
	for _, bucket := range l.buckets {
		// A bucket that was never written holds minute zero, which is 1970 and
		// therefore older than any window anybody can ask for.
		if bucket.minute < oldest {
			continue
		}
		loss.Events += bucket.events
		loss.Notices += bucket.notices
		loss.Reconnects += bucket.reconnects
	}
	return loss
}
