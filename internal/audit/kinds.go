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
	KindAddon          = "Addon"
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

	// KindProjectSecret is the third such kind: a credential a project gave
	// its own application — a database it runs itself, a third-party API key
	// — being set, replaced or deleted. The record carries the secret's name
	// and never its value.
	//
	// The Kubernetes object behind it is a Secret, and recording it as one
	// would name an object the developer who wrote it has never seen; folded
	// into KindProject it would be buried among every settings change the
	// project ever had. "Every credential this application was given, and
	// when each was last rotated" has to be one query, so it is one kind.
	KindProjectSecret = "ProjectSecret"

	// KindEvidenceExport is the fourth, and it records a *read*: somebody
	// took an audit pack of a project (#142). The pack is the platform's
	// whole compliance answer for one project over one range, and "who
	// exported the evidence, for which window, and what digest did they get"
	// is exactly the sentence this log exists to be able to produce — the
	// same argument that makes a platform backup an `export` record rather
	// than nothing at all.
	//
	// It is its own kind rather than a KindProject record with the export
	// operation, because the log filters on kind and has no filter on
	// operation: one query has to answer "every pack ever taken", and buried
	// among a project's settings changes it would not.
	KindEvidenceExport = "EvidenceExport"

	// KindProjectFile is the fifth, and it is the content of a project's
	// *secret* configuration file being written or replaced (#311) — the
	// file the application is configured by, held where no response reads it
	// back.
	//
	// It is not KindProjectSecret, though both are credentials and both are
	// classified as credential writes so that one privileged view shows
	// both. A secret and a config file are two things a project declares,
	// with two routes, two screens and two lists, and a log that answered
	// "every secret this project holds" with a file among them would be
	// answering a question nobody asked. Declaring the file — its path, the
	// workloads that read it — is a settings change and is recorded as one,
	// on the project; this kind is the content alone.
	KindProjectFile = "ProjectFile"

	// KindNotificationSubscription is where the platform was told to send an
	// account of itself (#77), and it is a credential write: the signing key
	// goes in with the subscription and is rotated through the same route.
	//
	// It is its own kind rather than a KindConnection record, though both
	// hold a credential the platform never reads back, because a connection
	// is something the platform reads *from* and a subscription is somewhere
	// it writes *to*. "Every address this platform sends its activity to, and
	// who added each" is one query, and it is the one an auditor asks.
	KindNotificationSubscription = "NotificationSubscription"

	// KindPersistentVolume is the seventh, and the one Kubernetes kind here
	// the platform writes without a CRD of its own in front of it (#406): a
	// volume an operator wrote for storage that existed before the cluster
	// did, so that a claim can bind it without anybody reaching for kubectl.
	//
	// It is recorded for the reason the connection kinds are. The object is
	// cluster-scoped and points at somebody's data, and "who told the
	// platform this NAS export exists, and who removed the record" is a
	// question the log has to be able to answer on its own — the reclaim
	// policy means no data is ever destroyed either way, which is exactly
	// why the record is the only trace left.
	KindPersistentVolume = "PersistentVolume"

	// KindAuditAnchor is the eighth, and it is the log recording something
	// about itself: the object the chain's end is claimed through being
	// established, and in particular being established by adopting the
	// table's own last record (#428).
	//
	// It is its own kind because it is the one record whose absence is the
	// finding. "Has this chain's numbering ever been taken from the table it
	// is supposed to bound, and when" has to be one query and has to be
	// answerable years later; folded into KindKitchen it would sit among
	// every settings change the platform ever had, which is where nobody
	// looks.
	KindAuditAnchor = "AuditAnchor"

	// KindSignalMitigation is the ninth, and it records what somebody did
	// about a condition the signal catalogue found: acknowledged it,
	// silenced it with a reason and an expiry, or claimed the escalated
	// ticket (#471).
	//
	// It is its own kind rather than a record on the project, for the reason
	// the retention sweep's is its own: "who said they had seen this outage,
	// who decided it should stop being said, and who took it" has to be one
	// query. Folded into KindProject it would sit among every settings
	// change the project ever had, which is where nobody looks — and half of
	// these records are about a platform condition that names no project at
	// all.
	//
	// The record names the delivery rather than the condition, in its
	// correlation: one fingerprint is two rows, delivered to two audiences,
	// and acknowledging one is explicitly not acknowledging the other.
	KindSignalMitigation = "SignalMitigation"

	// KindSignalPolicy is a change to the thresholds the signal catalogue is
	// read against: what counts as a correlation, how long a failure may run
	// unmitigated, how long a silence may last, and whether this
	// installation pages at all (#472).
	//
	// Its own kind for the reason KindRetention is, and with more force. The
	// compliance posture reads *the floor* rather than any project's
	// override, so "what was the floor when this incident was left untended
	// for six hours" is a question an auditor asks and the answer has to be
	// one query. A finding also records the values it was evaluated against,
	// so the two halves meet: the finding says what the numbers were, and
	// this says who changed them to that.
	KindSignalPolicy = "SignalPolicy"
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

	// ChangeAuditAnchorAdopted marks the chain's anchor being seeded from
	// the log's own last record rather than starting with the chain. The
	// record names the sequence it was taken from, which is the line
	// everything before is bounded by the hash chain alone.
	ChangeAuditAnchorAdopted = "audit-anchor-adopted"
)
