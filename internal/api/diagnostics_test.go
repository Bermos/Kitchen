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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

const diagnosticsPath = "/api/v1/environments/" + testEnvironment + "/diagnostics"

// crashedPod is what a crash loop looks like by the time anyone opens the
// screen: the container is waiting out its backoff, and the exit that matters
// is the run before this one.
func crashedPod(name string, finished time.Time, reason string, exitCode int32) *corev1.Pod {
	pod := runningPod(name)
	pod.Status.ContainerStatuses[0].RestartCount = 12
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
		Waiting: &corev1.ContainerStateWaiting{
			Reason:  "CrashLoopBackOff",
			Message: "back-off 5m0s restarting failed container",
		},
	}
	pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
		Terminated: &corev1.ContainerStateTerminated{
			ExitCode:   exitCode,
			Reason:     reason,
			StartedAt:  metav1.NewTime(finished.Add(-time.Minute)),
			FinishedAt: metav1.NewTime(finished),
		},
	}
	return pod
}

// runningPod is one of the environment's pods, serving and having never died.
func runningPod(name string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: controller.AppNamespace(feedProject),
			Labels:    map[string]string{controller.LabelEnvironment: testEnvironment},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  controller.AppContainerName,
				Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
					StartedAt: metav1.NewTime(time.Now().Add(-time.Hour)),
				}},
			}},
		},
	}
}

// The crash report is the join: one request, and everything the platform knows
// about the moment a container died comes back assembled.
func TestCrashReportAssemblesEverythingAboutTheCrash(t *testing.T) {
	finished := time.Now().Add(-2 * time.Minute).UTC().Truncate(time.Second)
	h := newHarness(t, nil, append(fixtures(),
		crashedPod("shop-production-7d9f4", finished, "OOMKilled", 137))...)
	h.logs.lines = []clickhouse.LogLine{{Timestamp: finished.Add(-time.Second), Message: "heap limit reached"}}
	h.logs.series = clickhouse.ResourceSeries{MemoryLimitBytes: 536870912, Restarts: 3, OOMKills: 1}
	h.logs.k8sEvents = []clickhouse.K8sEvent{{
		Timestamp: finished, Reason: "OOMKilling", Kind: "Pod", Message: "Memory cgroup out of memory",
	}}
	h.logs.correlated = []clickhouse.Request{{
		Timestamp: finished, Method: http.MethodPost, Path: "/import", Status: 502,
	}}

	res := h.do(t, http.MethodGet, diagnosticsPath+"?logs=5&requests=7", "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", diagnosticsPath, res.Code, res.Body.String())
	}
	body := decode[diagnosticsView](t, res)
	if !body.Crashed || body.Report == nil {
		t.Fatalf("a container was OOMKilled: %+v", body)
	}

	// What died, and the one distinction that changes what the fix is.
	crash := body.Report.Crash
	if !crash.OOMKilled || crash.ExitCode != 137 || crash.Container != controller.AppContainerName {
		t.Errorf("the termination did not survive: %+v", crash)
	}
	if !crash.Previous || crash.Restarts != 12 || !strings.Contains(crash.Waiting, "CrashLoopBackOff") {
		t.Errorf("expected the trajectory of a crash loop: %+v", crash)
	}
	if body.Restarts != 12 {
		t.Errorf("restarts = %d, want the environment's 12", body.Restarts)
	}

	// And the four windows onto it, all present in one answer.
	report := body.Report
	if len(report.Logs) != 1 || len(report.Events) != 1 || len(report.Requests) != 1 {
		t.Fatalf("the report is the join, and it is missing a half: %+v", report)
	}
	if report.Resources.MemoryLimitBytes == 0 || report.Resources.OOMKills != 1 {
		t.Errorf("the memory series and the restart trajectory belong in the report: %+v", report.Resources)
	}

	// The lines are the dead container's own, and they stop where it died.
	selection := h.logs.lastFilter
	if !strings.Contains(selection.Query, "pod:shop-production-7d9f4") ||
		!strings.Contains(selection.Query, "container:"+controller.AppContainerName) {
		t.Errorf("the log selection should name the pod that died: %q", selection.Query)
	}
	if !selection.Until.Equal(finished) || !selection.Since.Equal(finished.Add(-crashLookback)) {
		t.Errorf("the lines are the ones before the termination instant: %+v", selection)
	}
	if selection.Limit != 5 || h.logs.lastCorrelated.Limit != 7 {
		t.Errorf("the limits did not reach the store: %d, %d", selection.Limit, h.logs.lastCorrelated.Limit)
	}
	// The events keep arriving after the exit — the BackOff is the cluster
	// naming the loop — so their window runs past the instant.
	if !h.logs.lastK8sEvents.Until.Equal(finished.Add(crashLookahead)) {
		t.Errorf("the events window should outlast the crash: %+v", h.logs.lastK8sEvents)
	}
	if !h.logs.lastCorrelated.At.Equal(finished) || h.logs.lastCorrelated.Environment != testEnvironment {
		t.Errorf("the edge's requests are the ones around the instant: %+v", h.logs.lastCorrelated)
	}
}

