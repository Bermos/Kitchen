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
	monitorapi "github.com/cilium/cilium/pkg/monitor/api"
)

// TestStreamFiltersAskForEachKeptShapeSeparately guards the one way this can
// be wrong without looking wrong: a whitelist is an *or* across its filters
// and an *and* within one, so folding the three shapes into a single
// FlowFilter would ask Relay for a dropped L7 SYN and silently receive nothing
// at all.
func TestStreamFiltersAskForEachKeptShapeSeparately(t *testing.T) {
	filters := streamFilters()
	if len(filters) != 3 {
		t.Fatalf("asked for %d filters, want one per kept shape", len(filters))
	}

	l7, drops, syns := 0, 0, 0
	for _, filter := range filters {
		fields := 0
		if types := filter.GetEventType(); len(types) > 0 {
			fields++
			l7++
			if types[0].GetType() != int32(monitorapi.MessageTypeAccessLog) {
				t.Errorf("event type = %d, want the access-log type L7 records carry", types[0].GetType())
			}
			// Matching on a sub-type would narrow this to one L7 protocol,
			// which is a second place to remember when a second one matters.
			if types[0].GetMatchSubType() {
				t.Error("the L7 filter should not match on a sub-type")
			}
		}
		if verdicts := filter.GetVerdict(); len(verdicts) > 0 {
			fields++
			drops++
			if verdicts[0] != flowpb.Verdict_DROPPED {
				t.Errorf("verdict = %s, want DROPPED", verdicts[0])
			}
		}
		if flags := filter.GetTcpFlags(); len(flags) > 0 {
			fields++
			syns++
			if !flags[0].GetSYN() {
				t.Error("the connection filter should ask for SYN")
			}
			// Hubble's flags filter asks whether the flow carries at least the
			// flags named, so naming ACK too would exclude the plain SYN this
			// exists to keep.
			if flags[0].GetACK() {
				t.Error("the connection filter must not also require ACK")
			}
		}
		if fields != 1 {
			t.Errorf("a filter names %d kinds of thing, want exactly one", fields)
		}
	}
	if l7 != 1 || drops != 1 || syns != 1 {
		t.Errorf("asked for %d L7, %d drop and %d SYN filters, want one each", l7, drops, syns)
	}
}

// TestStreamFiltersKeepEverythingObserveKeeps is the property that matters:
// the filters are the server's approximation of observe(), and every flow
// observe() would have recorded has to survive them, or the whitelist has
// silently deleted rows from the traffic view.
func TestStreamFiltersKeepEverythingObserveKeeps(t *testing.T) {
	for _, tc := range []struct {
		name string
		flow *flowpb.Flow
	}{
		{"an http response", &flowpb.Flow{
			Verdict:   flowpb.Verdict_FORWARDED,
			EventType: &flowpb.CiliumEventType{Type: int32(monitorapi.MessageTypeAccessLog)},
			L7: &flowpb.Layer7{
				Type:   flowpb.L7FlowType_RESPONSE,
				Record: &flowpb.Layer7_Http{Http: &flowpb.HTTP{Code: 200, Method: "GET", Url: "http://h/"}},
			},
		}},
		{"a tcp syn", &flowpb.Flow{
			Verdict:   flowpb.Verdict_FORWARDED,
			EventType: &flowpb.CiliumEventType{Type: int32(monitorapi.MessageTypeTrace)},
			L4:        &flowpb.Layer4{Protocol: &flowpb.Layer4_TCP{TCP: &flowpb.TCP{Flags: &flowpb.TCPFlags{SYN: true}}}},
		}},
		{"a dropped udp packet", &flowpb.Flow{
			Verdict:   flowpb.Verdict_DROPPED,
			EventType: &flowpb.CiliumEventType{Type: int32(monitorapi.MessageTypeDrop)},
			L4:        &flowpb.Layer4{Protocol: &flowpb.Layer4_UDP{UDP: &flowpb.UDP{}}},
		}},
		{"a drop with no l4 header at all", &flowpb.Flow{
			Verdict:   flowpb.Verdict_DROPPED,
			EventType: &flowpb.CiliumEventType{Type: int32(monitorapi.MessageTypeDrop)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, keep := observe(tc.flow); !keep {
				t.Fatalf("the fixture is wrong: observe() does not keep %s", tc.name)
			}
			if !matchesWhitelist(streamFilters(), tc.flow) {
				t.Errorf("the whitelist would drop %s before observe() ever saw it", tc.name)
			}
		})
	}
}

// matchesWhitelist reimplements Hubble's own combination rule over the three
// fields these filters use: every field named inside one filter must match,
// and a flow matching any one filter is sent.
func matchesWhitelist(filters []*flowpb.FlowFilter, flow *flowpb.Flow) bool {
	for _, filter := range filters {
		if matchesFilter(filter, flow) {
			return true
		}
	}
	return false
}

func matchesFilter(filter *flowpb.FlowFilter, flow *flowpb.Flow) bool {
	for _, wanted := range filter.GetEventType() {
		if flow.GetEventType().GetType() != wanted.GetType() {
			return false
		}
	}
	if verdicts := filter.GetVerdict(); len(verdicts) > 0 {
		matched := false
		for _, verdict := range verdicts {
			matched = matched || flow.GetVerdict() == verdict
		}
		if !matched {
			return false
		}
	}
	if flags := filter.GetTcpFlags(); len(flags) > 0 {
		observed := flow.GetL4().GetTCP().GetFlags()
		if observed == nil || (flags[0].GetSYN() && !observed.GetSYN()) {
			return false
		}
	}
	return true
}
