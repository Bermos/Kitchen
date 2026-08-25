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
	"bufio"
	"context"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// A build's output, read off its own pod.
//
// Every log on the platform is read out of the telemetry store: the collector
// ships every container's output into ClickHouse, which is what makes a
// build's log outlive the pod that wrote it and a preview's logs outlive the
// preview. A build is the one place that is not enough on its own.
//
// The collector is a DaemonSet, and a DaemonSet whose pods are refused at
// admission has no pods at all — it looks healthy and files nothing. A build
// that failed with no log is then a build nobody can diagnose, on exactly the
// installation that has not been looked at yet. And even where the collector
// is well, a build that failed in its first seconds can finish before its
// first line has been shipped.
//
// So while the pod is still there — the job keeps it for its TTL — it is asked
// too. The store stays the source of record and answers first; this answers
// when it has nothing to say.

// podLogLines is a source of log lines: everything since a point in time,
// oldest first. Nil is a source that does not exist.
type podLogLines func(ctx context.Context, since time.Time) ([]clickhouse.LogLine, error)

// PodLogReader opens a container's output.
//
// It is a function because container logs are a subresource served by the
// kubelet, which the manager's cached client does not speak — and because a
// test has no kubelet. Nil means the API answers builds' logs from the
// telemetry store alone, which is what it did before this existed.
type PodLogReader func(ctx context.Context, namespace, pod, container string, since time.Time) (io.ReadCloser, error)

// ClientsetPodLogs reads container logs through a typed client. It is
// exported because the operator's main is what has the rest configuration to
// build one from.
func ClientsetPodLogs(clientset kubernetes.Interface) PodLogReader {
	return func(ctx context.Context, namespace, pod, container string, since time.Time) (io.ReadCloser, error) {
		options := &corev1.PodLogOptions{
			Container: container,
			// The line's own time, from the container runtime. Without it a
			// followed tail has nothing to advance its window by.
			Timestamps: true,
			// A build that printed a hundred megabytes is not going to be
			// read to the end by anybody, and reading it would be this
			// process's memory rather than the reader's.
			LimitBytes: ptr.To(int64(livePodLogBytes)),
		}
		if since.IsZero() {
			options.TailLines = ptr.To(int64(livePodLogLines))
		} else {
			options.SinceTime = ptr.To(metav1.NewTime(since))
		}
		return clientset.CoreV1().Pods(namespace).GetLogs(pod, options).Stream(ctx)
	}
}

const (
	// livePodLogLines bounds the first read of a build's live log, which is
	// the whole of it for a build that has just started and a tail for one
	// that has been going for ten minutes.
	livePodLogLines = 2000

	// livePodLogBytes bounds it again by size: one line of a bundler's output
	// can be longer than a thousand of anything else.
	livePodLogBytes = 4 << 20
)

// buildPodLines is the live source for one build's log, or nil when there is
// no pod to read: the job has been collected, or the installation does not
// grant the log subresource.
//
// The container is chosen rather than merged. A build pod is a clone and a
// builder, and interleaving two containers' output by the timestamps two
// different processes wrote would produce a log that is neither. The builder
// is what a reader wants; the clone is what they want when the builder never
// ran.
func (s *Server) buildPodLines(
	ctx context.Context, build *kitchenv1alpha1.Build, query clickhouse.LogQuery,
) podLogLines {
	if s.PodLogs == nil {
		return nil
	}
	pod, container := s.buildPodContainer(ctx, build)
	if pod == nil || container == "" {
		return nil
	}
	// A read narrowed to a container this pod is not running has an answer,
	// and the answer is nothing rather than the other container's output.
	if query.Container != "" && query.Container != container {
		return nil
	}
	template := clickhouse.LogLine{
		Source:    clickhouse.SourceBuild,
		Project:   build.Spec.ProjectRef.Name,
		Build:     build.Name,
		Pod:       pod.Name,
		Container: container,
		Stream:    "stdout",
	}
	return func(ctx context.Context, since time.Time) ([]clickhouse.LogLine, error) {
		if since.IsZero() {
			since = query.Since
		}
		stream, err := s.PodLogs(ctx, pod.Namespace, pod.Name, container, since)
		if err != nil {
			// A pod deleted between the two calls, or a cluster that will not
			// serve its logs, is not an error the reader can act on: the
			// store's answer — which is usually the whole log — stands.
			return nil, nil
		}
		defer func() { _ = stream.Close() }()
		return narrow(parsePodLog(stream, template, since), query), nil
	}
}

