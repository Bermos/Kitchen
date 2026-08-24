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

package clickhouse

import (
	"context"
	"fmt"
	"strings"
)

// Storage-level immutability for the audit table, and the exact size of the
// claim.
//
// The hash chain already catches an editor who has the store but not the
// chain: a mutated row no longer hashes to the hash beside it, a deleted row
// orphans its successor, an inserted row has nowhere to link. What it does not
// catch is somebody who can rewrite the whole tail, because recomputing every
// hash from the edit onwards is as cheap for them as it was for the platform.
//
// What this adds is one specific thing, and it is worth having precisely
// because it is specific: **the platform's own credential cannot rewrite the
// log.** Kitchen's operator and its REST API both hold the telemetry
// connection secret; a bug, a compromised operator pod or an authenticated
// caller who found a way to reach Exec would otherwise be able to `ALTER TABLE
// audit_log DELETE` and then rebuild the chain, and there is no cryptography
// in the design that would notice. Revoking the mutation privileges from that
// user is the difference between "the log is as trustworthy as the platform"
// and "the log is as trustworthy as whoever administers ClickHouse".
//
// What it does **not** stop, said out loud because a defence nobody can state
// the limits of is a defence nobody should rely on:
//
//   - a ClickHouse administrator, who can grant the privileges back;
//   - anybody with the store's filesystem, since a MergeTree part is a
//     directory;
//   - a restore of the whole database from a doctored backup;
//   - and it is not retroactive — it revokes a privilege, it does not seal
//     the rows that are already there.
//
// Bounding *those* is what an external anchor is for (§4.3), and this is
// deliberately not that.
//
// It is best effort by construction. ClickHouse's RBAC will refuse a partial
// revoke against a user granted everything at a wider scope unless
// `partial_revokes` is enabled for them, and an installation pointing Kitchen
// at a store it does not administer may not be allowed to revoke anything at
// all. A failure here is reported on the singleton's compliance status rather
// than failing the reconcile, because the honest consequence is "the log's
// immutability rests on the chain alone", which is exactly what the status
// should say and not a reason to stop the platform.

// auditImmutablePrivileges are what the platform's own user must not hold over
// the audit table.
//
// ALTER TTL is deliberately absent: EnsureAuditSchema keeps the table's
// retention in step with the configured one and cannot do that without it. It
// is also the one mutation that is *bounded by the floor* — a retention below
// AuditFloorDays is refused at admission and at the API, so the privilege that
// is kept is the one whose blast radius the schema already constrains.
var auditImmutablePrivileges = []string{
	"ALTER UPDATE",
	"ALTER DELETE",
	"TRUNCATE",
	"DROP TABLE",
}

// AuditImmutability is what the store reports about the attempt.
type AuditImmutability struct {
	// Revoked is true when the privileges came off, which is the whole of
	// the claim: the platform's own credential can append to the audit table
	// and cannot rewrite it.
	Revoked bool

	// Privileges names what was revoked, so a status carries the claim's
	// exact shape rather than a boolean somebody has to go and look up.
	Privileges []string

	// Message explains an attempt that did not take.
	Message string
}

// EnsureAuditImmutability revokes the audit table's mutation privileges from
// the user this client connects as.
//
// It never returns an error. Everything it can go wrong at is somebody else's
// ClickHouse being administered by somebody else, and the caller's job is to
// publish the outcome rather than to fail over it.
func (c *Client) EnsureAuditImmutability(ctx context.Context) AuditImmutability {
	if !identifierPattern.MatchString(c.cfg.Username) {
		// The user name reaches the statement as an identifier, so a name
		// this package cannot spell safely is one it does not spell at all.
		return AuditImmutability{Message: fmt.Sprintf(
			"the telemetry store's user name %q cannot be named in a REVOKE, so the audit table's "+
				"mutation privileges were left as they are; the log's immutability rests on the hash "+
				"chain alone", c.cfg.Username)}
	}

	statement := fmt.Sprintf("REVOKE %s ON %s.%s FROM %s",
		strings.Join(auditImmutablePrivileges, ", "),
		quoteIdentifier(c.cfg.Database), quoteIdentifier(AuditTable),
		quoteIdentifier(c.cfg.Username))
	if err := c.Exec(ctx, statement); err != nil {
		return AuditImmutability{Message: "the audit table's mutation privileges could not be revoked from " +
			c.cfg.Username + ", so the log's immutability rests on the hash chain alone: " + err.Error()}
	}
	return AuditImmutability{Revoked: true, Privileges: append([]string(nil), auditImmutablePrivileges...)}
}
