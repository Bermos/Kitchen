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
// It has no reconciler and needs none: unlike a Domain or a ResourceClaim,
// which do nothing until something acts on them, a saved query has its whole
// effect by existing. Reading it back is the feature.
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

	// Where is a ClickHouse expression, the escape hatch the query bar can
	// be switched into. A saved query may carry either or both, exactly as
	// the view composes them.
	// +kubebuilder:validation:MaxLength=2000
	// +optional
	Where string `json:"where,omitempty"`

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
}

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Title",type=string,JSONPath=`.spec.title`
// +kubebuilder:printcolumn:name="Query",type=string,JSONPath=`.spec.query`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SavedQuery is the Schema for the savedqueries API.
//
// It has no status: there is no observed state, because nothing observes it.
type SavedQuery struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec SavedQuerySpec `json:"spec,omitempty"`
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
