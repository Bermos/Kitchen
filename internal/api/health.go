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

// A workload's health check, written and read back (#236).
//
// It is the same shape on a project's web process and on one of its workers,
// because it is the same declaration: what the platform asks, how often, and
// how many refusals in a row mean the container is not working. The one
// difference is the port — a worker publishes none, so it has to say which
// one its health listener is on, and that is refused here as well as at
// admission so the caller gets a sentence rather than a CEL rule.

// healthRequest is a health check as a PATCH carries it, defined in
// internal/appconfig alongside the process it also belongs to: the REST API
// and a repository's kitchen.json describe one check, so they share one
// shape and one validator.
type healthRequest = appconfig.Health

// healthView is a health check read back, with every timing resolved. The
// defaults are filled in rather than left absent because the question a
// reader has is "what is actually checked, how often", and an empty field
// answers it only for somebody who already knows the defaults.
type healthView struct {
	Path                    string `json:"path,omitempty"`
	Port                    int32  `json:"port,omitempty"`
	PeriodSeconds           int32  `json:"periodSeconds"`
	TimeoutSeconds          int32  `json:"timeoutSeconds"`
	FailureThreshold        int32  `json:"failureThreshold"`
	StartupFailureThreshold int32  `json:"startupFailureThreshold"`
}

// healthFromRequest validates one health check. subject names what it belongs
// to in a refusal — "the health check", or a process by name — and needsPort
// is set for a workload that publishes no port of its own.
func healthFromRequest(request healthRequest, subject string, needsPort bool) (*kitchenv1alpha1.HealthSpec, error) {
	return appconfig.HealthSpec(request, subject, needsPort)
}

// newHealthView reports a health check with its timings resolved. A workload
// that declared none still gets one: every environment is probed, and what it
// is probed with is the answer either way.
func newHealthView(health *kitchenv1alpha1.HealthSpec) *healthView {
	return &healthView{
		Path:                    health.HTTPPath(),
		Port:                    health.ProbePort(0),
		PeriodSeconds:           health.Period(),
		TimeoutSeconds:          health.Timeout(),
		FailureThreshold:        health.Failures(),
		StartupFailureThreshold: health.StartupFailures(),
	}
}
