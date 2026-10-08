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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func terminated(name string, exit int32, reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: exit, Reason: reason,
		}},
	}
}

func waiting(name, reason, message string) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}},
	}
}

// A buildpacks pod is a clone that worked in front of a builder that did not,
// which is the shape that makes reading containers in order the wrong answer.
func TestFailureFromPodBlamesTheContainerThatFailed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		pod           corev1.Pod
		wantContainer string
		wantExit      *int32
		wantReason    string
		wantMessage   string
	}{
		{
			name: "the builder behind a clone that succeeded",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				InitContainerStatuses: []corev1.ContainerStatus{terminated("clone", 0, "Completed")},
				ContainerStatuses:     []corev1.ContainerStatus{terminated("creator", 51, "Error")},
			}},
			wantContainer: "creator",
			wantExit:      ptr.To(int32(51)),
			wantReason:    "Error",
			wantMessage:   "creator exited 51",
		},
		{
			name: "the clone, when it is the clone",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				InitContainerStatuses: []corev1.ContainerStatus{terminated("clone", 128, "Error")},
				ContainerStatuses:     []corev1.ContainerStatus{waiting("creator", reasonPodInitializing, "")},
			}},
			wantContainer: "clone",
			wantExit:      ptr.To(int32(128)),
			wantReason:    "Error",
			wantMessage:   "clone exited 128",
		},
		{
			name: "a container that never started",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{
					waiting("creator", "ImagePullBackOff", "back-off pulling image"),
				},
			}},
			wantContainer: "creator",
			wantReason:    "ImagePullBackOff",
			wantMessage:   "creator never started: ImagePullBackOff: back-off pulling image",
		},
		{
			name: "an ending no container can explain",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:                 corev1.PodFailed,
				Reason:                "Evicted",
				Message:               "the node was low on ephemeral-storage",
				InitContainerStatuses: []corev1.ContainerStatus{terminated("clone", 0, "Completed")},
				ContainerStatuses:     []corev1.ContainerStatus{terminated("creator", 0, "Completed")},
			}},
			wantReason:  "Evicted",
			wantMessage: "Evicted: the node was low on ephemeral-storage",
		},
		{
			name: "the kubelet's own account of the exit",
			pod: corev1.Pod{Status: corev1.PodStatus{
				Phase:             corev1.PodFailed,
				ContainerStatuses: []corev1.ContainerStatus{terminated("creator", 137, "OOMKilled")},
			}},
			wantContainer: "creator",
			wantExit:      ptr.To(int32(137)),
			wantReason:    "OOMKilled",
			wantMessage:   "creator exited 137 (OOMKilled)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failure := failureFromPod(&tc.pod)
			if failure == nil {
				t.Fatal("failureFromPod() = nil, want a failure")
			}
			if failure.Container != tc.wantContainer {
				t.Errorf("container = %q, want %q", failure.Container, tc.wantContainer)
			}
			if tc.wantExit == nil && failure.ExitCode != nil {
				t.Errorf("exitCode = %d, want none", *failure.ExitCode)
			}
			if tc.wantExit != nil && (failure.ExitCode == nil || *failure.ExitCode != *tc.wantExit) {
				t.Errorf("exitCode = %v, want %d", failure.ExitCode, *tc.wantExit)
			}
			if failure.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", failure.Reason, tc.wantReason)
			}
			if failure.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", failure.Message, tc.wantMessage)
			}
		})
	}
}

// A pod that says nothing is not a failure this can invent one for: the
// caller falls back to the Job's own sentence rather than to a guess.
func TestFailureFromPodSaysNothingWhenItHasNothing(t *testing.T) {
	pod := corev1.Pod{Status: corev1.PodStatus{
		Phase:             corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{Name: "creator", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}},
	}}
	if failure := failureFromPod(&pod); failure != nil {
		t.Fatalf("failureFromPod() = %+v, want nil", failure)
	}
	if got := failureMessage(nil, "Job has reached the specified backoff limit"); got != "Job has reached the specified backoff limit" {
		t.Errorf("failureMessage() = %q, want the job's own message", got)
	}
}

