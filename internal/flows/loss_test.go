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
	"testing"
	"time"
)

func TestLossCountsWhatHubbleSaysItDropped(t *testing.T) {
	collector := &Collector{}

	// Silence is the ordinary answer, and the one a signal should say nothing
	// about.
	if loss := collector.Loss(time.Hour); !loss.Lossless() {
		t.Errorf("a fresh collector reports %+v, want nothing lost", loss)
	}

	collector.loss.lost(4000)
	collector.loss.lost(1500)
	collector.loss.reconnected()

	loss := collector.Loss(time.Hour)
	if loss.Events != 5500 || loss.Notices != 2 || loss.Reconnects != 1 {
		t.Errorf("loss = %+v, want 5500 events over 2 notices and 1 reconnect", loss)
	}
	if loss.Lossless() {
		t.Error("a window with losses in it should not read as lossless")
	}
	if loss.Window != time.Hour {
		t.Errorf("window = %s, want an hour", loss.Window)
	}
	if loss.Latest.IsZero() {
		t.Error("loss should carry when it last happened")
	}
}

func TestLossForgetsWhatFellOutOfTheWindow(t *testing.T) {
	ledger := &lossLedger{}

	// A bucket the ring has come back around to is stale, not something to add
	// to — that is what makes a fixed array a sliding window rather than a
	// counter that wraps.
	ledger.at(time.Now().Add(-2 * LossWindow)).events = 900
	ledger.lost(10)

	if loss := ledger.snapshot(LossWindow); loss.Events != 10 {
		t.Errorf("loss = %+v, want only the 10 inside the window", loss)
	}

	// A window nobody bounded, or one past what the ledger keeps, is answered
	// with what the ledger keeps rather than refused.
	if loss := ledger.snapshot(0); loss.Window != LossWindow {
		t.Errorf("an unbounded window = %s, want %s", loss.Window, LossWindow)
	}
	if loss := ledger.snapshot(24 * time.Hour); loss.Window != LossWindow {
		t.Errorf("an oversized window = %s, want %s", loss.Window, LossWindow)
	}
}

func TestLossIsSafeToReadWhileTheStreamIsRunning(t *testing.T) {
	// The signals evaluator reads this from whichever goroutine is answering a
	// screen, while the follower is still consuming the stream. Run under -race
	// this is the assertion; without it, it at least exercises both paths.
	collector := &Collector{}
	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for range 500 {
			collector.loss.lost(1)
		}
	}()
	go func() {
		defer group.Done()
		for range 500 {
			_ = collector.Loss(time.Minute)
		}
	}()
	group.Wait()

	if loss := collector.Loss(LossWindow); loss.Events != 500 {
		t.Errorf("loss = %+v, want 500 events", loss)
	}
}
