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

// SavedQuerySpec is a question about the logs that was worth keeping.
//
// The observability view already puts its whole selection in the URL, so any
// question is a link. What a link cannot do is be found again by someone who
// did not receive it — "the one that shows the checkout 500s" lives in a
// person's history or nowhere. This is that question with a name on it,
// shared by everyone on the platform.
//
// A saved query without an alert has no reconciler and needs none: unlike a
// Domain or a ResourceClaim, which do nothing until something acts on them, a
// saved query has its whole effect by existing, and reading it back is the
// feature. An alert is the exception, and the only one — it is a standing
// question that something has to actually ask, on a schedule, which is what
// SavedQueryReconciler does.
type SavedQuerySpec struct {
	// Title is what the query is called in the dashboard. It is free text,
	// unlike the object's name, which has to be a DNS label.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=120
	Title string `json:"title"`

	// Description says what the query is for, when the title cannot.
	// +kubebuilder:validation:MaxLength=500
	// +optional
	Description string `json:"description,omitempty"`

	// Query is Kitchen's log query language: `level:error service:shop`.
	// +kubebuilder:validation:MaxLength=2000
	// +optional
	Query string `json:"query,omitempty"`

	// Where was a ClickHouse expression, the escape hatch the query bar could
	// be switched into. **It is no longer evaluated, and no longer accepted**
	// (issue #421): it was composed into the statement as written, bounded
	// only by a conjunct, so it could read the whole telemetry store rather
	// than the projects its author could see.
	//
	// The field stays on the type rather than being removed so that a query
	// saved before that change keeps saying what it was, which is what lets
	// the API and the dashboard tell whoever finds it why it no longer runs
	// and what to write instead. Dropping the field would instead widen such
	// a query silently — its remaining half selects everything the `where`
	// used to narrow.
	// +kubebuilder:validation:MaxLength=2000
	// +optional
	Where string `json:"where,omitempty"`

	// Scope is what an alert on this query may count: the projects whoever
	// saved it could see, recorded at that moment.
	//
	// It is recorded rather than resolved at evaluation time because the only
	// identity a saved query carries is `savedBy`, which is a byline — an
	// address that changes when the account's does, and one nothing verified
	// as belonging to a `sub`. Reconstructing a role from it would be a role
	// nobody granted; a scope written down when a token had just been checked
	// is one that was.
	//
	// A query carrying none — every query saved before this field existed —
	// is not evaluated at all, and says so in status.message. That is the
	// safe direction: an absent scope means nothing, never everything.
	// +optional
	Scope *SavedQueryScope `json:"scope,omitempty"`

	// RangeMinutes is the window the question is asked over, relative to
	// whenever it is opened — which is what makes it worth saving. Zero
	// means everything retained.
	//
	// An absolute window is deliberately not storable: "the spike on
	// Tuesday" stops being a question and becomes a screenshot, and the
	// retention deletes it out from under the name anyway.
	// +kubebuilder:validation:Minimum=0
	// +optional
	RangeMinutes int32 `json:"rangeMinutes,omitempty"`

	// Limit is how many lines the query asks for.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=5000
	// +optional
	Limit int32 `json:"limit,omitempty"`

	// View is which of the observability view's tabs the query is read in:
	// `lines` or `patterns`. A query saved because its patterns are the
	// interesting part should open on them.
	// +kubebuilder:validation:Enum=lines;patterns
	// +optional
	View string `json:"view,omitempty"`

	// IncludeCluster keeps the logs of pods Kitchen did not deploy, which
	// the view scopes out by default.
	// +optional
	IncludeCluster bool `json:"includeCluster,omitempty"`

	// SavedBy is who saved it, as the API knew them. It is recorded rather
	// than enforced: saved queries are the platform's, not a person's, and
	// this is a byline.
	// +optional
	SavedBy string `json:"savedBy,omitempty"`

	// Alert turns the question into a standing one: evaluated on a schedule
	// against a threshold over a window, and announced when it crosses.
	//
	// It is on the saved query rather than in an object of its own because
	// the query *is* the alert's definition — an alert with a query nobody
	// can open in the observability view, edit and save again is an alert
	// nobody can tune. Absent means the query is only ever asked by a person.
	// +optional
	Alert *SavedQueryAlert `json:"alert,omitempty"`
}

