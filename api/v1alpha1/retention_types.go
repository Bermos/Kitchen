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

// Retention: how long each class of what the platform keeps is kept, said in
// one place. See docs/COMPLIANCE.md §14 for the model and for what the
// immutability claim over the audit table does and does not cover.
//
// It is one block rather than a knob per collector because the question a
// records-retention policy asks is "how long do you keep container logs", not
// "what did you configure ClickHouse's log table to do" — and because the
// answers were already scattered across two spec fields and four packages
// that each had their own idea. Every one of those now reads this.
//
// Every field is a pointer and every absent field inherits the knob it used to
// be: the telemetry classes fall back to
// `spec.observability.clickhouse.retentionDays` and the audit class to
// `spec.compliance.audit.retentionDays`. That is what keeps an installation
// that predates this block reading exactly as it did — the old settings are
// this model's defaults rather than a second model beside it.

// AuditFloorDays is the shortest audit retention the platform will accept
// without a written override.
//
// It is not a rounded-up guess. An incident reporting duty runs from when an
// institution *became aware*, which can be well after the transition that
// caused it; a log that has already aged out cannot substantiate the report.
// Ninety days is the shortest window in which "we found out, then we looked"
// is still a sentence the log can support.
const AuditFloorDays int32 = 90

// RetentionSpec is how long each class is kept, in days.
//
// The CEL rules below are the floor and its escape hatch: an audit retention
// under AuditFloorDays is refused at admission unless `auditFloorOverride` is
// present, and an override with nothing to override is refused too — a field
// whose only effect is to look like a decision somebody made is worse than no
// field.
//
// +kubebuilder:validation:XValidation:rule="!has(self.audit) || self.audit >= 90 || has(self.auditFloorOverride)",message="spec.retention.audit is below the 90-day floor: an incident reporting duty runs from when an institution became aware, which can be long after the transition that caused it. Raise it to at least 90, or set spec.retention.auditFloorOverride with a reason and an approver — the override is itself an audit record"
// +kubebuilder:validation:XValidation:rule="!has(self.auditFloorOverride) || has(self.audit)",message="spec.retention.auditFloorOverride overrides nothing: it is only meaningful beside spec.retention.audit"
type RetentionSpec struct {
	// ContainerLogs is how long the stdout and stderr of application,
	// platform and cluster containers are kept.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ContainerLogs *int32 `json:"containerLogs,omitempty"`

	// BuildLogs is how long a build's own output is kept.
	//
	// It is its own class because it answers a different question. A
	// container log is operational — what was this process doing on Tuesday
	// — and a build log is part of the account of how an artifact came to
	// exist, which is read months later beside the artifact's provenance.
	// Installations routinely want the second kept longer than the first.
	//
	// Both live in the same table, which has a consequence worth knowing
	// before setting them apart: see docs/COMPLIANCE.md §14.2.
	// +kubebuilder:validation:Minimum=1
	// +optional
	BuildLogs *int32 `json:"buildLogs,omitempty"`

	// Flows is how long observed network flows are kept — who talked to
	// whom, which is what the traffic view draws.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Flows *int32 `json:"flows,omitempty"`

	// Metrics is how long metric series and their rollups are kept.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Metrics *int32 `json:"metrics,omitempty"`

	// Traces is how long spans are kept.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Traces *int32 `json:"traces,omitempty"`

	// Requests is how long HTTP request telemetry is kept. It is the one
	// class the store scales rather than applies: raw rows are the densest
	// thing in it and live a week or this window, whichever is shorter, and
	// the hourly rollup keeps twelve of these windows so a year-scale view
	// has something to read. Those two ratios are not configurable.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Requests *int32 `json:"requests,omitempty"`

	// ClusterEvents is how long the cluster's Warning-event history is kept
	// — the API server expires the originals about an hour after they
	// happen, so this is the only copy.
	// +kubebuilder:validation:Minimum=1
	// +optional
	ClusterEvents *int32 `json:"clusterEvents,omitempty"`

	// Activity is how long the dashboard's activity feed is kept. It is
	// prose for a person catching up rather than evidence — the audit log is
	// the evidence — so it is the one class where a short window costs
	// nothing but convenience.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Activity *int32 `json:"activity,omitempty"`

	// Audit is how long audit records and the policy decisions they gate are
	// kept. The two share a retention deliberately: a decision and the audit
	// record that gated it substantiate each other, and aging them out
	// separately would leave whichever survives pointing at nothing.
	//
	// The floor is AuditFloorDays and the way under it is the override
	// below, not a smaller number.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Audit *int32 `json:"audit,omitempty"`

	// AuditFloorOverride is the written decision to keep audit records for
	// less than the floor.
	//
	// It exists because a floor with no way past it is a floor somebody
	// eventually removes from the code. An installation that genuinely
	// cannot keep ninety days — a demonstration cluster, a jurisdiction
	// whose data-minimisation rule bites first — should be able to say so
	// *in the object*, with a reason and a name against it, rather than by
	// patching the platform.
	//
	// Setting it is a privileged audit record in its own right, recorded
	// with the number and the reason, so "who decided we keep sixty days"
	// has a written answer.
	// +optional
	AuditFloorOverride *RetentionOverrideSpec `json:"auditFloorOverride,omitempty"`
}

