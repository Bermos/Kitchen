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

// The kinds transitions are recorded under.
//
// They are constants, and shared between the reconcilers and the REST API,
// for one reason that matters more than tidiness: the log is queried by kind,
// and a record written as "build" where every other says "Build" is a record
// the query for that object will never find. A typo in a log line is
// cosmetic; a typo here loses evidence.
//
// They are the Kubernetes kinds rather than a vocabulary of Kitchen's own, so
// that a record can be taken back to the object it is about without a
// translation table.
const (
	KindAccessReview   = "AccessReview"
	KindBuild          = "Build"
	KindConnection     = "Connection"
	KindDomain         = "Domain"
	KindEnvironment    = "Environment"
	KindException      = "Exception"
	KindKitchen        = "Kitchen"
	KindPlatformUpdate = "PlatformUpdate"
	KindProject        = "Project"
	KindPromotion      = "Promotion"
	KindRelease        = "Release"
	KindResourceClaim  = "ResourceClaim"

	// KindPromotionDecision is the one kind here that is not a Kubernetes
	// kind: a policy decision lives in the decision store, not in the
	// cluster, and the record's name carries the object the decision was
	// about while its correlation carries the decision id — which is the
	// translation table back to the stored row.
	KindPromotionDecision = "PromotionDecision"

	// KindRetention is the second such kind, and it is here for the reason
	// the log has kinds at all: "show me the evidence that data was deleted
	// when it expired" has to be one query. The sweep's records are about
	// the platform singleton, so recording them as KindKitchen would be
	// defensible and would bury them among every settings change ever made.
	// They are their own kind instead, and the record's name is the
	// singleton's.
	KindRetention = "Retention"
)

// The `change` key a record's details carry, which is what makes one kind of
// edit findable among the rest. They are strings in the details rather than
// columns because the log's columns are the same for every record; a reader
// filters on the kind first and on this second.
const (
	// ChangeRetentionSweep marks the deletion evidence one retention sweep
	// produced: every class, the horizon each was measured against, and what
	// the pass removed.
	ChangeRetentionSweep = "retention-sweep"

	// ChangeAuditFloorOverride marks a write that put the audit retention
	// below its documented floor, with the reason and the approver. It is
	// its own value because "who decided we keep less evidence than the
	// platform recommends, and why" is a question that must be answerable
	// from the log alone.
	ChangeAuditFloorOverride = "audit-floor-override"
)