// The log a node with user.max_user_namespaces=0 produces says "no space left
// on device" and never names the sysctl, which is the whole reason it is
// recognised here. These are the lines a Talos node actually printed.
func TestUserNamespacesDeniedIsReadOffRootlesskit(t *testing.T) {
	denied := &kitchenv1alpha1.BuildFailureStatus{
		Container: "buildkit",
		ExitCode:  ptr.To(int32(1)),
		Message:   "buildkit exited 1",
		Log: []string{
			"could not connect to unix:///run/user/1000/buildkit/buildkitd.sock after 10 trials",
			"========== log ==========",
			`level=warning msg="[rootlesskit:parent] /proc/sys/user/max_user_namespaces needs to be set to non-zero."`,
			"[rootlesskit:parent] error: failed to start the child: fork/exec /proc/self/exe: no space left on device",
		},
	}
	if !userNamespacesDenied(denied) {
		t.Fatal("userNamespacesDenied() = false for the log the failure actually produces")
	}

	message := userNamespacesDeniedMessage(denied)
	for _, want := range []string{
		"buildkit", "user namespaces", "user.max_user_namespaces", "buildpacks",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the message does not mention %q: %s", want, message)
		}
	}
}

// Everything else keeps the failure it already had. The disk case is the one
// that matters: "no space left on device" from a builder that really did fill
// a volume is not a sysctl, which is why rootlesskit has to be named too.
func TestUserNamespacesDeniedLeavesOtherFailuresAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure *kitchenv1alpha1.BuildFailureStatus
	}{
		{
			name: "a build the repository broke",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Container: "buildkit",
				Message:   "buildkit exited 1",
				Log:       []string{`ERROR: process "/bin/sh -c go build ./..." did not complete successfully: exit code: 2`},
			},
		},
		{
			name: "a volume that really is full",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Container: "creator",
				Message:   "creator exited 1",
				Log:       []string{"failed to export: write /layers/app.tgz: no space left on device"},
			},
		},
		{name: "no failure at all", failure: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if userNamespacesDenied(tc.failure) {
				t.Errorf("userNamespacesDenied(%+v) = true, want false", tc.failure)
			}
		})
	}
}

// The two sentences a BuildKit push ended with on an installation whose
// bundled registry had filled its volume: zot ran out of room mid-upload,
// deleted what it had, and the client heard about an upload that no longer
// existed or a blob that no longer matched. These are the lines it printed.
const (
	pushUploadUnknown = "error: failed to solve: failed to push registry.bermos.dev/enterprise-migrate:59a47e2ac570: " +
		"unknown: blob upload unknown to registry"
	pushDigestMismatch = "error: failed to solve: failed to push registry.bermos.dev/enterprise-migrate:5a22bbc091fa: " +
		"unknown: provided digest did not match uploaded content"
	testRegistryHost = "registry.bermos.dev"
)

func TestRegistryRejectedUploadIsReadOffThePush(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failure *kitchenv1alpha1.BuildFailureStatus
		want    bool
		phrase  string
	}{
		{
			name: "BuildKit, the upload gone",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Container: "buildkit", ExitCode: ptr.To(int32(1)), Message: "buildkit exited 1",
				Log: []string{"#12 pushing layers 4.1s done", pushUploadUnknown},
			},
			want:   true,
			phrase: "blob upload unknown to registry",
		},
		{
			name: "BuildKit, the digest not matching",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Container: "buildkit", ExitCode: ptr.To(int32(1)), Message: "buildkit exited 1",
				Log: []string{pushDigestMismatch},
			},
			want:   true,
			phrase: "provided digest did not match uploaded content",
		},
		{
			// go-containerregistry, which the lifecycle's exporter pushes
			// with, relays the same registry message behind the spec's code.
			name: "the buildpacks exporter, in go-containerregistry's words",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Container: "exporter", ExitCode: ptr.To(int32(62)), Message: "exporter exited 62",
				Log: []string{
					"ERROR: failed to export: saving image: failed to write image to the following tags: " +
						"[registry.bermos.dev/shop:abc123: PATCH https://registry.bermos.dev/v2/shop/blobs/uploads/x: " +
						"BLOB_UPLOAD_UNKNOWN: blob upload unknown to registry]",
				},
			},
			want:   true,
			phrase: "blob upload unknown to registry",
		},
		{
			name: "a build the repository broke",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Container: "buildkit", Message: "buildkit exited 1",
				Log: []string{`ERROR: process "/bin/sh -c go build ./..." did not complete successfully: exit code: 2`},
			},
		},
		{
			// The builder filling its own disk is the repository's failure,
			// and it shares only the errno with a full registry.
			name: "a builder that filled its own disk",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Container: "creator", Message: "creator exited 1",
				Log: []string{"failed to export: write /layers/app.tgz: no space left on device"},
			},
		},
		{
			name: "a push refused for its credential",
			failure: &kitchenv1alpha1.BuildFailureStatus{
				Container: "buildkit", Message: "buildkit exited 1",
				Log: []string{"error: failed to solve: failed to push registry.bermos.dev/shop:abc: " +
					"unexpected status from HEAD request: 401 Unauthorized"},
			},
		},
		{name: "no failure at all"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := registryRejectedUpload(tc.failure); got != tc.want {
				t.Errorf("registryRejectedUpload() = %v, want %v", got, tc.want)
			}
			if got := registryUploadLostPhrase(tc.failure); got != tc.phrase {
				t.Errorf("registryUploadLostPhrase() = %q, want %q", got, tc.phrase)
			}
		})
	}
}

