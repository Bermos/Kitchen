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

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The clock-sync check, against its acceptance criterion: clock drift beyond a
// threshold appears in `kitchen default`'s component status.
//
// The rest of what is asserted here is the honesty of the measurement, which
// matters as much: a check that reported every unrenewed lease as a broken
// clock would be turned off in a week, and a check that could not say what to
// do about a real one would send somebody to a search engine.

const clockNow = "2026-08-24T12:00:00Z"

// clockFixtures is a platform with nodes whose leases were renewed at the
// offsets given, in seconds from the operator's own clock.
func clockFixtures(t *testing.T, threshold int32, offsets map[string]time.Duration) (
	*KitchenReconciler, *kitchenv1alpha1.Kitchen, time.Time,
) {
	t.Helper()

	now, err := time.Parse(time.RFC3339, clockNow)
	if err != nil {
		t.Fatal(err)
	}

	objects := []client.Object{}
	for name, offset := range offsets {
		objects = append(objects, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}})
		objects = append(objects, &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Namespace: nodeLeaseNamespace, Name: name},
			Spec: coordinationv1.LeaseSpec{
				RenewTime: ptr.To(metav1.NewMicroTime(now.Add(offset))),
			},
		})
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "example.com"},
	}
	kitchen.Spec.Observability.ClockSync.MaxDriftSeconds = threshold
	return &KitchenReconciler{Client: c}, kitchen, now
}

// TestDriftBeyondTheThresholdIsAnUnhealthyComponent is the acceptance
// criterion, literally: it appears in status.components.
func TestDriftBeyondTheThresholdIsAnUnhealthyComponent(t *testing.T) {
	r, kitchen, now := clockFixtures(t, 5, map[string]time.Duration{
		"node-a": 200 * time.Millisecond,
		"node-b": 47 * time.Second,
	})

	component, status := r.surveyClockSync(context.Background(), kitchen, now)
	if component == nil {
		t.Fatal("a cluster with a 47-second clock offset produced no component at all")
	}
	if component.Healthy {
		t.Error("a 47-second offset against a 5-second threshold reads as healthy")
	}
	if component.Name != clockComponentName {
		t.Errorf("the component is called %q", component.Name)
	}
	if component.Desired != 2 || component.Available != 1 {
		t.Errorf("the component reports %d of %d nodes in sync, want 1 of 2",
			component.Available, component.Desired)
	}
	if status == nil || status.WorstNode != "node-b" {
		t.Errorf("the worst node is reported as %+v, want node-b", status)
	}
	if status.Drifted != 1 {
		t.Errorf("%d nodes are reported as drifted, want 1", status.Drifted)
	}
}

// TestTheMessageSaysWhatToDoAboutIt. "Clock drift detected" sends somebody to
// a search engine; this has to send them to a machine.
func TestTheMessageSaysWhatToDoAboutIt(t *testing.T) {
	r, kitchen, now := clockFixtures(t, 5, map[string]time.Duration{
		"node-b": 90 * time.Second,
	})

	component, status := r.surveyClockSync(context.Background(), kitchen, now)
	if component == nil {
		t.Fatal("no component")
	}
	for _, want := range []string{"node-b", "chrony", "maxDriftSeconds"} {
		if !strings.Contains(status.Message, want) {
			t.Errorf("the message does not mention %q: %s", want, status.Message)
		}
	}
	if component.Message == "" {
		t.Error("the component carries no message, so the survey says a name and nothing else")
	}
}

// TestASynchronisedClusterIsHealthyAndSaysNothing.
func TestASynchronisedClusterIsHealthyAndSaysNothing(t *testing.T) {
	r, kitchen, now := clockFixtures(t, 5, map[string]time.Duration{
		"node-a": 12 * time.Millisecond,
		"node-b": -3 * time.Second,
	})

	component, status := r.surveyClockSync(context.Background(), kitchen, now)
	if component == nil {
		t.Fatal("no component")
	}
	if !component.Healthy {
		t.Errorf("a synchronised cluster reads as unhealthy: %s", component.Message)
	}
	if status.Message != "" {
		t.Errorf("a healthy check has something to say: %s", status.Message)
	}
}

// TestAnUnrenewedLeaseIsNotABrokenClock. The kubelet renews every ten seconds
// and the read is always at least that stale, so the past direction is
// forgiven up to the renewal grace — otherwise every cluster fails this check
// every few minutes.
func TestAnUnrenewedLeaseIsNotABrokenClock(t *testing.T) {
	r, kitchen, now := clockFixtures(t, 5, map[string]time.Duration{
		"node-a": -40 * time.Second,
	})

	component, _ := r.surveyClockSync(context.Background(), kitchen, now)
	if component == nil {
		t.Fatal("no component")
	}
	if !component.Healthy {
		t.Errorf("a lease renewed 40 seconds ago reads as a broken clock: %s", component.Message)
	}
}

