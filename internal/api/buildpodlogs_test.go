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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Bermos/Kitchen/internal/clickhouse"
)

func TestParsePodLog(t *testing.T) {
	out := strings.Join([]string{
		"2026-08-25T10:00:00.000000000Z installing dependencies",
		"2026-08-25T10:00:01.500000000Z ERROR: failed to build: exit status 1",
		"not a log line at all",
		"2026-08-25T10:00:02.000000000Z ",
	}, "\n")

	lines := parsePodLog(strings.NewReader(out), clickhouse.LogLine{
		Source: clickhouse.SourceBuild, Build: "shop-bld-abc", Container: "creator",
	}, time.Time{})

	if len(lines) != 3 {
		t.Fatalf("parsePodLog() returned %d lines, want 3: %+v", len(lines), lines)
	}
	if lines[0].Message != "installing dependencies" {
		t.Errorf("first message = %q", lines[0].Message)
	}
	if lines[0].Build != "shop-bld-abc" || lines[0].Container != "creator" {
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
