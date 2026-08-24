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
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The criterion this file exists for: "tolerance values are wired to alerting,
// not decorative". The proof is the first test — two environments identical in
// every way the platform can see, differing only in a number the institution
// declared, and only one of them fires.

const testProduction = "production"

// outage is a Deployment of `environment` that has served nothing since
// `since` before now.
func outage(environment string, since time.Duration) appsv1.Deployment {
	return appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:              environment,
			Namespace:         controller.AppNamespace(testProject),
			CreationTimestamp: metav1.NewTime(testNow.Add(-24 * time.Hour)),
			Labels: map[string]string{
				controller.LabelProject:     testProject,
				controller.LabelEnvironment: environment,
			},
		},
		Spec: appsv1.DeploymentSpec{Replicas: ptr(int32(2))},
		Status: appsv1.DeploymentStatus{
			AvailableReplicas: 0,
			Conditions: []appsv1.DeploymentCondition{{
				Type:               appsv1.DeploymentAvailable,
				Status:             corev1.ConditionFalse,
				Reason:             "MinimumReplicasUnavailable",
				LastTransitionTime: metav1.NewTime(testNow.Add(-since)),
			}},
		},
	}
}

func ptr[T any](value T) *T { return &value }

// designated puts one environment's resolved tolerance on the snapshot, the
// way [Gather] folds it.
func designated(snapshot *Snapshot, environment string, continuity kitchenv1alpha1.Continuity) {
	if snapshot.Continuity == nil {
		snapshot.Continuity = map[EnvKey]ContinuityFor{}
	}
	snapshot.Continuity[EnvKey{Project: testProject, Environment: environment}] = ContinuityFor{
		Continuity: continuity, Project: testProject,
	}
}

func TestTheDeclaredObjectiveIsTheThreshold(t *testing.T) {
	// Two environments, the same twenty-minute outage. One holds a
	// fifteen-minute RTO and one holds four hours. Nothing else differs.
	snapshot := newSnapshot()
	snapshot.Deployments = []appsv1.Deployment{
		outage(testProduction, 20*time.Minute),
		outage(testEnvironment, 20*time.Minute),
	}
	designated(snapshot, testProduction, kitchenv1alpha1.Continuity{RTO: "15m"})
	designated(snapshot, testEnvironment, kitchenv1alpha1.Continuity{RTO: "4h"})

	findings := evaluateRTOAtRisk(snapshot)
	if len(findings) != 1 {
		t.Fatalf("the objective is not deciding anything: %+v", findings)
	}
	if findings[0].Scope.Environment != testProduction {
		t.Fatalf("the wrong environment fired: %+v", findings[0].Scope)
	}
	if findings[0].Severity != SeverityCritical {
		t.Fatalf("twenty minutes is past a fifteen-minute objective, got %s", findings[0].Severity)
	}
	if !strings.Contains(findings[0].Detail, "15m") {
		t.Fatalf("the finding does not quote the objective it fired against: %s", findings[0].Detail)
	}
}

func TestHalfTheObjectiveWarnsBeforeItIsBreached(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Deployments = []appsv1.Deployment{outage(testProduction, 35*time.Minute)}
	designated(snapshot, testProduction, kitchenv1alpha1.Continuity{RTO: "1h"})

	findings := evaluateRTOAtRisk(snapshot)
	if len(findings) != 1 || findings[0].Severity != SeverityWarning {
		t.Fatalf("more than half an hour of a one-hour objective must warn: %+v", findings)
	}
	// Dated from the outage, not from the evaluation, so a background loop
	// records when it opened rather than when it noticed.
	if !findings[0].Since.Equal(testNow.Add(-35 * time.Minute)) {
		t.Fatalf("the finding is dated %s, want the start of the outage", findings[0].Since)
	}

	// A quarter of the way in is nobody's problem yet.
	snapshot.Deployments = []appsv1.Deployment{outage(testProduction, 15*time.Minute)}
	if got := evaluateRTOAtRisk(snapshot); len(got) != 0 {
		t.Fatalf("a quarter of the objective fired: %+v", got)
	}
}

func TestAnEnvironmentWithNoObjectiveIsNotThisRulesProblem(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Deployments = []appsv1.Deployment{outage(testProduction, 6*time.Hour)}

	// Undesignated: nothing on the snapshot at all.
	if got := evaluateRTOAtRisk(snapshot); len(got) != 0 {
		t.Fatalf("an undesignated environment fired: %+v", got)
	}
	// Designated criticality, no tolerance — still not a threshold.
	designated(snapshot, testProduction, kitchenv1alpha1.Continuity{
		Criticality: kitchenv1alpha1.CriticalityCritical,
	})
	if got := evaluateRTOAtRisk(snapshot); len(got) != 0 {
		t.Fatalf("a criticality without an RTO was read as one: %+v", got)
	}
}

