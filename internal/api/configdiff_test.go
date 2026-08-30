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
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The rollback diff (#181). Two properties are worth pinning from the outside,
// and one of them is the reason the route exists at all:
//
//  1. it says what would change, in the direction somebody about to write is
//     asking about — the release in the path is where the environment is
//     going, `against` is where it is now;
//  2. **no value ever appears in the answer.** The whole point of computing
//     the comparison here is that the literals do not have to travel to be
//     compared, and a body carrying one would be a leak the dashboard would
//     happily render.

const configDiffPath = "/api/v1/releases/" + testPreviousRelease + "/config-diff?against=" + testRelease

// snapshotSecret is the literal the "changed" variable holds in the newer
// release. It is deliberately distinctive so that the leak assertion is
// looking for something that could only have come from a snapshot.
const snapshotSecret = "s3cr3t-cdn-token-eu"

// snapshotMemory is the memory request only the older release carries: the
// runtime half of the diff, asserted on twice and written into the fixture
// once.
const snapshotMemory = "512Mi"

// withSnapshots gives the two fixture releases configurations that differ in
// every way the answer distinguishes: a literal that changed, a variable that
// only the newer one carries, one that only the older one does, one that is
// the same on both, and one whose value never moved but whose *source* did.
func withSnapshots(t *testing.T, h *harness) {
	t.Helper()

	newer := kitchenv1alpha1.ConfigSnapshot{
		Env: []kitchenv1alpha1.EnvVar{
			{Name: "NEXT_PUBLIC_CDN", Value: snapshotSecret},
			{Name: "FEATURE_BULK_IMPORT", Value: "true"},
			{Name: "NODE_ENV", Value: "production"},
			{Name: "DATABASE_URL", FromResourceClaim: &kitchenv1alpha1.ResourceClaimKeySelector{
				Name: "shop-db", Key: "url"}},
			{Name: "SESSION_SECRET", SecretRef: &kitchenv1alpha1.SecretKeySelector{
				Name: "shop-session", Key: "secret"}},
			{Name: "DEBUG_PANEL", Value: "off", PreviewValue: "on"},
		},
		Runtime: kitchenv1alpha1.RuntimeSpec{
			Port:     3000,
			Replicas: ptr.To(int32(3)),
			Command:  []string{"./server"},
			Args:     []string{"--config=prod.toml"},
		},
		Processes: []kitchenv1alpha1.ProcessSpec{
			{Name: "nightly", Type: kitchenv1alpha1.ProcessCron, Schedule: "0 3 * * *"},
			{Name: "mailer", Type: kitchenv1alpha1.ProcessWorker},
		},
	}
	older := kitchenv1alpha1.ConfigSnapshot{
		Env: []kitchenv1alpha1.EnvVar{
			{Name: "NEXT_PUBLIC_CDN", Value: "cdn.example.com"},
			{Name: "NODE_ENV", Value: "production"},
			{Name: "LEGACY_IMPORTER", Value: "1"},
			{Name: "DATABASE_URL", FromResourceClaim: &kitchenv1alpha1.ResourceClaimKeySelector{
				Name: "shop-db", Key: "url"}},
			// Same variable, same purpose, read from a Secret instead of a
			// claim binding: a change no comparison of values would find.
			{Name: "SESSION_SECRET", SecretRef: &kitchenv1alpha1.SecretKeySelector{
				Name: "shop-session", Key: "old-secret"}},
			// The same literal for every environment; only what a preview
			// overrides it with moved.
			{Name: "DEBUG_PANEL", Value: "off", PreviewValue: "verbose"},
		},
		Runtime: kitchenv1alpha1.RuntimeSpec{
			Port:     3000,
			Replicas: ptr.To(int32(2)),
			Command:  []string{"./server"},
			Args:     []string{"--config=old.toml"},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(snapshotMemory)},
			},
		},
		Processes: []kitchenv1alpha1.ProcessSpec{
			{Name: "nightly", Type: kitchenv1alpha1.ProcessCron, Schedule: "0 2 * * *"},
		},
	}

	setSnapshot(t, h, testRelease, newer)
	setSnapshot(t, h, testPreviousRelease, older)
}

