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
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The crash report: everything the platform knows about a container that died,
// on one screen, assembled here rather than by the person reading it.
//
// The parts exist separately already — the pod's last termination is on the
// workload endpoint, the lines are in the log store, the memory series is on
// the metrics endpoint, the cluster's warnings are in their own table and the
// edge's requests in theirs. What nobody has is the join, and the join is the
// whole feature: the exit code says *what*, the last lines before the
// termination instant say *why*, the memory climbing into the limit says
// whether the kernel was right, and the requests in the window say what was
// being asked of it when it went. Hunting for those five things in five places,
// each with its own window, is the work this endpoint exists to delete.

// The crash report's window: how far back it reads, and how far past the
// termination instant it keeps looking.
//
// Half an hour is what a crash loop's trajectory fits into — it is the span the
// restart signals are written against — and it is enough memory history to
// watch a leak climb into its limit. The trailing five minutes are there
// because the events that explain a crash keep arriving after it: the BackOff
// that follows the exit is the cluster naming the loop.
const (
	crashLookback  = 30 * time.Minute
	crashLookahead = 5 * time.Minute
)

// How much of each half the report carries by default. Both are deliberately
// small: this is one assembled screen, not a log search — `/logs` and
// `…/requests` are where someone goes when fifty is not enough.
const (
	defaultCrashLogLines = 50
	defaultCrashRequests = 50
)

// reasonOOMKilled is the kubelet's word for the kernel having taken the
// decision. It is the one termination reason worth telling apart from every
// other, because the fix is a limit rather than a bug.
const reasonOOMKilled = "OOMKilled"

// reasonPodInitializing is the waiting reason of every container that is
// simply behind an init container. It is progress, not a fault, and telling
// the two apart is what keeps a pod whose *init* container failed from
// reporting the wait instead of the failure.
const reasonPodInitializing = "PodInitializing"

// The two answers there is no report for. They are separate sentences because
// they send the reader somewhere different: one to the environment's own
// conditions, the other nowhere at all.
const (
	noPodsMessage = "this environment has no pods, so nothing has terminated: " +
		"whether anything was ever materialized for it is on the Environment's own conditions"
	nothingCrashedMessage = "no container in this environment has terminated abnormally, " +
		"so there is no crash to report"
)

// diagnosticsView is the answer whether or not anything crashed. A healthy
// environment gets the first five fields and nothing else: an empty report with
// four empty sections would read as "the platform lost the evidence" rather
// than "nothing happened".
type diagnosticsView struct {
	Environment string `json:"environment"`
	Namespace   string `json:"namespace"`
	// Crashed is whether a container terminated abnormally — a non-zero exit,
	// or an OOM kill. A container that exited zero ran to completion.
	Crashed bool `json:"crashed"`
	// Message says why there is no report, and is empty when there is one.
	Message string `json:"message,omitempty"`
	// Restarts is every restart the environment's pods carry right now, read
	// off the API server rather than out of the store: it is the trajectory's
	// current total, exact, and available even where nothing was collected.
	Restarts int32 `json:"restarts"`

	Report *crashReport `json:"report,omitempty"`
}

// crashReport is the assembled view: the termination itself, and the four
// windows onto the moment it happened.
type crashReport struct {
	Crash crashView `json:"crash"`
	// Since and Until bound the assembly. The sections do not all use the whole
	// span, and each says which part it used: the lines and the resource series
	// stop at the termination instant because they are what led up to it, the
	// events run past it because a crash loop keeps announcing itself, and the
	// requests are the seconds either side of it.
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`

	// Logs are the last lines the container that died wrote, oldest first — its
	// last words, which is the one thing no other endpoint can hand over
	// without knowing when it died.
	Logs []clickhouse.LogLine `json:"logs"`
	// Resources is the usage leading up to the termination. It carries the
	// memory series the OOM case is read against — usage against the limit the
	// release set — and, per bucket, the restart trajectory: where in the
	// window the restarts happened rather than how many there have ever been.
	Resources clickhouse.ResourceSeries `json:"resources"`
	// Events are the cluster's Warnings for this environment around the
	// crash — FailedScheduling, BackOff, the OOM kill from the kubelet's side.
	Events []clickhouse.K8sEvent `json:"events"`
	// Requests are what the edge served either side of the instant: a 502 here
	// is the edge noticing the pod go, and a slow 200 just before it is the
	// load that preceded it.
	Requests []clickhouse.Request `json:"requests"`
}

// crashView is the termination itself, in the words the kubelet reported it in.
type crashView struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	// Reason is the kubelet's — OOMKilled, Error, ContainerStatusUnknown — and
	// OOMKilled promotes itself to its own field, because "the kernel killed it
	// for using too much memory" and "it crashed" are different problems with
	// different fixes and the same exit code.
	Reason    string `json:"reason,omitempty"`
	OOMKilled bool   `json:"oomKilled"`
	ExitCode  int32  `json:"exitCode"`
	Signal    int32  `json:"signal,omitempty"`
	// Message is what the container left in its termination message file, which
	// is usually nothing at all.
	Message    string     `json:"message,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt time.Time  `json:"finishedAt"`
	// Restarts is this container's own count, which is the crash loop's rate
	// when read against how long the pod has existed.
	Restarts int32 `json:"restarts"`
	// Waiting is why the container is not running now — the CrashLoopBackOff
	// and its message — and is empty for one that restarted and is serving
	// again.
	Waiting string `json:"waiting,omitempty"`
	// Previous says the termination ended the container's previous run rather
	// than its current one: it died and came back, which is the ordinary shape
	// of a crash loop.
	Previous bool `json:"previous"`
}

