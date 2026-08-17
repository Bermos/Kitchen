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
	"net/url"
	"strings"
)

// Where a finding sends the reader.
//
// A finding without a link is a sentence about a number nobody can check, and
// the design is explicit that every problem carries its evidence. So the link
// points at the screen that *shows the numbers behind this finding* — the
// requests charts for an error rate, the events explorer filtered to the
// object for an admission refusal — rather than at whichever screen the
// finding happens to be rendered on.
//
// The paths are the dashboard's own routes, relative because the dashboard and
// the API answering with them are the same origin. The platform ones are the
// section docs/OBSERVABILITY.md §6.2 adds; naming them here rather than in each
// rule means the day a route is renamed there is one file to fix.
const (
	EvidencePlatform          = "/platform"
	EvidencePlatformNodes     = "/platform/nodes"
	EvidencePlatformWorkloads = "/platform/workloads"
	EvidencePlatformEdge      = "/platform/edge"
	EvidencePlatformStorage   = "/platform/storage"
	EvidencePlatformEvents    = "/platform/events"
	EvidenceBuilds            = "/builds"
)

// The named sections of the environment page, so that a finding lands the
// reader on the part of it that shows the evidence rather than at the top.
const (
	sectionRequests  = "requests"
	sectionResources = "resources"
	sectionWorkload  = "workload"
)

// environmentEvidence links at one environment's page, optionally at a section
// of it.
func environmentEvidence(environment, section string) string {
	link := "/environments/" + url.PathEscape(environment)
	if section != "" {
		link += "?section=" + url.QueryEscape(section)
	}
	return link
}

// projectEvidence links at one project's page.
func projectEvidence(project string) string {
	return "/projects/" + url.PathEscape(project)
}

// buildEvidence links at one build.
func buildEvidence(build string) string {
	return EvidenceBuilds + "/" + url.PathEscape(build)
}

// nodeEvidence links at one node on the platform's Nodes screen, which is
// where its conditions, its saturation series and its telemetry freshness are.
func nodeEvidence(node string) string {
	return EvidencePlatformNodes + "?node=" + url.QueryEscape(node)
}

// eventsEvidence links at the events explorer, filtered to one object. This is
// the link that matters most on the rules that read k8s_events: the finding
// quotes one message, and the explorer holds the rest of them.
func eventsEvidence(namespace, kind, name string) string {
	query := url.Values{}
	for key, value := range map[string]string{"namespace": namespace, "kind": kind, "name": name} {
		if value != "" {
			query.Set(key, value)
		}
	}
	if len(query) == 0 {
		return EvidencePlatformEvents
	}
	return EvidencePlatformEvents + "?" + query.Encode()
}

// workloadEvidence links at one workload on the platform's Workloads screen —
// used for the platform's own workloads, which belong to no environment page.
func workloadEvidence(namespace, name string) string {
	query := url.Values{"namespace": {namespace}, "name": {name}}
	return EvidencePlatformWorkloads + "?" + query.Encode()
}

// claimEvidence links at one claim on the platform's Storage screen.
func claimEvidence(namespace, claim string) string {
	query := url.Values{"namespace": {namespace}, "claim": {claim}}
	return EvidencePlatformStorage + "?" + query.Encode()
}

// hostEvidence links at one hostname on the platform's Edge screen, which
// holds the certificate table and the unrouted-host bucket both.
func hostEvidence(host string) string {
	return EvidencePlatformEdge + "?host=" + url.QueryEscape(strings.TrimSpace(host))
}