// RetentionOverrideSpec is the reason and the name behind a retention set
// below its documented floor.
//
// Both fields are required and the reason has a length floor, for the reason
// every break-glass field in this suite has one: "n/a" is not a reason, and a
// field that accepts it is a field that will contain it.
type RetentionOverrideSpec struct {
	// Reason is why this installation keeps less than the floor. It is read
	// by whoever asks why the log does not go back far enough.
	// +kubebuilder:validation:MinLength=20
	// +kubebuilder:validation:MaxLength=500
	Reason string `json:"reason"`

	// ApprovedBy is who decided it, as an address or a name a person can be
	// found by. The platform does not resolve it against the identity
	// provider — an override may well be approved by somebody who has no
	// account here — so it is recorded as written.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=200
	ApprovedBy string `json:"approvedBy"`
}

// ClockSyncSpec configures the node time-sync check.
//
// It is under observability rather than under compliance because it is a
// measurement, not a piece of evidence the platform produces: what it
// measures is whether the timestamps everything else here is stamped with
// mean the same thing on every node. Its consequence — a component the survey
// reports as unhealthy — is where an operator already looks.
type ClockSyncSpec struct {
	// Enabled measures node clock drift on every platform reconcile.
	//
	// The empty-object default on the block above is what turns it on for an
	// installation that predates it; this pointer is what lets somebody turn
	// it off again, on a cluster whose kubelets renew their leases so
	// slowly that the measurement says nothing useful.
	// +kubebuilder:default=true
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// MaxDriftSeconds is how far a node's clock may be from the operator's
	// before the survey calls the check unhealthy.
	//
	// Five seconds is the default and it is chosen against the *use*, not
	// against NTP's accuracy: a correlation in an incident report is built
	// from log lines, a request trace and an audit record that were stamped
	// on three different machines, and five seconds is roughly where "these
	// happened in this order" stops being safe to say. A properly
	// synchronised cluster sits three orders of magnitude inside it, so a
	// breach means time sync is broken rather than merely imprecise.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=3600
	// +kubebuilder:default=5
	// +optional
	MaxDriftSeconds int32 `json:"maxDriftSeconds,omitempty"`
}

