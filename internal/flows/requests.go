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
// Two columns are deliberately left empty. `trace_id` stays reserved until
// something can genuinely fill it — whether Cilium populates `trace_context`
// from an incoming `traceparent` at the Gateway is §9's open question, and a
// guessed id is worse than none. And no build or release is recorded at all:
// the edge routes to a Service, so during a rollout both revisions answer
// under one route and any release on the row would be a coin toss (§1).
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
	}, true
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
