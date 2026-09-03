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
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The memory limit every resource test in this file runs against: 512Mi, the
// size the log collector was OOM-killing itself at.
const testMemoryLimit = 512 << 20

func TestCrashLoopFiresOnTheContainerAndCarriesTheRestartCount(t *testing.T) {
	snapshot := newSnapshot()
	pod := waitingPod(reasonCrashLoopBackOff, "back-off 5m0s restarting failed container")
	pod.Status.ContainerStatuses[0].RestartCount = 12
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{
			Reason:     reasonOOMKilled,
			ExitCode:   137,
			FinishedAt: metav1.NewTime(testNow.Add(-2 * time.Minute)),
		},
	}
	snapshot.Pods = []corev1.Pod{pod}
	snapshot.Resources[testKey] = resourcesOf(testMemoryLimit, 1,
		func(i int) clickhouse.ResourcePoint {
			if i >= resourceBuckets-6 {
				return clickhouse.ResourcePoint{Restarts: 2}
			}
			return clickhouse.ResourcePoint{}
		})

	finding := expectOne(t, evaluate(t, SignalCrashLoop, snapshot))
	if finding.Fingerprint != "workload.crashloop/shop/pr-41/web" {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
	if finding.Title != "crash-looping" {
		t.Fatalf("title = %q", finding.Title)
	}
	// The strip renders `title (first clause of detail)`, which is what makes
	// "crash-looping (12 restarts in 30m)" one line.
	if !strings.HasPrefix(finding.Detail, "12 restarts in 30m") {
		t.Fatalf("detail does not lead with the restart count: %s", finding.Detail)
	}
	expectDetail(t, finding, "the memory limit, not the program")
}

// A container that exits and restarts fast enough never enters
// CrashLoopBackOff, because the kubelet's back-off resets. The restart delta
// is the only thing that sees it.
func TestCrashLoopFiresOnRestartsWithNoBackoff(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Pods = []corev1.Pod{readyPod()}
	snapshot.Resources[testKey] = resourcesOf(testMemoryLimit, 1,
		func(i int) clickhouse.ResourcePoint {
			if i >= resourceBuckets-6 {
				return clickhouse.ResourcePoint{Restarts: 3}
			}
			return clickhouse.ResourcePoint{}
		})

	finding := expectOne(t, evaluate(t, SignalCrashLoop, snapshot))
	if finding.Fingerprint != "workload.crashloop/shop/pr-41" {
		t.Fatalf("fingerprint = %q, want the environment-scoped one", finding.Fingerprint)
	}
	expectDetail(t, finding, "no container is in CrashLoopBackOff")
}

// One problem, one row: the environment-scoped restart finding steps aside
// where a container already reported the same restarts.
func TestCrashLoopDoesNotReportTheSameRestartsTwice(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Pods = []corev1.Pod{waitingPod(reasonCrashLoopBackOff, "back-off")}
	snapshot.Resources[testKey] = resourcesOf(testMemoryLimit, 1,
		func(int) clickhouse.ResourcePoint { return clickhouse.ResourcePoint{Restarts: 3} })

	if findings := evaluate(t, SignalCrashLoop, snapshot); len(findings) != 1 {
		t.Fatalf("expected one finding, got %s", describe(findings))
	}
}

func TestCrashLoopStaysQuietOnAHealthyEnvironment(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Pods = []corev1.Pod{readyPod()}
	snapshot.Resources[testKey] = resourcesOf(testMemoryLimit, 1,
		func(int) clickhouse.ResourcePoint { return clickhouse.ResourcePoint{} })
	expectNone(t, evaluate(t, SignalCrashLoop, snapshot))
}

func TestOOMKilledFiresAndNamesTheLimit(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Resources[testKey] = resourcesOf(testMemoryLimit, 1,
		func(i int) clickhouse.ResourcePoint {
			if i == resourceBuckets-2 {
				return clickhouse.ResourcePoint{OOMKills: 1}
			}
			return clickhouse.ResourcePoint{}
		})

	finding := expectOne(t, evaluate(t, SignalOOMKilled, snapshot))
	expectDetail(t, finding, "1 OOM kill in 1h")
	expectDetail(t, finding, "memory limit 512Mi")
}

