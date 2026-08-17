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
	"strings"
	"testing"
	"time"

	flowpb "github.com/cilium/cilium/api/v1/flow"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func responseFlow(url string) *flowpb.Flow {
	return &flowpb.Flow{
		Time:    timestamppb.New(time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)),
		Verdict: flowpb.Verdict_FORWARDED,
		// The response travels application → gateway, which is why the
		// endpoints say nothing about whose request it was.
		Source:      endpointFor(appNamespace, hostProduction),
		Destination: endpointFor(platformNS, "kitchen-gateway"),
		L7: &flowpb.Layer7{
			Type:      flowpb.L7FlowType_RESPONSE,
			LatencyNs: 42_000_000,
			Record: &flowpb.Layer7_Http{Http: &flowpb.HTTP{
				Code:     200,
				Method:   "GET",
				Url:      url,
				Protocol: "HTTP/1.1",
			}},
		},
	}
}

func TestRequestOfReadsWhatTheEdgeObserved(t *testing.T) {
	request, keep := requestOf(responseFlow("http://" + productionHost + "/users/12345?token=secret"))
	if !keep {
		t.Fatal("an HTTP response should produce a request row")
	}

	if request.Host != productionHost {
		t.Errorf("host = %q, want %q", request.Host, productionHost)
	}
	if request.Method != "GET" || request.Status != 200 || request.Protocol != "HTTP/1.1" {
		t.Errorf("unexpected request %+v", request)
	}
	// Envoy times the whole exchange, which is what makes a scale-to-zero cold
	// start visible as the first request's tail latency.
	if request.DurationMs != 42 {
		t.Errorf("duration = %v ms, want 42", request.DurationMs)
	}
	if request.Path != "/users/12345" {
		t.Errorf("path = %q, want the path without its query string", request.Path)
	}
	if request.Source != clickhouse.RequestSourceGateway {
		t.Errorf("source = %q, want %q", request.Source, clickhouse.RequestSourceGateway)
	}
	// A request that carried no trace context — which is every request from an
	// uninstrumented client — leaves the column empty.
	if request.TraceID != "" {
		t.Errorf("traceId = %q, want it empty without a trace context", request.TraceID)
	}
	if request.Timestamp.UTC().Hour() != 9 {
		t.Errorf("timestamp = %s, want the flow's own", request.Timestamp)
	}
}

func TestRequestOfKeepsOnlyAnsweredExchanges(t *testing.T) {
	// The request half carries no answer yet; counting it would double every
	// number on the golden-signal header.
	request := responseFlow("http://" + productionHost + "/")
	request.L7.Type = flowpb.L7FlowType_REQUEST
	if _, keep := requestOf(request); keep {
		t.Error("an HTTP request flow should not produce a row")
	}

	// A TCP SYN is traffic, but it is not a request.
	syn := &flowpb.Flow{
		Verdict: flowpb.Verdict_FORWARDED,
		L4:      &flowpb.Layer4{Protocol: &flowpb.Layer4_TCP{TCP: &flowpb.TCP{Flags: &flowpb.TCPFlags{SYN: true}}}},
	}
	if _, keep := requestOf(syn); keep {
		t.Error("a TCP SYN should not produce a request row")
	}

	// Neither is a DNS lookup, though it is an L7 record and arrives over the
	// same filtered stream.
	dns := &flowpb.Flow{L7: &flowpb.Layer7{
		Type:   flowpb.L7FlowType_RESPONSE,
		Record: &flowpb.Layer7_Dns{Dns: &flowpb.DNS{Query: "shop.example.com"}},
	}}
	if _, keep := requestOf(dns); keep {
		t.Error("a DNS record should not produce a request row")
	}
}

func TestCanonicalMethodBoundsTheVerbSet(t *testing.T) {
	for _, tc := range []struct{ sent, want string }{
		{"GET", "GET"},
		{"PATCH", "PATCH"},
		{"get", "GET"},
		{"PROPFIND", methodOther},
		{"\x16\x03\x01", methodOther},
		{"", methodOther},
	} {
		if got := canonicalMethod(tc.sent); got != tc.want {
			t.Errorf("canonicalMethod(%q) = %q, want %q", tc.sent, got, tc.want)
		}
	}
}

