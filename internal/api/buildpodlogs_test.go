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
	"io"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

func TestParsePodLog(t *testing.T) {
	out := strings.Join([]string{
		"2026-08-25T10:00:00.000000000Z installing dependencies",
		"2026-08-25T10:00:01.500000000Z ERROR: failed to build: exit status 1",
		"not a log line at all",
		"2026-08-25T10:00:02.000000000Z ",
	}, "\n")

	lines := parsePodLog(strings.NewReader(out), clickhouse.LogLine{
		Source: clickhouse.SourceBuild, Build: multiBuild, Container: "creator",
	}, time.Time{})

	if len(lines) != 3 {
		t.Fatalf("parsePodLog() returned %d lines, want 3: %+v", len(lines), lines)
	}
	if lines[0].Message != "installing dependencies" {
		t.Errorf("first message = %q", lines[0].Message)
	}
	if lines[0].Build != multiBuild || lines[0].Container != "creator" {
		t.Errorf("the template's fields did not travel: %+v", lines[0])
	}
	if !lines[1].Timestamp.Equal(time.Date(2026, 8, 25, 10, 0, 1, 500000000, time.UTC)) {
		t.Errorf("second timestamp = %s", lines[1].Timestamp)
	}
	// A line whose message is empty is still a line the container printed.
	if lines[2].Message != "" {
		t.Errorf("third message = %q, want empty", lines[2].Message)
	}
}

// SinceTime filters to the second on the kubelet's side, so a followed tail
// asks for the second it stopped at and has to drop what it already sent.
func TestParsePodLogDropsWhatItAlreadySent(t *testing.T) {
	since := time.Date(2026, 8, 25, 10, 0, 1, 0, time.UTC)
	out := strings.Join([]string{
		"2026-08-25T10:00:00.900000000Z before",
		"2026-08-25T10:00:01.000000000Z exactly at the boundary",
		"2026-08-25T10:00:01.100000000Z after",
	}, "\n")

	lines := parsePodLog(strings.NewReader(out), clickhouse.LogLine{}, since)
	if len(lines) != 1 || lines[0].Message != "after" {
		t.Fatalf("parsePodLog() = %+v, want only the line after the boundary", lines)
	}
}

// A live read has to answer the same question the store answers, or the same
// request would mean two different things a minute apart.
func TestNarrowAppliesWhatTheKubeletCannot(t *testing.T) {
	at := func(second int) time.Time { return time.Date(2026, 8, 25, 10, 0, second, 0, time.UTC) }
	lines := []clickhouse.LogLine{
		{Timestamp: at(1), Message: "installing dependencies"},
		{Timestamp: at(2), Message: "ERROR: could not resolve"},
		{Timestamp: at(3), Message: "error: and again"},
		{Timestamp: at(4), Message: "done"},
	}

	kept := narrow(append([]clickhouse.LogLine(nil), lines...), clickhouse.LogQuery{Search: "ERROR"})
	if len(kept) != 2 {
		t.Fatalf("search kept %d lines, want 2 — the match is case-insensitive: %+v", len(kept), kept)
	}

	kept = narrow(append([]clickhouse.LogLine(nil), lines...), clickhouse.LogQuery{Until: at(2)})
	if len(kept) != 2 || kept[1].Message != "ERROR: could not resolve" {
		t.Fatalf("until kept %+v, want the window's own lines", kept)
	}

	kept = narrow(append([]clickhouse.LogLine(nil), lines...), clickhouse.LogQuery{Limit: 2})
	if len(kept) != 2 || kept[0].Message != "error: and again" {
		t.Fatalf("limit kept %+v, want the newest two", kept)
	}
}

