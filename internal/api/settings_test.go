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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/ui"
)

// settingsPath is the one URL both settings routes answer on.
const settingsPath = "/api/v1/settings"

func TestReadingTheSettings(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodGet, settingsPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[settingsView](t, recorder)
	if body.BaseDomain != "apps.example.com" {
		t.Fatalf("want the base domain, got %+v", body)
	}
	if !body.AuthEnabled || body.AuthHost == "" {
		t.Fatalf("want the identity provider reported, got %+v", body)
	}
	if body.APIExternalURL != "https://kitchen.apps.example.com" {
		t.Fatalf("want the derived external URL, got %q", body.APIExternalURL)
	}
}

func TestChangingTheSettings(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, settingsPath,
		`{"buildStrategy": "dockerfile", "buildConcurrency": 4, "releaseRetention": 25, "logRetentionDays": 7}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[settingsView](t, recorder)
	if body.BuildStrategy != "dockerfile" || body.BuildConcurrency != 4 ||
		body.ReleaseRetention != 25 || body.LogRetentionDays != 7 {
		t.Fatalf("the answer does not carry the change: %+v", body)
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Spec.Builds.DefaultStrategy != kitchenv1alpha1.BuildStrategyDockerfile ||
		kitchen.Spec.Builds.Concurrency != 4 ||
		kitchen.Spec.Builds.ReleaseRetention != 25 ||
		kitchen.Spec.Observability.ClickHouse.RetentionDays != 7 {
		t.Fatalf("the singleton was not updated: %+v", kitchen.Spec)
	}
}

func TestChangingTheSettingsLeavesOmittedFieldsAlone(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, settingsPath, `{"buildConcurrency": 3}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Spec.Builds.Concurrency != 3 {
		t.Fatalf("the concurrency was not updated: %+v", kitchen.Spec.Builds)
	}
	if kitchen.Spec.Builds.DefaultStrategy != "" {
		t.Fatalf("an omitted field was changed: %+v", kitchen.Spec.Builds)
	}
}

// Zero is the one number here that means "no bound" rather than "unset": the
// platform keeps every release a project ever built, which is what it did
// before there was a count at all.
func TestChangingTheSettingsAcceptsUnboundedReleases(t *testing.T) {
	h := newHarness(t, nil)

	if recorder := h.do(t, http.MethodPatch, settingsPath, `{"releaseRetention": 5}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	recorder := h.do(t, http.MethodPatch, settingsPath, `{"releaseRetention": 0}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Spec.Builds.ReleaseRetention != 0 {
		t.Fatalf("the count was not cleared: %+v", kitchen.Spec.Builds)
	}
}

// The ceiling is the other half of the concurrency, and it is set from the
// same screen through the same route.
func TestChangingTheBuildCeiling(t *testing.T) {
	h := newHarness(t, nil)

	recorder := h.do(t, http.MethodPatch, settingsPath,
		`{"buildConcurrency": 3, "buildCPU": "1500m", "buildMemory": "6Gi"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[settingsView](t, recorder)
	if body.BuildCPU != "1500m" || body.BuildMemory != "6Gi" {
		t.Fatalf("the answer does not carry the ceiling: %+v", body)
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Spec.Builds.Resources.CPU != "1500m" || kitchen.Spec.Builds.Resources.Memory != "6Gi" {
		t.Fatalf("the singleton was not updated: %+v", kitchen.Spec.Builds)
	}
}

// The empty string is the one way to end up with no ceiling, and it has to be
// written rather than fallen into — so it is accepted, and a request that does
// not name the field cannot do it by accident.
func TestClearingTheBuildCeilingIsAllowedAndDeliberate(t *testing.T) {
	h := newHarness(t, nil)

	if recorder := h.do(t, http.MethodPatch, settingsPath,
		`{"buildCPU": "2", "buildMemory": "4Gi"}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if recorder := h.do(t, http.MethodPatch, settingsPath,
		`{"buildMemory": ""}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Spec.Builds.Resources.Memory != "" {
		t.Fatalf("the memory ceiling was not cleared: %+v", kitchen.Spec.Builds.Resources)
	}
	if kitchen.Spec.Builds.Resources.CPU != "2" {
		t.Fatalf("clearing one ceiling disturbed the other: %+v", kitchen.Spec.Builds.Resources)
	}
}

func TestChangingTheSettingsRejectsNonsense(t *testing.T) {
	h := newHarness(t, nil)

	for name, body := range map[string]string{
		"an unknown strategy":              `{"buildStrategy": "guess"}`,
		"zero concurrency":                 `{"buildConcurrency": 0}`,
		"no retention at all":              `{"logRetentionDays": 0}`,
		"a negative release count":         `{"releaseRetention": -1}`,
		"a field it never knew":            `{"baseDomain": "elsewhere.example.com"}`,
		"a ceiling that is not a quantity": `{"buildMemory": "lots"}`,
		// Not "no ceiling" — a build that cannot start.
		"a ceiling of nothing": `{"buildCPU": "0"}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, settingsPath, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}
}

// TestTheDashboardIsServedNextToTheAPI proves the split: everything under
// /api/ keeps its token check while the SPA and its bootstrap configuration
// are public.
func TestTheDashboardIsServedNextToTheAPI(t *testing.T) {
	h := newHarness(t, nil)
	h.server.UI = ui.Handler(UIConfig(h.server.Client, "kitchen-ui"))
	handler := h.server.Handler()

	// The SPA answers anonymously, on deep links too.
	for _, path := range []string{"/", "/projects/shop", "/auth/callback"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: want 200, got %d", path, recorder.Code)
		}
		if !strings.Contains(recorder.Header().Get("Content-Type"), "text/html") {
			t.Fatalf("%s: want the app shell, got %q", path, recorder.Header().Get("Content-Type"))
		}
	}

	// Its bootstrap configuration says where to sign in, and for what.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/config.json", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("config.json: want 200, got %d", recorder.Code)
	}
	config := decode[ui.Config](t, recorder)
	if config.ClientID != "kitchen-ui" || config.Issuer == "" ||
		config.APIURL != "https://kitchen.apps.example.com" {
		t.Fatalf("the config does not add up: %+v", config)
	}

	// The API next door still refuses an anonymous caller.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("the API answered an anonymous caller: %d", recorder.Code)
	}
}