func TestOOMKilledStaysQuietWithoutKills(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Resources[testKey] = resourcesOf(testMemoryLimit, 1,
		func(int) clickhouse.ResourcePoint { return clickhouse.ResourcePoint{} })
	expectNone(t, evaluate(t, SignalOOMKilled, snapshot))
}

func TestNearMemoryLimitFiresWhenSustained(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Resources[testKey] = resourcesOf(testMemoryLimit, 1,
		func(i int) clickhouse.ResourcePoint {
			fraction := 0.5
			if i >= resourceBuckets-4 {
				fraction = 0.96
			}
			return clickhouse.ResourcePoint{MemoryPeakBytes: uint64(fraction * testMemoryLimit)}
		})

	finding := expectOne(t, evaluate(t, SignalNearMemoryLimit, snapshot))
	// The diagnostics strip's own example sentence.
	if finding.Title != "memory at 96% of limit" {
		t.Fatalf("title = %q", finding.Title)
	}
	expectDetail(t, finding, "of the 512Mi limit")
}

// One bad bucket is a spike. This is the difference between the rule and a
// threshold comparison.
func TestNearMemoryLimitIgnoresASpike(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Resources[testKey] = resourcesOf(testMemoryLimit, 1,
		func(i int) clickhouse.ResourcePoint {
			fraction := 0.5
			if i == resourceBuckets-1 {
				fraction = 0.96
			}
			return clickhouse.ResourcePoint{MemoryPeakBytes: uint64(fraction * testMemoryLimit)}
		})
	expectNone(t, evaluate(t, SignalNearMemoryLimit, snapshot))
}

// An environment whose release sets no limits cannot be near one, and the
// node's capacity is not a substitute for a ceiling nobody configured.
func TestNearMemoryLimitStaysQuietWithoutALimit(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Resources[testKey] = resourcesOf(0, 0, func(int) clickhouse.ResourcePoint {
		return clickhouse.ResourcePoint{MemoryPeakBytes: 900 << 20}
	})
	expectNone(t, evaluate(t, SignalNearMemoryLimit, snapshot))
}

func TestAtCPULimitFiresWhenPinned(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Resources[testKey] = resourcesOf(testMemoryLimit, 2,
		func(i int) clickhouse.ResourcePoint {
			cores := 0.4
			if i >= resourceBuckets-4 {
				cores = 1.98
			}
			return clickhouse.ResourcePoint{CPUPeakCores: cores}
		})

	finding := expectOne(t, evaluate(t, SignalAtCPULimit, snapshot))
	expectDetail(t, finding, "throttled")
}

func TestImagePullFires(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Pods = []corev1.Pod{
		waitingPod(reasonImagePullBackOff, `Back-off pulling image "registry.example.com/shop:sha-1234"`),
	}
	finding := expectOne(t, evaluate(t, SignalImagePull, snapshot))
	expectDetail(t, finding, "registry.example.com/shop:sha-1234")
}

func TestImagePullIgnoresAStartingContainer(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Pods = []corev1.Pod{waitingPod("ContainerCreating", "")}
	expectNone(t, evaluate(t, SignalImagePull, snapshot))
}

func TestUnschedulableFiresPastTheGraceAndCarriesTheSchedulersReason(t *testing.T) {
	snapshot := newSnapshot()
	pod := appPod(testPodName)
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodScheduled,
		Status:             corev1.ConditionFalse,
		Reason:             "Unschedulable",
		Message:            "0/3 nodes are available: 3 Insufficient memory",
		LastTransitionTime: metav1.NewTime(testNow.Add(-20 * time.Minute)),
	}}
	snapshot.Pods = []corev1.Pod{*pod}

	finding := expectOne(t, evaluate(t, SignalUnschedulable, snapshot))
	expectDetail(t, finding, "3 Insufficient memory")
}

