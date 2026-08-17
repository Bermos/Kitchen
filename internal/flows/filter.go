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
	flowpb "github.com/cilium/cilium/api/v1/flow"
	monitorapi "github.com/cilium/cilium/pkg/monitor/api"
)

// streamFilters is what the follower asks Relay to send, and nothing else.
//
// Until now it subscribed to every flow in the cluster and discarded around
// nine in ten of them in observe(): the whole packet-level record of a cluster
// crossing the network so that the operator could drop it in memory.
//
// The semantics are worth stating exactly, because they are the opposite way
// round from most APIs and getting them backwards would silently delete rows
// from the traffic view. Inside one FlowFilter every field must match, so a
// filter naming two things is an *and*; across the whitelist any one filter
// matching is enough, so the list is an *or*. The three below are therefore
// the three shapes observe() and requestOf() keep, one filter each, and every
// one of them is deliberately wider than what is kept — the follower still
// makes the final decision, and a filter that narrowed past it would remove
// observations the store receives today:
//
//   - L7 records, which is where every HTTP request comes from. Matching on
//     the event type rather than on `protocol: http` also brings DNS and Kafka
//     records, which cost nothing and are dropped in observe(); the narrower
//     filter would be a second place to remember when a second L7 protocol
//     becomes interesting.
//   - Drops, whatever the protocol, and including the ones with no L4 header
//     at all — which observe() records with an empty protocol today.
//   - TCP SYNs. Hubble's flags filter asks whether the flow carries at least
//     the flags named, so this also matches SYN+ACK, which observe() then
//     drops as the reply half of the same handshake.
//
// Lost-event notices are unaffected. Hubble exempts them from filtering
// altogether, on the stated grounds that nobody would ever ask for them and
// everybody needs them, so §3.2's loss accounting keeps working over a
// filtered stream — which is the whole reason it can be turned on here without
// blinding the thing that measures how much is being missed.
func streamFilters() []*flowpb.FlowFilter {
	return []*flowpb.FlowFilter{
		{EventType: []*flowpb.EventTypeFilter{{Type: int32(monitorapi.MessageTypeAccessLog)}}},
		{Verdict: []flowpb.Verdict{flowpb.Verdict_DROPPED}},
		{TcpFlags: []*flowpb.TCPFlags{{SYN: true}}},
	}
}
