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

// The compliance surface: the two things the platform produces so that what it
// did can be substantiated later. See docs/COMPLIANCE.md for the model they
// belong to.
//
// Both are on the platform singleton rather than on a Project, because both
// are the operator's word rather than the application team's. A team that
// could turn its own audit log off, or sign its own evidence with a key it
// chose, would be attesting to nothing.

// AuditSpec configures the tamper-evident record of state transitions.
//
// Retention is its own knob rather than the telemetry one. Everything else in
// the store is an account of how the platform behaved and ages out in weeks;
// the audit log is the evidence an incident is reconstructed from months
// later, and a shared retention would either throw that away with the logs or
// keep a year of request rows to save it.
type AuditSpec struct {
	// Enabled records every state transition the platform makes.
	//
	// Off is a deliberate choice with a consequence worth stating: the
	// decisions the rest of the compliance suite produces still happen, they
	// just leave nothing behind that can be shown to anyone.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// RetentionDays is how long audit records are kept.
	//
	// The floor is 90 days and it is not a rounded-up guess: the incident
	// reporting duty the log exists to serve runs from when an institution
	// became aware, which can be well after the transition that caused it,
	// and a log that has already aged out cannot substantiate the report.
	// Installations under a records-retention obligation will want years
	// rather than months — the ceiling is disk.
	// +kubebuilder:validation:Minimum=90
	// +kubebuilder:default=365
	// +optional
	RetentionDays int32 `json:"retentionDays,omitempty"`
}

// AttestationSpec configures the evidence attached to built artifacts.
//
// What it configures is custody of the signing key, which is the only part of
// the scheme an institution cannot delegate: the envelopes, the statements and
// the way they are attached to an image are all standards, verifiable by
// anything that speaks them, and deliberately so.
type AttestationSpec struct {
	// Enabled signs an attestation for every artifact the platform builds
	// and attaches it to the artifact's digest.
	// +kubebuilder:default=true
	// +optional
	Enabled bool `json:"enabled"`

	// SigningKeyRef names the Secret in the platform namespace holding the
	// key attestations are signed with: the keys `tls.key` and `tls.crt`
	// spelling are deliberately not used — it is `private.pem` and
	// `public.pem`, an ECDSA P-256 keypair in PKCS#8 and PKIX PEM.
	//
	// Left unset the operator generates one once, into
	// `kitchen-attestation-key`, and keeps it across upgrades. Supplying one
	// is how an installation whose key custody rules forbid a key the
	// platform generated brings its own.
	// +optional
	SigningKeyRef *LocalObjectReference `json:"signingKeyRef,omitempty"`

	// Build asks the builder itself for provenance and a bill of materials,
	// which are claims the reconciler cannot make on its own.
	// +optional
	Build BuildAttestationSpec `json:"build,omitempty"`
}

// BuildAttestationSpec configures the evidence the *builder* produces, as
// distinct from the evidence the reconciler produces about the build.
//
// Both knobs cost build time, which is why they are knobs. Provenance is
// nearly free — BuildKit already has everything it records. An SBOM is not: it
// runs a scanner image over the finished filesystem, and that image is pulled
// on every build, because the build pod is ephemeral and nothing survives it.
// An installation that cannot reach the generator, or will not spend the
// seconds, turns it off and says why in its own records rather than having the
// platform decide for it.
type BuildAttestationSpec struct {
	// Provenance asks the builder how the artifact was produced: the source
	// commit it resolved, the base images it pulled and their digests, and
	// the parameters it was invoked with.
	//
	// This is SLSA provenance, and it is a different and stronger claim than
	// Kitchen's own build record: it is made by the process that did the
	// work rather than by the one that asked for it.
	// +kubebuilder:default=true
	// +optional
	Provenance bool `json:"provenance"`

	// SBOM asks the builder for a bill of materials for the finished image.
	// +kubebuilder:default=true
	// +optional
	SBOM bool `json:"sbom"`

	// SBOMGenerator is the scanner image the builder runs to produce it.
	//
	// The **format follows the generator**, and the platform records what
	// came out rather than converting it: the default emits SPDX 2.3, which
	// Grype, Trivy and OSV-Scanner all read unmodified, and a generator that
	// emits CycloneDX produces a CycloneDX attestation whose predicate type
	// says so. Kitchen does not transcode between them — a bill of materials
	// rewritten by something that did not scan the image is a claim by the
	// transcoder.
	//
	// Left unset the operator uses a pinned default. Pinning matters here
	// more than it looks: the tag the ecosystem points at is a floating one,
	// and a build's evidence should not change because an image someone else
	// owns moved overnight.
	// +optional
	SBOMGenerator string `json:"sbomGenerator,omitempty"`
}

