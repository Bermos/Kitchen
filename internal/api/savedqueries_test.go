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

package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestSavingAQueryNamesItFromItsTitle(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodPost, "/api/v1/logs/saved",
		`{"title":"Checkout 500s","query":"level:error service:shop","rangeMinutes":60,"limit":500,"view":"patterns"}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("POST /logs/saved = %d: %s", res.Code, res.Body.String())
	}
	body := decode[savedQueryView](t, res)
	if body.Name != "checkout-500s" {
		t.Errorf("the caller should not have to invent a name, got %q", body.Name)
	}
	if body.Query != "level:error service:shop" || body.RangeMinutes != 60 || body.Limit != 500 {
		t.Errorf("the selection did not survive: %+v", body)
	}
	// Which tab it was read in is part of the question: one saved because its
	// patterns were the point should open on them.
	if body.View != "patterns" {
		t.Errorf("the view did not survive: %+v", body)
	}
	// The byline comes from the token, not from the body.
	if body.SavedBy != testCaller {
		t.Errorf("want the caller recorded, got %q", body.SavedBy)
	}

	list := h.do(t, http.MethodGet, "/api/v1/logs/saved", "")
	items := decode[struct {
		Items []savedQueryView `json:"items"`
	}](t, list).Items
	if len(items) != 1 || items[0].Title != "Checkout 500s" {
		t.Errorf("unexpected list %+v", items)
	}
}

// A saved query that cannot be run is worse than none: it is found later, by
// someone who did not write it, at the moment they needed the answer.
func TestAQueryThatDoesNotCompileIsNotSaved(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodPost, "/api/v1/logs/saved", `{"title":"Broken","query":"level:error OR"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", res.Code, res.Body.String())
	}
	// The parser's own diagnostic is the answer, because it says how to fix it.
	if !strings.Contains(res.Body.String(), "ends where a term was expected") {
		t.Errorf("want the parser's diagnostic, got %s", res.Body.String())
	}
}

func TestSavingAQueryNeedsATitle(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodPost, "/api/v1/logs/saved", `{"query":"level:error"}`)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", res.Code, res.Body.String())
	}
}

// The name is derived, so the API server's conflict would name something the
// caller never typed.
func TestASecondQueryOfTheSameNameIsRefusedInThePlatformsWords(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	first := h.do(t, http.MethodPost, "/api/v1/logs/saved", `{"title":"Checkout 500s"}`)
	if first.Code != http.StatusCreated {
		t.Fatalf("POST /logs/saved = %d: %s", first.Code, first.Body.String())
	}
	second := h.do(t, http.MethodPost, "/api/v1/logs/saved", `{"title":"checkout 500s"}`)
	if second.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", second.Code, second.Body.String())
	}
	if body := second.Body.String(); !strings.Contains(body, "different title") ||
		strings.Contains(body, "kitchen.bermos.dev") {
		t.Errorf("the conflict should be in the platform's words, got %s", body)
	}
}

// An empty selection over a window is a legitimate question — it is what the
// page asks when someone opens it — so it is a saveable one.
func TestAnEmptySelectionCanBeSaved(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodPost, "/api/v1/logs/saved", `{"title":"Everything, last day","rangeMinutes":1440}`)
	if res.Code != http.StatusCreated {
		t.Fatalf("want 201, got %d: %s", res.Code, res.Body.String())
	}
}

func TestATitleWithNothingToSlugStillGetsAName(t *testing.T) {
	for title, want := range map[string]string{
		"Checkout 500s":            "checkout-500s",
		"  spaced  out  ":          "spaced-out",
		"…":                        "query",
		"-leading and trailing-":   "leading-and-trailing",
		"MiXeD CaSe & punctuation": "mixed-case-punctuation",
	} {
		if got := savedQueryName(title); got != want {
			t.Errorf("savedQueryName(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestRemovingASavedQuery(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	created := h.do(t, http.MethodPost, "/api/v1/logs/saved", `{"title":"Checkout 500s"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("POST /logs/saved = %d: %s", created.Code, created.Body.String())
	}

	// Nothing owns a saved query and nothing cleans up after it, so the delete
	// is finished when the API server accepts it.
	res := h.do(t, http.MethodDelete, "/api/v1/logs/saved/checkout-500s", "")
	if res.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", res.Code, res.Body.String())
	}

	list := h.do(t, http.MethodGet, "/api/v1/logs/saved", "")
	items := decode[struct {
		Items []savedQueryView `json:"items"`
	}](t, list).Items
	if len(items) != 0 {
		t.Errorf("the query should be gone, got %+v", items)
	}
}

func TestRemovingAQueryThatIsNotThere(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodDelete, "/api/v1/logs/saved/nope", "")
	if res.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", res.Code, res.Body.String())
	}
}