// RetentionClassStatus is what the platform is actually doing about one class,
// as opposed to what it was asked to do.
type RetentionClassStatus struct {
	// Class names the class, in the vocabulary the spec uses.
	Class string `json:"class"`

	// Days is the retention in force — the configured value, or the knob it
	// inherits from.
	Days int32 `json:"days"`

	// Source says where Days came from: `retention` for this block, or the
	// legacy field it fell back to. It is here because an operator reading
	// "30" wants to know whether anybody chose it.
	// +optional
	Source string `json:"source,omitempty"`

	// Enforced is true when the store is holding the retention this class
	// asks for. False means the platform asked and the store has not (yet)
	// agreed — the message says why.
	Enforced bool `json:"enforced"`

	// Rows the class currently holds, as of the last sweep.
	// +optional
	Rows int64 `json:"rows,omitempty"`

	// Oldest surviving row, as of the last sweep. This is the claim
	// retention actually makes: nothing about this class is older than this.
	// +optional
	Oldest *metav1.Time `json:"oldest,omitempty"`

	// Expired counts rows still present on the wrong side of the horizon at
	// the last sweep. It is normally zero or one partition's worth; a number
	// that stays large means the store is holding data past its date, which
	// is a thing this platform reports rather than hides.
	// +optional
	Expired int64 `json:"expired,omitempty"`

	// Removed is how many rows the last sweep deleted itself. It is the one
	// number here that is exact rather than observed: the sweep only ever
	// drops a partition every row of which is past the horizon, so it can
	// count them. Everything the store expired on its own schedule is
	// invisible to it, and §14.4 says so.
	// +optional
	Removed int64 `json:"removed,omitempty"`

	// Message explains a class that is not being enforced, or a store that
	// could not be asked.
	// +optional
	Message string `json:"message,omitempty"`
}

// RetentionStatus reports the retention model as it is actually in force.
type RetentionStatus struct {
	// Classes is every class, in name order.
	// +optional
	// +listType=map
	// +listMapKey=class
	Classes []RetentionClassStatus `json:"classes,omitempty"`

	// AuditFloorOverridden is true when audit records are kept for less than
	// AuditFloorDays under a written override. It is on the status as well
	// as in the spec so that a reader who came for "is this installation
	// keeping its evidence" gets the answer without having to know which
	// field to look at.
	// +optional
	AuditFloorOverridden bool `json:"auditFloorOverridden,omitempty"`

	// LastSweep is when the retention sweep last completed a pass.
	// +optional
	LastSweep *metav1.Time `json:"lastSweep,omitempty"`

	// Message explains a model that is configured and not being enforced —
	// an installation with no telemetry store, most often.
	// +optional
	Message string `json:"message,omitempty"`
}

// ClockSyncStatus is the last measurement of how far the cluster's clocks are
// from the operator's own.
//
// Method is carried because the honest reading of every number here depends on
// it: see docs/COMPLIANCE.md §14.5, which says what this measures, what it
// cannot measure, and why the platform does not reach for an external time
// source to do better.
type ClockSyncStatus struct {
	// Checked is when the measurement was taken.
	// +optional
	Checked *metav1.Time `json:"checked,omitempty"`

	// Method names how it was taken, so a number is never read without the
	// caveat that belongs to it.
	// +optional
	Method string `json:"method,omitempty"`

	// Nodes measured.
	// +optional
	Nodes int32 `json:"nodes,omitempty"`

	// Drifted is how many of them are beyond the threshold.
	// +optional
	Drifted int32 `json:"drifted,omitempty"`

	// MaxDriftSeconds is the threshold that was applied.
	// +optional
	MaxDriftSeconds int32 `json:"maxDriftSeconds,omitempty"`

	// WorstNode is the node furthest from the operator's clock, and
	// WorstDriftMillis how far. Milliseconds because a healthy cluster's
	// answer is a two-digit number of them and rounding it to seconds would
	// report every healthy cluster as perfect.
	// +optional
	WorstNode string `json:"worstNode,omitempty"`

	// +optional
	WorstDriftMillis int64 `json:"worstDriftMillis,omitempty"`

	// Message is what to do about it, for a check that is failing, and why
	// nothing was measured, for one that could not run.
	// +optional
	Message string `json:"message,omitempty"`
}

// ClockSyncEnabled reads the pointer with its default applied, for a Kitchen
// object written before the field existed.
func (c *ClockSyncSpec) ClockSyncEnabled() bool {
	return c == nil || c.Enabled == nil || *c.Enabled
}

// DriftThreshold is the configured threshold with the compiled-in default
// applied. It is a method rather than a field read so that every caller gets
// the same answer for an object the API server never defaulted.
func (c *ClockSyncSpec) DriftThreshold() int32 {
	if c == nil || c.MaxDriftSeconds < 1 {
		return DefaultMaxDriftSeconds
	}
	return c.MaxDriftSeconds
}

// DefaultMaxDriftSeconds matches the CRD default, for Kitchen objects written
// before the field existed.
const DefaultMaxDriftSeconds int32 = 5