// narrow applies the parts of the query the kubelet cannot: the search term,
// the upper bound of the window, and the limit. The store applies all three in
// SQL, and a live read that ignored them would answer a different question
// from the one the same request answers a minute later.
func narrow(lines []clickhouse.LogLine, query clickhouse.LogQuery) []clickhouse.LogLine {
	search := strings.ToLower(query.Search)
	kept := lines[:0]
	for _, line := range lines {
		if !query.Until.IsZero() && line.Timestamp.After(query.Until) {
			continue
		}
		if search != "" && !strings.Contains(strings.ToLower(line.Message), search) {
			continue
		}
		kept = append(kept, line)
	}
	// The newest lines are what a limit keeps — a log is read from its end.
	if query.Limit > 0 && len(kept) > query.Limit {
		kept = kept[len(kept)-query.Limit:]
	}
	return kept
}

// buildPodContainer is the build's pod and the container worth reading: the
// builder, unless it never ran, in which case whatever did.
func (s *Server) buildPodContainer(ctx context.Context, build *kitchenv1alpha1.Build) (*corev1.Pod, string) {
	pods := &corev1.PodList{}
	namespace := controller.AppNamespace(build.Spec.ProjectRef.Name)
	if err := s.reader().List(ctx, pods, client.InNamespace(namespace),
		client.MatchingLabels{"job-name": build.Name}); err != nil {
		return nil, ""
	}
	if len(pods.Items) == 0 {
		return nil, ""
	}
	pod := &pods.Items[len(pods.Items)-1]
	return pod, readableContainer(pod)
}

// readableContainer is the container of a build pod whose output is the
// build's log: the builder, unless it has not started, in which case the clone
// in front of it is the whole of the build so far. Empty before anything has
// run, when there is nothing to read.
func readableContainer(pod *corev1.Pod) string {
	for i := len(pod.Status.ContainerStatuses) - 1; i >= 0; i-- {
		status := &pod.Status.ContainerStatuses[i]
		if status.State.Running != nil || status.State.Terminated != nil {
			return status.Name
		}
	}
	for i := len(pod.Status.InitContainerStatuses) - 1; i >= 0; i-- {
		status := &pod.Status.InitContainerStatuses[i]
		if status.State.Running != nil || status.State.Terminated != nil {
			return status.Name
		}
	}
	return ""
}

// parsePodLog turns `<RFC3339Nano> <message>` lines into the same shape the
// store answers with, so that a reader cannot tell which of the two it got.
//
// Lines at or before `since` are dropped: SinceTime is a second-resolution
// filter on the kubelet's side, so a followed tail would otherwise re-send the
// second it resumed from. The stream loop deduplicates too, and this is what
// keeps it from having to.
func parsePodLog(r io.Reader, template clickhouse.LogLine, since time.Time) []clickhouse.LogLine {
	lines := []clickhouse.LogLine{}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64<<10), livePodLogBytes)
	for scanner.Scan() {
		at, message, ok := strings.Cut(strings.TrimRight(scanner.Text(), "\r"), " ")
		if !ok {
			continue
		}
		timestamp, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			continue
		}
		if !since.IsZero() && !timestamp.After(since) {
			continue
		}
		line := template
		line.Timestamp = timestamp
		line.Message = message
		lines = append(lines, line)
	}
	return lines
}