// QualityGateSpec is one gate the platform runs over every artifact it builds.
//
// A gate is a pod: an image somebody else wrote, pointed at the artifact, that
// writes what it found to a file. Kitchen contributes the artifact reference,
// the credential to pull it with, and a signature over the result — and
// nothing else. That is what makes adding a gate a change to this list rather
// than a change to every application repository.
//
// **A gate records findings and never a verdict.** Whether a finding is
// disqualifying is a policy question about the environment being deployed to,
// and putting the answer here would fix it platform-wide at the moment of
// scanning — which is precisely what makes the same scan unable to be
// acceptable in staging and blocking in production.
type QualityGateSpec struct {
	// Name identifies the gate in its attestation and on the Build. It has to
	// be stable: a policy that requires "trivy" to have run is matching on
	// this, and renaming a gate silently invalidates every artifact that
	// carries the old name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=40
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Name string `json:"name"`

	// Image is the runner. It is run as an unprivileged user with no access
	// to the cluster, and the only thing it is given is the artifact and the
	// credential to pull it.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`

	// Args are passed to the image. Kubernetes' own `$(VAR)` expansion
	// applies, so a gate names the artifact as `$(KITCHEN_ARTIFACT)` rather
	// than through any templating of Kitchen's — along with
	// `$(KITCHEN_FINDINGS)` for where to write, and `$(KITCHEN_PROJECT)`,
	// `$(KITCHEN_BUILD)` and `$(KITCHEN_COMMIT)` for what is being scanned.
	// +optional
	Args []string `json:"args,omitempty"`

	// Version is what the gate's own version is recorded as in the
	// attestation. A finding is only reproducible against the version that
	// produced it, and the image tag is not always the answer — a scanner
	// whose vulnerability database updates hourly is a different gate every
	// hour under the same tag.
	// +optional
	Version string `json:"version,omitempty"`

	// Format names the shape of what the gate writes, recorded alongside the
	// findings so a reader knows how to parse them. It is informational:
	// nothing here validates it, because a gate that lied about its format
	// would still have produced whatever it produced.
	// +optional
	Format string `json:"format,omitempty"`

	// Disabled stops the gate running without removing it, so that turning
	// one off is a visible line in the configuration rather than a deletion
	// nobody can date.
	// +optional
	Disabled bool `json:"disabled,omitempty"`

	// TimeoutSeconds bounds one run. A gate that hangs must not hold up the
	// evidence the other gates produced.
	// +kubebuilder:validation:Minimum=30
	// +kubebuilder:default=900
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// ComplianceSpec configures what evidence the platform produces about its own
// operation.
type ComplianceSpec struct {
	// Audit is the tamper-evident log of state transitions.
	// +optional
	Audit AuditSpec `json:"audit,omitempty"`

	// Attestation is the signed evidence attached to built artifacts.
	// +optional
	Attestation AttestationSpec `json:"attestation,omitempty"`

	// Gates are the quality gates run over every artifact the platform
	// builds. They live here, on the operator's own object, and not on a
	// Project: a team that chose which scanners ran over its own code would
	// be marking its own homework.
	// +optional
	// +listType=map
	// +listMapKey=name
	Gates []QualityGateSpec `json:"gates,omitempty"`

	// MachineIdentities are accounts whose commits are exempt from a
	// project's pull request requirement.
	//
	// The list exists because the requirement is otherwise unsatisfiable by
	// the automation every repository has. Renovate opens and merges its own
	// dependency bumps; release-please merges its own release commits; this
	// repository's release automation would fail the check on day one. None
	// of them will ever have an independent reviewer, and the realistic
	// alternative to naming them here is somebody turning the requirement off
	// altogether.
	//
	// Naming them is what makes the exemption **auditable**: every use of it
	// is an audit record saying which identity was exempted for which commit,
	// so "who is allowed to bypass review" is a question with a written
	// answer and a history, rather than a property of whoever last edited a
	// pipeline. They are the operator's list, not a project's, for the same
	// reason: a team that could add its own service account to its own
	// exemption list has no requirement at all.
	//
	// Entries are provider usernames, matched case-insensitively and exactly
	// — no patterns. A glob here would eventually exempt more than whoever
	// wrote it meant, and an exemption that surprises its author is the one
	// kind this must not have.
	// +optional
	MachineIdentities []string `json:"machineIdentities,omitempty"`
}

// AuditStatus reports whether the audit log is actually recording, which is
// not the same question as whether it is enabled: it needs the telemetry
// store, and an installation configured without one records nothing however
// the spec reads.
type AuditStatus struct {
	// Recording is true when the operator has a store to append to.
	Recording bool `json:"recording"`

	// Sequence is the number of the last record appended, so that a jump
	// backwards is visible without reading the log itself. Zero means
	// nothing has been recorded yet.
	// +optional
	Sequence int64 `json:"sequence,omitempty"`

	// Message explains a log that is not recording.
	// +optional
	Message string `json:"message,omitempty"`
}

// AttestationStatus reports the identity evidence is signed under. The key id
// is the SHA-256 of the public key's DER encoding, which is what a verifier
// matches an envelope's `keyid` against.
type AttestationStatus struct {
	// Signing is true when the operator holds a usable key.
	Signing bool `json:"signing"`

	// KeyID identifies the public key attestations are signed under.
	// +optional
	KeyID string `json:"keyID,omitempty"`

	// SecretName is where that key lives, so an operator rotating it knows
	// what to replace without guessing at defaults.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// Message explains a platform that is not signing.
	// +optional
	Message string `json:"message,omitempty"`
}

// ComplianceStatus reports what the compliance machinery is doing.
type ComplianceStatus struct {
	// +optional
	Audit *AuditStatus `json:"audit,omitempty"`

	// +optional
	Attestation *AttestationStatus `json:"attestation,omitempty"`
}
