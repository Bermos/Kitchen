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

package api

import (
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/provider/database"
)

// The backup policy half of the claim API (#245 phase 2): what happens to a
// database this platform runs, asked for on the claim that provisions it and
// answered on every read of that claim.
//
// It adds **no route**. A backup policy is part of what a claim is, not a
// second object beside it, so it rides the claim's own create and its own
// view — which keeps the enforcement table exactly as it was, and keeps the
// policy in the same request as the `deletionPolicy` it has to be read
// against.
//
// The credential follows the same rule every credential this API stores
// follows and there is no exception here: the keys are written into a Secret
// the operator reads, the Secret carries the managed-by label, it is removed
// with the claim, and **no response ever echoes a key**. What a read answers
// about the credential is whether there is one.

// claimBackupWrite is a backup policy as a create request states it.
type claimBackupWrite struct {
	// Enabled turns the policy on or off for this claim. Absent inherits:
	// the Connection's default, and failing that the platform's, which is on
	// wherever a backup destination is configured.
	Enabled *bool `json:"enabled,omitempty"`

	// Schedule for the base backup, in **CloudNativePG's** cron: six fields,
	// seconds first. Absent takes the Connection's, and failing that a
	// nightly backup in a quiet hour.
	Schedule string `json:"schedule,omitempty"`

	// RetentionPolicy is how long the destination keeps this database's
	// backups: "30d", "4w", "6m". Absent keeps everything.
	RetentionPolicy string `json:"retentionPolicy,omitempty"`

	// Destination sends this claim's backups somewhere other than the
	// platform's own bucket. Absent — which is almost always — takes the
	// platform's, under a prefix of this database's own.
	Destination *claimBackupDestinationWrite `json:"destination,omitempty"`
}

// claimBackupDestinationWrite is a bucket of the claim's own, with the
// credential to write it. The credential half is write-only.
type claimBackupDestinationWrite struct {
	Bucket               string `json:"bucket"`
	Prefix               string `json:"prefix,omitempty"`
	Region               string `json:"region,omitempty"`
	Endpoint             string `json:"endpoint,omitempty"`
	ForcePathStyle       bool   `json:"forcePathStyle,omitempty"`
	ServerSideEncryption string `json:"serverSideEncryption,omitempty"`

	// AccessKeyID and SecretAccessKey are the credential: both or neither,
	// because half a key pair is a destination that cannot authenticate and
	// that should be refused here rather than discovered at 03:00. Neither
	// means the credential chain the operator's pod already has.
	AccessKeyID     string `json:"accessKeyId,omitempty"`
	SecretAccessKey string `json:"secretAccessKey,omitempty"`
}

// claimBackupView is what a read of the claim answers about its backups: the
// policy in force, and — the field that is the point of the whole feature —
// the oldest moment the destination can still put this database back to.
type claimBackupView struct {
	// Enabled is whether the platform is configuring backups for this claim.
	Enabled bool `json:"enabled"`

	// ProviderManaged is a claim whose provider keeps its own backups, which
	// the platform neither configures nor could turn off. It is the honest
	// third state between backed up and not: such a claim is protected, by
	// somebody else, and Reason is that provider's own sentence about it.
	ProviderManaged bool `json:"providerManaged,omitempty"`

	// Reason says why the state is what it is, in the words that name the
	// fix.
	Reason string `json:"reason,omitempty"`

	// Schedule, RetentionPolicy and Destination are the policy in force
	// after the claim, its Connection and the platform have been resolved.
	// Destination is described and never its credential.
	Schedule        string `json:"schedule,omitempty"`
	RetentionPolicy string `json:"retentionPolicy,omitempty"`
	Destination     string `json:"destination,omitempty"`

	// LastBackup and LastFailure are when a base backup last succeeded and
	// last failed, as the database's own operator reports them.
	LastBackup  *time.Time `json:"lastBackup,omitempty"`
	LastFailure *time.Time `json:"lastFailure,omitempty"`

	// FirstRecoverablePoint is the oldest moment this database can still be
	// reconstructed to. Absent until the first base backup has been taken
	// and read back — which is a real state and is answered as the absence
	// it is, because "backups are configured" and "we can restore to 03:14
	// last Tuesday" are different claims and only the second is worth
	// anything.
	FirstRecoverablePoint *time.Time `json:"firstRecoverablePoint,omitempty"`

	// Archiving is whether the write-ahead log is reaching the destination
	// between base backups — healthy, failing or unknown — and
	// ArchivingMessage the database's own account of it. A base backup whose
	// WAL is not archiving recovers to the base backup and no further.
	Archiving        string `json:"archiving,omitempty"`
	ArchivingMessage string `json:"archivingMessage,omitempty"`
}

// claimBackupSecretName is where a claim-scoped destination's credential is
// written. It matches the name the operator copies into the database
// namespace, so the two halves of one credential are legibly one thing.
func claimBackupSecretName(claim string) string { return claim + "-backup-destination" }

