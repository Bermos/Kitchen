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
	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/appconfig"
)

// A project's security posture, written and read back (#276).
//
// It is the health check's shape for the same reason: one declaration, two
// ways to make it — this route and a repository's kitchen.json — so there is
// one wire shape and one validator, in internal/appconfig, and the two cannot
// come to disagree about what a capability is spelled like.

// securityRequest is a posture as a PATCH carries it.
type securityRequest = appconfig.Security

// securityView is a posture read back, resolved. Like the health check it
// fills the platform's answers in rather than leaving them absent: the
// question a reader has is what the workload actually runs under, and an
// empty object answers it only for somebody who already knows the defaults.
type securityView struct {
	RunAsNonRoot bool `json:"runAsNonRoot"`
	// RunAsUser and RunAsGroup are absent when the image's own are used,
	// which is not the same as running as uid 0 and must not read like it.
	RunAsUser  int64 `json:"runAsUser,omitempty"`
	RunAsGroup int64 `json:"runAsGroup,omitempty"`
	// FSGroup and its change policy are absent when the volumes keep their
	// own ownership, on the same reading: a workload with no volume claim
	// has nothing for them to say.
	FSGroup                  int64    `json:"fsGroup,omitempty"`
	FSGroupChangePolicy      string   `json:"fsGroupChangePolicy,omitempty"`
	ReadOnlyRootFilesystem   bool     `json:"readOnlyRootFilesystem"`
	AllowPrivilegeEscalation bool     `json:"allowPrivilegeEscalation"`
	DropCapabilities         []string `json:"dropCapabilities,omitempty"`
	// SeccompProfile is the platform's and is not settable: it is the
	// container runtime's own profile, which Kubernetes does not apply
	// unless asked, and applying it costs a working image nothing. It is
	// reported so that "what is this running under" has a complete answer.
	SeccompProfile string `json:"seccompProfile"`
	// Declared is the posture in words — one phrase per constraint this
	// project asked for beyond the platform's default, empty for a project
	// that asked for none. It is the same list an environment's condition
	// names when a workload cannot start under it.
	Declared []string `json:"declared,omitempty"`
	// Overrides names the fields of this block the *workload* declared
	// itself, where the rest are the unit's (#399). It is a workload's
	// effective posture alone: a project's own posture is nobody's override,
	// and a workload's declaration is all override, so neither carries it.
	//
	// It exists because a merge makes the effective posture a computation
	// rather than a value. Without it a reader is shown one block and cannot
	// tell which of the two declarations behind it won, which is the whole
	// thing a per-workload posture is at risk of.
	Overrides []string `json:"overrides,omitempty"`
}

// securityFromRequest validates one posture. It answers nil for a posture
// that declares nothing, which is how a project takes one back off.
func securityFromRequest(request securityRequest, subject string) (*kitchenv1alpha1.SecuritySpec, error) {
	return appconfig.SecuritySpec(request, subject)
}

// newWorkloadSecurityView is the posture one workload runs under: the unit's
// with the workload's own merged over it, and the names of the fields that
// came from the workload (#399).
func newWorkloadSecurityView(
	unit, workload *kitchenv1alpha1.SecuritySpec,
) *securityView {
	view := newSecurityView(kitchenv1alpha1.ResolveSecurity(unit, workload))
	view.Overrides = kitchenv1alpha1.SecurityOverrides(workload)
	return view
}

// newSecurityView reports the posture a project's workloads run under. A
// project that declared none still runs under one, and this is what it is.
func newSecurityView(security *kitchenv1alpha1.SecuritySpec) *securityView {
	view := &securityView{
		AllowPrivilegeEscalation: security.EscalationAllowed(),
		SeccompProfile:           string(kitchenv1alpha1.SeccompProfileRuntimeDefault),
		Declared:                 security.Declared(),
	}
	if security == nil {
		return view
	}
	view.RunAsNonRoot = security.RunAsNonRoot
	view.RunAsUser = security.RunAsUser
	view.RunAsGroup = security.RunAsGroup
	view.FSGroup = security.FSGroup
	view.FSGroupChangePolicy = string(security.FSGroupChangePolicy)
	view.ReadOnlyRootFilesystem = security.ReadOnlyRootFilesystem
	view.DropCapabilities = security.DropCapabilities
	return view
}
