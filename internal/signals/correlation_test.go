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

package signals

import (
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// correlatedProjects is the affected set most tests here use: three projects,
// which is what the balanced policy calls a correlation.
var correlatedSet = []string{"api", "docs", "shop"}

// crashingIn is one project's crash-loop finding, dated `ago` before the
// snapshot's instant. It is deliberately a rule that is *not* the two traffic
// detectors — the point of the widened rung 1 is that it correlates the rest
// of the catalogue.
func crashingIn(project string, ago time.Duration) Finding {
	scope := Scope{
		Kind: ScopeEnvironment, Project: project, Environment: testEnvironment, Name: testContainer,
	}
	finding := fire(SignalCrashLoop, SeverityCritical, scope, testNow.Add(-ago),
		"crash-looping", "12 restarts in 30m", "")
	finding.Audience = AudienceDeveloper
	return finding
}

func roundOf(projects []string, ago time.Duration) Findings {
	round := make(Findings, 0, len(projects))
	for _, project := range projects {
		round = append(round, crashingIn(project, ago))
	}
	return round
}

// Rung 1: coincidence in time alone is enough to raise a row, and the row says
// what it does not know. This is decision 3, and it is the argument the whole
// issue turns on.
func TestCorrelatedRaisesOnTimeAloneAndSaysSo(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = roundOf(correlatedSet, 5*time.Minute)

	finding := expectOne(t, correlateOnly(t, snapshot))
	if finding.Confidence != ConfidenceCoincidence {
		t.Fatalf("confidence = %q, want coincidence", finding.Confidence)
	}
	expectDetail(t, finding, "nothing explains it yet")
	for _, project := range correlatedSet {
		expectDetail(t, finding, project)
	}
	// The scope is the condition, never the project set: which projects are
	// caught up changes minute to minute and the fingerprint must not.
	if finding.Fingerprint != string(SignalCorrelated)+"/"+string(SignalCrashLoop) {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
}

// The threshold is the policy's, which is the requirement §9 asked to be
// shown: three is a number a small estate never reaches.
func TestCorrelatedUsesThePolicyThreshold(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = roundOf([]string{"api", "shop"}, 5*time.Minute)
	expectNone(t, correlateOnly(t, snapshot))

	homelab := newSnapshot()
	homelab.Policy, _ = Preset(PresetHomelab)
	homelab.Round = roundOf([]string{"api", "shop"}, 5*time.Minute)
	if finding := expectOne(t, correlateOnly(t, homelab)); finding.Title == "" {
		t.Fatal("the homelab preset raised a finding with no title")
	}
}

// Two failures that began an hour apart are not one moment. The window is
// measured between the conditions rather than back from the evaluation, which
// is what lets a correlation be noticed after the fact.
func TestCorrelatedIgnoresFailuresOutsideTheWindow(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = Findings{
		crashingIn("api", 2*time.Minute),
		crashingIn("docs", 3*time.Minute),
		crashingIn("shop", 3*time.Hour),
	}
	expectNone(t, correlateOnly(t, snapshot))

	// The same three, all inside the window, correlate — and the window is
	// between them, not back from now: these all began an hour ago.
	late := newSnapshot()
	late.Round = roundOf(correlatedSet, time.Hour)
	expectOne(t, correlateOnly(t, late))
}

// The two traffic detectors read a stronger input than a count of findings, so
// the widened rung leaves their dimensions alone rather than putting a second
// row on the screen for one incident.
func TestCorrelatedLeavesTheTrafficDetectorsAlone(t *testing.T) {
	snapshot := newSnapshot()
	round := make(Findings, 0, len(correlatedSet))
	for _, project := range correlatedSet {
		scope := Scope{Kind: ScopeEnvironment, Project: project, Environment: testEnvironment}
		round = append(round, fire(SignalErrorRate, SeverityCritical, scope,
			testNow.Add(-5*time.Minute), "error rate", "9% of requests are failing", ""))
	}
	snapshot.Round = round
	expectNone(t, correlateOnly(t, snapshot))
}

// Rung 2: the affected set intersected against what those projects share. It
// names the intersection and stops short of naming a culprit.
func TestLadderClimbsToASharedDependency(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = roundOf(correlatedSet, 5*time.Minute)
	for _, project := range correlatedSet {
		snapshot.Pods = append(snapshot.Pods, podOn(project, testNode))
	}

	finding := expectOne(t, correlateOnly(t, snapshot))
	if finding.Confidence != ConfidenceDependency {
		t.Fatalf("confidence = %q, want dependency", finding.Confidence)
	}
	expectDetail(t, finding, "node "+testNode)
}

// A dependency two of three projects share explains two of three failures,
// which is a weaker sentence than this rung makes — so it is not made.
func TestLadderRefusesAPartialIntersection(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = roundOf(correlatedSet, 5*time.Minute)
	snapshot.Pods = []corev1.Pod{
		podOn("api", testNode),
		podOn("docs", testNode),
		podOn("shop", testOtherNode),
	}

	finding := expectOne(t, correlateOnly(t, snapshot))
	if finding.Confidence != ConfidenceCoincidence {
		t.Fatalf("confidence = %q, want coincidence: only two of three share the node", finding.Confidence)
	}
}

// A claimed database is the issue's own example of the shared dependency worth
// naming, and two projects never share the claim object — what they share is
// what it resolves to.
func TestLadderNamesASharedClaim(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = roundOf(correlatedSet, 5*time.Minute)
	for _, project := range correlatedSet {
		snapshot.ResourceClaims = append(snapshot.ResourceClaims, kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: project + "-db"},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: project},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "shared-postgres"},
				Type:          "postgres",
			},
		})
	}

	finding := expectOne(t, correlateOnly(t, snapshot))
	if finding.Confidence != ConfidenceDependency {
		t.Fatalf("confidence = %q, want dependency", finding.Confidence)
	}
	expectDetail(t, finding, "shared-postgres")
}

