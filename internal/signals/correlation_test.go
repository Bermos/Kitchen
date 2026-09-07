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

// severalNodes gives the snapshot a cluster with more than one node, which is
// what rung 2 requires before "these share a node" says anything: on a
// one-node cluster everything shares the only node.
func severalNodes(snapshot *Snapshot) {
	snapshot.Nodes = []corev1.Node{node(testNode, "4", "16Gi"), node(testOtherNode, "4", "16Gi")}
}

// severalStorageClasses does the same for storage: a claim on a second class,
// belonging to a project no correlation here is about.
func severalStorageClasses(snapshot *Snapshot) {
	other := "fast-ssd"
	snapshot.Claims = append(snapshot.Claims, corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: controller.AppNamespace("elsewhere")},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &other},
	})
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
	severalNodes(snapshot)
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
	severalNodes(snapshot)
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

	severalNodes(snapshot)
	// A crash loop with no restart history to date it leaves Since to the
	// registry, which stamps the round's instant — so the *history* is what
	// says when these began. This is the ordinary case for most of the
	// catalogue, and without it the correlator is right to stay quiet; see
	// TestACorrelationNeedsAKnownStart.
	for _, project := range correlatedSet {
		snapshot.OpenedAt[Fingerprint(SignalCrashLoop, Scope{
			Kind: ScopeEnvironment, Project: project, Environment: testEnvironment,
			Name: testContainer,
		})] = testNow.Add(-9 * time.Minute)
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

// A correlation is never louder than the conditions it stands in front of.
//
// The failure this pins is specific and was live: `platform.correlated`
// declares a page because it can fold three crash loops, and it folded three
// *warnings* — three volumes past 85% — into one critical row at the paging
// tier. The three warning rows disappear into the fold at the same time, so
// the screen lost three warnings and gained a page nobody asked for.
func TestACorrelationIsNoLouderThanWhatItFolds(t *testing.T) {
	warningIn := func(project string) Finding {
		scope := Scope{Kind: ScopeEnvironment, Project: project, Environment: testEnvironment}
		finding := fire(SignalPVCFilling, SeverityWarning, scope, testNow.Add(-5*time.Minute),
			"a volume is filling", "88% of 10Gi used", "")
		finding.Audience = AudienceDeveloper
		return finding
	}

	snapshot := newSnapshot()
	for _, project := range correlatedSet {
		snapshot.Round = append(snapshot.Round, warningIn(project))
	}
	finding := expectOne(t, correlateOnly(t, snapshot))
	if finding.Severity != SeverityWarning {
		t.Errorf("three warnings correlate to %q", finding.Severity)
	}
	if finding.Tier != TierTicket {
		t.Errorf("three warnings correlate to the %q tier, which pages about a condition none of "+
			"whose parts was worth paging about", finding.Tier)
	}

	// And the other direction, so the fix is not "never page": a correlation
	// of critical conditions is the page the rule declares.
	critical := newSnapshot()
	critical.Round = roundOf(correlatedSet, 5*time.Minute)
	raised := expectOne(t, correlateOnly(t, critical))
	if raised.Severity != SeverityCritical || raised.Tier != TierPage {
		t.Errorf("three critical conditions correlate to %q at %q", raised.Severity, raised.Tier)
	}
}

// The registry stamps `Since` with the round's own instant for every rule that
// sets none, so a whole round shares one timestamp. Reading that as evidence
// would make three volumes that began filling in March, June and September
// "firing at once" — the platform inventing a fact rather than measuring one.
func TestACorrelationNeedsAKnownStart(t *testing.T) {
	// Deliberately built the way the registry would leave them: Since is the
	// snapshot's instant, because the rule dated nothing.
	stampedNow := func(project string) Finding {
		scope := Scope{Kind: ScopeEnvironment, Project: project, Environment: testEnvironment}
		finding := fire(SignalPVCFilling, SeverityWarning, scope, testNow,
			"a volume is filling", "88% of 10Gi used", "")
		finding.Audience = AudienceDeveloper
		return finding
	}

	snapshot := newSnapshot()
	for _, project := range correlatedSet {
		snapshot.Round = append(snapshot.Round, stampedNow(project))
	}
	expectNone(t, correlateOnly(t, snapshot))

	// The history is where a start comes from, and with it they correlate.
	withHistory := newSnapshot()
	for _, project := range correlatedSet {
		finding := stampedNow(project)
		withHistory.Round = append(withHistory.Round, finding)
		withHistory.OpenedAt[finding.Fingerprint] = testNow.Add(-6 * time.Minute)
	}
	expectOne(t, correlateOnly(t, withHistory))

	// And the history is believed over the stamp, which is what makes three
	// conditions that opened months apart three conditions.
	apart := newSnapshot()
	for i, project := range correlatedSet {
		finding := stampedNow(project)
		apart.Round = append(apart.Round, finding)
		apart.OpenedAt[finding.Fingerprint] = testNow.Add(-time.Duration(i+1) * 30 * 24 * time.Hour)
	}
	expectNone(t, correlateOnly(t, apart))
}

// On a one-node cluster everything shares the only node, and on a
// single-StorageClass cluster every claim is on the only class. Naming either
// describes the installation rather than the incident — and it is the homelab
// preset's own installation, where every correlation would otherwise reach
// rung 2 explaining nothing.
func TestRungTwoSaysNothingAboutAClusterOfOne(t *testing.T) {
	one := newSnapshot()
	one.Nodes = []corev1.Node{node(testNode, "4", "16Gi")}
	one.Round = roundOf(correlatedSet, 5*time.Minute)
	for _, project := range correlatedSet {
		one.Pods = append(one.Pods, podOn(project, testNode))
	}
	if finding := expectOne(t, correlateOnly(t, one)); finding.Confidence != ConfidenceCoincidence {
		t.Errorf("a one-node cluster reached %q by naming its only node", finding.Confidence)
	}

	class := "local-path"
	single := newSnapshot()
	single.Round = roundOf(correlatedSet, 5*time.Minute)
	for _, project := range correlatedSet {
		single.Claims = append(single.Claims, corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name: "data", Namespace: controller.AppNamespace(project),
			},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
		})
	}
	if finding := expectOne(t, correlateOnly(t, single)); finding.Confidence != ConfidenceCoincidence {
		t.Errorf("a single-class cluster reached %q by naming its only class", finding.Confidence)
	}

	// A second class in the cluster makes the shared one worth saying.
	severalStorageClasses(single)
	if finding := expectOne(t, correlateOnly(t, single)); finding.Confidence != ConfidenceDependency {
		t.Errorf("with two classes in the cluster, the shared one is %q", finding.Confidence)
	}
}

