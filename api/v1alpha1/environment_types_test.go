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

// #494's default, which is the whole of the safe half: an environment that
// declares nothing serves nobody, and a declaration is read in the platform's
// own order rather than the order somebody typed it.
func TestAnEnvironmentServesNobodyUntilItSaysOtherwise(t *testing.T) {
	var absent *Environment
	if absent.Admits(EnvironmentPreview) {
		t.Fatal("no environment admits anything")
	}
	env := &Environment{}
	for _, class := range EnvironmentTypes() {
		if env.Admits(class) {
			t.Fatalf("an environment declaring nothing must not admit %s", class)
		}
	}
	if got := env.ServedConsumers(); len(got) != 0 {
		t.Fatalf("want nobody, got %v", got)
	}

	env.Spec.Serves = &EnvironmentServes{Consumers: []EnvironmentType{}}
	if got := env.ServedConsumers(); len(got) != 0 {
		t.Fatalf("an empty list is a lock, not an open door: %v", got)
	}

	env.Spec.Serves.Consumers = []EnvironmentType{EnvironmentPreview, EnvironmentProduction}
	if !env.Admits(EnvironmentPreview) || !env.Admits(EnvironmentProduction) {
		t.Fatalf("what was declared is admitted: %v", env.Spec.Serves.Consumers)
	}
	if env.Admits(EnvironmentStage) {
		t.Fatal("and what was not declared is not")
	}
	got := env.ServedConsumers()
	if len(got) != 2 || got[0] != EnvironmentProduction || got[1] != EnvironmentPreview {
		t.Fatalf("want production then preview whatever order it was written in, got %v", got)
	}
}
