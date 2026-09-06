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

// Package signals is the operator's catalogue of derived rules: the things
// worth telling somebody about, computed from what the platform already
// collects.
//
// It exists because the interesting failures do not announce themselves in any
// one place. A crash-looping container is in pod status; the OOM kill that
// caused it is in the metric rollup; the reason the pods were never created at
// all is a Warning event on a workload that shows no pods to look at. Reading
// those three sources by hand is what the platform is supposed to replace, so
// the reading is written down once, here, and every screen asks the same
// question of it.
//
// # The shape, and why it is this shape
//
// A [Signal] is a named, versioned rule: `Evaluate(*Snapshot) []Finding`. It is
// pure — it performs no I/O, keeps no state between calls, and reads nothing
// but the snapshot it is handed. Every read the rules need happens once, in
// [Gather], before any rule runs. That split is the whole design:
//
//   - A rule is a unit test with a struct literal. No cluster, no store, no
//     clock — the snapshot carries its own [Snapshot.Now].
//   - Thirty-odd rules over one snapshot cost one round of reads, not thirty-odd.
//   - Evaluating on request (when a screen asks) and evaluating on a timer
//     (what internal/detection does) are the same call. The loop was additive
//     when it landed, exactly as this said it would be: nothing here moved
//     for it.
//
// A [Finding] is one firing condition, and its [Finding.Fingerprint] is stable
// for the same underlying condition across evaluations — `workload.crashloop/
// shop/pr-41/web` names that container's crash loop today, tomorrow, and after
// an operator restart. That stability is what makes a round of findings
// diffable against the previous one, which is what turns "the screen shows a
// problem" into "the inbox recorded that the problem opened at 03:14".
//
// # Transitions
//
// [Tracker] is the diff, and it is as pure as the rules are: the previous
// round in, the next one in, and what changed out. A transition is keyed on
// (fingerprint, audience) rather than on the fingerprint alone, because a
// condition reaches up to two audiences and those are two deliveries — see
// [TransitionKey]. What runs the tracker on a timer and writes the result to
// `signal_transitions` is internal/detection; what reads the result back is
// that loop, on a restart, and the API, to answer a screen without
// re-evaluating.
//
// # Honest degradation
//
// A rule whose input could not be read does not report "no problem". It reports
// that it could not be evaluated, as a [SeverityUnknown] finding naming the
// input and the reason — see [Snapshot.MarkUnreadable]. An input that does not
// apply to this installation (DNS probing behind cloudflared, say) is marked
// [Snapshot.MarkNotApplicable] instead and produces nothing at all, because a
// permanent "cannot evaluate" row on the problems list is noise rather than
// news.
//
// # Thresholds
//
// Every number a rule compares against lives in thresholds.go, each with a
// comment saying why that number. They are constants, not configuration:
// configurable thresholds are an alerting-era feature, and the catalogue is
// versioned code either way.
package signals