// The operator list is enforced against and seeded on upgrade, so it has to be
// readable and writable from here: a platform surface nothing serves is one
// somebody reaches for kubectl to see, which is the thing this platform exists
// to abolish (#104, CLAUDE.md's "Nothing needs kubectl").

func TestTheSettingsCarryTheOperatorList(t *testing.T) {
	h := newHarness(t, nil)

	body := decode[settingsView](t, h.do(t, http.MethodGet, settingsPath, ""))
	if len(body.Operators) != 1 {
		t.Fatalf("want the platform's one operator, got %+v", body.Operators)
	}
	if body.Operators[0].Subject != testSubject || body.Operators[0].Email != testCaller {
		t.Fatalf("want the operator by subject and address, got %+v", body.Operators[0])
	}
}

func TestChangingTheOperatorList(t *testing.T) {
	h := newHarness(t, nil)
	directory := h.withDirectory()

	// An address goes in and the issuer's `sub` is what lands, exactly as it
	// does for a project grant — the resolution is the same one.
	recorder := h.do(t, http.MethodPatch, settingsPath,
		`{"operators": [{"subject": "`+testSubject+`"}, {"email": "`+annaEmail+`"}]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if len(directory.asked) != 1 || directory.asked[0] != annaEmail {
		t.Fatalf("the address was not resolved at the identity provider: %v", directory.asked)
	}
	body := decode[settingsView](t, recorder)
	if len(body.Operators) != 2 || body.Operators[1].Subject != annaSubject || body.Operators[1].Email != annaEmail {
		t.Fatalf("want anna named by her subject, got %+v", body.Operators)
	}

	stored := operatorsOf(t, h)
	if len(stored) != 2 || stored[1].Subject != annaSubject {
		t.Fatalf("the singleton does not carry the list: %+v", stored)
	}
	// And it is the list the platform now enforces against.
	if h.do(t, http.MethodGet, settingsPath, "").Code != http.StatusOK {
		t.Fatal("the caller kept themselves on the list and was refused anyway")
	}
}

// The same rule the last admin on a project follows, and for the same reason:
// there would be nobody left who could appoint the next one.
func TestTheLastOperatorCannotBeRemoved(t *testing.T) {
	h := newHarness(t, nil)
	h.withDirectory()

	recorder := h.do(t, http.MethodPatch, settingsPath, `{"operators": []}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := errorOf(t, recorder.Body.String()); got != lastOperatorRefusal {
		t.Fatalf("want the refusal to say what would fix it, got %q", got)
	}
	if stored := operatorsOf(t, h); len(stored) != 1 {
		t.Fatalf("the list was emptied anyway: %+v", stored)
	}

	// Handing the platform to somebody else in the same write is fine: the
	// rule is about the list being emptied, not about who is on it.
	if recorder := h.do(t, http.MethodPatch, settingsPath,
		`{"operators": [{"email": "`+annaEmail+`"}]}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestTheOperatorListRefusesUnusableWrites(t *testing.T) {
	h := newHarness(t, nil)
	h.withDirectory()

	for name, body := range map[string]string{
		"an entry naming nobody":       `{"operators": [{}]}`,
		"an entry naming two things":   `{"operators": [{"email": "` + annaEmail + `", "subject": "user_9"}]}`,
		"a subject that is an address": `{"operators": [{"subject": "` + annaEmail + `"}]}`,
		"the same account twice":       `{"operators": [{"subject": "user_9"}, {"subject": "user_9"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := h.do(t, http.MethodPatch, settingsPath, body)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
			}
		})
	}

	// An address the identity provider has never heard of is a 404 about the
	// person, not a grant written to somebody who does not exist.
	recorder := h.do(t, http.MethodPatch, settingsPath, `{"operators": [{"email": "`+stranger+`"}]}`)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if stored := operatorsOf(t, h); len(stored) != 1 || stored[0].Subject != testSubject {
		t.Fatalf("a refused write moved the list: %+v", stored)
	}
}

// A settings patch that does not mention the operators must not disturb them.
// client.MergeFrom diffs two marshalled objects, so this is a property of the
// bytes rather than of the handler, and it is worth pinning: the list is what
// decides who may call this endpoint at all.
func TestASettingsPatchDoesNotDisturbTheOperatorList(t *testing.T) {
	h := newHarness(t, nil)
	h.updateKitchen(t, func(kitchen *kitchenv1alpha1.Kitchen) {
		kitchen.Spec.Access.Operators = append(kitchen.Spec.Access.Operators,
			kitchenv1alpha1.AccessSubject{Subject: annaSubject, Email: annaEmail})
	})

	if recorder := h.do(t, http.MethodPatch, settingsPath, `{"buildConcurrency": 3}`); recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := operatorsOf(t, h)
	if len(stored) != 2 || stored[0].Subject != testSubject || stored[1].Subject != annaSubject {
		t.Fatalf("a patch that said nothing about the operators moved them: %+v", stored)
	}
}

// The other end of the same property: an **empty** list is a deliberate
// narrowing and an **absent** one is "the reconciler has not seeded yet"
// (AccessSpec), so a settings patch must never turn one into the other. It is
// checked on the patch bytes because a platform whose list is empty has no
// operator to make the request with — the endpoint is closed to everybody
// there, which is exactly what the last-operator refusal above prevents the
// API from ever producing.
func TestAnEmptiedOperatorListIsNotReSeededByASettingsPatch(t *testing.T) {
	for name, operators := range map[string][]kitchenv1alpha1.AccessSubject{
		"an empty list":  {},
		"an absent list": nil,
	} {
		t.Run(name, func(t *testing.T) {
			kitchen := &kitchenv1alpha1.Kitchen{}
			kitchen.Name = controller.KitchenSingletonName
			kitchen.Spec.Access.Operators = operators

			edited := kitchen.DeepCopy()
			patch := client.MergeFrom(kitchen.DeepCopy())
			edited.Spec.Builds.Concurrency = 3

			data, err := patch.Data(edited)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "operators") {
				t.Fatalf("a patch about the build concurrency rewrote the operator list: %s", data)
			}
		})
	}

	// And the two read back differently, so a dashboard can tell "nobody has
	// said yet" from "somebody said nobody": `null` against `[]`.
	empty := newSettingsView(&kitchenv1alpha1.Kitchen{
		Spec: kitchenv1alpha1.KitchenSpec{
			Access: kitchenv1alpha1.AccessSpec{Operators: []kitchenv1alpha1.AccessSubject{}},
		},
	})
	absent := newSettingsView(&kitchenv1alpha1.Kitchen{})
	if got := marshalled(t, empty)["operators"]; got != "[]" {
		t.Fatalf("want an empty list to marshal as [], got %s", got)
	}
	if got := marshalled(t, absent)["operators"]; got != "null" {
		t.Fatalf("want an absent list to marshal as null, got %s", got)
	}
}