// What the build says is read by every member of the project, so it states
// what happened and what it most often means; only the platform's own
// registry gets the sentence about where its storage is shown, and nothing
// in it is operator vocabulary.
func TestRegistryRejectedUploadMessage(t *testing.T) {
	failure := &kitchenv1alpha1.BuildFailureStatus{
		Container: "buildkit", ExitCode: ptr.To(int32(1)), Message: "buildkit exited 1",
		Log: []string{pushDigestMismatch},
	}
	for _, tc := range []struct {
		name   string
		target pushTarget
		want   []string
		absent []string
	}{
		{
			name:   "the platform's own registry",
			target: pushTarget{Host: testRegistryHost, Bundled: true},
			want: []string{
				"buildkit could not push the image",
				"the registry at " + testRegistryHost,
				"discarded an upload it had already accepted",
				`"provided digest did not match uploaded content"`,
				"storage is full",
				"the platform's own registry",
				"Storage screen",
			},
		},
		{
			name:   "a registry the platform does not run",
			target: pushTarget{Host: "harbor.example.com"},
			want: []string{
				"buildkit could not push the image",
				"the registry at harbor.example.com",
				"storage is full",
			},
			absent: []string{"Storage screen", "platform's own registry"},
		},
		{
			name:   "a registry with no host to name",
			target: pushTarget{},
			want:   []string{"buildkit could not push the image: the registry discarded"},
			absent: []string{"Storage screen"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := registryRejectedUploadMessage(failure, tc.target)
			for _, want := range tc.want {
				if !strings.Contains(message, want) {
					t.Errorf("the message does not say %q: %s", want, message)
				}
			}
			for _, absent := range append(tc.absent, "PVC", "PersistentVolumeClaim", "kubectl", "namespace") {
				if strings.Contains(message, absent) {
					t.Errorf("the message says %q: %s", absent, message)
				}
			}
		})
	}
}

// The restatement is wired into the one place a failed build's message is
// decided, and it keeps the reason of a build that failed: it ran, and the
// image it made had nowhere to go.
func TestRestateFailureExplainsARegistryThatLostTheUpload(t *testing.T) {
	build := &kitchenv1alpha1.Build{Status: kitchenv1alpha1.BuildStatus{
		Failure: &kitchenv1alpha1.BuildFailureStatus{
			Container: "buildkit", ExitCode: ptr.To(int32(1)), Message: "buildkit exited 1",
			Log: []string{pushUploadUnknown},
		},
	}}
	reason, message := restateFailure(&kitchenv1alpha1.Project{}, build,
		planOutcome{Message: "Job has reached the specified backoff limit"}, "",
		pushTarget{Host: testRegistryHost, Bundled: true})
	if reason != reasonBuildFailed {
		t.Errorf("reason = %q, want %q", reason, reasonBuildFailed)
	}
	if !strings.Contains(message, "storage is full") || !strings.Contains(message, "Storage screen") {
		t.Errorf("message = %q, want the registry's storage named", message)
	}
	if build.Status.Failure.Message != message {
		t.Errorf("failure.message = %q, want the restated message %q", build.Status.Failure.Message, message)
	}
	if len(build.Status.Failure.Log) != 1 || build.Status.Failure.Log[0] != pushUploadUnknown {
		t.Errorf("failure.log = %q, want the builder's own line kept", build.Status.Failure.Log)
	}
}