// SavedQueryScope is which projects a saved query's alert may count over.
//
// The two fields are not the same as each other's absence. Platform is the
// whole store, including the lines belonging to no project — Kitchen's own
// components and the rest of the cluster — and is what an operator's saved
// query records. A list of projects is what everybody else's records. Neither
// set is a scope that reads everything by default: an object carrying no scope
// at all counts nothing.
type SavedQueryScope struct {
	// Platform is every line in the store. It is recorded for a query saved
	// by an operator, who may read all of it.
	// +optional
	Platform bool `json:"platform,omitempty"`

	// Projects names the projects the query may count over.
	// +optional
	Projects []string `json:"projects,omitempty"`
}

// SavedQueryAlert is a threshold over a window, asked on a schedule.
//
// It is the second trigger onto the notification path (issue #77), and it
// deliberately adds nothing to that path: crossing the threshold records an
// `alert.firing` activity event exactly the way a reconciler records a deploy,
// and the subscriptions do the rest. What it adds in front is a schedule and a
// comparison, which is the whole difference between the two triggers.
type SavedQueryAlert struct {
	// WindowMinutes is how far back each evaluation counts. It is separate
	// from the query's own rangeMinutes, which is what a person opening the
	// query in the dashboard sees: an alert wants a short window it can
	// evaluate often, and the same query is usually worth reading over a
	// longer one.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1440
	WindowMinutes int32 `json:"windowMinutes"`

	// Threshold is the number of matching lines the count is compared
	// against.
	// +kubebuilder:validation:Minimum=0
	Threshold int64 `json:"threshold"`

	// Comparison is which side of the threshold fires. `above` is the usual
	// one — more errors than this. `below` is the heartbeat: a service that
	// logs every minute and has stopped.
	// +kubebuilder:validation:Enum=above;below
	// +kubebuilder:default=above
	// +optional
	Comparison string `json:"comparison,omitempty"`

	// IntervalMinutes is how often it is evaluated. It is a floor rather
	// than a promise: an evaluation is due after this long, and the
	// reconciler gets to it when it gets to it.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=1440
	// +optional
	IntervalMinutes int32 `json:"intervalMinutes,omitempty"`

	// Suspended stops evaluation without deleting the alert or the query.
	// +optional
	Suspended bool `json:"suspended,omitempty"`
}

// DefaultAlertIntervalMinutes is how often an alert that does not say is
// evaluated.
const DefaultAlertIntervalMinutes int32 = 5

// The two directions an alert watches, spelled once. The CRD's enum, the API's
// validation, the reconciler's message and the dashboard's wording are all
// this string, and a second spelling of it is a comparison that silently never
// fires.
const (
	AlertComparisonAbove = "above"
	AlertComparisonBelow = "below"
)

// Interval is the evaluation period this alert asked for.
func (a *SavedQueryAlert) Interval() int32 {
	if a == nil || a.IntervalMinutes <= 0 {
		return DefaultAlertIntervalMinutes
	}
	return a.IntervalMinutes
}

// Fires reports whether a count crosses the threshold in the direction this
// alert watches.
func (a *SavedQueryAlert) Fires(count int64) bool {
	if a == nil {
		return false
	}
	if a.Comparison == AlertComparisonBelow {
		return count < a.Threshold
	}
	return count > a.Threshold
}

// SavedQueryStatus is what the alert has observed. A saved query with no alert
// never has one written.
type SavedQueryStatus struct {
	// LastEvaluationTime is when the alert was last asked, and LastCount what
	// it answered. Both are on the object rather than only in the log so that
	// "is this alert working" is answerable without one.
	// +optional
	LastEvaluationTime *metav1.Time `json:"lastEvaluationTime,omitempty"`
	// +optional
	LastCount int64 `json:"lastCount,omitempty"`

	// Firing is whether the threshold is crossed right now. The notification
	// is sent on the *edge* into it — a threshold that stays crossed for an
	// afternoon is one message, not one every five minutes.
	// +optional
	Firing bool `json:"firing,omitempty"`
	// +optional
	FiringSince *metav1.Time `json:"firingSince,omitempty"`

	// Message is the last evaluation in words, including the reason an
	// evaluation could not be made — no telemetry store, a query the store
	// refused — which is otherwise invisible on an alert that has simply
	// never fired.
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`
// +kubebuilder:printcolumn:name="Query",type=string,JSONPath=`.spec.query`
// +kubebuilder:printcolumn:name="Firing",type=boolean,JSONPath=`.status.firing`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SavedQuery is the Schema for the savedqueries API.
//
// Its status is the alert's and only the alert's: a saved query nobody asked
// to be alerted on is observed by nothing and carries none.
type SavedQuery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   SavedQuerySpec   `json:"spec,omitempty"`
	Status SavedQueryStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// SavedQueryList contains a list of SavedQuery.
type SavedQueryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SavedQuery `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SavedQuery{}, &SavedQueryList{})
}
