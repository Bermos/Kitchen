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

// Package retention is the one place that says how long each class of what
// the platform keeps is kept.
//
// It is a package rather than a helper on the CRD for one reason: everything
// that enforces a retention has to agree about what it is enforcing. The store
// sets a TTL, the sweep names a horizon, the API answers a question about it
// and the status reports it — four readers of one decision, which were four
// decisions before this existed.
//
// Nothing here talks to a store. Resolve turns the singleton into a model, the
// model answers questions about days and horizons, and the enforcement lives
// where the class does. That keeps this importable from the API, the
// controllers and the store alike without an arrow pointing back.
package retention

import (
	"sort"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Class is one kind of thing the platform keeps. The string is the vocabulary
// the spec, the API and the status all use — a class spelled two ways is a
// retention nobody can look up.
type Class string

const (
	ClassContainerLogs Class = "containerLogs"
	ClassBuildLogs     Class = "buildLogs"
	ClassFlows         Class = "flows"
	ClassMetrics       Class = "metrics"
	ClassTraces        Class = "traces"
	ClassRequests      Class = "requests"
	ClassClusterEvents Class = "clusterEvents"
	ClassActivity      Class = "activity"
	ClassAudit         Class = "audit"
)

// Source says where a class's retention came from, which is the difference
// between a number somebody chose and a number nobody has looked at.
const (
	// SourceModel is spec.retention: somebody set this class.
	SourceModel = "retention"
	// SourceTelemetry is spec.observability.clickhouse.retentionDays, the
	// one knob every telemetry class had before this model existed.
	SourceTelemetry = "observability.clickhouse.retentionDays"
	// SourceAudit is spec.compliance.audit.retentionDays, which the audit
	// class inherits for exactly the same reason.
	SourceAudit = "compliance.audit.retentionDays"
)

// The compiled-in defaults, which match the CRD's, for a Kitchen object
// written before a field existed or created by something that did not default
// it. They are duplicated from the markers rather than derived because a
// default the API server did not apply still has to come out somewhere.
const (
	DefaultTelemetryDays int32 = 30
	DefaultAuditDays     int32 = 365
)

// AuditFloorDays is re-exported so that nothing enforcing the floor has to
// reach into the API types for the number.
const AuditFloorDays = kitchenv1alpha1.AuditFloorDays

// Definition is what is true about a class regardless of how long it is kept:
// what it holds, and whether the sweep is allowed to delete it.
type Definition struct {
	Class Class

	// Label is the class as a person reads it.
	Label string

	// Description is the one line the API and the dashboard show beside it.
	Description string

	// Sweepable is whether the retention sweep may delete this class's
	// expired data itself.
	//
	// It is false in two situations, and both are rules rather than
	// configurations. The **audit** class is not sweepable because a sweeper
	// that could delete audit rows on a schedule is a sweeper that could
	// delete the record of its own deletions; its expiry is left entirely to
	// the store's own TTL. **Container logs and build logs** are not
	// sweepable because they share one table, and the sweep's only exact
	// deletion is dropping a partition whole — which would take the
	// longer-lived class with it. The rule the two cases share is the one
	// worth remembering: *the sweep never deletes rows it cannot attribute
	// to exactly one class.*
	Sweepable bool
}

// definitions is the register, in the order a person would read it: what the
// platform collects, then what it decided, then what it can prove.
var definitions = []Definition{
	{ClassContainerLogs, "Container logs",
		"stdout and stderr from application, platform and cluster containers", false},
	{ClassBuildLogs, "Build logs",
		"the output of every build, read beside an artifact's provenance", false},
	{ClassFlows, "Network flows",
		"observed connections between workloads — the traffic view", true},
	{ClassMetrics, "Metrics",
		"metric series and their rollups", true},
	{ClassTraces, "Traces",
		"spans applications exported, and the trace lookup", true},
	{ClassRequests, "Requests",
		"HTTP request telemetry; raw rows live a week or this window, whichever is shorter", true},
	{ClassClusterEvents, "Cluster events",
		"the cluster's Warning history, which the API server itself keeps for about an hour", true},
	{ClassActivity, "Activity",
		"the dashboard's activity feed — prose for a person catching up, not evidence", true},
	{ClassAudit, "Audit log",
		"state transitions and the policy decisions they gate; the evidence an incident is reconstructed from", false},
}

// Definitions is the register in reading order.
func Definitions() []Definition {
	out := make([]Definition, len(definitions))
	copy(out, definitions)
	return out
}

// DefinitionFor answers what a class is, and whether it is one at all.
func DefinitionFor(class Class) (Definition, bool) {
	for _, definition := range definitions {
		if definition.Class == class {
			return definition, true
		}
	}
	return Definition{}, false
}

// Setting is one class's resolved retention: the number in force and where it
// came from.
type Setting struct {
	Days   int32
	Source string
}

// Model is the whole resolved answer. It is a value, not a view onto the
// singleton: everything enforcing a retention resolves once and then works
// from the same numbers, so a reconcile cannot half-apply a change made
// underneath it.
type Model struct {
	settings map[Class]Setting

	// AuditFloorOverride is the written decision behind an audit retention
	// under the floor, or nil.
	AuditFloorOverride *kitchenv1alpha1.RetentionOverrideSpec
}

// Resolve reads the singleton into a model, applying the inheritance that
// keeps an installation predating spec.retention reading exactly as it did.
func Resolve(kitchen *kitchenv1alpha1.Kitchen) Model {
	telemetry := DefaultTelemetryDays
	audit := DefaultAuditDays
	spec := kitchenv1alpha1.RetentionSpec{}
	var override *kitchenv1alpha1.RetentionOverrideSpec

	if kitchen != nil {
		if configured := kitchen.Spec.Observability.ClickHouse.RetentionDays; configured >= 1 {
			telemetry = configured
		}
		if configured := kitchen.Spec.Compliance.Audit.RetentionDays; configured >= 1 {
			audit = configured
		}
		spec = kitchen.Spec.Retention
		override = spec.AuditFloorOverride
	}

	model := Model{
		settings:           make(map[Class]Setting, len(definitions)),
		AuditFloorOverride: override,
	}
	for _, definition := range definitions {
		inherited, source := telemetry, SourceTelemetry
		if definition.Class == ClassAudit {
			inherited, source = audit, SourceAudit
		}
		if configured := configuredFor(spec, definition.Class); configured != nil && *configured >= 1 {
			model.settings[definition.Class] = Setting{Days: *configured, Source: SourceModel}
			continue
		}
		model.settings[definition.Class] = Setting{Days: inherited, Source: source}
	}
	return model
}

// configuredFor is the one place a class maps to its spec field. It is a
// switch rather than reflection so that adding a class without wiring it does
// not compile into a class that silently always inherits.
func configuredFor(spec kitchenv1alpha1.RetentionSpec, class Class) *int32 {
	switch class {
	case ClassContainerLogs:
		return spec.ContainerLogs
	case ClassBuildLogs:
		return spec.BuildLogs
	case ClassFlows:
		return spec.Flows
	case ClassMetrics:
		return spec.Metrics
	case ClassTraces:
		return spec.Traces
	case ClassRequests:
		return spec.Requests
	case ClassClusterEvents:
		return spec.ClusterEvents
	case ClassActivity:
		return spec.Activity
	case ClassAudit:
		return spec.Audit
	default:
		return nil
	}
}

// Uniform is the model an installation had before there was a model: one
// number for every class. It is what a caller uses when it genuinely means
// "all of it, this long" — a test, or a store call that is about the mechanism
// rather than about the policy — and it is deliberately not what Resolve
// produces, because the whole point of the block is that the numbers can
// differ.
func Uniform(days int32) Model {
	model := Model{settings: make(map[Class]Setting, len(definitions))}
	for _, definition := range definitions {
		model.settings[definition.Class] = Setting{Days: days, Source: SourceModel}
	}
	return model
}

// Days is the retention in force for a class. An unknown class answers 0,
// which every caller treats as "do not enforce anything" rather than as "keep
// nothing" — a typo must not become a deletion.
func (m Model) Days(class Class) int32 {
	return m.settings[class].Days
}

// Source is where that number came from.
func (m Model) Source(class Class) string {
	return m.settings[class].Source
}

// Setting is both at once, and whether the class exists.
func (m Model) Setting(class Class) (Setting, bool) {
	setting, ok := m.settings[class]
	return setting, ok
}

// Horizon is the instant a class's retention cuts at: anything stamped before
// it is past its date. Computed from a passed-in `now` rather than from the
// wall clock, because a sweep that read the clock twice could report a horizon
// it did not enforce.
func (m Model) Horizon(class Class, now time.Time) time.Time {
	days := m.Days(class)
	if days < 1 {
		return time.Time{}
	}
	return now.UTC().AddDate(0, 0, -int(days))
}

// AuditBelowFloor is whether audit records are kept for less than the floor.
func (m Model) AuditBelowFloor() bool {
	return m.Days(ClassAudit) < AuditFloorDays
}

// Classes is every class in reading order, which is the order the status and
// the API answer in.
func (m Model) Classes() []Class {
	classes := make([]Class, 0, len(definitions))
	for _, definition := range definitions {
		classes = append(classes, definition.Class)
	}
	return classes
}

// Sorted is every class in name order, for a caller that wants a stable
// alphabetical answer rather than the reading one — the singleton's status is
// a map list and reads better sorted.
func Sorted(classes []Class) []Class {
	out := make([]Class, len(classes))
	copy(out, classes)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// ValidateAudit is the floor, enforced where a CEL rule cannot reach: the API
// applies it to a request body before the object exists in the form admission
// would judge, so that the refusal names the field and the way past it rather
// than coming back as a webhook message.
//
// It returns the empty string when the setting is acceptable.
func ValidateAudit(days int32, override *kitchenv1alpha1.RetentionOverrideSpec) string {
	if days >= AuditFloorDays || override != nil {
		return ""
	}
	return "audit retention cannot be set below the 90-day floor without an explicit override: " +
		"an incident reporting duty runs from when an institution became aware, which can be long after " +
		"the transition that caused it, and a log that has already aged out cannot substantiate the report. " +
		"Raise it to at least 90, or send auditFloorOverride with a reason and an approver — " +
		"the override is itself an audit record"
}
