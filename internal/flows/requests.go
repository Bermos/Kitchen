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
	"time"

	flowpb "github.com/cilium/cilium/api/v1/flow"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The second output the follower gained (§3.2): one request row per HTTP
// exchange the platform's edge served.
//
// Nothing new observes anything for this. Every request to every Kitchen
// application crosses the shared Gateway, which is Cilium's embedded Envoy,
// and Envoy already streams its access records to the agent — which is where
// the L7 flows this package has followed all along come from. The follower
// simply stopped throwing the method and the URL away.

// methodOther is what a verb outside the known set is recorded as.
const methodOther = "OTHER"

// httpMethods is the verb set a request row may carry. Anything else becomes
// methodOther: the column has a dictionary behind it and is part of the
// rollups' ordering key, so a client that invents verbs — which is to say a
// scanner — would otherwise mint an entry for each one it tries.
var httpMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "PATCH": {},
	"DELETE": {}, "CONNECT": {}, "OPTIONS": {}, "TRACE": {},
}

// requestOf derives the observable half of a request row from one Hubble flow:
// everything the flow itself carries. Attribution and the route template are
// the caller's, because both need state the flow knows nothing about — the
// platform's published hostnames, and the environment's route budget.
//
// It keeps the same half of the exchange observe() does. The RESPONSE flow is
// the one that has been answered: it carries the status, and Envoy's latency
// is measured across the whole transaction, which is what makes a scale-to-zero
// cold start visible as the tail latency of the first request rather than as
// nothing at all.
//
// `trace_id` is filled from the flow's trace context where the request carried
// one. Whether Cilium populates it at the Gateway was §9's open question; the
// stage-0 job answered it by sending a `traceparent` and reading the same id
// back off the flow, so the column the schema reserved can be written without
// a migration. A request that carried no trace context leaves it empty, which
// is every request from an uninstrumented client.
//
// No build or release is recorded at all: the edge routes to a Service, so
// during a rollout both revisions answer under one route and any release on
// the row would be a coin toss (§1).
func requestOf(flow *flowpb.Flow) (clickhouse.Request, bool) {
	http := flow.GetL7().GetHttp()
	if http == nil || flow.GetL7().GetType() != flowpb.L7FlowType_RESPONSE {
		return clickhouse.Request{}, false
	}

	timestamp := time.Now()
	if t := flow.GetTime(); t != nil {
		timestamp = t.AsTime()
	}

	authority, path := splitURL(http.GetUrl())
	return clickhouse.Request{
		Timestamp: timestamp,
		Host:      normaliseHost(authority),
		Method:    canonicalMethod(http.GetMethod()),
		Path:      truncatePath(path),
		Status:    uint16(http.GetCode()),
		// Envoy measures the whole exchange, so this is the number a visitor
		// waited, not the time the application spent.
		DurationMs: float64(flow.GetL7().GetLatencyNs()) / 1e6,
		Protocol:   http.GetProtocol(),
		Source:     clickhouse.RequestSourceGateway,
		TraceID:    traceIDOf(flow),
	}, true
}

// traceIDLength is a W3C trace id in hex: sixteen bytes, thirty-two characters.
const traceIDLength = 32

// traceIDOf reads the id an incoming `traceparent` carried, or an empty string.
//
// It is validated rather than copied because the value arrives from whoever
// made the request. The column exists so a request row can be joined to the
// spans in `otel_traces`, and a join key nothing can match is worse than an
// empty one: it looks like a trace that has been lost rather than a request
// that was never traced. So anything that is not a well-formed id — wrong
// length, not hex, or the all-zero id W3C defines as "no trace" — is dropped.
//
// Hex digits are folded to lower case because that is how the spans are
// stored, and a join is a string comparison. A client sending upper case is
// out of spec, but its request is real and its trace is findable.
func traceIDOf(flow *flowpb.Flow) string {
	id := flow.GetTraceContext().GetParent().GetTraceId()
	if len(id) != traceIDLength {
		return ""
	}
	lowered := make([]byte, 0, traceIDLength)
	zero := true
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= '0' && c <= '9':
			zero = zero && c == '0'
		case c >= 'a' && c <= 'f':
			zero = false
		case c >= 'A' && c <= 'F':
			zero = false
			c += 'a' - 'A'
		default:
			return ""
		}
		lowered = append(lowered, c)
	}
	if zero {
		return ""
	}
	return string(lowered)
}

// canonicalMethod folds a verb onto the known set. The exact spelling is tried
// first because that is what a well-behaved client sends and it costs one map
// lookup; the upper-cased form is tried second so that a lower-case verb is
// counted as the request it plainly was rather than as garbage.
func canonicalMethod(method string) string {
	if _, known := httpMethods[method]; known {
		return method
	}
	upper := strings.ToUpper(strings.TrimSpace(method))
	if _, known := httpMethods[upper]; known {
		return upper
	}
	return methodOther
}
