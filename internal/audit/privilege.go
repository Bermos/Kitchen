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
	"encoding/json"
)

// Privileged transitions: the ones that change what the platform will allow,
// rather than what it is running.
//
// Deploying a release, editing an environment variable and rolling back are
// ordinary. Waiving a rule, changing what an environment demands, rotating a
// credential, moving a data class and changing who holds which role are not:
// each of them moves the bar the rest of the log is judged against, and an
// auditor's first question is always about those and never about the four
// hundred deploys between them. So they are separable — one filter, not a
// reader's eye over a page of prose.
//
// # Why this is a detail and not a column
//
// The obvious shape is a `privileged` column on audit_log, and it is the
// wrong one for exactly one reason: ChainHash covers every field of a record,
// so a new field changes the hash of every record ever written and the whole
// log stops verifying. Details is the extension point the chain was designed
// with — it is hashed verbatim as one length-prefixed string, so a record
// that carries a classification is *covered* by the chain rather than
// annotated beside it, and one that predates the classification still
// verifies. A privileged marking cannot be added or removed after the fact
// without the chain saying so, which is the property that makes the marking
// worth anything.
//
// # One mechanism
//
// Records carried `details.privileged = true` before this file existed, set
// by hand at each site. This is that convention, made into a type: callers
// set Transition.Privileged and the recorder writes both keys, so there is
// one vocabulary, one spelling, and one place a new class is added.

// Privilege names what class of privileged act a transition is. The empty
// value is the ordinary case and the overwhelmingly common one — a record
// carries a class only when it is one of the six below.
//
// The list is short deliberately, and it is a classification rather than a
// description: the record's reason says what happened, this says which
// control it moved. A seventh value is a claim that a new kind of authority
// exists on this platform, which is a thing to notice rather than to add
// quietly.
type Privilege string

const (
	// PrivilegeBreakGlass is a rule waived: an exception granted, relied on,
	// resolved, or expiring unresolved. The class an incident is
	// reconstructed from.
	PrivilegeBreakGlass Privilege = "break-glass"

	// PrivilegeRequirements is a change to what an environment demands —
	// its bundle, its parameters, its owners. Whoever moves this decides
	// what every future promotion is measured against.
	PrivilegeRequirements Privilege = "requirements"

	// PrivilegeClassification is a data class or a residency moving. It is
	// privileged because the class is an input to policy: lowering one makes
	// promotions possible that were refused an hour earlier.
	PrivilegeClassification Privilege = "classification"

	// PrivilegeAccess is who may do what: a project's grants, the platform's
	// operator list, and a recertification cycle's decisions.
	PrivilegeAccess Privilege = "access"

	// PrivilegeCredential is a credential the platform holds being written
	// or replaced. The API never reads one back, so the record is the only
	// evidence a rotation happened at all.
	PrivilegeCredential Privilege = "credential"

	// PrivilegeIntegrity is a write to a Kitchen-managed object that no
	// reconcile made — the detection in docs/COMPLIANCE.md §11.4. It is
	// privileged in the strongest sense: the actor held more authority than
	// this platform grants anybody, because they went round it.
	PrivilegeIntegrity Privilege = "integrity"
)

// The detail keys a classification is written under.
//
// PrivilegedDetail is a boolean and predates the class: it is what every
// reader and every stored record already keys on, so it stays, and a filter
// for "everything privileged" is one predicate over the whole history rather
// than a list of class names that grows.
const (
	PrivilegedDetail     = "privileged"
	PrivilegeClassDetail = "privilegedClass"
)

// Valid reports whether p is one of the classes above. It exists so that a
// class arriving from outside the process — a query parameter — is checked
// against the vocabulary rather than passed through to a store query that
// would simply answer nothing.
func (p Privilege) Valid() bool {
	switch p {
	case PrivilegeBreakGlass, PrivilegeRequirements, PrivilegeClassification,
		PrivilegeAccess, PrivilegeCredential, PrivilegeIntegrity:
		return true
	default:
		return false
	}
}

// Privileges is every class, in the order a reader meets them. The API
// publishes it so a client does not have to hard-code the vocabulary.
func Privileges() []Privilege {
	return []Privilege{
		PrivilegeBreakGlass, PrivilegeRequirements, PrivilegeClassification,
		PrivilegeAccess, PrivilegeCredential, PrivilegeIntegrity,
	}
}

// PrivilegeOf reads a record's classification back out of its details.
//
// It answers (class, privileged). A record marked privileged with no class is
// still privileged — records written before the vocabulary existed are
// exactly that — so the boolean is the one to branch on and the class is the
// refinement.
func PrivilegeOf(details string) (Privilege, bool) {
	if details == "" {
		return "", false
	}
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(details), &decoded); err != nil {
		// Details is opaque to this package and hashed verbatim; something
		// that will not decode is not a reason to claim anything about the
		// record, in either direction.
		return "", false
	}
	privileged, _ := decoded[PrivilegedDetail].(bool)
	if !privileged {
		return "", false
	}
	class, _ := decoded[PrivilegeClassDetail].(string)
	return Privilege(class), true
}
