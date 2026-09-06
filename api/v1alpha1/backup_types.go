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

// BackupDestinationType is where archives are written. There is exactly one
// value, and the enum is the shape rather than the promise: a second backend
// is a new value here *plus* an implementation of
// internal/backup/destination.Destination, on the rule the claim types keep.
//
// There is deliberately no "local" destination. The only place on the cluster
// to put an archive is a volume on the cluster the archive exists to survive
// the loss of, and a feature whose default configuration produces a worthless
// backup is worse than no feature.
// +kubebuilder:validation:Enum=s3
type BackupDestinationType string

const (
	// BackupDestinationS3 is any S3-compatible store: AWS, MinIO, R2,
	// Backblaze, Wasabi, Ceph, Garage. The endpoint override is what makes
	// those one code path rather than six backends.
	BackupDestinationS3 BackupDestinationType = "s3"
)

// The at-rest encryption an S3 destination can be asked for, spelled once
// because two surfaces refuse everything else by name — the platform's own
// destination and a claim's — and the CRD's enum marker beside
// S3Destination.ServerSideEncryption is the third place the pair appears.
const (
	// ServerSideEncryptionAES256 is the store's own key.
	ServerSideEncryptionAES256 = "AES256"
	// ServerSideEncryptionKMS is a key named by KMSKeyID.
	ServerSideEncryptionKMS = "aws:kms"
)

// BackupEncryptionMode is how an archive is protected on its way to a
// destination, and it has exactly two honest answers.
//
// The archive is every credential the platform holds. Server-side encryption
// is worth asking for and is not this: a store that encrypts at rest still
// decrypts for anybody it answers, so it rests the confidentiality of the
// cluster's root credentials on that bucket's configuration and its ACL.
// Encrypting before the upload rests it on a key that is not in the bucket,
// is not in the archive, and never leaves this cluster except in the
// operator's own hands.
// +kubebuilder:validation:Enum=aes256-gcm;none
type BackupEncryptionMode string

const (
	// BackupEncryptionAES256GCM encrypts the archive under the operator's own
	// key before it is uploaded. It is the default, and an empty mode means
	// it: an archive of every platform credential going to somebody's bucket
	// in the clear should be something an installation asked for out loud.
	BackupEncryptionAES256GCM BackupEncryptionMode = "aes256-gcm"

	// BackupEncryptionNone uploads the archive as it is. It is a decision an
	// installation makes rather than a state it drifts into, which is the
	// whole reason this field exists.
	BackupEncryptionNone BackupEncryptionMode = "none"
)

// BackupEncryptionSpec is what protects an archive at the destination.
//
// The key is the operator's and is supplied, never generated here — the rule
// a notification subscription's signing key already keeps. A key this
// platform minted and answered once would live in a shell history, a
// browser's memory and whatever logged the response; a key the operator
// generated is one they still have on the day the cluster is gone, which is
// the only day it is needed.
type BackupEncryptionSpec struct {
	// Mode is how the archive is encrypted before it is uploaded. Empty means
	// BackupEncryptionAES256GCM: encryption is what an installation gets
	// without saying anything, and BackupEncryptionNone is the explicit
	// opt-out.
	// +optional
	Mode BackupEncryptionMode `json:"mode,omitempty"`

	// KeySecretRef names a Secret in the platform namespace holding the
	// 32-byte key under `key`. It is written by
	// PUT /platform/backup/destination and never read back, like every other
	// credential this API stores.
	//
	// **Keep a copy of that key off this cluster.** It is deliberately not in
	// the archive — an archive carrying the key that opens it is an archive
	// with no encryption — so an archive whose key was only ever in the
	// cluster that died restores into nothing.
	// +optional
	KeySecretRef *LocalObjectReference `json:"keySecretRef,omitempty"`
}

