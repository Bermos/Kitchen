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

// Package contract is the vocabulary every claim contract shares: what a
// provider declares about itself before it has provisioned anything.
//
// A preview environment gets a copy of production's database from Neon and
// an empty one from CloudNativePG. Both are correct, and the difference
// between them is the difference between a preview that can read production
// data and one that cannot — which is not something to leave inside each
// provisioner where nobody choosing a dependency can see it. So a provider
// declares, in code and next to its implementation, what a preview binds to
// and what its binding does to the workload that reads it; the platform
// records the declaration on the claim, shows it where the dependency is
// chosen, and generates the matrix in docs/api/claims.md from it.
//
// It is deliberately not a policy engine. It records what a provider can do;
// whether a shared preview is acceptable is the operator's call, and
// DataClass plus the existing policy machinery is where that call lives.
package contract

// PreviewMode is what a claim binds to in a preview environment.
type PreviewMode string

const (
	// PreviewBranch is a cheap copy of production's data under its own
	// address — a copy-on-write branch. Production-derived, and declared so.
	PreviewBranch PreviewMode = "branch"
	// PreviewFresh is a new, empty resource of the same shape, created with
	// the preview and torn down with it. Synthetic by construction.
	PreviewFresh PreviewMode = "fresh"
	// PreviewShared is the production resource itself. It is what everyone
	// does informally and it is how a preview writes to production, which is
	// why a claim has to ask for it by name: it is never a default and never
	// inferred.
	PreviewShared PreviewMode = "shared"
	// PreviewNone binds nothing in a preview, and the claim says why. For
	// anything unsolved: an imperfect contract ships, as long as the
	// imperfection is legible.
	PreviewNone PreviewMode = "none"
)

// Isolated reports whether the mode gives a preview a resource of its own —
// the two modes under which the claim reconciler creates and tears down one
// per preview environment.
func (m PreviewMode) Isolated() bool {
	return m == PreviewBranch || m == PreviewFresh
}

// Known reports whether the value is one of the four modes.
func (m PreviewMode) Known() bool {
	switch m {
	case PreviewBranch, PreviewFresh, PreviewShared, PreviewNone:
		return true
	}
	return false
}

// PreviewModes is every mode, in the order the docs list them.
var PreviewModes = []PreviewMode{PreviewBranch, PreviewFresh, PreviewShared, PreviewNone}

// Declaration is what one provider says about the claims it fulfils, before
// any of them exists. Every field is a fact about the provider, not about a
// claim: it is the same for every claim the provider serves, which is what
// makes it a table rather than a status.
type Declaration struct {
	// Preview is what a preview environment gets when the claim asks for a
	// resource of its own. A provider with no tenancy story declares shared;
	// one that has not solved previews at all declares none.
	Preview PreviewMode

	// PreviewNote is the sentence behind Preview — why the provider gives
	// previews what it gives them — for the docs matrix and the screen where
	// a dependency is chosen.
	PreviewNote string

	// KeepsPodsRunning says the binding holds the workload up: a worker
	// holding an outbound connection to the provider never idles, so no
	// environment reading this claim scales to zero. Stated here because it
	// is otherwise invisible until the bill.
	KeepsPodsRunning bool

	// ForcesRecreate says the provisioned resource can be attached to one
	// pod at a time, so the workload that reads it is deployed by stopping
	// the old pod before starting the new one — a gap in serving on every
	// deploy. Stated here because it is otherwise invisible until the
	// rollout hangs.
	ForcesRecreate bool

	// CanIdle says the provisioner can park a preview's own resource when
	// the preview parks, and bring it back on wake — the second half of
	// #294, and the half where the c × e multiplication actually costs.
	//
	// A provider declares it false when it has nothing to park (a bucket, a
	// logical database at somebody else's server, an OAuth client) or when
	// it parks itself without being asked. Either way the claim says which
	// through IdleNote, because "this preview's database is still running"
	// is otherwise invisible until the node notices.
	CanIdle bool

	// IdleNote is the sentence behind CanIdle — what idling a preview does
	// to what this provider gave it. It is required either way: a provider
	// that idles has to say what survives, and one that does not has to say
	// why. The test in internal/provider/declarations enforces that.
	IdleNote string

	// WorkloadNote is the sentence behind either of the two flags above,
	// empty when neither is set.
	WorkloadNote string
}