func TestUnschedulableWaitsOutTheGrace(t *testing.T) {
	snapshot := newSnapshot()
	pod := appPod(testPodName)
	pod.Status.Phase = corev1.PodPending
	pod.Status.Conditions = []corev1.PodCondition{{
		Type:               corev1.PodScheduled,
		Status:             corev1.ConditionFalse,
		Reason:             "Unschedulable",
		LastTransitionTime: metav1.NewTime(testNow.Add(-10 * time.Second)),
	}}
	snapshot.Pods = []corev1.Pod{*pod}
	expectNone(t, evaluate(t, SignalUnschedulable, snapshot))
}

// The PodSecurity lesson: a DaemonSet whose pods are refused at admission has
// no pods at all, so the pod list looks clean and the rejection is only ever
// an event on the workload.
func TestAdmissionRefusedNamesPodSecurity(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.DaemonSets = []appsv1.DaemonSet{{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "kitchen-collector",
			Namespace: controller.PlatformNamespace,
		},
		Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 3, NumberAvailable: 0},
	}}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{refusedEvent(
		controller.PlatformNamespace, "DaemonSet", "kitchen-collector",
		`Error creating: pods "kitchen-collector-" is forbidden: `+
			`violates PodSecurity "baseline:latest": hostPath volumes`)}

	finding := expectOne(t, evaluate(t, SignalAdmissionRefused, snapshot))
	expectDetail(t, finding, "pod-security.kubernetes.io/enforce")
	if finding.Fingerprint != "workload.admission-refused/kitchen-system/kitchen-collector" {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
	if !strings.HasPrefix(finding.Evidence, EvidencePlatformEvents) {
		t.Fatalf("evidence = %q, want the events explorer", finding.Evidence)
	}
}

func TestAdmissionRefusedStaysQuietWhenPodsExist(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Deployments = []appsv1.Deployment{deployment(2, 1)}
	snapshot.Pods = []corev1.Pod{readyPod()}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{{
		Timestamp: testNow.Add(-time.Minute),
		Namespace: controller.AppNamespace(testProject),
		Kind:      "Deployment",
		Name:      testEnvironment,
		Reason:    reasonFailedCreate,
		Message:   "Error creating: exceeded quota",
	}}
	expectNone(t, evaluate(t, SignalAdmissionRefused, snapshot))
}