// Encrypted is whether archives are encrypted before they are uploaded. An
// unset mode is encrypted: this is the field that decides it, and it decides
// it the safe way round.
func (e BackupEncryptionSpec) Encrypted() bool {
	return e.EffectiveMode() == BackupEncryptionAES256GCM
}

// EffectiveMode is the mode as a run would actually apply it.
func (e BackupEncryptionSpec) EffectiveMode() BackupEncryptionMode {
	if e.Mode == "" {
		return BackupEncryptionAES256GCM
	}
	return e.Mode
}

// The CEL rule below spells "empty" as size() == 0 rather than as a comparison
// against an empty string literal, and it has to: gofmt reformats doc comments,
// and a pair of adjacent single quotes in one is rewritten into a typographic
// quote — which would leave the marker generating a CRD whose CEL does not
// parse, with nothing but a stale-manifests failure to say why. This note is
// out here rather than in the doc comment so that it does not become part of
// the field's description in the CRD.

// S3Destination is a bucket at an S3-compatible store.
//
// The endpoint rule is the same rule notification webhooks keep, applied to
// the one upload that carries every credential the platform holds: an archive
// travelling to `http://minio.internal:9000` travels in the clear, over
// whatever is between this cluster and that store. It is refused at admission
// rather than reported afterwards, and AllowInsecureEndpoint is how an
// installation that means it says so.
// +kubebuilder:validation:XValidation:rule="!has(self.endpoint) || self.endpoint.size() == 0 || self.endpoint.startsWith('https://') || (has(self.allowInsecureEndpoint) && self.allowInsecureEndpoint)",message="spec.backup.destination.s3.endpoint must be an https:// URL: the archive is every credential this platform holds, and an endpoint that is not https carries it in the clear. Set allowInsecureEndpoint if the store is genuinely reached over a network you trust."
type S3Destination struct {
	// Bucket archives are written into. It should be a bucket of its own:
	// see the retention note below, and docs/BACKUP.md on why this bucket is
	// the cluster's root credential store.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Prefix is the key prefix inside the bucket. Pruning only ever considers
	// objects under it that are named like an archive this platform wrote, so
	// a bucket somebody also keeps other things in does not lose them.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region the bucket is in. Most S3-compatible stores want one even where
	// it means nothing; us-east-1 is the conventional answer for those.
	// +optional
	Region string `json:"region,omitempty"`

	// Endpoint overrides AWS. This is what makes MinIO, R2, Backblaze,
	// Wasabi, Ceph and Garage the same code path rather than five backends.
	//
	// It must be an `https://` URL. Empty is the AWS endpoint, which is
	// https either way.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// AllowInsecureEndpoint permits an endpoint that is not `https://`.
	//
	// It exists because a store on a cluster-internal network is a real
	// installation and not a mistake, and it is explicit for the reason
	// every other opt-out here is: an archive going to a plaintext endpoint
	// is every credential this platform holds on somebody else's wire, and
	// that should be a sentence in the object rather than a silent default.
	// The archive's own encryption is a separate decision and still applies.
	// +optional
	AllowInsecureEndpoint bool `json:"allowInsecureEndpoint,omitempty"`

	// ForcePathStyle addresses the bucket as <endpoint>/<bucket> rather than
	// <bucket>.<endpoint>. Every store that is reached by IP address or by a
	// name with no wildcard certificate behind it needs this — MinIO in a
	// cluster is the usual one.
	// +optional
	ForcePathStyle bool `json:"forcePathStyle,omitempty"`

	// ServerSideEncryption asks the store to encrypt the object at rest:
	// ServerSideEncryptionAES256 or ServerSideEncryptionKMS. The archive is
	// every credential the platform holds, so a store that can encrypt it
	// should be told to.
	// +kubebuilder:validation:Enum={"AES256","aws:kms"}
	// +optional
	ServerSideEncryption string `json:"serverSideEncryption,omitempty"`

	// KMSKeyID names the key when the encryption is aws:kms.
	// +optional
	KMSKeyID string `json:"kmsKeyId,omitempty"`

	// CredentialsSecretRef names a Secret in the platform namespace holding
	// accessKeyId and secretAccessKey. It is written by
	// PUT /platform/backup/destination and never read back, like every other
	// credential this API stores.
	//
	// Absent means the ambient credential chain — IRSA, EKS Pod Identity, an
	// instance role — which is the better answer where it is available,
	// because there is then no long-lived key anywhere to leak.
	// +optional
	CredentialsSecretRef *LocalObjectReference `json:"credentialsSecretRef,omitempty"`
}

