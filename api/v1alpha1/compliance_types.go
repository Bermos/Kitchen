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
