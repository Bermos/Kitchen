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
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/platformhost"
)

// Creating a project is self-service and its name becomes a hostname under
// the base domain, so the names the platform already answers on are refused
// here rather than left to collide on the shared Gateway (#423).

func TestCreatingAProjectRefusesANameThePlatformServes(t *testing.T) {
	for _, name := range platformhost.Reserved() {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil, fixtures()...)
			body := fmt.Sprintf(
				`{"name":%q,"repo":"acme/thing","connection":"gh","registry":"registry"}`, name)
			recorder := h.do(t, http.MethodPost, "/api/v1/projects", body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
			// The refusal names the hostname that is taken, the way attaching
			// a custom domain under the base domain does.
			if want := name + ".apps.example.com"; !strings.Contains(recorder.Body.String(), want) {
				t.Errorf("the refusal does not name %s: %s", want, recorder.Body.String())
			}
			if err := h.server.get(context.Background(), name, &kitchenv1alpha1.Project{}); err == nil {
				t.Fatal("the project was created anyway")
			}
		})
	}
}

// The other collision needs no reserved name: project `shop-pr-7` publishes
// shop-pr-7.<base>, which is also where project `shop` publishes pull request
// 7. Both are generated, so the only place to tell them apart is the name.
func TestCreatingAProjectRefusesAPreviewShapedName(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/projects",
		`{"name":"shop-pr-7","repo":"acme/shop","connection":"gh","registry":"registry"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "-pr-") {
		t.Errorf("the refusal does not say what the shape is: %s", recorder.Body.String())
	}
	if err := h.server.get(context.Background(), "shop-pr-7", &kitchenv1alpha1.Project{}); err == nil {
		t.Fatal("the project was created anyway")
	}
}

// And the rule is narrow: a name that only looks like one of the above is
// still an ordinary name.
func TestCreatingAProjectStillAcceptsAnOrdinaryName(t *testing.T) {
	for _, name := range []string{"blog", "kitchen-sink", "auth-service", "pr-7", "shop-pr-preview"} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, nil, fixtures()...)
			body := fmt.Sprintf(
				`{"name":%q,"repo":"acme/thing","connection":"gh","registry":"registry"}`, name)
			recorder := h.do(t, http.MethodPost, "/api/v1/projects", body)
			if recorder.Code != http.StatusCreated {
				t.Fatalf("want 201, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}
