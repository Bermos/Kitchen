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
	"fmt"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// A workload's health check, written and read back (#236).
//
// It is the same shape on a project's web process and on one of its workers,
// because it is the same declaration: what the platform asks, how often, and
// how many refusals in a row mean the container is not working. The one
// difference is the port — a worker publishes none, so it has to say which
// one its health listener is on, and that is refused here as well as at
// admission so the caller gets a sentence rather than a CEL rule.

// healthRequest is a health check as a PATCH carries it. Sending `{}` is how
// a declared check is taken back off: an empty check is exactly the default —
// a TCP connect to the container's port on the platform's timings.
type healthRequest struct {
	// Path is the HTTP path the probe asks for. Empty makes it a TCP
	// connect, which is deliberately not `GET /`.
	Path string `json:"path,omitempty"`
	// Port the probe is made against; empty takes the container's own. A
	// process has none, so a process's check must name one.
	Port int32 `json:"port,omitempty"`
	// The timings. Zero takes the platform's default for each.
	PeriodSeconds           int32 `json:"periodSeconds,omitempty"`
	TimeoutSeconds          int32 `json:"timeoutSeconds,omitempty"`
	FailureThreshold        int32 `json:"failureThreshold,omitempty"`
	StartupFailureThreshold int32 `json:"startupFailureThreshold,omitempty"`
}

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
	health := &kitchenv1alpha1.HealthSpec{
		Path:                    strings.TrimSpace(request.Path),
		Port:                    request.Port,
		PeriodSeconds:           request.PeriodSeconds,
		TimeoutSeconds:          request.TimeoutSeconds,
		FailureThreshold:        request.FailureThreshold,
		StartupFailureThreshold: request.StartupFailureThreshold,
	}
	if health.Path != "" && !strings.HasPrefix(health.Path, "/") {
		return nil, fmt.Errorf("%s: path must start with / (got %q)", subject, request.Path)
	}
	if health.Port < 0 || health.Port > 65535 {
		return nil, fmt.Errorf(
			"%s: port must be between 1 and 65535, or 0 to probe the port the application is published on (got %d)",
			subject, health.Port)
	}
	if needsPort && health.Port == 0 {
		return nil, fmt.Errorf(
			"%s: name the port the check is made against — a process publishes no port of its own", subject)
	}
	for _, timing := range []struct {
		name  string
		value int32
	}{
		{"periodSeconds", health.PeriodSeconds},
		{"timeoutSeconds", health.TimeoutSeconds},
		{"failureThreshold", health.FailureThreshold},
		{"startupFailureThreshold", health.StartupFailureThreshold},
	} {
		if timing.value < 0 {
			return nil, fmt.Errorf("%s: %s cannot be negative, and 0 takes the platform's default (got %d)",
				subject, timing.name, timing.value)
		}
	}
	return health, nil
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