// BackupDestination is where the scheduled archive goes.
//
// The type/block pairing is checked at admission for the same reason, and in
// the same shape, as an `acme` block under a non-ACME TLS mode: a destination
// that is half-configured is a schedule that fires into nothing, and finding
// that out from a condition six weeks later is the failure this whole feature
// exists to prevent.
//
// +kubebuilder:validation:XValidation:rule="self.type != 's3' || has(self.s3)",message="spec.backup.destination.s3 is required when the destination type is s3: it names the bucket archives are written to."
// +kubebuilder:validation:XValidation:rule="self.type == 's3' || !has(self.s3)",message="an s3 block belongs to a destination of type s3 alone."
type BackupDestination struct {
	// Type is the kind of store. The enum lives on BackupDestinationType
	// itself, so it is not repeated here.
	Type BackupDestinationType `json:"type"`

	// +optional
	S3 *S3Destination `json:"s3,omitempty"`
}

// BackupRetentionSpec is how many archives the destination keeps. Both bounds
// apply where both are set: an archive is kept while it is among the newest
// KeepLast *and* newer than KeepDays.
//
// It is not a safety property, and should not be read as one: retention that
// deletes is retention somebody who reaches the credential can use. The
// answer to that is the store's — S3 Object Lock, or object versioning — and
// Kitchen does not manage it. See docs/BACKUP.md.
type BackupRetentionSpec struct {
	// KeepLast is how many of the newest archives survive a prune.
	// +kubebuilder:validation:Minimum=1
	// +optional
	KeepLast *int32 `json:"keepLast,omitempty"`

	// KeepDays deletes archives older than this many days.
	// +kubebuilder:validation:Minimum=1
	// +optional
	KeepDays *int32 `json:"keepDays,omitempty"`
}

// BackupSpec is the platform's own scheduled backup: when it runs, where the
// archive goes, and how much of it is kept.
//
// The two admission rules below are the `tls.mode: acme` rules again, and for
// the same reason. A schedule with nowhere to write to would produce an
// archive on a volume on the cluster it exists to survive the loss of; a
// retention with no destination overrides nothing. Both are refused at
// admission rather than reported afterwards on the status of an object that
// is already wrong.
//
// The first rule asks `has` *and* the size, because both spellings of "no
// schedule" reach the API server: the field is `omitempty`, so a client that
// clears it removes the key, and a hand-written manifest may still carry it as
// an empty string.
// +kubebuilder:validation:XValidation:rule="!has(self.schedule) || size(self.schedule) == 0 || has(self.destination)",message="spec.backup.destination is required when a schedule is set: an archive written to a volume on this cluster does not survive the loss of this cluster, so there is deliberately no local destination. Configure a destination, or clear the schedule."
// +kubebuilder:validation:XValidation:rule="!has(self.retention) || (!has(self.retention.keepLast) && !has(self.retention.keepDays)) || has(self.destination)",message="spec.backup.retention needs a destination to apply to: retention prunes what is at the destination, and with no destination it overrides nothing."
type BackupSpec struct {
	// Schedule is a five-field cron expression, in UTC — as every schedule on
	// this platform is, and worth saying out loud on a platform that already
	// measures node clock drift. Empty means no scheduled backup, which is
	// what an installation that predates this field keeps having.
	//
	// A quiet hour is the right answer: the accounts half of an archive is
	// taken through the identity provider's database, and a dump competing
	// with sign-ins helps nobody.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Suspend pauses the schedule without losing it — the answer to "we are
	// doing maintenance" that is not "delete the configuration and hope
	// somebody puts it back".
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// Destination is where archives are written.
	// +optional
	Destination *BackupDestination `json:"destination,omitempty"`

	// Encryption is what protects an archive at the destination. Left empty
	// it is AES-256-GCM under a key the operator supplied, and a run with no
	// key to hand fails rather than uploading the platform's credentials in
	// the clear.
	// +optional
	Encryption BackupEncryptionSpec `json:"encryption,omitempty"`

	// Retention is how many archives the destination keeps. Left empty,
	// nothing is ever pruned, which is the safe default: an archive costs
	// pennies and the failure this feature exists to prevent is having too
	// few of them.
	// +optional
	Retention BackupRetentionSpec `json:"retention,omitempty"`

	// Timeout bounds one run: the export, the upload, the read-back and the
	// prune together.
	// +kubebuilder:default="30m"
	// +optional
	Timeout metav1.Duration `json:"timeout,omitempty"`
}

