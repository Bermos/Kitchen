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

package signals

import (
	"fmt"
	"strings"
	"time"
)

// Audience is who a signal is for.
type Audience string

const (
	// AudienceOperator is the platform's own screens only.
	AudienceOperator Audience = "operator"
	// AudienceDeveloper is additive: a developer signal also appears on the
	// operator's problems list, because a project failing is a thing the
	// operator wants to know. Nothing is developer-only.
	AudienceDeveloper Audience = "developer"
)

// Input names one thing the rules read. It is the unit availability is tracked
// in: the snapshot records, per input, whether it was read, and the registry
// turns a missing input into "this rule could not be evaluated" rather than
// into silence.
//
// The names are the source as the design names it — a table, a Kubernetes kind,
// a derived reading — so that a finding saying "could not read http_requests_1m"
// points at something a person can go and look at.
type Input string

const (
	// From the API server, through the operator's cache.
	InputPods         Input = "pods"
	InputWorkloads    Input = "workloads"
	InputNodes        Input = "nodes"
	InputClaims       Input = "persistentvolumeclaims"
	InputGateways     Input = "gateways"
	InputRoutes       Input = "httproutes"
	InputCertificates Input = "certificates"
	InputEnvironments Input = "environments"
	InputBuilds       Input = "builds"
	InputKitchen      Input = "kitchen"

	// From the telemetry store.
	InputClusterEvents Input = "k8s_events"
	// InputRawRequests is the request rows themselves and InputRequests the
	// minute rollup over them. They are two inputs rather than one because they
	// fail separately and are read by different rules: only the unrouted bucket
	// groups over the raw table, and marking the rollup for its failure would
	// both name a table the failing query never touched and darken the six
	// rules that do read the rollup.
	InputRawRequests Input = "http_requests"
	InputRequests    Input = "http_requests_1m"
	InputResources   Input = "metrics_5m"
	InputHostMetrics Input = "host_metrics"
	InputVolumeStats Input = "kubelet_volume_stats"
	InputFreshness   Input = "telemetry_freshness"
	InputStore       Input = "clickhouse_system"

	// Derived by the operator itself.
	InputDNS    Input = "dns"
	InputIngest Input = "ingest_accounting"
)

// Signal is one rule of the catalogue.
type Signal struct {
	// ID names the rule and prefixes every fingerprint it produces.
	ID ID

	// Version is the rule's own. It moves when what the rule *means* changes —
	// a new threshold, a different condition — so that a transition log can say
	// why a finding that never resolved suddenly did. It does not move for a
	// reworded title.
	Version int

	// Audience decides whether the rule also reaches the environment page's
	// diagnostics strip.
	Audience Audience

	// Summary is one line of what the rule fires on, for the catalogue listing
	// and for explaining a finding whose title is necessarily terse.
	Summary string

	// Requires names the inputs without which the rule cannot answer at all.
	// The registry checks them before calling Evaluate, so no rule has to
	// write its own "I could not read this" path — see [Registry.Evaluate].
	//
	// An input a rule merely *prefers* does not belong here: the rule reads it
	// through [Snapshot.Available] and says in its detail that the number is
	// missing. workload.crashloop is the example — it fires from pod status
	// alone and is only richer with the restart counts beside it.
	Requires []Input

	// Evaluate is the rule. It is pure: no I/O, no state, no clock beyond
	// [Snapshot.Now].
	Evaluate func(*Snapshot) []Finding
}

// fire builds one finding. Every rule goes through it, so that no two rules
// can spell a fingerprint differently or forget to bound a detail.
func fire(id ID, severity Severity, scope Scope, since time.Time, title, detail, evidence string) Finding {
	return Finding{
		Signal:      id,
		Severity:    severity,
		Scope:       scope,
		Fingerprint: Fingerprint(id, scope),
		Title:       title,
		Detail:      truncate(detail),
		Since:       since,
		Evidence:    evidence,
	}
}

// truncate bounds one explanation so that a pathological Kubernetes message
// cannot dominate a screen. The reason is always at the front of these — a
// scheduler's complaint, a container's exit, an ACME error — so cutting the
// tail loses nothing that matters. It mirrors the component survey's own
// bound, for the same reason.
func truncate(detail string) string {
	detail = strings.TrimSpace(detail)
	if len(detail) > MaxDetailLength {
		return detail[:MaxDetailLength] + "…"
	}
	return detail
}

// validate rejects a catalogue entry that cannot work, at construction rather
// than at the first evaluation that needed it.
func (s Signal) validate() error {
	switch {
	case s.ID == "":
		return fmt.Errorf("a signal must have an id")
	case s.Version < 1:
		return fmt.Errorf("signal %q must carry a version", s.ID)
	case s.Summary == "":
		return fmt.Errorf("signal %q must summarise what it fires on", s.ID)
	case s.Evaluate == nil:
		return fmt.Errorf("signal %q has no rule", s.ID)
	case s.Audience != AudienceOperator && s.Audience != AudienceDeveloper:
		return fmt.Errorf("signal %q has an unknown audience %q", s.ID, s.Audience)
	}
	return nil
}