// environmentDiagnostics assembles the crash report.
func (s *Server) environmentDiagnostics(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := s.environmentOf(w, req)
	if env == nil {
		return
	}
	lineLimit, err := intParam(req, "logs", defaultCrashLogLines)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	requestLimit, err := intParam(req, "requests", defaultCrashRequests)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	appNS := controller.AppNamespace(env.Spec.ProjectRef.Name)
	pods := &corev1.PodList{}
	if err := s.reader().List(ctx, pods,
		client.InNamespace(appNS),
		client.MatchingLabels{controller.LabelEnvironment: env.Name},
	); err != nil {
		s.writeError(w, err)
		return
	}

	view := diagnosticsView{Environment: env.Name, Namespace: appNS}
	for i := range pods.Items {
		for _, status := range pods.Items[i].Status.ContainerStatuses {
			view.Restarts += status.RestartCount
		}
	}

	crash := newestCrash(pods.Items)
	if crash == nil {
		// Two different nothings: an environment with no pods at all sends the
		// reader to the Environment's own conditions, a healthy one nowhere.
		view.Message = nothingCrashedMessage
		if len(pods.Items) == 0 {
			view.Message = noPodsMessage
		}
		writeJSON(w, http.StatusOK, view)
		return
	}

	// Only now is a store needed: whether anything crashed is the API server's
	// answer, and an installation without telemetry can still be told no.
	store := s.openLogStore(w, req)
	if store == nil {
		return
	}
	report, what, err := assembleCrashReport(ctx, store, env, *crash, lineLimit, requestLimit)
	if err != nil {
		// Four reads answer this endpoint, so the failure names which one it
		// was; the store's own diagnostic stays in the operator's log.
		s.writeStoreError(w, err, what)
		return
	}
	view.Crashed = true
	view.Report = report
	writeJSON(w, http.StatusOK, view)
}