// setSnapshot writes a configuration snapshot onto a fixture release. The spec
// is immutable at admission, but the fake client the harness runs against does
// not admit — which is what lets a fixture be given the snapshot a build would
// have frozen onto it.
func setSnapshot(t *testing.T, h *harness, name string, snapshot kitchenv1alpha1.ConfigSnapshot) {
	t.Helper()
	release := &kitchenv1alpha1.Release{}
	key := types.NamespacedName{Namespace: testNamespace, Name: name}
	if err := h.server.Client.Get(context.Background(), key, release); err != nil {
		t.Fatal(err)
	}
	release.Spec.ConfigSnapshot = snapshot
	if err := h.server.Client.Update(context.Background(), release); err != nil {
		t.Fatal(err)
	}
}

func diffBody(t *testing.T, h *harness, path string) configDiffBody {
	t.Helper()
	recorder := h.do(t, http.MethodGet, path, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	body := configDiffBody{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("unreadable body %q: %v", recorder.Body.String(), err)
	}
	return body
}

func variable(t *testing.T, body configDiffBody, name string) variableChangeView {
	t.Helper()
	for _, v := range body.Variables {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no variable %q in %+v", name, body.Variables)
	return variableChangeView{}
}

func TestTheConfigDiffSaysWhatARollbackWouldChange(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	withSnapshots(t, h)

	body := diffBody(t, h, configDiffPath)
	if body.Release != testPreviousRelease || body.Against != testRelease {
		t.Fatalf("want %s against %s, got %s against %s",
			testPreviousRelease, testRelease, body.Release, body.Against)
	}

	// The direction is the write's: the variable the live release sets and
	// the target does not is *removed*, because that is what would happen.
	for name, want := range map[string]string{
		"NEXT_PUBLIC_CDN":     changeChanged,
		"SESSION_SECRET":      changeChanged,
		"FEATURE_BULK_IMPORT": changeRemoved,
		"LEGACY_IMPORTER":     changeAdded,
		"NODE_ENV":            changeUnchanged,
		"DATABASE_URL":        changeUnchanged,
		"DEBUG_PANEL":         changeChanged,
	} {
		if got := variable(t, body, name).Change; got != want {
			t.Errorf("%s: want %s, got %s", name, want, got)
		}
	}

	// A variable that moved between kinds of source says so on both sides,
	// which is the part a diff of values could not have explained.
	session := variable(t, body, "SESSION_SECRET")
	if session.Source != sourceSecret || session.AgainstSource != sourceSecret {
		t.Errorf("want a secret on both sides, got %q and %q", session.Source, session.AgainstSource)
	}
	if session.Ref == nil || session.Ref.Key != "old-secret" {
		t.Errorf("want the target's own key reported, got %+v", session.Ref)
	}
	if session.AgainstRef == nil || session.AgainstRef.Key != "secret" {
		t.Errorf("want the live key reported, got %+v", session.AgainstRef)
	}
	if got := variable(t, body, "DATABASE_URL").Source; got != sourceClaim {
		t.Errorf("want a claim binding, got %q", got)
	}
	if got := variable(t, body, "NEXT_PUBLIC_CDN").Source; got != sourceValue {
		t.Errorf("want a literal, got %q", got)
	}

	// A change to a preview override alone is marked as one. On a production
	// environment it is not a change to production, and a row that did not
	// say so would read as though it were.
	if got := variable(t, body, "DEBUG_PANEL"); !got.PreviewOnly {
		t.Errorf("want a preview-only change, got %+v", got)
	}
	if got := variable(t, body, "NEXT_PUBLIC_CDN"); got.PreviewOnly {
		t.Errorf("want a change to every environment, got %+v", got)
	}

	// What changed is read first, and what did not is read last — both
	// clients render the list in the order it arrives.
	if body.Variables[0].Change != changeChanged {
		t.Errorf("want the changed variables first, got %+v", body.Variables[0])
	}
	if last := body.Variables[len(body.Variables)-1]; last.Change != changeUnchanged {
		t.Errorf("want the unchanged variables last, got %+v", last)
	}
}

// The property the endpoint exists for: the comparison crosses the wire, the
// literals do not. Neither snapshot's values appear anywhere in the answer —
// not the one that changed, not the one that is being removed, not even the
// one that did not move.
func TestTheConfigDiffNeverReadsAValueBack(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	withSnapshots(t, h)

	recorder := h.do(t, http.MethodGet, configDiffPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	answer := recorder.Body.String()
	for _, value := range []string{snapshotSecret, "cdn.example.com", "production", "verbose"} {
		if strings.Contains(answer, value) {
			t.Fatalf("the diff read a stored value back: %q appears in %s", value, answer)
		}
	}
}

func TestTheConfigDiffReportsTheRuntimeAndTheProcesses(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	withSnapshots(t, h)

	body := diffBody(t, h, configDiffPath)

	runtime := map[string]fieldChangeView{}
	for _, field := range body.Runtime {
		runtime[field.Field] = field
	}
	// The replica count and the memory request come back with the release,
	// and both are reported as themselves: a port is not a secret.
	if got := runtime["replicas"]; !got.Changed || got.From != "3" || got.To != "2" {
		t.Errorf("want replicas 3 → 2, got %+v", got)
	}
	if got := runtime["memoryRequest"]; !got.Changed || got.From != "" || got.To != snapshotMemory {
		t.Errorf("want the memory request restored, got %+v", got)
	}
	if got := runtime["port"]; got.Changed {
		t.Errorf("want the port unchanged, got %+v", got)
	}
	// Arguments are configuration, and a rollback that restored the image but
	// not the flags would have restored the wrong thing.
	if got := runtime["args"]; !got.Changed || got.From != "--config=prod.toml" || got.To != "--config=old.toml" {
		t.Errorf("want the arguments reported, got %+v", got)
	}
	if got := runtime["command"]; got.Changed || got.To != "./server" {
		t.Errorf("want the command unchanged and reported, got %+v", got)
	}
	if !body.Runtime[0].Changed {
		t.Errorf("want the changed runtime fields first, got %+v", body.Runtime[0])
	}

	processes := map[string]processChangeView{}
	for _, process := range body.Processes {
		processes[process.Name] = process
	}
	// A rollback that quietly restored yesterday's schedule is exactly the
	// surprise this route exists to prevent, so the schedule travels.
	if got := processes["nightly"]; got.Change != changeChanged || got.Schedule != "0 2 * * *" {
		t.Errorf("want the nightly job's restored schedule, got %+v", got)
	}
	if got := processes["mailer"]; got.Change != changeRemoved || got.Type != string(kitchenv1alpha1.ProcessWorker) {
		t.Errorf("want the mailer described by what it was, got %+v", got)
	}
}

func TestTheConfigDiffRefusesAComparisonThatMeansNothing(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	for name, want := range map[string]string{
		"/api/v1/releases/" + testRelease + "/config-diff":                        "?against=",
		"/api/v1/releases/" + testRelease + "/config-diff?against=" + testRelease: "cannot be compared against itself",
		// Two projects' snapshots are not comparable however holds the token,
		// and the caller here holds every role there is.
		"/api/v1/releases/" + testRelease + "/config-diff?against=blog-rel-0": "belongs to project",
	} {
		recorder := h.do(t, http.MethodGet, name, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: want 400, got %d: %s", name, recorder.Code, recorder.Body.String())
		}
		if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, want) {
			t.Errorf("%s: want a refusal mentioning %q, got %q", name, want, got)
		}
	}
}

// The route is a viewer's read like the release itself, and a member of
// another project is refused it — the authorization is resolved from the
// release in the path, the same as GET /releases/{name}.
func TestTheConfigDiffIsAViewersRead(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	withSnapshots(t, h)

	if recorder := h.do(t, http.MethodGet, configDiffPath, ""); recorder.Code != http.StatusOK {
		t.Fatalf("want a viewer to read it, got %d: %s", recorder.Code, recorder.Body.String())
	}

	stranger := asMember(t, "")
	recorder := stranger.do(t, http.MethodGet, configDiffPath, "")
	if recorder.Code != http.StatusForbidden && recorder.Code != http.StatusNotFound {
		t.Fatalf("want a stranger refused, got %d: %s", recorder.Code, recorder.Body.String())
	}
}