// backupOf is the claim's backup policy as the API answers it, and nothing at
// all for a claim of a type nothing backs up.
func backupOf(claim *kitchenv1alpha1.ResourceClaim) *claimBackupView {
	status := claim.Status.Backup
	if status == nil {
		return nil
	}
	view := &claimBackupView{
		Enabled:               status.Enabled,
		ProviderManaged:       status.ProviderManaged,
		Reason:                status.Reason,
		Schedule:              status.Schedule,
		RetentionPolicy:       status.RetentionPolicy,
		Destination:           status.Destination,
		LastBackup:            timeOrNil(status.LastBackup),
		LastFailure:           timeOrNil(status.LastFailure),
		FirstRecoverablePoint: timeOrNil(status.FirstRecoverablePoint),
		Archiving:             string(status.Archiving),
		ArchivingMessage:      status.ArchivingMessage,
	}
	return view
}

// claimBackupSpec validates the requested policy and writes its credential.
//
// It answers the spec the claim carries, or nil where the request asked for
// nothing — a claim with no policy of its own inherits the whole of it, which
// is what most claims should do. ok=false means a refusal has already been
// written.
func (s *Server) claimBackupSpec(
	w http.ResponseWriter,
	req *http.Request,
	claimType kitchenv1alpha1.ClaimType,
	body *createClaimRequest,
) (*kitchenv1alpha1.ClaimBackupSpec, bool) {
	if body.Backup == nil {
		return nil, true
	}
	// The CRD refuses this at admission too; this layer is the one that can
	// say why in a sentence before anything exists.
	if claimType.Name != kitchenv1alpha1.ClaimTypePostgres {
		badRequest(w, "backup belongs to a claim whose resource this platform runs and can configure "+
			"backups for, which is postgres alone: %s claim names %s somebody else runs, and it is backed "+
			"up by whoever runs it", withArticle(claimType.Name), withArticle(claimType.Resource))
		return nil, false
	}

	spec := &kitchenv1alpha1.ClaimBackupSpec{
		Schedule:        strings.TrimSpace(body.Backup.Schedule),
		RetentionPolicy: strings.TrimSpace(body.Backup.RetentionPolicy),
	}
	if body.Backup.Enabled != nil {
		enabled := *body.Backup.Enabled
		spec.Enabled = &enabled
	}
	if spec.Schedule != "" {
		// Six fields, seconds first. The five-field spelling is not an error
		// the database operator would report — it is a *valid* expression
		// meaning something else — so it is refused here, with the shape that
		// works.
		if fields := len(strings.Fields(spec.Schedule)); fields != 6 {
			badRequest(w, "backup.schedule is a CloudNativePG schedule: six fields with seconds first, and "+
				"this has %d (%q). A five-field schedule is not rejected by the database — it is read as "+
				"something else entirely — so write %q for 03:00 UTC nightly",
				fields, spec.Schedule, database.DefaultClaimBackupSchedule)
			return nil, false
		}
	}
	if spec.RetentionPolicy != "" && !retentionPolicyPattern.MatchString(spec.RetentionPolicy) {
		badRequest(w, "backup.retentionPolicy is a count and a unit — \"30d\", \"4w\", \"6m\" (got %q)",
			spec.RetentionPolicy)
		return nil, false
	}
	if body.Backup.Destination == nil {
		return spec, true
	}

	write := body.Backup.Destination
	if strings.TrimSpace(write.Bucket) == "" {
		badRequest(w, "backup.destination.bucket is required: it is where this database's backups are "+
			"written. Leave the destination out entirely to use the platform's own, which is what almost "+
			"every claim should do")
		return nil, false
	}
	switch encryption := write.ServerSideEncryption; encryption {
	case "", kitchenv1alpha1.ServerSideEncryptionAES256, kitchenv1alpha1.ServerSideEncryptionKMS:
	default:
		badRequest(w, "backup.destination.serverSideEncryption must be %s or %s (got %q), or "+
			"empty for whatever the bucket does by default",
			kitchenv1alpha1.ServerSideEncryptionAES256, kitchenv1alpha1.ServerSideEncryptionKMS, encryption)
		return nil, false
	}
	hasKey := write.AccessKeyID != "" || write.SecretAccessKey != ""
	if hasKey && (write.AccessKeyID == "" || write.SecretAccessKey == "") {
		badRequest(w, "backup.destination.accessKeyId and secretAccessKey go together: half a key pair is "+
			"a destination that cannot authenticate")
		return nil, false
	}

	destination := &kitchenv1alpha1.BackupDestination{
		Type: kitchenv1alpha1.BackupDestinationS3,
		S3: &kitchenv1alpha1.S3Destination{
			Bucket:               strings.TrimSpace(write.Bucket),
			Prefix:               strings.Trim(strings.TrimSpace(write.Prefix), "/"),
			Region:               strings.TrimSpace(write.Region),
			Endpoint:             strings.TrimSpace(write.Endpoint),
			ForcePathStyle:       write.ForcePathStyle,
			ServerSideEncryption: write.ServerSideEncryption,
		},
	}
	if hasKey {
		name := claimBackupSecretName(body.Name)
		if err := s.writeCredentialsSecret(req, name, map[string][]byte{
			backup.CredentialKeyAccessKeyID:     []byte(write.AccessKeyID),
			backup.CredentialKeySecretAccessKey: []byte(write.SecretAccessKey),
		}, corev1.SecretTypeOpaque); err != nil {
			s.writeError(w, err)
			return nil, false
		}
		destination.S3.CredentialsSecretRef = &kitchenv1alpha1.LocalObjectReference{Name: name}
	}
	spec.Destination = destination
	return spec, true
}