// The bundled registry is the Connection the operator recorded seeding and
// still labels as its own. A Connection of the same name somebody made by
// hand, one in another namespace, or a Kitchen object that cannot be read
// all make an ordinary registry, which is the reading that names no remedy.
func TestPushTargetOfRecognisesTheBundledRegistry(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Status: kitchenv1alpha1.KitchenStatus{Registry: &kitchenv1alpha1.ImageRegistryStatus{
			Host: testRegistryHost, Connection: RegistryConnectionName,
		}},
	}
	connection := func(name, namespace string, platforms bool) *kitchenv1alpha1.Connection {
		conn := &kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		if platforms {
			conn.Labels = map[string]string{labelManagedByKey: labelManagedByValue}
		}
		return conn
	}
	for _, tc := range []struct {
		name    string
		objects []client.Object
		conn    *kitchenv1alpha1.Connection
		want    bool
	}{
		{
			name:    "the seeded connection",
			objects: []client.Object{kitchen},
			conn:    connection(RegistryConnectionName, PlatformNamespace, true),
			want:    true,
		},
		{
			name:    "the same name, created by hand",
			objects: []client.Object{kitchen},
			conn:    connection(RegistryConnectionName, PlatformNamespace, false),
		},
		{
			name:    "another registry the platform labelled",
			objects: []client.Object{kitchen},
			conn:    connection("harbor", PlatformNamespace, true),
		},
		{
			name:    "outside the platform namespace",
			objects: []client.Object{kitchen},
			conn:    connection(RegistryConnectionName, "elsewhere", true),
		},
		{
			name: "no Kitchen object to ask",
			conn: connection(RegistryConnectionName, PlatformNamespace, true),
		},
		{name: "no connection at all", objects: []client.Object{kitchen}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &BuildReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objects...).Build()}
			target := r.pushTargetOf(context.Background(), tc.conn, testRegistryHost)
			if target.Bundled != tc.want {
				t.Errorf("bundled = %v, want %v", target.Bundled, tc.want)
			}
			if target.Host != testRegistryHost {
				t.Errorf("host = %q, want %q", target.Host, testRegistryHost)
			}
		})
	}
}

// The kubelet's termination message is a file the builder wrote, so it is
// somebody else's bytes and gets bounded like any other.
func TestTerminatedMessageBoundsWhatTheBuilderWrote(t *testing.T) {
	long := strings.Repeat("x", buildFailureMessageMax*2)
	message := terminatedMessage("creator", &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error", Message: long})
	if !strings.HasSuffix(message, "…") {
		t.Errorf("message = %q, want it truncated", message[:64])
	}
	if len(message) > buildFailureMessageMax+64 {
		t.Errorf("message is %d bytes, want it bounded near %d", len(message), buildFailureMessageMax)
	}
}

func TestTailLines(t *testing.T) {
	for _, tc := range []struct {
		name  string
		out   string
		limit int
		want  []string
	}{
		{name: "nothing at all", out: "", limit: 5},
		{name: "only blank lines", out: "\n\n\n", limit: 5},
		{
			name:  "the last of them, oldest first",
			out:   "one\ntwo\nthree\nfour\n",
			limit: 2,
			want:  []string{"three", "four"},
		},
		{
			name:  "carriage returns and trailing blanks removed",
			out:   "\nfirst\r\nsecond\r\n\n",
			limit: 5,
			want:  []string{"first", "second"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tailLines(tc.out, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("tailLines() = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// The pod that failed is the one worth reading, even when the job left
// another behind.
func TestFailedPodPrefersTheOneThatFailed(t *testing.T) {
	pods := []corev1.Pod{
		{Status: corev1.PodStatus{Phase: corev1.PodRunning}},
		{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: "Evicted"}},
	}
	pod := failedPod(pods)
	if pod == nil || pod.Status.Reason != "Evicted" {
		t.Fatalf("failedPod() = %+v, want the evicted one", pod)
	}
	if failedPod(nil) != nil {
		t.Error("failedPod(nil) should be nil")
	}
}
