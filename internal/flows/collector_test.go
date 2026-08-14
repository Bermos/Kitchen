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

package flows

import (
	"testing"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

func endpointFor(namespace, workload string) *flowpb.Endpoint {
	return &flowpb.Endpoint{
		Namespace: namespace,
		PodName:   workload + "-6d5f9-xk2p9",
		Workloads: []*flowpb.Workload{{Name: workload, Kind: "Deployment"}},
	}
}

func TestObserveRecordsHTTPResponsesAsTheRequestEdge(t *testing.T) {
	// An HTTP response travels server→client; the edge must read the way the
	// request ran: client → server.
	flow := &flowpb.Flow{
		Verdict:     flowpb.Verdict_FORWARDED,
		Source:      endpointFor("kitchen-shop", "shop-production"),
		Destination: endpointFor("kitchen-system", "gateway"),
		L7: &flowpb.Layer7{
			Type:      flowpb.L7FlowType_RESPONSE,
			LatencyNs: 42_000_000,
			Record:    &flowpb.Layer7_Http{Http: &flowpb.HTTP{Code: 503, Method: "GET", Url: "/"}},
		},
	}

	observation, keep := observe(flow)
	if !keep {
		t.Fatal("an HTTP response should be recorded")
	}
	if observation.Source != "gateway" || observation.Destination != "shop-production" {
		t.Errorf("edge = %s → %s, want gateway → shop-production", observation.Source, observation.Destination)
	}
	if observation.Protocol != "HTTP" || observation.HTTPStatus != 503 || observation.LatencyMs != 42 {
		t.Errorf("unexpected observation %+v", observation)
	}

	// The request half of the same exchange carries no answer yet and would
	// double-count the edge.
	flow.L7.Type = flowpb.L7FlowType_REQUEST
	if _, keep := observe(flow); keep {
		t.Error("an HTTP request flow should not be recorded")
	}
}

func TestObserveKeepsSYNsAndDropsOnly(t *testing.T) {
	syn := &flowpb.Flow{
		Verdict:     flowpb.Verdict_FORWARDED,
		Source:      endpointFor("kitchen-shop", "shop-production"),
		Destination: endpointFor("kitchen-system", "clickhouse"),
		L4: &flowpb.Layer4{Protocol: &flowpb.Layer4_TCP{TCP: &flowpb.TCP{
			Flags: &flowpb.TCPFlags{SYN: true},
		}}},
	}
	if observation, keep := observe(syn); !keep || observation.Protocol != "TCP" {
		t.Errorf("a TCP SYN should be recorded as a TCP observation, got keep=%v %+v", keep, observation)
	}

	// The same connection's later packets say nothing new about the edge.
	syn.GetL4().GetTCP().Flags = &flowpb.TCPFlags{ACK: true}
	if _, keep := observe(syn); keep {
		t.Error("a mid-connection packet should not be recorded")
	}

	// ...unless Cilium dropped it: drops are always signal.
	syn.Verdict = flowpb.Verdict_DROPPED
	observation, keep := observe(syn)
	if !keep || observation.Verdict != "DROPPED" {
		t.Errorf("a drop should be recorded, got keep=%v %+v", keep, observation)
	}

	// Replies never are, drop or not — the forward direction owns the edge.
	syn.Verdict = flowpb.Verdict_FORWARDED
	syn.GetL4().GetTCP().Flags = &flowpb.TCPFlags{SYN: true}
	syn.IsReply = wrapperspb.Bool(true)
	if _, keep := observe(syn); keep {
		t.Error("a reply flow should not be recorded")
	}
}

func TestEndpointNameFallsBack(t *testing.T) {
	if got := endpointName(nil, []string{"api.github.com"}); got.name != "api.github.com" || got.namespace != "" {
		t.Errorf("dns fallback = %+v", got)
	}
	if got := endpointName(nil, nil); got.name != "world" {
		t.Errorf("unknown endpoint = %+v, want world", got)
	}
	if got := endpointName(&flowpb.Endpoint{Namespace: "kitchen-shop", PodName: "one-off"}, nil); got.name != "one-off" {
		t.Errorf("pod fallback = %+v", got)
	}
}
