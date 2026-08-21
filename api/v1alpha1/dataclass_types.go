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

// DataClass is the sensitivity classification of data, in the order the
// platform compares it: public < internal < confidential <
// strictlyConfidential.
//
// It is a schema field rather than documentation because a regulator expects
// the classification of data to be *enforced*, not described: a claim's class
// is checked against its project's at the API, and a project's against an
// environment's at promotion, through the policy engine. Absence is a state
// of its own — unclassified — surfaced as such everywhere and never defaulted
// to anything, because a silent default is a classification nobody made.
// +kubebuilder:validation:Enum=public;internal;confidential;strictlyConfidential
type DataClass string

const (
	DataClassPublic               DataClass = "public"
	DataClassInternal             DataClass = "internal"
	DataClassConfidential         DataClass = "confidential"
	DataClassStrictlyConfidential DataClass = "strictlyConfidential"
)

// dataClassRank is the one ordering every comparison reads. The policy
// bundle's class_rank map must agree with it — the rego rule is the same
// comparison made where promotion decisions are made.
var dataClassRank = map[DataClass]int{
	DataClassPublic:               0,
	DataClassInternal:             1,
	DataClassConfidential:         2,
	DataClassStrictlyConfidential: 3,
}

// Rank is the class's position in the ordering, and -1 for anything that is
// not a class — the empty string above all, which is how "unclassified" stays
// distinguishable from "public" in every comparison.
func (c DataClass) Rank() int {
	rank, ok := dataClassRank[c]
	if !ok {
		return -1
	}
	return rank
}

// Classified reports whether a class was actually declared.
func (c DataClass) Classified() bool {
	return c.Rank() >= 0
}

// Exceeds reports whether data of this class may not be held by something
// rated `other`: true when this class outranks it, and true when this class
// is declared but `other` is not — classified data has no business in a
// container nobody has rated. Two unclassified sides exceed nothing.
func (c DataClass) Exceeds(other DataClass) bool {
	if !c.Classified() {
		return false
	}
	if !other.Classified() {
		return true
	}
	return c.Rank() > other.Rank()
}

// AtMost is the narrowable-never-wideable check spelled the way the rule
// reads: this class fits within `other`.
func (c DataClass) AtMost(other DataClass) bool {
	return !c.Exceeds(other)
}

// DataClasses lists the classes in ascending order, for refusals that name
// the vocabulary.
func DataClasses() []DataClass {
	return []DataClass{
		DataClassPublic, DataClassInternal, DataClassConfidential, DataClassStrictlyConfidential,
	}
}