// On an installation with one shared Gateway, "these are all behind the
// gateway" describes the cluster rather than the incident, so it is not said.
func TestLadderDoesNotNameTheOnlyGateway(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = roundOf(correlatedSet, 5*time.Minute)
	snapshot.Gateways = []gatewayv1.Gateway{{
		ObjectMeta: metav1.ObjectMeta{Name: "kitchen", Namespace: controller.PlatformNamespace},
	}}
	for _, project := range correlatedSet {
		snapshot.Routes = append(snapshot.Routes, routeTo(project, "kitchen"))
	}

	finding := expectOne(t, correlateOnly(t, snapshot))
	if finding.Confidence != ConfidenceCoincidence {
		t.Fatalf("confidence = %q, want coincidence: one gateway is not an intersection", finding.Confidence)
	}
}

// Rung 3: the same, joined against the platform's own timeline. It is the only
// rung that can offer an action on the cause rather than on the symptom.
func TestLadderClimbsToASharedChange(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = roundOf(correlatedSet, 5*time.Minute)
	snapshot.PlatformChanges = []PlatformChange{{
		At:      testNow.Add(-8 * time.Minute),
		Kind:    "addon",
		Summary: "the keda addon was upgraded (succeeded)",
	}}

	finding := expectOne(t, correlateOnly(t, snapshot))
	if finding.Confidence != ConfidenceChange {
		t.Fatalf("confidence = %q, want change", finding.Confidence)
	}
	expectDetail(t, finding, "the keda addon was upgraded")
}

// A change that landed after the failures started did not cause them, and a
// ladder that ignored the direction of time would promote every correlation
// raised during a rollout to its highest rung.
func TestLadderIgnoresAChangeAfterTheFailures(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = roundOf(correlatedSet, 30*time.Minute)
	snapshot.PlatformChanges = []PlatformChange{{
		At:      testNow.Add(-2 * time.Minute),
		Kind:    "release",
		Summary: "this platform was upgraded to 0.38.0 (running)",
	}}

	finding := expectOne(t, correlateOnly(t, snapshot))
	if finding.Confidence != ConfidenceCoincidence {
		t.Fatalf("confidence = %q, want coincidence", finding.Confidence)
	}
}

// Every finding records the thresholds it was evaluated against — decision 1.
// Two installations on catalogue v1 with different policies disagree about
// whether a rule fired, and this is what makes them distinguishable.
func TestEveryFindingCarriesItsPolicyProvenance(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Policy, _ = Preset(PresetStrict)
	snapshot.Round = roundOf([]string{"api", "shop"}, 5*time.Minute)

	finding := expectOne(t, correlateOnly(t, snapshot))
	if !strings.Contains(finding.Policy, "preset=strict") {
		t.Fatalf("the finding does not record what it was evaluated against: %q", finding.Policy)
	}
	if !strings.Contains(finding.Policy, "correlatedProjects=2") {
		t.Fatalf("the provenance does not carry the threshold that raised it: %q", finding.Policy)
	}
}

// The whole catalogue runs in one pass and the correlator in a second, which
// is what "widening it to the rest of the catalogue's findings" means: the
// other rules know nothing about it.
func TestTheCorrelatorSeesTheWholeRound(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Pods = []corev1.Pod{}
	for i, project := range correlatedSet {
		pod := podOn(project, testNode)
		pod.Name = fmt.Sprintf("crashing-%d", i)
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: testContainer,
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff", Message: "back-off 5m0s",
			}},
		}}
		snapshot.Pods = append(snapshot.Pods, pod)
	}

	findings := Catalogue().Evaluate(snapshot)
	var correlated *Finding
	for i := range findings {
		if findings[i].Signal == SignalCorrelated {
			correlated = &findings[i]
		}
	}
	if correlated == nil {
		t.Fatalf("the correlator did not see the round: %s", describe(findings.Firing()))
	}
	// It reached rung 2 without anything but the pods, because the pods are
	// both the condition and the shared placement.
	if correlated.Confidence != ConfidenceDependency {
		t.Fatalf("confidence = %q, want dependency", correlated.Confidence)
	}
}

// correlateOnly runs the catalogue's second pass over a round a test built by
// hand.
//
// It is not [evaluate] because the two passes are the point: [Registry.Evaluate]
// sets [Snapshot.Round] from the first pass, so a one-signal registry holding
// only the correlator would hand it an empty round and correlate nothing. This
// drives the same pass with the round supplied, which is what lets a test place
// three projects' conditions where it wants them without materialising the pods
// behind each one.
func correlateOnly(t *testing.T, snapshot *Snapshot) Findings {
	t.Helper()
	snapshot.Policy = snapshot.Policy.Normalised()
	findings := Catalogue().pass(snapshot, true)
	findings.Sort()
	return findings
}

// podOn is one project's pod, scheduled somewhere.
func podOn(project, node string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      project + "-web",
			Namespace: controller.AppNamespace(project),
			Labels: map[string]string{
				controller.LabelProject:     project,
				controller.LabelEnvironment: testEnvironment,
			},
		},
		Spec:   corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// routeTo is one project's route, hanging off a named Gateway.
func routeTo(project, gateway string) gatewayv1.HTTPRoute {
	return gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      project,
			Namespace: controller.AppNamespace(project),
		},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{Name: gatewayv1.ObjectName(gateway)}},
			},
		},
	}
}