// Nothing crashed is an answer, not an empty report: four empty sections would
// read as evidence the platform lost rather than as a healthy environment.
func TestDiagnosticsWhenNothingHasCrashed(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), runningPod("shop-production-8a1c2"))...)

	res := h.do(t, http.MethodGet, diagnosticsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", diagnosticsPath, res.Code, res.Body.String())
	}
	body := decode[diagnosticsView](t, res)
	if body.Crashed || body.Report != nil {
		t.Fatalf("nothing terminated abnormally: %+v", body)
	}
	if !strings.Contains(body.Message, "no crash to report") {
		t.Errorf("the answer should say so plainly: %q", body.Message)
	}
	// And it costs the store nothing to say it.
	if h.logs.lastCorrelated.Environment != "" {
		t.Errorf("a healthy environment should not be assembled: %+v", h.logs.lastCorrelated)
	}
}

// An environment with no pods at all is a different answer again, and the one
// the reader has to be sent somewhere else for.
func TestDiagnosticsWithoutPods(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	res := h.do(t, http.MethodGet, diagnosticsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", diagnosticsPath, res.Code, res.Body.String())
	}
	body := decode[diagnosticsView](t, res)
	if body.Crashed || !strings.Contains(body.Message, "no pods") {
		t.Errorf("expected the no-pods answer, got %+v", body)
	}
}

// A rollout leaves several pods behind; the report is about the most recent
// death, not whichever pod the API server listed first.
func TestTheCrashReportIsAboutTheNewestTermination(t *testing.T) {
	newest := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	oldest := newest.Add(-time.Hour)
	h := newHarness(t, nil, append(fixtures(),
		crashedPod("shop-production-old", oldest, "Error", 1),
		crashedPod("shop-production-new", newest, "Error", 2))...)

	res := h.do(t, http.MethodGet, diagnosticsPath, "")
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", diagnosticsPath, res.Code, res.Body.String())
	}
	body := decode[diagnosticsView](t, res)
	if body.Report == nil || body.Report.Crash.Pod != "shop-production-new" {
		t.Fatalf("expected the newest crash, got %+v", body.Report)
	}
	if body.Report.Crash.OOMKilled || body.Report.Crash.ExitCode != 2 {
		t.Errorf("a plain crash is not an OOM kill: %+v", body.Report.Crash)
	}
	if body.Restarts != 24 {
		t.Errorf("restarts = %d, want both pods' 24", body.Restarts)
	}
}

// One half of the report failing fails the request, naming the read: a section
// that silently came back empty would be read as "nothing was logged".
func TestACrashReportIsAllOrNothing(t *testing.T) {
	finished := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	h := newHarness(t, nil, append(fixtures(),
		crashedPod("shop-production-7d9f4", finished, "Error", 1))...)
	h.logs.filterErr = &clickhouse.QueryError{Message: "Code: 47. DB::Exception: Unknown identifier"}

	res := h.do(t, http.MethodGet, diagnosticsPath, "")
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "the crash log query failed") {
		t.Errorf("the answer should name the read that failed: %s", res.Body.String())
	}
}