// The same lesson on a Job, which is where it is hardest to see: an install
// that is refused leaves a Job reporting nothing, no pod for anything else to
// report on, and a platform feature that simply never arrives.
func TestAdmissionRefusedReportsARefusedPlatformJob(t *testing.T) {
	const name = "kitchen-keda-install"
	snapshot := newSnapshot()
	snapshot.Jobs = []batchv1.Job{installJob(name)}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{refusedEvent(
		controller.PlatformNamespace, "Job", name,
		`Error creating: pods "kitchen-keda-install-" is forbidden: `+
			`violates PodSecurity "baseline:latest": hostPath volumes`)}

	finding := expectOne(t, evaluate(t, SignalAdmissionRefused, snapshot))
	expectDetail(t, finding, "Job wants 1 and has none")
	expectDetail(t, finding, "pod-security.kubernetes.io/enforce")
	if finding.Fingerprint != "workload.admission-refused/kitchen-system/"+name {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
}

// An environment's own Job is the developer's, and it is named: two of an
// environment's runs refused at once are two findings, and neither of them is
// the environment's Deployment.
func TestAdmissionRefusedNamesAnEnvironmentsJob(t *testing.T) {
	const name = "shop-pr-41-migrate-3"
	snapshot := newSnapshot()
	snapshot.Jobs = []batchv1.Job{runJob(name)}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{refusedEvent(
		controller.AppNamespace(testProject), "Job", name,
		"Error creating: pods is forbidden: exceeded quota: shop, requested: pods=1")}

	finding := expectOne(t, evaluate(t, SignalAdmissionRefused, snapshot))
	expectDetail(t, finding, "ResourceQuota")
	if finding.Scope.Kind != ScopeEnvironment || finding.Scope.Name != name {
		t.Fatalf("scope = %+v, want the environment's, naming the Job", finding.Scope)
	}
	if finding.Fingerprint != "workload.admission-refused/"+testProject+"/"+testEnvironment+"/"+name {
		t.Fatalf("fingerprint = %q", finding.Fingerprint)
	}
}

// A build's Job is build.stalled's to report: that rule names the Build, this
// one would name a Job whose name nobody chose.
func TestAdmissionRefusedLeavesABuildJobAlone(t *testing.T) {
	snapshot := newSnapshot()
	job := runJob("shop-build-9f2c1a")
	job.Labels[labelBuild] = "shop-9f2c1a"
	snapshot.Jobs = []batchv1.Job{job}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{refusedEvent(
		job.Namespace, "Job", job.Name,
		`Error creating: pods "shop-build-9f2c1a-" is forbidden: violates PodSecurity "baseline:latest"`)}

	expectNone(t, evaluate(t, SignalAdmissionRefused, snapshot))
}

// A Job nothing on the platform created is nothing for the platform to report
// on. A cluster's Deployments are almost all Kitchen's; its Jobs are not.
func TestAdmissionRefusedIgnoresAJobKitchenDidNotCreate(t *testing.T) {
	snapshot := newSnapshot()
	job := installJob("nightly-backup")
	job.Labels = map[string]string{"app.kubernetes.io/part-of": "someone-elses-backup"}
	snapshot.Jobs = []batchv1.Job{job}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{refusedEvent(
		job.Namespace, "Job", job.Name, "Error creating: exceeded quota")}

	expectNone(t, evaluate(t, SignalAdmissionRefused, snapshot))
}

// A Job that has run has no pods because it is done, which is the ordinary
// state of most Jobs in a cluster and the one this rule must never report.
func TestAdmissionRefusedSkipsAFinishedJob(t *testing.T) {
	snapshot := newSnapshot()
	job := installJob("kitchen-keda-install")
	job.Status.Succeeded = 1
	job.Status.CompletionTime = &metav1.Time{Time: testNow.Add(-40 * time.Minute)}
	snapshot.Jobs = []batchv1.Job{job}
	// The first attempt was refused and the second was not: the event is still
	// in the window, and the Job is finished.
	snapshot.ClusterEvents = []clickhouse.K8sEvent{refusedEvent(
		job.Namespace, "Job", job.Name, "Error creating: exceeded quota")}

	expectNone(t, evaluate(t, SignalAdmissionRefused, snapshot))
}

// A Job whose pod exists — or existed — is not refused, whatever the pod went
// on to do. The counters are read rather than the pod list, because an
// environment's Job shares its namespace and labels with the Deployment
// serving it.
func TestAdmissionRefusedSkipsAJobWithPods(t *testing.T) {
	snapshot := newSnapshot()
	job := runJob("shop-pr-41-migrate-3")
	job.Status.Failed = 1
	snapshot.Jobs = []batchv1.Job{job}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{refusedEvent(
		job.Namespace, "Job", job.Name, "Error creating: exceeded quota")}

	expectNone(t, evaluate(t, SignalAdmissionRefused, snapshot))
}

// Creating a pod takes a moment on a busy cluster. A Job younger than the
// grace has not failed to create one yet.
func TestAdmissionRefusedWaitsTheGraceOnAYoungJob(t *testing.T) {
	snapshot := newSnapshot()
	job := installJob("kitchen-keda-install")
	job.CreationTimestamp = metav1.NewTime(testNow.Add(-30 * time.Second))
	snapshot.Jobs = []batchv1.Job{job}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{refusedEvent(
		job.Namespace, "Job", job.Name, "Error creating: exceeded quota")}

	expectNone(t, evaluate(t, SignalAdmissionRefused, snapshot))
}

// A suspended Job wants no pods, and the other two rules over workloads never
// see a Job at all: a run in progress is not an environment failing to serve.
func TestJobsReachOnlyTheAdmissionRule(t *testing.T) {
	snapshot := newSnapshot()
	suspended := installJob("kitchen-rescan")
	suspended.Spec.Suspend = ptr(true)
	snapshot.Jobs = []batchv1.Job{suspended, runJob("shop-pr-41-migrate-3")}
	snapshot.ClusterEvents = []clickhouse.K8sEvent{refusedEvent(
		suspended.Namespace, "Job", suspended.Name, "Error creating: exceeded quota")}

	expectNone(t, evaluate(t, SignalAdmissionRefused, snapshot))
	expectNone(t, evaluate(t, SignalNotReady, snapshot))
	expectNone(t, evaluate(t, SignalRTOAtRisk, snapshot))
}

func TestNotReadyFiresPastTheGrace(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Deployments = []appsv1.Deployment{deployment(3, 1)}
	snapshot.Pods = []corev1.Pod{readyPod()}

	finding := expectOne(t, evaluate(t, SignalNotReady, snapshot))
	if finding.Title != "1 of 3 pods available" {
		t.Fatalf("title = %q", finding.Title)
	}
	if finding.Severity != SeverityWarning {
		t.Fatalf("severity = %q, want warning while something is still serving", finding.Severity)
	}
}

func TestNotReadyIsCriticalWhenNothingServes(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Deployments = []appsv1.Deployment{deployment(3, 0)}
	snapshot.Pods = []corev1.Pod{*appPod(testPodName)}

	finding := expectOne(t, evaluate(t, SignalNotReady, snapshot))
	if finding.Severity != SeverityCritical {
		t.Fatalf("severity = %q, want critical", finding.Severity)
	}
}

func TestNotReadyToleratesARollout(t *testing.T) {
	snapshot := newSnapshot()
	rolling := deployment(3, 2)
	rolling.CreationTimestamp = metav1.NewTime(testNow.Add(-time.Minute))
	rolling.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:               appsv1.DeploymentAvailable,
		Status:             corev1.ConditionFalse,
		LastTransitionTime: metav1.NewTime(testNow.Add(-time.Minute)),
	}}
	snapshot.Deployments = []appsv1.Deployment{rolling}
	snapshot.Pods = []corev1.Pod{readyPod()}
	expectNone(t, evaluate(t, SignalNotReady, snapshot))
}