// TestRequestAttributionSurvivesTheProxiesInFront is the pipeline end to end
// over the case §3.2 is about: the flow's endpoints name the gate and the
// interceptor, and only the host names the application.
func TestRequestAttributionSurvivesTheProxiesInFront(t *testing.T) {
	table := hostsFromRoutes([]gatewayv1.HTTPRoute{
		routeFor(hostPreview, hostProject, hostPreview, previewHost),
	})
	budgets := newRouteBudgets()

	// A protected, idling preview: the response comes back from the shared
	// forward-auth gate in the platform namespace, which serves every preview
	// on the platform and belongs to none of them.
	flow := responseFlow("http://" + previewHost + "/orders/98765")
	flow.Source = endpointFor(platformNS, "kitchen-preview-gate")

	request, keep := requestOf(flow)
	if !keep {
		t.Fatal("an HTTP response should produce a request row")
	}
	owner := table.lookup(request.Host)
	request.Project, request.Environment = owner.project, owner.environment
	request.Route = budgets.route(owner.project, owner.environment, request.Path)

	if request.Project != hostProject || request.Environment != hostPreview {
		t.Errorf("attributed to %s/%s, want %s/%s",
			request.Project, request.Environment, hostProject, hostPreview)
	}
	if request.Route != "/orders/:id" {
		t.Errorf("route = %q, want /orders/:id", request.Route)
	}
	if request.Path != "/orders/98765" {
		t.Errorf("path = %q, want the raw path beside the template", request.Path)
	}
}

// TestRequestsForUnpublishedHostsShareOneBudget is the unrouted bucket doing
// the job §7 reads it for: a scanner walking hostnames the platform never
// published costs the store one budget in total, not one per name it invents.
func TestRequestsForUnpublishedHostsShareOneBudget(t *testing.T) {
	table := hostsFromRoutes(nil)
	budgets := newRouteBudgets()

	for i := range routeBudget {
		request, _ := requestOf(responseFlow("http://scan-" + unclassifiable(i)[1:] + ".example.com/"))
		owner := table.lookup(request.Host)
		budgets.route(owner.project, owner.environment, unclassifiable(i))
	}
	if got := budgets.environments.len(); got != 1 {
		t.Errorf("unrouted traffic opened %d budgets, want 1", got)
	}

	request, _ := requestOf(responseFlow("http://" + unroutedHost + "/probe"))
	owner := table.lookup(request.Host)
	if owner.project != "" || owner.environment != "" {
		t.Errorf("an unpublished host attributed to %+v, want the unrouted bucket", owner)
	}
	if got := budgets.route(owner.project, owner.environment, request.Path); got != overflowRoute {
		t.Errorf("route past the shared budget = %q, want %q", got, overflowRoute)
	}
}

// tracedFlow is a response flow whose request carried a `traceparent`, the way
// the stage-0 job observed one arriving at the Gateway.
func tracedFlow(traceID string) *flowpb.Flow {
	flow := responseFlow("http://" + productionHost + "/checkout")
	flow.TraceContext = &flowpb.TraceContext{
		Parent: &flowpb.TraceParent{TraceId: traceID},
	}
	return flow
}

// The column the schema reserved is written now that the edge is known to
// carry the id. What it must never carry is something a join cannot match: a
// key that finds no span reads as a trace that was lost, which is a worse
// answer than a request that was never traced.
func TestRequestsCarryTheTraceIdTheEdgeSaw(t *testing.T) {
	const wellFormed = "4bf92f3577b34da6a3ce929d0e0e4736"

	for _, tc := range []struct {
		name    string
		traceID string
		want    string
	}{
		{"the id the request carried", wellFormed, wellFormed},
		// Out of spec, but the request is real and its spans are stored lower
		// case, so the join has to be able to find them.
		{"upper case is folded", strings.ToUpper(wellFormed), wellFormed},
		// W3C defines the all-zero id as "no trace"; it is not an id.
		{"the all-zero id is no trace", strings.Repeat("0", 32), ""},
		{"too short", wellFormed[:31], ""},
		{"too long", wellFormed + "0", ""},
		{"not hex", strings.Repeat("z", 32), ""},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request, keep := requestOf(tracedFlow(tc.traceID))
			if !keep {
				t.Fatal("a traced request is still a request")
			}
			if request.TraceID != tc.want {
				t.Errorf("traceId = %q, want %q", request.TraceID, tc.want)
			}
		})
	}
}
