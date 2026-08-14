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

package v1alpha1

import (
	"fmt"
	"testing"
)

func TestRecordReleaseMove(t *testing.T) {
	env := &Environment{}

	if env.RecordReleaseMove("", ReleaseMoveSuperseded, "") {
		t.Fatal("no stint to close without an outgoing release")
	}
	if !env.RecordReleaseMove("rel-1", ReleaseMovePromoted, "bld-2") {
		t.Fatal("the first move should record")
	}
	if env.RecordReleaseMove("rel-1", ReleaseMoveSuperseded, "someone-else") {
		t.Fatal("a move whose entry already exists must not record again")
	}
	if len(env.Status.History) != 1 {
		t.Fatalf("want the one entry, got %+v", env.Status.History)
	}
	if got := env.Status.History[0]; got.Reason != ReleaseMovePromoted || got.By != "bld-2" {
		t.Fatalf("the second writer must not overwrite the first: %+v", got)
	}

	if !env.RecordReleaseMove("rel-2", ReleaseMoveRolledBack, "ada@example.com") {
		t.Fatal("a different outgoing release should record")
	}
	newest, older := env.Status.History[0], env.Status.History[1]
	if newest.Release != "rel-2" || older.Release != "rel-1" {
		t.Fatalf("want newest first, got %+v", env.Status.History)
	}
	if !newest.From.Equal(&older.To) {
		t.Fatalf("a stint starts where the previous one ended: %+v then %+v", older, newest)
	}
}

func TestRecordReleaseMoveKeepsOnlyRecentHistory(t *testing.T) {
	env := &Environment{}
	for i := 1; i <= MaxReleaseHistory+5; i++ {
		if !env.RecordReleaseMove(fmt.Sprintf("rel-%d", i), ReleaseMovePromoted, "bld") {
			t.Fatalf("move %d should record", i)
		}
	}
	if len(env.Status.History) != MaxReleaseHistory {
		t.Fatalf("want the history capped at %d, got %d", MaxReleaseHistory, len(env.Status.History))
	}
	if got := env.Status.History[0].Release; got != fmt.Sprintf("rel-%d", MaxReleaseHistory+5) {
		t.Fatalf("want the newest entry kept, got %q", got)
	}
}