func TestErrorRateFiresAgainstItsOwnBaseline(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Traffic[testKey] = trafficOf(func(i int) clickhouse.RequestPoint {
		if i >= recentFrom {
			return clickhouse.RequestPoint{Requests: 100, Errors: 30, ErrorRate: 0.30}
		}
		return clickhouse.RequestPoint{Requests: 100, Errors: 1, ErrorRate: 0.01}
	})

	finding := expectOne(t, evaluate(t, SignalErrorRate, snapshot))
	if finding.Title != "failing 30% of requests" {
		t.Fatalf("title = %q", finding.Title)
	}
	expectDetail(t, finding, "baseline 1%")
}

// An application that always answers 6% errors is not news every fifteen
// minutes forever.
func TestErrorRateIsExcusedByAServicesOwnNormal(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Traffic[testKey] = trafficOf(func(int) clickhouse.RequestPoint {
		return clickhouse.RequestPoint{Requests: 100, Errors: 6, ErrorRate: 0.06}
	})
	expectNone(t, evaluate(t, SignalErrorRate, snapshot))
}

// Below the minimum, one failure is already past the threshold by arithmetic.
func TestErrorRateRefusesToJudgeTooLittleTraffic(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Traffic[testKey] = trafficOf(func(i int) clickhouse.RequestPoint {
		if i >= recentFrom {
			return clickhouse.RequestPoint{Requests: 1, Errors: 1, ErrorRate: 1}
		}
		return clickhouse.RequestPoint{Requests: 100, ErrorRate: 0}
	})
	expectNone(t, evaluate(t, SignalErrorRate, snapshot))
}

