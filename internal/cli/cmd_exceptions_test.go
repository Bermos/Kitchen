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
	"time"
)

// The exceptions commands, run the way somebody would run them: argv in,
// HTTP out, one JSON document back.

func exceptionsFixture(h *harness) {
	h.platform.exceptions = []exception{{
		Name:        "shop-exc-x7k2p",
		Project:     "shop",
		Environment: "shop-production",
		RuleIDs:     []string{"max-severity"},
		Reason:      "hotfix for INC-421",
		RequestedBy: "grace@example.com",
		ApprovedBy:  "heidi@example.com",
		IncidentRef: "INC-421",
		ExpiresAt:   time.Date(2026, 8, 22, 9, 0, 0, 0, time.UTC),
		Phase:       "Active",
		UsedBy:      []string{"shop-promo-7"},
	}}
}

func TestExceptionsListSendsTheFiltersAndAnswersTheRegister(t *testing.T) {
	h := newHarness(t)
	exceptionsFixture(h)

	if code := h.run("exceptions", "list", "--json",
		"--project", "shop", "--environment", "shop-production", "--historical"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, h.stderr.String())
	}
	answer := list[exception]{}
	h.answer(&answer)
	if len(answer.Items) != 1 || answer.Items[0].Name != "shop-exc-x7k2p" {
		t.Fatalf("answered %+v", answer.Items)
	}
	if answer.Items[0].ApprovedBy != "heidi@example.com" || answer.Items[0].Phase != "Active" {
		t.Fatalf("the grant must survive the trip whole, got %+v", answer.Items[0])
	}

	sent := h.platform.sent("GET", "/exceptions")
	if len(sent) != 1 {
		t.Fatalf("want one list call, got %v", h.platform.requests)
	}
	for _, fragment := range []string{"project=shop", "environment=shop-production", "historical=true"} {
		if !strings.Contains(sent[0].Query, fragment) {
			t.Errorf("the query must carry %s, got %q", fragment, sent[0].Query)
		}
	}
}

func TestExceptionsShowAnswersOneGrantWhole(t *testing.T) {
	h := newHarness(t)
	exceptionsFixture(h)

	if code := h.run("exceptions", "show", "shop-exc-x7k2p", "--json"); code != 0 {
		t.Fatalf("exit %d\nstderr: %s", code, h.stderr.String())
	}
	answer := exception{}
	h.answer(&answer)
	if answer.Name != "shop-exc-x7k2p" || len(answer.UsedBy) != 1 {
		t.Fatalf("answered %+v", answer)
	}

	if code := h.run("exceptions", "show", "no-such-exception", "--json"); code != exitNotFound {
		t.Fatalf("a missing exception exits notFound, got %d", code)
	}
}
