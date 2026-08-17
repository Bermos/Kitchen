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

package main

import (
	"strings"
	"testing"
	"time"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	observerpb "github.com/cilium/cilium/api/v1/observer"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const probeURL = "http://echo.stage0.example.com/stage0/"

// response builds the flow Cilium is expected to produce for one HTTP
// transaction, so the reader's judgement can be exercised without a cluster —
// which is the only place the real ones come from.
func response(kind flowpb.L7FlowType, label string, code uint32, latency uint64) *observerpb.GetFlowsResponse {
	flow := &flowpb.Flow{
		Time:     timestamppb.New(time.Unix(1700000000, 0)),
		Verdict:  flowpb.Verdict_FORWARDED,
		NodeName: "kind-control-plane",
		Source:   &flowpb.Endpoint{Labels: []string{"reserved:ingress"}},
		Destination: &flowpb.Endpoint{
			Namespace: "kitchen-stage0",
			PodName:   "echo-0",
		},
		L7: &flowpb.Layer7{
			Type:      kind,
			LatencyNs: latency,
			Record: &flowpb.Layer7_Http{Http: &flowpb.HTTP{
				Code:     code,
				Method:   "GET",
				Url:      probeURL + label,
				Protocol: "HTTP/1.1",
			}},
		},
	}
	return &observerpb.GetFlowsResponse{
		ResponseTypes: &observerpb.GetFlowsResponse_Flow{Flow: flow},
	}
}

func absorbAll(responses ...*observerpb.GetFlowsResponse) *observations {
	seen := &observations{lost: map[string]uint64{}}
	for _, response := range responses {
		seen.absorb(response, "/stage0/")
	}
	return seen
}

func TestAssertionHoldsOnACompleteResponseFlow(t *testing.T) {
	seen := absorbAll(
		response(flowpb.L7FlowType_REQUEST, labelOK, 0, 0),
		response(flowpb.L7FlowType_RESPONSE, labelOK, 200, 1_830_000),
	)
	if seen.l7 != 2 || len(seen.probes) != 2 {
		t.Fatalf("expected both flows kept, got l7=%d probes=%d", seen.l7, len(seen.probes))
	}

	proof, missing := assertVantagePoint(seen)
	if len(missing) != 0 {
		t.Fatalf("expected the assertion to hold, missing: %v", missing)
	}
	if proof.label != labelOK || proof.kind != responseType || proof.latencyNS == 0 {
		t.Fatalf("wrong flow proved it: %+v", proof)
	}
	if proof.source != "reserved:ingress" {
		t.Errorf("expected the ingress identity as the source, got %q", proof.source)
	}
	if proof.destination != "kitchen-stage0/echo-0" {
		t.Errorf("expected the backend as the destination, got %q", proof.destination)
	}
}

// A flow that arrives with no latency is the failure the design most needs to
// hear about: without it there is no p95, and §3.1b's fallback is the answer.
func TestAssertionNamesTheMissingField(t *testing.T) {
	seen := absorbAll(response(flowpb.L7FlowType_RESPONSE, labelOK, 200, 0))
	_, missing := assertVantagePoint(seen)
	if len(missing) != 1 || !strings.Contains(missing[0], "latency_ns") {
		t.Fatalf("expected the missing latency to be named, got %v", missing)
	}
}

// The request half alone must not satisfy it: a status and a duration only
// exist on the response.
func TestAssertionIgnoresRequestFlows(t *testing.T) {
	seen := absorbAll(response(flowpb.L7FlowType_REQUEST, labelOK, 0, 0))
	_, missing := assertVantagePoint(seen)
	if len(missing) == 0 {
		t.Fatal("expected the assertion to fail with only a request flow")
	}
	if !strings.Contains(strings.Join(missing, " "), "reached Relay") {
		t.Fatalf("expected the absence to be reported, got %v", missing)
	}
}

func TestReportFailsClosedWithNoFlows(t *testing.T) {
	sections, held := report(absorbAll(), options{match: "/stage0/", window: time.Second})
	if held {
		t.Fatal("expected an empty stream to fail the assertion")
	}
	if len(sections) != 7 {
		t.Fatalf("expected every section to be rendered, got %d", len(sections))
	}
	for _, section := range sections {
		if section.title == "" {
			t.Error("a section has no title")
		}
	}
}

// Observe mode is what the harness dumps recent flows with when something else
// has already failed: it must assert nothing, and claim nothing about traffic
// it did not send.
func TestObserveModeAssertsNothing(t *testing.T) {
	sections, held := report(absorbAll(), options{match: "/stage0/", observe: true})
	if !held {
		t.Fatal("observe mode must not fail")
	}
	for _, section := range sections {
		if strings.Contains(section.title, "ASSERTION") || strings.Contains(section.title, "OBSERVATION") {
			t.Errorf("observe mode rendered %q", section.title)
		}
	}
}

// The open questions are observations: whatever they find, the job passes.
func TestOpenQuestionsNeverFailTheJob(t *testing.T) {
	seen := absorbAll(
		response(flowpb.L7FlowType_RESPONSE, labelOK, 200, 1_830_000),
		response(flowpb.L7FlowType_REQUEST, labelDead, 0, 0),
	)
	sections, held := report(seen, options{match: "/stage0/", traceID: "abc"})
	if !held {
		t.Fatal("a dead-backend observation must not fail the assertion")
	}
	dead := findSection(t, sections, "requests Envoy answers itself")
	if !strings.Contains(strings.Join(dead.lines, " "), "no response flow") {
		t.Errorf("expected the half-observed 503 to be reported, got %v", dead.lines)
	}
	traces := findSection(t, sections, "trace context")
	if !strings.Contains(strings.Join(traces.lines, " "), "nothing to read") {
		t.Errorf("expected the absent traceparent flow to be reported, got %v", traces.lines)
	}
}

func TestLostEventsAreCounted(t *testing.T) {
	seen := absorbAll(&observerpb.GetFlowsResponse{
		ResponseTypes: &observerpb.GetFlowsResponse_LostEvents{LostEvents: &flowpb.LostEvent{
			Source:        flowpb.LostEventSource_HUBBLE_RING_BUFFER,
			NumEventsLost: 12,
		}},
	})
	if seen.notices != 1 || seen.lost["HUBBLE_RING_BUFFER"] != 12 {
		t.Fatalf("expected the loss notice to be counted, got %d %v", seen.notices, seen.lost)
	}
	if seen.flows != 0 {
		t.Errorf("a loss notice is not a flow, got %d", seen.flows)
	}
}

func TestLabelAndAuthority(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{probeURL + "ok", labelOK},
		{probeURL + "dead?x=1", labelDead},
		{"/stage0/trace/deeper", labelTrace},
		{"http://host/elsewhere", ""},
	} {
		if got := label(tc.raw, "/stage0/"); got != tc.want {
			t.Errorf("label(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	if got := authority(probeURL + "ok"); got != "echo.stage0.example.com" {
		t.Errorf("authority = %q, want the host §3.2 attributes on", got)
	}
	if got := authority("/stage0/ok"); got != "" {
		t.Errorf("a path-only url has no authority, got %q", got)
	}
}

func findSection(t *testing.T, sections []section, fragment string) section {
	t.Helper()
	for _, section := range sections {
		if strings.Contains(section.title, fragment) {
			return section
		}
	}
	t.Fatalf("no section titled %q", fragment)
	return section{}
}