func TestAnIdledEnvironmentIsNotAnOutage(t *testing.T) {
	// Scale-to-zero parks an environment at no pods on purpose. The rule
	// reads desired, so the parked case never reaches the objective.
	snapshot := newSnapshot()
	parked := outage(testEnvironment, 6*time.Hour)
	parked.Spec.Replicas = ptr(int32(0))
	snapshot.Deployments = []appsv1.Deployment{parked}
	designated(snapshot, testEnvironment, kitchenv1alpha1.Continuity{RTO: "15m"})

	if got := evaluateRTOAtRisk(snapshot); len(got) != 0 {
		t.Fatalf("a parked environment was reported as breaching its RTO: %+v", got)
	}
}

func TestAnInheritedObjectiveSaysWhereItCameFrom(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Deployments = []appsv1.Deployment{outage(testProduction, 2*time.Hour)}
	designated(snapshot, testProduction, kitchenv1alpha1.Continuity{
		RTO: "1h", Inherited: []string{"rto"},
	})

	findings := evaluateRTOAtRisk(snapshot)
	if len(findings) != 1 {
		t.Fatalf("nothing fired: %+v", findings)
	}
	if !strings.Contains(findings[0].Detail, "inherited from project "+testProject) {
		t.Fatalf("an inherited objective was shown as a declared one: %s", findings[0].Detail)
	}
}

func TestACriticalDesignationRaisesAWarning(t *testing.T) {
	// The second half of the wiring: the designation decides how loudly the
	// rest of the catalogue speaks about this environment.
	round := Findings{
		{Severity: SeverityWarning, Detail: "p95 250ms", Scope: Scope{
			Kind: ScopeEnvironment, Project: testProject, Environment: testProduction}},
		{Severity: SeverityWarning, Detail: "p95 250ms", Scope: Scope{
			Kind: ScopeEnvironment, Project: testProject, Environment: testEnvironment}},
		{Severity: SeverityInfo, Detail: "quiet", Scope: Scope{
			Kind: ScopeEnvironment, Project: testProject, Environment: testProduction}},
	}
	round.escalate(map[EnvKey]ContinuityFor{
		{Project: testProject, Environment: testProduction}: {
			Continuity: kitchenv1alpha1.Continuity{Criticality: kitchenv1alpha1.CriticalityCritical},
			Project:    testProject,
		},
		{Project: testProject, Environment: testEnvironment}: {
			// Important is deliberately not escalated: if the middle rung
			// escalated too there would be nothing left at warning.
			Continuity: kitchenv1alpha1.Continuity{Criticality: kitchenv1alpha1.CriticalityImportant},
			Project:    testProject,
		},
	})

	if round[0].Severity != SeverityCritical {
		t.Fatalf("a warning about a critical environment stayed a warning: %+v", round[0])
	}
	if !strings.Contains(round[0].Detail, "designated critical") {
		t.Fatalf("the escalation is silent about why: %s", round[0].Detail)
	}
	if round[1].Severity != SeverityWarning {
		t.Fatalf("an important environment escalated: %+v", round[1])
	}
	if round[2].Severity != SeverityInfo {
		t.Fatalf("an info finding was escalated: %+v", round[2])
	}
}

func TestContinuityFactsResolveEveryEnvironmentOnce(t *testing.T) {
	projects := []kitchenv1alpha1.Project{{
		ObjectMeta: metav1.ObjectMeta{Name: testProject},
		Spec: kitchenv1alpha1.ProjectSpec{
			Criticality: kitchenv1alpha1.CriticalityCritical, RTO: "1h",
		},
	}}
	environments := []kitchenv1alpha1.Environment{{
		ObjectMeta: metav1.ObjectMeta{Name: testProduction},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
			Type:       kitchenv1alpha1.EnvironmentProduction,
		},
	}, {
		ObjectMeta: metav1.ObjectMeta{Name: testEnvironment},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: testProject},
			Type:       kitchenv1alpha1.EnvironmentPreview,
		},
	}}

	facts := ContinuityFacts(projects, environments)
	production := facts[EnvKey{Project: testProject, Environment: testProduction}]
	if production.Criticality != kitchenv1alpha1.CriticalityCritical || production.RTO != "1h" {
		t.Fatalf("production did not resolve to its project's designation: %+v", production)
	}
	preview := facts[EnvKey{Project: testProject, Environment: testEnvironment}]
	if preview.Designated() {
		t.Fatalf("a preview resolved to a designation: %+v", preview)
	}
}