func TestLatencyRegressedFiresWhenSustained(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Traffic[testKey] = trafficOf(func(i int) clickhouse.RequestPoint {
		latency := 300.0
		if i >= recentFrom {
			latency = 1400
		}
		return clickhouse.RequestPoint{Requests: 100, P95Ms: latency}
	})

	finding := expectOne(t, evaluate(t, SignalLatencyRegressed, snapshot))
	expectDetail(t, finding, "against a 300ms baseline")
}

// A doubling below the floor is a tripling of nothing.
func TestLatencyRegressedIgnoresFastServices(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Traffic[testKey] = trafficOf(func(i int) clickhouse.RequestPoint {
		latency := 4.0
		if i >= recentFrom {
			latency = 15
		}
		return clickhouse.RequestPoint{Requests: 100, P95Ms: latency}
	})
	expectNone(t, evaluate(t, SignalLatencyRegressed, snapshot))
}

func TestTrafficVanishedFires(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Traffic[testKey] = trafficOf(func(i int) clickhouse.RequestPoint {
		if i >= recentFrom {
			return clickhouse.RequestPoint{}
		}
		return clickhouse.RequestPoint{Requests: 100}
	})

	finding := expectOne(t, evaluate(t, SignalTrafficVanished, snapshot))
	expectDetail(t, finding, "a DNS record, a certificate or a route")
}

// An environment that never had traffic has not lost any.
func TestTrafficVanishedIgnoresAnIdleEnvironment(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Traffic[testKey] = trafficOf(func(int) clickhouse.RequestPoint {
		return clickhouse.RequestPoint{}
	})
	expectNone(t, evaluate(t, SignalTrafficVanished, snapshot))
}

func TestNoBackendFires(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Traffic[testKey] = trafficOf(func(i int) clickhouse.RequestPoint {
		if i >= recentFrom {
			return clickhouse.RequestPoint{Requests: 20, Errors: 20, ErrorRate: 1}
		}
		return clickhouse.RequestPoint{Requests: 100}
	})
	snapshot.Pods = []corev1.Pod{*appPod(testPodName)}

	finding := expectOne(t, evaluate(t, SignalNoBackend, snapshot))
	expectDetail(t, finding, "no ready pod")
}

func TestNoBackendStaysQuietWhileAPodServes(t *testing.T) {
	snapshot := newSnapshot()
	snapshot.Traffic[testKey] = trafficOf(func(i int) clickhouse.RequestPoint {
		if i >= recentFrom {
			return clickhouse.RequestPoint{Requests: 20, Errors: 20, ErrorRate: 1}
		}
		return clickhouse.RequestPoint{Requests: 100}
	})
	snapshot.Pods = []corev1.Pod{readyPod()}
	expectNone(t, evaluate(t, SignalNoBackend, snapshot))
}

// A platform pod's fingerprint must not carry the replica-set hash, or every
// rollout would resolve and reopen the same problem.
func TestPlatformPodScopeIsStableAcrossRollouts(t *testing.T) {
	first := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "kitchen-auth-7f9c8d-abcde",
		Namespace: controller.PlatformNamespace,
		Labels:    map[string]string{labelComponentKind: "auth"},
	}}
	second := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "kitchen-auth-9a1b2c-fghij",
		Namespace: controller.PlatformNamespace,
		Labels:    map[string]string{labelComponentKind: "auth"},
	}}
	if podScope(first, testContainer).Path() != podScope(second, testContainer).Path() {
		t.Fatalf("scope moved with the replica set: %q vs %q",
			podScope(first, testContainer).Path(), podScope(second, testContainer).Path())
	}
}
