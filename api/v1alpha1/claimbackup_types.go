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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The backup policy of one claim: continuous WAL archiving to an object store
// plus a scheduled base backup, for a database this platform runs itself.
//
// It is the second half of the platform's backup story and a different half.
// The platform archive (BackupSpec on the Kitchen singleton) carries
// configuration, credentials and accounts, and deliberately carries no
// application data; this is the application data, per claim, kept by the
// database's own machinery rather than by a tar of the cluster.
//
// Three rules shape every field below:
//
//   - **The operator sets the default and the developer overrides it.** The
//     policy is defaulted from the claim's Connection — one decision for every
//     database an installation provisions — and a claim may narrow or widen
//     it. That is the split cnpgConfig already has for images and storage.
//   - **Backups outlive the claim.** Under either deletion policy: `Delete`
//     destroys the database, and if it also destroyed the backups then
//     "Delete" would quietly destroy the recovery point, which is the one
//     thing deletion protection exists to prevent. What is at the destination
//     is pruned by RetentionPolicy and by nothing else.
//   - **What the provider keeps is the provider's.** A claim through a
//     provider that backs itself up — Neon — takes no policy at all and
//     reports so; the platform does not pretend to manage what it cannot.

// ClaimBackupSpec is the backup policy asked of one claim's resource.
//
// It is written against the platform's own destination by default, under a
// prefix of the claim's own, so that an installation that has configured
// `spec.backup.destination` once has said where its databases go too.
type ClaimBackupSpec struct {
	// Enabled turns continuous archiving and the scheduled base backup on or
	// off for this claim. Unset inherits: the Connection's default, and
	// failing that the platform's — which is on wherever a destination
	// resolves, and off with the reason on the status where none does.
	//
	// A preview's database is never backed up whatever this says. It is a
	// fresh, empty database declaring `dataProvenance: synthetic`, and
	// archiving one is pure cost.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// Schedule for the base backup. It is **CloudNativePG's** cron and not
	// Kubernetes': six fields, seconds first, which is a real difference and
	// not a footnote — "0 0 3 * * *" is three in the morning, and the
	// five-field spelling of that is refused by the operator rather than
	// silently read as something else. UTC, like every schedule here.
	//
	// Empty takes the Connection's, and failing that the platform's default
	// of a nightly base backup in a quiet hour.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// RetentionPolicy is how long the destination keeps this database's
	// backups and the WAL between them, in barman's own vocabulary: "30d",
	// "4w", "6m". It is the only thing that ever deletes them — see the note
	// on this type about backups outliving the claim.
	//
	// Empty keeps everything, which is the safe default for the same reason
	// the platform archive's retention is: an archive costs pennies and the
	// failure this exists to prevent is having too few of them.
	// +kubebuilder:validation:Pattern=`^[1-9][0-9]*[dwm]$`
	// +optional
	RetentionPolicy string `json:"retentionPolicy,omitempty"`

	// Destination is where this claim's backups are written. Empty takes the
	// Connection's, and failing that the platform's own
	// (`spec.backup.destination`) — under a prefix of this database's own, so
	// two claims never write over each other and neither writes over the
	// platform archive.
	//
	// The credential half is a Secret in the platform namespace, written by
	// the API and never read back, exactly as the platform destination's is.
	// +optional
	Destination *BackupDestination `json:"destination,omitempty"`
}

// ClaimBackupArchiving is the health of continuous WAL archiving, as the
// database's own operator reports it. It is a three-state on purpose:
// "unknown" is not "healthy", and a claim that has been configured for an
// hour and reports nothing is a claim somebody should look at.
// +kubebuilder:validation:Enum=healthy;failing;unknown
type ClaimBackupArchiving string

const (
	// ClaimArchivingHealthy: WAL is reaching the destination.
	ClaimArchivingHealthy ClaimBackupArchiving = "healthy"
	// ClaimArchivingFailing: it is not, and Message says what the database
	// said about it. This is the state that loses data quietly — a base
	// backup with no WAL after it recovers to the base backup and no further.
	ClaimArchivingFailing ClaimBackupArchiving = "failing"
	// ClaimArchivingUnknown: the database has not reported either way yet.
	ClaimArchivingUnknown ClaimBackupArchiving = "unknown"
)

// ClaimBackupStatus is what is actually happening to this claim's backups,
// read from the provider on every reconcile and never declared.
//
// FirstRecoverablePoint is the field this whole phase exists for. "Backups
// are configured" is worth nothing; "we can restore to 03:14 last Tuesday" is
// worth everything, and only the second is a fact about the destination
// rather than about the spec.
type ClaimBackupStatus struct {
	// Enabled is whether the platform is configuring backups for this claim,
	// after the claim, its Connection and the platform have all been read.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// ProviderManaged is true where the provider keeps its own backups and
	// the platform configures nothing — a Neon claim, whose point-in-time
	// history is inherent to its storage. Reason carries the provider's own
	// sentence about it.
	//
	// It is the honest third state between "backed up by us" and "not backed
	// up": such a claim is protected, by somebody else, and a screen that
	// showed it as unprotected would be wrong.
	// +optional
	ProviderManaged bool `json:"providerManaged,omitempty"`

	// Reason says why the state above is what it is, in the words that name
	// the fix: which destination is missing, which route configures one, or
	// why this claim's database is not one the platform writes to.
	// +optional
	Reason string `json:"reason,omitempty"`

	// Schedule, RetentionPolicy and Destination are the policy in force after
	// the claim, the Connection and the platform have been resolved — echoed
	// so a reader does not have to compute the inheritance themselves.
	// Destination is described and never its credential: "s3://bucket/prefix".
	// +optional
	Schedule string `json:"schedule,omitempty"`
	// +optional
	RetentionPolicy string `json:"retentionPolicy,omitempty"`
	// +optional
	Destination string `json:"destination,omitempty"`

	// LastBackup is when a base backup last succeeded, and LastFailure when
	// one last failed — both as the database's own operator reports them.
	// +optional
	LastBackup *metav1.Time `json:"lastBackup,omitempty"`
	// +optional
	LastFailure *metav1.Time `json:"lastFailure,omitempty"`

	// FirstRecoverablePoint is the oldest moment this database can still be
	// reconstructed to from what is at the destination. It moves forward as
	// retention prunes and it is empty until the first base backup has been
	// taken and read back by the database's own operator.
	//
	// It is also the seam point-in-time recovery for this provider (#247)
	// binds to: this timestamp is the earliest edge of the window a recovery
	// picker would offer.
	// +optional
	FirstRecoverablePoint *metav1.Time `json:"firstRecoverablePoint,omitempty"`

	// Archiving is whether WAL is reaching the destination between base
	// backups, and ArchivingMessage the database's own account of it. A base
	// backup whose WAL is not archiving recovers to the base backup and no
	// further, which is the failure worth surfacing rather than reporting a
	// green schedule.
	// +optional
	Archiving ClaimBackupArchiving `json:"archiving,omitempty"`
	// +optional
	ArchivingMessage string `json:"archivingMessage,omitempty"`
}
