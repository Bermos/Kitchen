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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// recordedUpgrade is one entry's history as the operator would have left it.
func recordedUpgrade(name string, started time.Time, from, to string,
	phase kitchenv1alpha1.AddonUpgradePhase) *kitchenv1alpha1.AddonUpgrade {
	at := metav1.NewTime(started)
	return &kitchenv1alpha1.AddonUpgrade{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, CreationTimestamp: at},
		Spec: kitchenv1alpha1.AddonUpgradeSpec{
			Addon:     controller.AddonKeda,
			From:      []kitchenv1alpha1.AddonChartStatus{{Name: "keda", Version: from}},
			To:        []kitchenv1alpha1.AddonChartStatus{{Name: "keda", Version: to}},
			Namespace: "keda",
			JobName:   "kitchen-keda-install-" + to,
		},
		Status: kitchenv1alpha1.AddonUpgradeStatus{Phase: phase, StartedAt: &at},
	}
}

// installedAddon is an Addon whose history this operator has been keeping
// since the instant given.
func installedAddon(since *time.Time) *kitchenv1alpha1.Addon {
	addon := &kitchenv1alpha1.Addon{
		ObjectMeta: metav1.ObjectMeta{Name: controller.AddonKeda, Namespace: testNamespace},
		Spec:       kitchenv1alpha1.AddonSpec{Install: true},
		Status:     kitchenv1alpha1.AddonStatus{Managed: true, Serving: true, Namespace: "keda"},
	}
	if since != nil {
		at := metav1.NewTime(*since)
		addon.Status.UpgradeHistorySince = &at
	}
	return addon
}

// The history is what the addon's own status cannot be: `installed` is
// singular and current, so an upgrade that broke something an hour later
// leaves a number that has always said what it says now.
func TestAddonUpgradesAnswerNewestFirst(t *testing.T) {
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	h := newHarness(t, nil,
		installedAddon(&base),
		recordedUpgrade("first", base.Add(24*time.Hour), "2.16.0", "2.17.2",
			kitchenv1alpha1.AddonUpgradeSucceeded),
		recordedUpgrade("second", base.Add(72*time.Hour), "2.17.2", "2.20.2",
			kitchenv1alpha1.AddonUpgradeFailed),
	)

	recorder := h.do(t, http.MethodGet, "/api/v1/addons/keda/upgrades", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := decode[addonUpgradesView](t, recorder)
	if body.Addon != controller.AddonKeda {
		t.Fatalf("the answer names the entry it is about, got %+v", body)
	}
	if len(body.Items) != 2 {
		t.Fatalf("want both attempts, got %+v", body.Items)
	}
	if body.Items[0].Name != "second" {
		t.Fatalf("the last thing that changed is what somebody is looking for, got %+v", body.Items)
	}
	if body.Items[0].Phase != string(kitchenv1alpha1.AddonUpgradeFailed) {
		t.Fatalf("an attempt that failed is history too, got %q", body.Items[0].Phase)
	}
	if body.Items[1].From[0].Version != "2.16.0" || body.Items[1].To[0].Version != "2.17.2" {
		t.Fatalf("both sides of the transition are the point, got %+v", body.Items[1])
	}
	if body.RecordedSince == nil || !body.RecordedSince.Time.Equal(base) {
		t.Fatalf("want the history's own start, got %+v", body.RecordedSince)
	}
}

// The distinction the list alone cannot draw: nothing has moved since we
// started watching, versus nobody was watching.
func TestAddonUpgradesSayWhetherAHistoryWasKeptAtAll(t *testing.T) {
	since := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	kept := newHarness(t, nil, installedAddon(&since))
	body := decode[addonUpgradesView](t, kept.do(t, http.MethodGet, "/api/v1/addons/keda/upgrades", ""))
	if len(body.Items) != 0 {
		t.Fatalf("nothing was recorded, got %+v", body.Items)
	}
	if body.RecordedSince == nil {
		t.Fatal("an empty list under a date means this entry has not moved since it")
	}

	// An installation whose addon predates the records: the same empty list,
	// and no claim about what it means.
	older := newHarness(t, nil, installedAddon(nil))
	body = decode[addonUpgradesView](t, older.do(t, http.MethodGet, "/api/v1/addons/keda/upgrades", ""))
	if len(body.Items) != 0 {
		t.Fatalf("nothing was recorded, got %+v", body.Items)
	}
	if body.RecordedSince != nil {
		t.Fatalf("an operator that kept no history must not claim one, got %+v", body.RecordedSince)
	}
}

// One entry's history is one entry's: the records live in one namespace
// together, and a route that answered with all of them would put CloudNativePG
// in KEDA's story.
func TestAddonUpgradesAreOneEntrysOwn(t *testing.T) {
	base := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	other := recordedUpgrade("cnpg", base, "0.28.0", "0.29.0", kitchenv1alpha1.AddonUpgradeSucceeded)
	other.Spec.Addon = controller.AddonCNPG
	h := newHarness(t, nil, installedAddon(&base), other,
		recordedUpgrade("keda", base, "2.17.2", "2.20.2", kitchenv1alpha1.AddonUpgradeSucceeded))

	body := decode[addonUpgradesView](t, h.do(t, http.MethodGet, "/api/v1/addons/keda/upgrades", ""))
	if len(body.Items) != 1 || body.Items[0].Name != "keda" {
		t.Fatalf("want only this entry's own records, got %+v", body.Items)
	}
}

// The catalogue is compiled in, so a name that is not in it is a 404 that
// names what is — the same answer the entry's own read gives.
func TestAddonUpgradesRefuseAnEntryThePlatformDoesNotKnow(t *testing.T) {
	h := newHarness(t, nil)
	recorder := h.do(t, http.MethodGet, "/api/v1/addons/not-a-real-addon/upgrades", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if body := decode[errorBody](t, recorder); body.Error == "" {
		t.Fatal("a refusal says what this platform does install")
	}
}