// operatorsOf is the operator list as it is actually written on the singleton.
func operatorsOf(t *testing.T, h *harness) []kitchenv1alpha1.AccessSubject {
	t.Helper()
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	return kitchen.Spec.Access.Operators
}

// marshalled is a view's JSON with each field left as it was written, so a
// test can tell an absent list from an empty one.
func marshalled(t *testing.T, view settingsView) map[string]string {
	t.Helper()
	raw := map[string]json.RawMessage{}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	fields := make(map[string]string, len(raw))
	for name, value := range raw {
		fields[name] = string(value)
	}
	return fields
}

// racingWrite lands one write on the Kitchen object between the handler
// reading it and patching it, which is the window a lost update happens in.
// It fires once, so a handler that patches after retrying is not raced twice.
func racingWrite(t *testing.T, h *harness, edit func(*kitchenv1alpha1.Kitchen)) {
	t.Helper()
	base, ok := h.server.Client.(client.WithWatch)
	if !ok {
		t.Fatalf("the harness's client cannot be intercepted: %T", h.server.Client)
	}
	var once sync.Once
	h.server.Client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(
			ctx context.Context,
			c client.WithWatch,
			obj client.Object,
			patch client.Patch,
			opts ...client.PatchOption,
		) error {
			once.Do(func() {
				current := &kitchenv1alpha1.Kitchen{}
				if err := c.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, current); err != nil {
					t.Error(err)
					return
				}
				edit(current)
				if err := c.Update(ctx, current); err != nil {
					t.Error(err)
				}
			})
			return c.Patch(ctx, obj, patch, opts...)
		},
	})
}

