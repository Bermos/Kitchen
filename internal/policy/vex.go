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

package policy

import (
	"sort"
	"time"

	"github.com/Bermos/Kitchen/internal/vex"
)

// Materializing the artifact's OpenVEX statements (issue #135).
//
// A VEX document is evidence like any other: a DSSE envelope attached to the
// artifact's digest, read back out of the registry with everything else. So
// the statements a rule matches on are a *projection of the evidence already
// materialized* rather than a second thing to go and fetch — which is why
// this is called from MaterializeInput and not, as the exception listing is,
// from each caller.
//
// That difference is the whole of the design decision. `input.Exceptions` is
// a listing: it needs the cluster, a context and an error path, and every
// evaluation path has to ask for it. `input.VEX` needs the `evidence`
// argument MaterializeInput already holds and the clock it already carries,
// so putting it anywhere else would be creating exactly the second
// materialization site issue #144 exists to prevent — the promotion and the
// eligibility preview would then differ by whichever one somebody remembered
// to update.
//
// Nothing here decides anything. A statement is flattened, not judged: the
// justification is carried whether or not it is one of OpenVEX's five, the
// author is carried whether or not anyone trusts them, and whether the
// signature was accepted travels as `verified`. The default bundle's
// `vex_not_affected` is what turns those facts into a suppression, and it is
// the environment's parameters that tune it.

// VEXFrom flattens every OpenVEX document in an evidence set into the
// statements the rules match on, as of `at`.
//
// Expiry is applied here and nowhere else, for the same reason and in the
// same shape as `ActiveExceptionsFor`: an expired statement is simply not in
// the listing the evaluation materializes from, the rules it was suppressing
// fire unsuppressed, and the continuous re-evaluation pass (§9) is what makes
// that visible on something already running. There is no expiry engine, and
// judging it against `at` rather than against the reader's own clock is what
// makes a replayed decision suppress exactly what the original suppressed.
//
// The result is sorted, which the evidence set is not: the registry returns
// attestations in whatever order it lists them, and an input digest that
// depended on that order would make two evaluations of the same facts two
// different decisions.
func VEXFrom(evidence []Evidence, at time.Time) []VEXStatement {
	statements := []VEXStatement{}
	for _, entry := range evidence {
		if !vex.IsOpenVEX(entry.PredicateType) || len(entry.Predicate) == 0 {
			continue
		}
		document, err := vex.Parse(entry.Predicate)
		if err != nil {
			// A document the platform cannot read asserts nothing it can act
			// on. It stays attached to the artifact and readable — evidence
			// is never deleted for being unreadable — and it suppresses
			// nothing, which is the safe direction.
			continue
		}
		for _, statement := range document.Statements {
			identifier := statement.Vulnerability.String()
			if identifier == "" || statement.Status == "" {
				continue
			}
			if expiry := vex.Expiry(document, statement); !expiry.IsZero() && !at.Before(expiry) {
				continue
			}
			statements = append(statements, VEXStatement{
				Vulnerability: identifier,
				Products:      vex.ProductIdentifiers(statement),
				Status:        statement.Status,
				Justification: statement.Justification,
				Author:        vex.AuthorOf(document, statement),
				Timestamp:     vex.TimestampOf(document, statement),
				Verified:      entry.Verified,
			})
		}
	}
	sort.SliceStable(statements, func(i, j int) bool {
		if statements[i].Vulnerability != statements[j].Vulnerability {
			return statements[i].Vulnerability < statements[j].Vulnerability
		}
		if statements[i].Author != statements[j].Author {
			return statements[i].Author < statements[j].Author
		}
		if statements[i].Status != statements[j].Status {
			return statements[i].Status < statements[j].Status
		}
		return statements[i].Timestamp < statements[j].Timestamp
	})
	return statements
}