// TestAStampInTheFutureCountsInFull. Nothing but a wrong clock stamps a time
// that has not happened yet, so that direction gets no grace at all.
func TestAStampInTheFutureCountsInFull(t *testing.T) {
	r, kitchen, now := clockFixtures(t, 5, map[string]time.Duration{
		"node-a": 20 * time.Second,
	})

	component, _ := r.surveyClockSync(context.Background(), kitchen, now)
	if component == nil {
		t.Fatal("no component")
	}
	if component.Healthy {
		t.Error("a node stamping 20 seconds into the future reads as synchronised")
	}
}

// TestTheThresholdIsConfigurable, and its default is the documented one.
func TestTheThresholdIsConfigurable(t *testing.T) {
	if kitchenv1alpha1.DefaultMaxDriftSeconds != 5 {
		t.Fatalf("the compiled-in default is %d seconds and the docs say 5",
			kitchenv1alpha1.DefaultMaxDriftSeconds)
	}

	r, kitchen, now := clockFixtures(t, 120, map[string]time.Duration{
		"node-a": 90 * time.Second,
	})
	component, status := r.surveyClockSync(context.Background(), kitchen, now)
	if component == nil {
		t.Fatal("no component")
	}
	if !component.Healthy {
		t.Error("a 90-second offset under a 120-second threshold reads as drift")
	}
	if status.MaxDriftSeconds != 120 {
		t.Errorf("the status reports the threshold as %d, want 120", status.MaxDriftSeconds)
	}
}

// TestTheCheckCanBeTurnedOffEntirely, for a cluster whose kubelets renew so
// slowly that the measurement says nothing useful.
func TestTheCheckCanBeTurnedOffEntirely(t *testing.T) {
	r, kitchen, now := clockFixtures(t, 5, map[string]time.Duration{
		"node-a": time.Hour,
	})
	kitchen.Spec.Observability.ClockSync.Enabled = ptr.To(false)

	component, status := r.surveyClockSync(context.Background(), kitchen, now)
	if component != nil || status != nil {
		t.Error("a check that is turned off still reported")
	}
}

// TestAClusterWithNoLeasesIsUnverifiedRatherThanUnhealthy. A node with no
// kubelet lease is a node this check has no opinion about, and reporting no
// opinion as a fault would be crying wolf.
func TestAClusterWithNoLeasesIsUnverifiedRatherThanUnhealthy(t *testing.T) {
	r, kitchen, now := clockFixtures(t, 5, nil)

	component, status := r.surveyClockSync(context.Background(), kitchen, now)
	if component != nil {
		t.Error("a cluster with nothing to measure produced a component in the survey")
	}
	if status == nil || !strings.Contains(status.Message, "unverified") {
		t.Errorf("nothing says time sync was not verified: %+v", status)
	}
}

// TestTheMethodIsCarriedBesideTheNumbers. Every number here is relative to the
// operator's own clock and one-sided in its precision; a status that reported
// the number without the method would be a number somebody over-reads.
func TestTheMethodIsCarriedBesideTheNumbers(t *testing.T) {
	r, kitchen, now := clockFixtures(t, 5, map[string]time.Duration{"node-a": time.Second})

	_, status := r.surveyClockSync(context.Background(), kitchen, now)
	if status == nil || status.Method == "" {
		t.Fatal("the measurement carries no method")
	}
	if status.Checked == nil {
		t.Error("the measurement is undated")
	}
}

// TestTheCheckJoinsTheSurveyAndTheConditionWithIt: the criterion says the
// drift appears in the component status, and the survey's own condition is
// what an operator reads first.
func TestTheCheckJoinsTheSurveyAndTheConditionWithIt(t *testing.T) {
	r, kitchen, _ := clockFixtures(t, 5, map[string]time.Duration{"node-a": time.Hour})

	conditions := map[string]string{}
	healthy := r.surveyComponents(context.Background(), kitchen,
		func(name string, _ metav1.ConditionStatus, _, message string) {
			conditions[name] = message
		})

	if healthy {
		t.Error("the survey reports every component healthy on a cluster an hour out of sync")
	}
	found := false
	for _, component := range kitchen.Status.Components {
		if component.Name == clockComponentName {
			found = true
			if component.Kind != "Node" {
				t.Errorf("the clock entry reports kind %q, want Node", component.Kind)
			}
		}
	}
	if !found {
		t.Fatalf("the component survey does not carry %s: %+v", clockComponentName, kitchen.Status.Components)
	}
	if !strings.Contains(conditions[condComponentsHealthy], clockComponentName) {
		t.Errorf("the components condition does not name the drifting check: %q",
			conditions[condComponentsHealthy])
	}
}