// Configured is whether a scheduled backup has been asked for at all. A
// schedule with no destination cannot exist — admission refuses it — so the
// schedule alone is the whole question.
func (b BackupSpec) Configured() bool {
	return b.Schedule != "" && b.Destination != nil
}

// BackupStatus is what the schedule has actually been doing, which is the
// half of this feature that matters most.
//
// Every other field on BackupSpec makes backups happen; this is the only
// thing that makes their *absence* visible. A backup system's characteristic
// failure is not a bad archive, it is six weeks of no archive that nobody
// noticed — so "when did one last work" is status, is on the platform screen,
// and is a row in the component survey.
//
// There is deliberately no nextRun. Computing it means parsing cron, and this
// platform hands every schedule it is given to Kubernetes rather than keeping
// a second opinion about what one means; the schedule is reported as written
// and the CronJob is the thing that knows.
type BackupStatus struct {
	// Schedule in force, echoed so that a reader of the status does not have
	// to fetch the spec to know what "late" would mean.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Suspended is whether the schedule is paused.
	// +optional
	Suspended bool `json:"suspended,omitempty"`

	// Destination described, and never its credential: "s3://bucket/prefix".
	// +optional
	Destination string `json:"destination,omitempty"`

	// Encryption is what is actually protecting the archives there:
	// "aes256-gcm", or "none" where the installation opted out. It is
	// reported rather than left to be inferred from the spec, because "what
	// is in that bucket, and what would it take to read it" is the question
	// somebody asks about a destination and not about a field.
	// +optional
	Encryption string `json:"encryption,omitempty"`

	// LastRun is when a run last started, whatever became of it.
	// +optional
	LastRun *metav1.Time `json:"lastRun,omitempty"`

	// LastSuccess is when a run last finished, having uploaded an archive and
	// read it back. It is the number an operator should be watching.
	// +optional
	LastSuccess *metav1.Time `json:"lastSuccess,omitempty"`

	// LastSuccessArchive is the key that run wrote, and LastSuccessBytes how
	// big it was.
	// +optional
	LastSuccessArchive string `json:"lastSuccessArchive,omitempty"`
	// +optional
	LastSuccessBytes int64 `json:"lastSuccessBytes,omitempty"`

	// LastFailure is when a run last failed.
	// +optional
	LastFailure *metav1.Time `json:"lastFailure,omitempty"`

	// Message explains the state above in the words that name the fix: why
	// there is no schedule, why the last run failed, or why a schedule that
	// exists has never produced an archive.
	// +optional
	Message string `json:"message,omitempty"`

	// Archives is how many archives the last run left at the destination,
	// after its prune.
	// +optional
	Archives int32 `json:"archives,omitempty"`
}
