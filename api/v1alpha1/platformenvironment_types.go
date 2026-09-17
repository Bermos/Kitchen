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

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// PlatformEnvironment is an instance-wide policy environment ("production",
// "staging", ...): who owns it, what it demands, and what it serves.
//
// Runtime Environments remain per-project and bind to one of these.
type PlatformEnvironmentSpec struct {
	// Owners name who may change this policy environment's governance
	// declaration. Platform operators always may.
	// +optional
	Owners []string `json:"owners,omitempty"`

	// Serves is who may bind to offerings served from runtime environments that
	// bind to this policy environment.
	// +optional
	Serves *EnvironmentServes `json:"serves,omitempty"`

	// Requirements is the policy bundle and parameters runtime environments
	// bound to this policy environment are judged against.
	// +optional
	Requirements *EnvironmentRequirements `json:"requirements,omitempty"`

	// DataClass is the highest sensitivity class this policy environment admits.
	// +optional
	DataClass DataClass `json:"dataClass,omitempty"`

	// Residency declares where this policy environment's data is located.
	// +optional
	Residency string `json:"residency,omitempty"`

	// Criticality and RTO/RPO are the continuity designation for this policy
	// environment.
	// +optional
	Criticality Criticality `json:"criticality,omitempty"`
	// +optional
	RTO Tolerance `json:"rto,omitempty"`
	// +optional
	RPO Tolerance `json:"rpo,omitempty"`
}

type PlatformEnvironmentStatus struct {
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="DataClass",type=string,JSONPath=`.spec.dataClass`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type PlatformEnvironment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformEnvironmentSpec   `json:"spec,omitempty"`
	Status PlatformEnvironmentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type PlatformEnvironmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlatformEnvironment `json:"items"`
}

func (e *PlatformEnvironment) ServesConsumers() []EnvironmentType {
	if e == nil || e.Spec.Serves == nil {
		return nil
	}
	var out []EnvironmentType
	for _, class := range EnvironmentTypes() {
		for _, admitted := range e.Spec.Serves.Consumers {
			if admitted == class {
				out = append(out, class)
				break
			}
		}
	}
	return out
}

func init() {
	SchemeBuilder.Register(&PlatformEnvironment{}, &PlatformEnvironmentList{})
}
