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

package cli

import (
	"strings"
	"testing"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

func topologyFixture(h *harness) {
	h.platform.topology = &topology{
		Nodes: []topologyNode{
			{ID: "environment/shop-production", Kind: "environment", Name: "shop-production", Project: "shop"},
			{ID: "offering/pricing/api", Kind: "offering", Name: "api", Project: "pricing", Foreign: true},
		},
		Edges: []topologyEdge{{
			ID: "consumes:environment/shop-production->offering/pricing/api", Kind: "consumes",
			From: "environment/shop-production", To: "offering/pricing/api", State: "PendingApproval",
		}},
	}
	h.platform.traffic = &topologyTraffic{
		Nodes: []topologyNode{{ID: "project/blog", Kind: "project", Name: "blog", Foreign: true}},
		Edges: []topologyObserved{{
			From: "project/blog", To: "environment/shop-production", Protocol: "HTTP",
			Flows: 6, RPS: 0.1, Status: "undeclared",
		}},
	}
}

func TestTopologyAnswersTheGraph(t *testing.T) {
	h := newHarness(t)
	topologyFixture(h)

	if code := h.run("topology", "--json", "--project", "shop"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, h.stderr.String())
	}
	answer := topology{}
	h.answer(&answer)
	if len(answer.Edges) != 1 || answer.Edges[0].State != "PendingApproval" {
		t.Fatalf("the edges come through whole, got %+v", answer.Edges)
	}
	if answer.Traffic != nil {
		t.Errorf("the observed half is only read when it is asked for")
	}
	sent := h.platform.sent("GET", "/topology")
	if len(sent) != 1 || !strings.Contains(sent[0].Query, "project=shop") {
		t.Fatalf("want one call narrowed to the project, got %v", h.platform.requests)
	}
	if len(h.platform.sent("GET", "/topology/traffic")) != 0 {
		t.Errorf("no traffic read without --traffic")
	}
}

func TestTopologyTrafficAddsTheObservedHalf(t *testing.T) {
	h := newHarness(t)
	topologyFixture(h)

	if code := h.run("topology", "--json", "--traffic", "15m"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, h.stderr.String())
	}
	answer := topology{}
	h.answer(&answer)
	if answer.Traffic == nil || len(answer.Traffic.Edges) != 1 || answer.Traffic.Edges[0].Status != "undeclared" {
		t.Fatalf("the observed pairs come through beside the graph, got %+v", answer.Traffic)
	}
	sent := h.platform.sent("GET", "/topology/traffic")
	if len(sent) != 1 || !strings.Contains(sent[0].Query, "since=") {
		t.Fatalf("the window is sent as an instant, got %v", h.platform.requests)
	}
}

func TestTopologyRendersEdgesByName(t *testing.T) {
	h := newHarness(t)
	topologyFixture(h)
	answer := *h.platform.topology
	answer.Traffic = h.platform.traffic

	out := renderTopology(tui.New(false), &answer)
	for _, want := range []string{"shop-production", "pricing/api", "PendingApproval", "blog", "undeclared"} {
		if !strings.Contains(out, want) {
			t.Errorf("the rendering should say %q:\n%s", want, out)
		}
	}
}