// Two operators removing each other at once is the case the last-operator rule
// exists to prevent, and a lost update is how it gets through: the list is
// replaced wholesale, so the loser's write puts back somebody the winner's
// took off — against a list that no longer exists by the time it lands.
func TestRemovingAnOperatorConcurrentlyIsRefusedRatherThanLost(t *testing.T) {
	h := newHarness(t, nil)
	h.withDirectory()
	h.updateKitchen(t, func(kitchen *kitchenv1alpha1.Kitchen) {
		kitchen.Spec.Access.Operators = []kitchenv1alpha1.AccessSubject{
			{Subject: testSubject, Email: testCaller},
			{Subject: annaSubject, Email: annaEmail},
			{Subject: "user_ben", Email: "ben@example.com"},
		}
	})
	// While this request is in flight, somebody else takes anna off.
	racingWrite(t, h, func(kitchen *kitchenv1alpha1.Kitchen) {
		kitchen.Spec.Access.Operators = []kitchenv1alpha1.AccessSubject{
			{Subject: testSubject, Email: testCaller},
			{Subject: "user_ben", Email: "ben@example.com"},
		}
	})

	// And this one takes ben off, from the list it read a moment ago.
	recorder := h.do(t, http.MethodPatch, settingsPath,
		`{"operators": [{"subject": "`+testSubject+`"}, {"subject": "`+annaSubject+`"}]}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stored := operatorsOf(t, h)
	if len(stored) != 2 || stored[1].Subject != "user_ben" {
		t.Fatalf("the concurrent removal must stand rather than being undone: %+v", stored)
	}
}

// The lock is on the list, not on the whole object. A settings patch that
// changes nothing anybody decided anything against must not fail because
// somebody else moved an unrelated field.
func TestASettingsChangeIsNotRefusedOverAnUnrelatedConcurrentEdit(t *testing.T) {
	h := newHarness(t, nil)
	racingWrite(t, h, func(kitchen *kitchenv1alpha1.Kitchen) {
		kitchen.Spec.Observability.ClickHouse.RetentionDays = 30
	})

	recorder := h.do(t, http.MethodPatch, settingsPath, `{"buildConcurrency": 4}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Spec.Builds.Concurrency != 4 {
		t.Fatalf("want the concurrency written, got %+v", kitchen.Spec.Builds)
	}
	if kitchen.Spec.Observability.ClickHouse.RetentionDays != 30 {
		t.Fatalf("a merge patch of one field must leave the other write alone, got %+v",
			kitchen.Spec.Observability.ClickHouse)
	}
}