// assembleCrashReport reads the four windows onto the crash, answering the name
// of the read that failed alongside the failure.
//
// It is deliberately all-or-nothing: a report missing a section without saying
// so is worse than an error, because whoever reads it concludes the section was
// empty — that nothing was logged, that no warning was raised.
func assembleCrashReport(
	ctx context.Context,
	store logReader,
	env *kitchenv1alpha1.Environment,
	crash crashView,
	lineLimit, requestLimit int,
) (*crashReport, string, error) {
	project := env.Spec.ProjectRef.Name
	at := crash.FinishedAt
	if at.IsZero() {
		// A termination the kubelet stamped no finish time on is not a reason
		// to read four windows around the year 1. They become the half hour
		// ending now, and the report reports them, so what it covers is never
		// something the reader has to guess at.
		at = time.Now()
	}
	since, until := at.Add(-crashLookback), at.Add(crashLookahead)

	report := &crashReport{Crash: crash, Since: since, Until: until}

	// The lines are the dead container's own, not the environment's: with three
	// replicas serving, two of them healthy, the interleaved output of all
	// three would bury the fifty lines that matter. The selection goes through
	// the query language rather than the raw expression, so the names leave as
	// parameters; they are DNS labels either way, and cannot carry an operator.
	lines, err := store.FilterLogs(ctx, clickhouse.LogFilter{
		LogSelection: clickhouse.LogSelection{
			Query: fmt.Sprintf("project:%s environment:%s pod:%s container:%s",
				project, env.Name, crash.Pod, crash.Container),
			// The report is about one environment of one project, so that is
			// the scope as well as the query: the names in the query narrow
			// the answer, and the scope is what bounds what may be read.
			Scope: clickhouse.LogScope{Projects: []string{project}},
			Since: since,
			Until: at,
		},
		Limit: lineLimit,
	})
	if err != nil {
		return nil, "the crash log query", err
	}
	report.Logs = itemsOf(lines)

	resources, err := store.ResourceSeries(ctx, clickhouse.ResourceSeriesQuery{
		Project:     project,
		Environment: env.Name,
		Since:       since,
		Until:       at,
	})
	if err != nil {
		return nil, "the crash resource history query", err
	}
	report.Resources = resources

	events, err := store.QueryK8sEvents(ctx, clickhouse.K8sEventQuery{
		Project:     project,
		Environment: env.Name,
		Since:       since,
		Until:       until,
	})
	if err != nil {
		return nil, "the cluster event query", err
	}
	report.Events = itemsOf(events)

	// The edge's side of the same moment, at the store's own correlation width
	// — the ±30 seconds the correlated-logs view uses, so the two halves of a
	// crash cover the same span.
	requests, err := store.CorrelatedRequests(ctx, clickhouse.RequestCorrelationQuery{
		Project:     project,
		Environment: env.Name,
		At:          at,
		Limit:       requestLimit,
	})
	if err != nil {
		return nil, "the correlated request query", err
	}
	report.Requests = itemsOf(requests)

	return report, "", nil
}

// newestCrash finds the most recent abnormal termination across an
// environment's pods, or nil where nothing died badly.
//
// Both states are read. A container's *current* termination is one that exited
// and has not been restarted; its *last* termination is the run before the one
// happening now, which is where a crash loop's evidence lives — by the time
// anyone looks, the container is either running again or waiting out its
// backoff, and either way the exit that matters is the previous one.
func newestCrash(pods []corev1.Pod) *crashView {
	var newest *crashView
	for i := range pods {
		pod := &pods[i]
		// Init containers are scanned too: a workload that never starts because
		// its init container dies is exactly the case someone opens this for,
		// and it is invisible in the app container's status.
		for _, statuses := range [][]corev1.ContainerStatus{
			pod.Status.InitContainerStatuses,
			pod.Status.ContainerStatuses,
		} {
			for j := range statuses {
				status := &statuses[j]
				for _, candidate := range []struct {
					terminated *corev1.ContainerStateTerminated
					previous   bool
				}{
					{status.State.Terminated, false},
					{status.LastTerminationState.Terminated, true},
				} {
					crash := crashOf(pod, status, candidate.terminated, candidate.previous)
					if crash == nil {
						continue
					}
					if newest == nil || crash.FinishedAt.After(newest.FinishedAt) {
						newest = crash
					}
				}
			}
		}
	}
	return newest
}

// crashOf reads one termination, or nothing where it was not abnormal. An exit
// of zero is a container that finished what it was doing — a completed job's
// pod, or a sidecar told to stop — and calling that a crash would make the
// report cry wolf on every rollout.
func crashOf(
	pod *corev1.Pod,
	status *corev1.ContainerStatus,
	terminated *corev1.ContainerStateTerminated,
	previous bool,
) *crashView {
	if terminated == nil {
		return nil
	}
	if terminated.ExitCode == 0 && terminated.Reason != reasonOOMKilled {
		return nil
	}
	crash := &crashView{
		Pod:        pod.Name,
		Container:  status.Name,
		Reason:     terminated.Reason,
		OOMKilled:  terminated.Reason == reasonOOMKilled,
		ExitCode:   terminated.ExitCode,
		Signal:     terminated.Signal,
		Message:    strings.TrimSpace(terminated.Message),
		FinishedAt: terminated.FinishedAt.Time,
		Restarts:   status.RestartCount,
		Previous:   previous,
	}
	if !terminated.StartedAt.IsZero() {
		started := terminated.StartedAt.Time
		crash.StartedAt = &started
	}
	if status.State.Waiting != nil {
		crash.Waiting, _ = containerMessage(status)
	}
	return crash
}