// Rung 1's sentence is a claim about what was checked. When the reads behind
// rungs 2 and 3 failed, "nothing explains it" is the one confidently wrong
// sentence on the screen — this package's whole ethic is that an answer
// produced without an input says so.
func TestRungOneAdmitsWhatItCouldNotCheck(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Round = roundOf(correlatedSet, 5*time.Minute)
	snapshot.MarkUnreadable(InputNodes, "the API server said no")
	snapshot.MarkUnreadable(InputAudit, "the store is down")

	finding := expectOne(t, correlateOnly(t, snapshot))
	expectDetail(t, finding, "could not check")
	expectDetail(t, finding, string(InputNodes))
	expectDetail(t, finding, string(InputAudit))
	if strings.Contains(finding.Detail, "nothing explains it yet") {
		t.Errorf("a blind evaluation still claims nothing explains it: %s", finding.Detail)
	}

	// A question that does not arise is not a blind spot: an installation
	// with no telemetry store has no audit log to consult, and saying so
	// every round would be a permanent apology the reader learns to skip.
	quiet := newSnapshot()
	quiet.Round = roundOf(correlatedSet, 5*time.Minute)
	quiet.MarkNotApplicable(InputAudit, "this installation keeps no audit log")
	if got := expectOne(t, correlateOnly(t, quiet)); strings.Contains(got.Detail, "could not check") {
		t.Errorf("a not-applicable input is reported as unchecked: %s", got.Detail)
	}
}
