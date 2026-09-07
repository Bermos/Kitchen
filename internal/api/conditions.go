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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// Whether a condition is a fault is not in its status.
//
// A condition's `status` says whether the statement in its `type` holds, and
// nothing about whether the world is well: `Previews=False` with reason
// `Disabled` means previews are turned off, which is what somebody asked for.
// The dashboard read every `False` as broken and every `Unknown` as a caution,
// so a project with previews switched off was drawn with a red dot, a red
// condition line and a place in the overview's attention band — spending the
// one surface that exists to put the worst problem where the eye lands on two
// projects where nothing was wrong (#436).
//
// The distinction cannot be made from `status`, and it is not the dashboard's
// to make: it belongs to whoever wrote the condition. `metav1.Condition` has
// no polarity field, so the API — which already reshapes conditions for its
// clients — attaches one here, and every client reads it rather than keeping a
// list of benign reasons of its own that would drift from the operator the
// first time somebody added one.
type conditionSeverity string

const (
	// severityError is something that is wrong and will stay wrong until
	// somebody acts.
	severityError conditionSeverity = "error"
	// severityWarning is something that may be wrong: unassessed, or on its
	// way somewhere.
	severityWarning conditionSeverity = "warning"
	// severityInfo is a statement worth reading that is not a fault — a
	// setting somebody chose, or a fact about what cannot be known.
	severityInfo conditionSeverity = "info"
	// severityNone is a condition with nothing to say: the statement in its
	// type holds.
	severityNone conditionSeverity = "none"
)

// conditionStatement is one condition type and one reason for it — the pair
// that decides a severity, since a reason is only meaningful about the type
// that carries it.
type conditionStatement struct {
	Type   string
	Reason string
}

// conditionSeverities is the whole of what the API knows that the status does
// not say.
//
// It is keyed on the operator's own exported constants rather than on strings,
// so a reason that is renamed or removed breaks this table at compile time
// instead of quietly falling back to the default. Everything absent from it
// takes the default in conditionSeverityOf: a reason nobody classified is
// treated as a fault, which is the safe direction to be wrong in.
//
// `TestEveryExportedReasonIsClassified` holds the other half of the bargain:
// every `Reason…` constant internal/controller exports has to appear here, so
// classifying a new benign reason is a step somebody is made to take rather
// than one they are trusted to remember.
var conditionSeverities = map[conditionStatement]conditionSeverity{
	// Previews are off, or cannot exist at all. Both are descriptions of a
	// project, not faults of one.
	{controller.ConditionPreviews, controller.ReasonPreviewsDisabled}: severityInfo,
	{controller.ConditionPreviews, controller.ReasonNoRepository}:     severityInfo,
	// A self-hosted Inngest publishes no app inventory, and the condition
	// says so. Unknown here is an answer, not an unfinished check.
	{controller.ConditionAppConnected, controller.ReasonNotReported}: severityInfo,
	// A serve binding registers the web process and nothing else, and the
	// unit runs more than that. It is a limit of the mode somebody chose
	// rather than a broken claim — the claim is bound and everything it
	// promised is there — but it is the reason functions nobody can find are
	// missing, so it is a caution rather than a fact filed away.
	{controller.ConditionServeCoverage, controller.ReasonServeCoversWebOnly}: severityWarning,
	// A preview nobody asked to gate is open on purpose.
	{controller.ConditionPreviewProtected, controller.ReasonPreviewPublic}: severityInfo,
	// A project that asked not to be published has no route and never will,
	// and cannot idle because nothing routes to it that could wake it. Both
	// are `spec.exposure: internal` doing exactly what it says — an internal
	// project drawn with a red dot for having no URL is the thing #436 is
	// about, one setting further along.
	{controller.ConditionRouteProgrammed, controller.ReasonInternalProject}: severityInfo,
	{controller.ConditionScaleToZero, controller.ReasonInternalProject}:     severityInfo,
	// A store left unencrypted is a choice this platform reports. The
	// component survey already reads it that way (internalTLSComponent).
	{controller.ConditionInternalCAReady, controller.ReasonStoreInTheClear}: severityInfo,
	// A dependency this installation did not ask for is not a failed install.
	{kitchenv1alpha1.AddonReady, controller.ReasonAddonNotInstalled}: severityInfo,

	// And the counter-case, spelled out rather than left to the default: an
	// installation with no scheduled backup is unprotected, which is worth
	// somebody's attention however green everything else is. "Not every
	// False is a fault" is not "no False is".
	{controller.ConditionBackupReady, controller.ReasonNotScheduled}: severityError,
}

// conditionSeverityOf is how much attention this condition deserves.
//
// The table decides where it has an opinion; otherwise the status does, on the
// reading the dashboard used to apply to everything: a statement that does not
// hold is a fault, one nobody could assess is a caution, and one that holds
// has nothing to say.
func conditionSeverityOf(condition metav1.Condition) conditionSeverity {
	if severity, ok := conditionSeverities[conditionStatement{Type: condition.Type, Reason: condition.Reason}]; ok {
		return severity
	}
	switch condition.Status {
	case metav1.ConditionTrue:
		return severityNone
	case metav1.ConditionUnknown:
		return severityWarning
	default:
		return severityError
	}
}
