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

package database

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Backups, in the two shapes a database provider can have them (#245 phase 2).
//
// The asymmetry is the same one recovery has and it is worth naming rather
// than smoothing over. A database this platform runs is one the platform can
// point at an object store and hand a schedule; a database somebody else runs
// is backed up by whoever runs it, and the honest answer for such a claim is
// "backed up by the provider" rather than either a policy nothing acts on or
// a screen saying "not protected".
//
// So there are two optional interfaces here rather than one method every
// provisioner has to fake — the shape CapableProvisioner and
// RecoverableProvisioner already set:
//
//   - BackupProvisioner is a provider whose backups the platform configures.
//     It takes a policy and reports what the destination actually holds.
//   - SelfBackingProvisioner is a provider that backs itself up. It takes no
//     policy and answers one sentence about what it keeps.
//
// A provisioner implementing neither has no backup story, and the claim says
// exactly that rather than implying one.

// ErrBackupNotManaged marks a resource the platform did not create and will
// therefore not write backup configuration onto. It is a refusal rather than
// a failure: an installation that handed the platform a database it already
// ran keeps whatever backup arrangement that database already had, and the
// claim reports which of the two it is.
var ErrBackupNotManaged = errors.New("backups are not configured for a resource this platform did not create")

// ErrBackupUnsupported marks a provider whose running version no longer
// accepts the mechanism this platform configures backups through. It exists
// because the failure it catches is otherwise **silent**: a Kubernetes API
// server prunes fields a CRD no longer declares without saying anything, so a
// configuration that has stopped being applied looks exactly like one that
// was applied.
var ErrBackupUnsupported = errors.New("this provider version does not accept the backup configuration the platform writes")

// BackupDestination is where one claim's backups go, in the vocabulary a
// provisioner needs rather than the CRD's: a bucket, a prefix, and a
// credential that has already been resolved to a Secret the provisioner's own
// objects can reference.
//
// It is deliberately not kitchenv1alpha1.BackupDestination. That type carries
// a Secret reference into the *platform* namespace, and a database provisioner
// writes objects in the namespace its databases live in — the credential has
// to have been put where the database can read it before this is built, which
// is the caller's job and is why the field here is a plain name.
type BackupDestination struct {
	// Bucket and Prefix are the object store address backups are written
	// under. The provisioner adds a component of its own below the prefix so
	// that two claims never write over each other.
	Bucket string
	Prefix string

	// Region and Endpoint are what makes a store that is not AWS reachable.
	Region   string
	Endpoint string

	// ServerSideEncryption asks the store to encrypt at rest: "AES256" or
	// "aws:kms". Empty leaves it to the bucket's own policy.
	ServerSideEncryption string

	// CredentialsSecret names a Secret **in the namespace the provisioner's
	// resources live in** holding the keys below. Empty means the ambient
	// credential chain — an instance role, IRSA — which is the better answer
	// where it is available because there is then no long-lived key to leak.
	CredentialsSecret string
	// AccessKeyIDKey, SecretAccessKeyKey and RegionKey are the keys inside
	// that Secret. RegionKey is answered only where the region was stored,
	// because the provider this ships for takes its region by secret
	// reference and not as a plain string.
	AccessKeyIDKey     string
	SecretAccessKeyKey string
	RegionKey          string

	// EndpointCASecret and EndpointCAKey name the PEM certificate of the
	// authority that signed the store's own, in the same Secret and the same
	// namespace as the credential above. They are answered only for a store
	// whose certificate no public root vouches for — the object store this
	// platform bundles, served on a `.svc` name from the platform's internal
	// CA (#382) — because barman runs in the database's namespace, where the
	// platform's CA is not published unless something puts it there.
	EndpointCASecret string
	EndpointCAKey    string
}

// Configured reports whether there is anywhere to write to at all. A policy
// with no bucket is not a half-configuration to report later: there is
// deliberately no local destination for a claim any more than for the
// platform archive, so this is the whole question.
func (d BackupDestination) Configured() bool { return strings.TrimSpace(d.Bucket) != "" }

// Path is the destination as an object store address, and never a credential.
// It is what the provisioner writes into its own objects and what the claim's
// status reports.
func (d BackupDestination) Path() string {
	if !d.Configured() {
		return ""
	}
	path := "s3://" + strings.Trim(strings.TrimSpace(d.Bucket), "/")
	if prefix := strings.Trim(strings.TrimSpace(d.Prefix), "/"); prefix != "" {
		path += "/" + prefix
	}
	return path
}

// BackupPolicy is what the platform asks a provisioner to keep: whether to
// back this resource up at all, how often a base backup is taken, how long
// what is at the destination survives, and where the destination is.
type BackupPolicy struct {
	// Enabled false means the platform stops configuring backups for this
	// resource — it does **not** mean anything at the destination is
	// deleted. Backups outlive the claim under either deletion policy, and
	// they outlive being switched off for the same reason.
	Enabled bool

	// Schedule for the base backup, in the provider's own cron dialect. The
	// provisioner refuses one it cannot read rather than passing it through
	// to be misinterpreted.
	Schedule string

	// RetentionPolicy is how long the destination keeps this resource's
	// backups, in the provider's own vocabulary ("30d"). Empty keeps
	// everything, which is the safe default.
	RetentionPolicy string

	// Destination is where they go.
	Destination BackupDestination
}

// ArchivingHealth is whether the continuous half of a backup — the write-ahead
// log between base backups — is reaching the destination.
//
// It is separate from "the last base backup succeeded" because the two fail
// independently and only one of them is visible: a weekly base backup with
// three days of unarchived WAL behind it recovers to the base backup and no
// further, and reports a green schedule the whole time.
type ArchivingHealth string

const (
	// ArchivingUnknown: the provider has not said either way yet.
	ArchivingUnknown ArchivingHealth = ""
	// ArchivingHealthy: WAL is reaching the destination.
	ArchivingHealthy ArchivingHealth = "healthy"
	// ArchivingFailing: it is not, and the message says what the provider
	// said about it.
	ArchivingFailing ArchivingHealth = "failing"
)

// BackupState is what the destination actually holds for one resource, read
// from the provider on every pass and never derived from the policy.
//
// The distinction is the whole point. A policy says backups are configured; a
// state says the earliest moment this data can still be reconstructed to, and
// only the second is worth anything to somebody in an incident.
type BackupState struct {
	// Configured is whether the provider is writing backups for this
	// resource right now, as the provider reports its own configuration.
	Configured bool

	// Schedule, RetentionPolicy and Destination are the policy as the
	// provider actually holds it — read back rather than echoed, so a
	// configuration that never landed cannot report itself as in force.
	Schedule        string
	RetentionPolicy string
	Destination     string

	// LastBackup and LastFailure are when a base backup last succeeded and
	// last failed, as the provider reports them. Nil where it reports
	// neither, which is a resource nothing has backed up yet.
	LastBackup  *time.Time
	LastFailure *time.Time

	// FirstRecoverablePoint is the oldest moment the destination can still
	// reconstruct this resource to. Nil until the provider has a base backup
	// it has read back; it moves forward as retention prunes.
	FirstRecoverablePoint *time.Time

	// Archiving and ArchivingMessage are the continuous half.
	Archiving        ArchivingHealth
	ArchivingMessage string
}

// BackupProvisioner is a Provisioner whose backups the platform configures:
// a resource this platform runs, which it can point at an object store and
// hand a schedule.
//
// Both methods are idempotent and tolerant of absence, like every other
// operation on a Provisioner: configuring a policy already in force changes
// nothing, and reading the state of a resource that is not there yet answers
// an empty state rather than an error.
type BackupProvisioner interface {
	Provisioner

	// ConfigureBackup applies the policy to the resource, or takes it away
	// where the policy is disabled. It answers ErrBackupNotManaged for a
	// resource the platform did not create, and ErrBackupUnsupported where
	// the provider's running version no longer accepts what was written.
	//
	// It never deletes anything at the destination: a policy switched off is
	// a policy switched off, and what has already been written is pruned by
	// retention and by nothing else.
	ConfigureBackup(ctx context.Context, instanceID string, policy BackupPolicy) error

	// BackupState reads what the destination holds for this resource.
	BackupState(ctx context.Context, instanceID string) (BackupState, error)
}

// SelfBackingProvisioner is a Provisioner whose provider keeps its own
// backups. The platform configures nothing and takes no policy; the claim
// reports the provider's own sentence about what it keeps.
//
// It is a separate interface from BackupProvisioner rather than a flag on it
// because the two are genuinely different answers, and the third answer — a
// provisioner implementing neither, which nothing backs up — has to stay
// distinguishable from both. A screen that showed a Neon claim as unprotected
// would be wrong; one that showed a bucket claim as protected would be worse.
type SelfBackingProvisioner interface {
	Provisioner

	// ManagedBackupNote is what the provider keeps, in its own words, for a
	// claim's status to report verbatim.
	ManagedBackupNote() string
}