func TestBuildPodContainerPrefersTheBuilder(t *testing.T) {
	running := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
	done := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}

	for _, tc := range []struct {
		name string
		pod  corev1.Pod
		want string
	}{
		{
			name: "the builder, once it is running",
			pod: corev1.Pod{Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{{Name: "clone", State: done}},
				ContainerStatuses:     []corev1.ContainerStatus{{Name: "creator", State: running}},
			}},
			want: "creator",
		},
		{
			name: "the clone, while it is all there is",
			pod: corev1.Pod{Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{{Name: "clone", State: running}},
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "creator",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
				}},
			}},
			want: "clone",
		},
		{
			name: "nothing, before anything has started",
			pod: corev1.Pod{Status: corev1.PodStatus{
				InitContainerStatuses: []corev1.ContainerStatus{{
					Name:  "clone",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ContainerCreating"}},
				}},
			}},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := readableContainer(&tc.pod); got != tc.want {
				t.Errorf("readableContainer() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The build every test here reads, and the pods its Jobs made. They are
// spelled once because a merge is only right if the same names come back on
// the right lines.
const (
	multiBuild    = "shop-bld-abc"
	webPod        = multiBuild + "-pod"
	apiJob        = multiBuild + "-api"
	apiPod        = apiJob + "-pod"
	workerJob     = multiBuild + "-worker"
	workerPod     = workerJob + "-pod"
	workerOneLine = "worker: one"
)

// The live tail of a build that is several images: every workload's pod is
// read, the lines are merged in the order they were written, and each one
// says which Job wrote it — the same way the stored rows do.
func TestBuildPodLinesMergesEveryWorkload(t *testing.T) {
	build := multiWorkloadBuild()
	server := podLogServer(t, map[string]string{
		webPod: strings.Join([]string{
			"2026-08-25T10:00:00.000000000Z web: installing dependencies",
			"2026-08-25T10:00:03.000000000Z web: pushed",
		}, "\n"),
		apiPod: strings.Join([]string{
			"2026-08-25T10:00:01.000000000Z api: installing dependencies",
			"2026-08-25T10:00:04.000000000Z api: ERROR: exit status 1",
		}, "\n"),
		workerPod: "2026-08-25T10:00:02.000000000Z worker: pushed\n",
	}, build)

	lines := server.buildPodLines(t.Context(), build, clickhouse.LogQuery{})
	if lines == nil {
		t.Fatal("buildPodLines() = nil, want a source over three pods")
	}
	got, err := lines(t.Context(), time.Time{})
	if err != nil {
		t.Fatalf("reading the merged tail: %v", err)
	}

	want := []struct{ message, run, pod string }{
		{"web: installing dependencies", multiBuild, webPod},
		{"api: installing dependencies", apiJob, apiPod},
		{"worker: pushed", workerJob, workerPod},
		{"web: pushed", multiBuild, webPod},
		{"api: ERROR: exit status 1", apiJob, apiPod},
	}
	if len(got) != len(want) {
		t.Fatalf("merged tail has %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i, expected := range want {
		if got[i].Message != expected.message {
			t.Errorf("line %d = %q, want %q — the merge is by timestamp", i, got[i].Message, expected.message)
		}
		// `run` is the Job's name on the stored rows too, so a reader cannot
		// tell the live tail from the history it turns into.
		if got[i].Run != expected.run || got[i].Pod != expected.pod {
			t.Errorf("line %d is attributed to run %q pod %q, want %q and %q",
				i, got[i].Run, got[i].Pod, expected.run, expected.pod)
		}
		if got[i].Build != multiBuild || got[i].Project != feedProject {
			t.Errorf("line %d lost the build it belongs to: %+v", i, got[i])
		}
	}
}

// The limit is the merged tail's end, not each pod's — four ends are not one.
func TestBuildPodLinesLimitsTheMergeRatherThanEachPod(t *testing.T) {
	build := multiWorkloadBuild()
	server := podLogServer(t, map[string]string{
		webPod: strings.Join([]string{
			"2026-08-25T10:00:00.000000000Z web: one",
			"2026-08-25T10:00:03.000000000Z web: two",
		}, "\n"),
		apiPod:    "2026-08-25T10:00:01.000000000Z api: one\n",
		workerPod: "2026-08-25T10:00:04.000000000Z worker: one\n",
	}, build)

	got, err := server.buildPodLines(t.Context(), build, clickhouse.LogQuery{Limit: 2})(t.Context(), time.Time{})
	if err != nil {
		t.Fatalf("reading the merged tail: %v", err)
	}
	if len(got) != 2 || got[0].Message != "web: two" || got[1].Message != workerOneLine {
		t.Fatalf("limit kept %+v, want the newest two lines of the merge", got)
	}
}

// A read narrowed to a container answers with that container's output or with
// nothing — never with another one's.
func TestBuildPodLinesNarrowedToAContainer(t *testing.T) {
	build := multiWorkloadBuild()
	output := map[string]string{
		webPod:    "2026-08-25T10:00:00.000000000Z web: one\n",
		apiPod:    "2026-08-25T10:00:01.000000000Z api: one\n",
		workerPod: "2026-08-25T10:00:02.000000000Z " + workerOneLine + "\n",
	}

	// The workloads' pods run `creator`; the web process's runs `buildkit`.
	server := podLogServer(t, output, build)
	got, err := server.buildPodLines(t.Context(), build, clickhouse.LogQuery{Container: "creator"})(
		t.Context(), time.Time{})
	if err != nil {
		t.Fatalf("reading the narrowed tail: %v", err)
	}
	if len(got) != 2 || got[0].Message != "api: one" || got[1].Message != workerOneLine {
		t.Fatalf("narrowing to creator kept %+v, want the two pods running it", got)
	}

	server = podLogServer(t, output, build)
	if source := server.buildPodLines(t.Context(), build, clickhouse.LogQuery{Container: "nothing"}); source != nil {
		lines, _ := source(t.Context(), time.Time{})
		t.Fatalf("a container no build pod runs answered %+v, want nothing", lines)
	}

	// `run` narrows to one workload's Job, which is what it does on the
	// stored rows — the live tail cannot answer a different question.
	server = podLogServer(t, output, build)
	got, err = server.buildPodLines(t.Context(), build, clickhouse.LogQuery{Run: apiJob})(
		t.Context(), time.Time{})
	if err != nil {
		t.Fatalf("reading the narrowed tail: %v", err)
	}
	if len(got) != 1 || got[0].Message != "api: one" {
		t.Fatalf("narrowing to one run kept %+v, want that Job's line alone", got)
	}
}

// The great majority of builds are one image, and their tail is what it was:
// one pod, read whole.
func TestBuildPodLinesSingleWorkloadIsUnchanged(t *testing.T) {
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: multiBuild, Namespace: testNamespace},
		Spec:       kitchenv1alpha1.BuildSpec{ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: feedProject}},
	}
	server := podLogServer(t, map[string]string{
		webPod: strings.Join([]string{
			"2026-08-25T10:00:00.000000000Z installing dependencies",
			"2026-08-25T10:00:01.000000000Z pushed",
		}, "\n"),
	}, build)

	got, err := server.buildPodLines(t.Context(), build, clickhouse.LogQuery{})(t.Context(), time.Time{})
	if err != nil {
		t.Fatalf("reading the tail: %v", err)
	}
	if len(got) != 2 || got[0].Message != "installing dependencies" || got[1].Message != "pushed" {
		t.Fatalf("single-workload tail = %+v, want the pod's two lines in order", got)
	}
	if got[0].Container != "buildkit" || got[0].Pod != webPod || got[0].Run != multiBuild {
		t.Errorf("single-workload line is attributed wrongly: %+v", got[0])
	}

	// A build whose pods have all been collected has no live source at all,
	// and the store's answer stands alone.
	empty := podLogServer(t, map[string]string{}, build)
	empty.Client = fake.NewClientBuilder().WithScheme(podLogScheme(t)).Build()
	if source := empty.buildPodLines(t.Context(), build, clickhouse.LogQuery{}); source != nil {
		t.Error("buildPodLines() over a build with no pods left, want nil")
	}
}

// One workload's pod that cannot be read does not take the others' output
// with it: a Job collected mid-read is not a reason to answer nothing.
func TestBuildPodLinesSurvivesOnePodItCannotRead(t *testing.T) {
	build := multiWorkloadBuild()
	server := podLogServer(t, map[string]string{
		webPod:    "2026-08-25T10:00:00.000000000Z web: one\n",
		workerPod: "2026-08-25T10:00:02.000000000Z " + workerOneLine + "\n",
	}, build)

	got, err := server.buildPodLines(t.Context(), build, clickhouse.LogQuery{})(t.Context(), time.Time{})
	if err != nil {
		t.Fatalf("reading the merged tail: %v", err)
	}
	if len(got) != 2 || got[0].Message != "web: one" || got[1].Message != workerOneLine {
		t.Fatalf("merged tail = %+v, want the two pods that answered", got)
	}
}

// multiWorkloadBuild is one commit that built three images: the project's own
// and two workloads', each in its own Job.
func multiWorkloadBuild() *kitchenv1alpha1.Build {
	return &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: multiBuild, Namespace: testNamespace},
		Spec:       kitchenv1alpha1.BuildSpec{ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: feedProject}},
		Status: kitchenv1alpha1.BuildStatus{Workloads: []kitchenv1alpha1.WorkloadBuildStatus{
			{Name: "api", Job: apiJob},
			{Name: "worker", Job: workerJob},
		}},
	}
}

func podLogScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the client-go scheme: %v", err)
	}
	return scheme
}

// podLogServer is a Server whose cluster is the build pods of `build` — one
// per Job it has — and whose kubelet answers with `output`, keyed by pod name.
// A pod missing from `output` is one whose logs the cluster will not serve.
func podLogServer(t *testing.T, output map[string]string, build *kitchenv1alpha1.Build) *Server {
	t.Helper()
	namespace := controller.AppNamespace(build.Spec.ProjectRef.Name)
	// The web process's Job builds with BuildKit; a workload's runs the
	// buildpacks lifecycle, so the two are not the same container.
	containers := map[string]string{build.Name + "-pod": "buildkit"}
	pods := []runtime.Object{buildJobPod(namespace, build.Name, "buildkit")}
	for _, workload := range build.Status.Workloads {
		containers[workload.Job+"-pod"] = "creator"
		pods = append(pods, buildJobPod(namespace, workload.Job, "creator"))
	}
	return &Server{
		Client:    fake.NewClientBuilder().WithScheme(podLogScheme(t)).WithRuntimeObjects(pods...).Build(),
		Namespace: testNamespace,
		PodLogs: func(_ context.Context, ns, pod, container string, _ time.Time) (io.ReadCloser, error) {
			if ns != namespace {
				return nil, fmt.Errorf("read from namespace %q, want %q", ns, namespace)
			}
			// The container is part of the read: a stub that answered
			// whatever it was asked for would not notice a merge that read
			// the wrong one.
			if want := containers[pod]; container != want {
				return nil, fmt.Errorf("read container %q of pod %q, want %q", container, pod, want)
			}
			out, ok := output[pod]
			if !ok {
				return nil, fmt.Errorf("pod %q has no logs", pod)
			}
			return io.NopCloser(strings.NewReader(out)), nil
		},
	}
}

// buildJobPod is the pod one build Job made, running one container.
func buildJobPod(namespace, job, container string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job + "-pod",
			Namespace: namespace,
			Labels:    map[string]string{"job-name": job},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  container,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}},
		}}},
	}
}
