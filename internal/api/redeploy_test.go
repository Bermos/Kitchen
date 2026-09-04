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
	"net/http"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// A corrected setting reaching an environment with no next commit (#392).
//
// The route's whole claim is that the platform can now do what recovery used
// to take two `kubectl delete`s for, so what these cases hold up is that
// claim: a new Release exists, it carries what the project says *now*, the
// environment is on it, and the Release that was running is exactly as it was.

// redeployPath is the route under test, spelled once.
const redeployPath = "/api/v1/environments/" + testEnvironment + "/redeploy"

// correctedSetting is the fix the issue's project was trying to apply: a
// posture the project declares and the running release never froze.
func correctedSetting() *kitchenv1alpha1.SecuritySpec {
	return &kitchenv1alpha1.SecuritySpec{RunAsNonRoot: true, RunAsUser: 1000}
}

// withCorrectedSetting is the fixtures with the project holding a setting its
// running release does not: the state a redeploy exists for.
func withCorrectedSetting(t *testing.T, h *harness) {
	t.Helper()
	project := &kitchenv1alpha1.Project{}
	if err := h.server.get(context.Background(), "shop", project); err != nil {
		t.Fatal(err)
	}
	project.Spec.Runtime.Security = correctedSetting()
	if err := h.server.Client.Update(context.Background(), project); err != nil {
		t.Fatal(err)
	}
}

func TestRedeployCutsAReleaseOfTheSameCommitWithTodaysSettings(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	withCorrectedSetting(t, h)

	recorder := h.do(t, http.MethodPost, redeployPath, "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[redeployView](t, recorder)
	if view.Release == testRelease || view.Release == "" {
		t.Fatalf("a redeploy makes a release of its own, got %q", view.Release)
	}
	if view.PreviousRelease != testRelease || view.Environment != testEnvironment {
		t.Fatalf("the answer does not say where it came from: %+v", view)
	}
	if !strings.HasPrefix(view.Release, testRelease) {
		t.Fatalf("the new release is still named after the commit it is of: %q", view.Release)
	}

	ctx := context.Background()
	fresh := &kitchenv1alpha1.Release{}
	if err := h.server.get(ctx, view.Release, fresh); err != nil {
		t.Fatalf("the release the answer names does not exist: %v", err)
	}
	// The same commit and the same artifact: this is the release that was
	// running, with a different configuration frozen onto it.
	if fresh.Spec.Image != testReleaseImage || fresh.Spec.BuildRef.Name != testBuild {
		t.Fatalf("a redeploy changed the artifact: %+v", fresh.Spec)
	}
	security := fresh.Spec.ConfigSnapshot.Runtime.Security
	if security == nil || security.RunAsUser != 1000 {
		t.Fatalf("the new snapshot does not carry the corrected setting: %+v", fresh.Spec.ConfigSnapshot.Runtime)
	}

	// The release that was running is a snapshot and stays one: rolling back
	// to it has to put back what it deployed.
	previous := &kitchenv1alpha1.Release{}
	if err := h.server.get(ctx, testRelease, previous); err != nil {
		t.Fatal(err)
	}
	if previous.Spec.ConfigSnapshot.Runtime.Security != nil {
		t.Fatalf("the release that was running was edited: %+v", previous.Spec.ConfigSnapshot.Runtime)
	}

	// And the environment is on the new one, which is the half that makes it
	// a deploy rather than a release nobody asked for.
	env := &kitchenv1alpha1.Environment{}
	if err := h.server.get(ctx, testEnvironment, env); err != nil {
		t.Fatal(err)
	}
	if env.Spec.ReleaseRef.Name != view.Release {
		t.Fatalf("the environment is still on %q", env.Spec.ReleaseRef.Name)
	}
	if len(env.Status.History) == 0 || env.Status.History[0].Release != testRelease {
		t.Fatalf("the move was not recorded: %+v", env.Status.History)
	}
	if env.Status.History[0].Reason != kitchenv1alpha1.ReleaseMoveSuperseded {
		t.Fatalf("a redeploy supersedes what was running, got %q", env.Status.History[0].Reason)
	}
}

// Redeploying twice with the same settings converges: the name is a
// fingerprint of what was frozen, so a Release name goes on identifying
// exactly one snapshot.
func TestRedeployingTheSameSettingsTwiceIsOneRelease(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	withCorrectedSetting(t, h)

	first := h.do(t, http.MethodPost, redeployPath, "")
	if first.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", first.Code, first.Body.String())
	}
	made := decode[redeployView](t, first).Release

	// Back to where it was, and then forward again: the second correction is
	// the first snapshot, and it is the release that already holds it.
	ctx := context.Background()
	env := &kitchenv1alpha1.Environment{}
	if err := h.server.get(ctx, testEnvironment, env); err != nil {
		t.Fatal(err)
	}
	env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: testRelease}
	if err := h.server.Client.Update(ctx, env); err != nil {
		t.Fatal(err)
	}

	second := h.do(t, http.MethodPost, redeployPath, "")
	if second.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", second.Code, second.Body.String())
	}
	if again := decode[redeployView](t, second).Release; again != made {
		t.Fatalf("one snapshot, two releases: %q and %q", made, again)
	}

	releases := &kitchenv1alpha1.ReleaseList{}
	if err := h.server.Client.List(ctx, releases); err != nil {
		t.Fatal(err)
	}
	redeployed := 0
	for i := range releases.Items {
		if strings.HasPrefix(releases.Items[i].Name, testRelease+"-") {
			redeployed++
		}
	}
	if redeployed != 1 {
		t.Fatalf("want one redeployed release, got %d", redeployed)
	}
}

func TestRedeployRefusesWhenThereIsNothingToRedeploy(t *testing.T) {
	ctx := context.Background()

	t.Run("the project declares what the release already froze", func(t *testing.T) {
		h := newHarness(t, nil, fixtures()...)

		recorder := h.do(t, http.MethodPost, redeployPath, "")
		if recorder.Code != http.StatusConflict {
			t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
		}
		// Said in words rather than answered with an identical release.
		if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "nothing to redeploy") {
			t.Fatalf("the refusal does not say why: %q", got)
		}
	})

	t.Run("the environment is running nothing yet", func(t *testing.T) {
		h := newHarness(t, nil, fixtures()...)
		env := &kitchenv1alpha1.Environment{}
		if err := h.server.get(ctx, testEnvironment, env); err != nil {
			t.Fatal(err)
		}
		env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{}
		if err := h.server.Client.Update(ctx, env); err != nil {
			t.Fatal(err)
		}

		recorder := h.do(t, http.MethodPost, redeployPath, "")
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
		}
		if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "not running a release yet") {
			t.Fatalf("the refusal does not say why: %q", got)
		}
	})

	t.Run("the build the commit's own settings live on is gone", func(t *testing.T) {
		h := newHarness(t, nil, fixtures()...)
		withCorrectedSetting(t, h)
		build := &kitchenv1alpha1.Build{}
		if err := h.server.get(ctx, testBuild, build); err != nil {
			t.Fatal(err)
		}
		if err := h.server.Client.Delete(ctx, build); err != nil {
			t.Fatal(err)
		}

		recorder := h.do(t, http.MethodPost, redeployPath, "")
		if recorder.Code != http.StatusConflict {
			t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
		}
		if got := errorOf(t, recorder.Body.String()); !strings.Contains(got, "kitchen.json") {
			t.Fatalf("the refusal does not say what cannot be read back: %q", got)
		}
	})
}

// An environment that declares requirements takes a redeployed release the
// same way it takes every other one: through a Promotion the policy engine
// decides on. One door into a gated environment, whichever route knocks.
func TestRedeployingAGatedEnvironmentAsksRatherThanMoves(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	withCorrectedSetting(t, h)

	ctx := context.Background()
	env := &kitchenv1alpha1.Environment{}
	if err := h.server.get(ctx, testEnvironment, env); err != nil {
		t.Fatal(err)
	}
	env.Spec.Requirements = &kitchenv1alpha1.EnvironmentRequirements{BundleDigest: "sha256:1234"}
	if err := h.server.Client.Update(ctx, env); err != nil {
		t.Fatal(err)
	}

	recorder := h.do(t, http.MethodPost, redeployPath, "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[redeployView](t, recorder)
	if view.Promotion == "" {
		t.Fatalf("a gated environment answers with the promotion the move became: %+v", view)
	}

	// The release was made — the promotion has to have something to land —
	// and the environment was not moved.
	if err := h.server.get(ctx, view.Release, &kitchenv1alpha1.Release{}); err != nil {
		t.Fatalf("the release the promotion names does not exist: %v", err)
	}
	if err := h.server.get(ctx, testEnvironment, env); err != nil {
		t.Fatal(err)
	}
	if env.Spec.ReleaseRef.Name != testRelease {
		t.Fatalf("a gated environment was moved from the redeploy route: %q", env.Spec.ReleaseRef.Name)
	}
}

// The same bar as a build and as the move it supersedes: this takes the
// project's current settings live, which is what any commit a developer
// pushes already does.
func TestRedeployIsADevelopersWrite(t *testing.T) {
	for _, testCase := range []struct {
		role kitchenv1alpha1.AccessRole
		want int
	}{
		{kitchenv1alpha1.AccessRoleViewer, http.StatusForbidden},
		{kitchenv1alpha1.AccessRoleDeveloper, http.StatusAccepted},
	} {
		t.Run(string(testCase.role), func(t *testing.T) {
			h := asMember(t, testCase.role)
			withCorrectedSetting(t, h)

			recorder := h.do(t, http.MethodPost, redeployPath, "")
			if recorder.Code != testCase.want {
				t.Fatalf("want %d, got %d: %s", testCase.want, recorder.Code, recorder.Body.String())
			}
			if testCase.want != http.StatusForbidden {
				return
			}
			want := "you have viewer on shop; redeploying an environment needs developer"
			if got := errorOf(t, recorder.Body.String()); got != want {
				t.Fatalf("want %q, got %q", want, got)
			}
		})
	}
}

// A project reclassified since its last deploy is exactly when a redeploy
// would land a classified release on an environment rated below it, and the
// check that refuses a rollback for that refuses this too (#137).
func TestRedeployRefusesToLandAClassifiedReleaseOnALowerEnvironment(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)
	withCorrectedSetting(t, h)

	ctx := context.Background()
	project := &kitchenv1alpha1.Project{}
	if err := h.server.get(ctx, "shop", project); err != nil {
		t.Fatal(err)
	}
	project.Spec.DataClass = kitchenv1alpha1.DataClassConfidential
	if err := h.server.Client.Update(ctx, project); err != nil {
		t.Fatal(err)
	}
	env := &kitchenv1alpha1.Environment{}
	if err := h.server.get(ctx, testEnvironment, env); err != nil {
		t.Fatal(err)
	}
	env.Spec.DataClass = kitchenv1alpha1.DataClassInternal
	if err := h.server.Client.Update(ctx, env); err != nil {
		t.Fatal(err)
	}

	recorder := h.do(t, http.MethodPost, redeployPath, "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The one shape assertion the fixtures cannot make on their own: a project
// whose commit carried a kitchen.json has that file applied over the
// project's settings, exactly as the build that produced the release did.
func TestARedeployReplaysTheCommitsOwnSettings(t *testing.T) {
	h := newHarness(t, nil, redeployFixturesWithRepoConfig()...)

	recorder := h.do(t, http.MethodPost, "/api/v1/environments/"+testEnvironment+"/redeploy", "")
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d: %s", recorder.Code, recorder.Body.String())
	}
	fresh := &kitchenv1alpha1.Release{}
	if err := h.server.get(context.Background(), decode[redeployView](t, recorder).Release, fresh); err != nil {
		t.Fatal(err)
	}
	// The port is the commit's, not the project's: a file in the repository
	// wins over the setting it takes over, and a redeploy is not the moment
	// that stops being true.
	if got := fresh.Spec.ConfigSnapshot.Runtime.Port; got != 4000 {
		t.Fatalf("the commit's own port was dropped: %d", got)
	}
	if got := fresh.Spec.ConfigSnapshot.Runtime.Replicas; got == nil || *got != 3 {
		t.Fatalf("the project's own correction was dropped: %+v", fresh.Spec.ConfigSnapshot.Runtime)
	}
}

// redeployFixturesWithRepoConfig is the fixtures with a commit that carried a
// kitchen.json and a project that has been corrected since.
func redeployFixturesWithRepoConfig() []runtime.Object {
	objects := fixtures()
	for _, object := range objects {
		switch typed := object.(type) {
		case *kitchenv1alpha1.Project:
			typed.Spec.Runtime.Port = 3000
			typed.Spec.Runtime.Replicas = ptr.To(int32(3))
		case *kitchenv1alpha1.Build:
			typed.Status.Config = &kitchenv1alpha1.RepoConfig{
				Runtime: &kitchenv1alpha1.RepoRuntimeConfig{Port: ptr.To(int32(4000))},
			}
		case *kitchenv1alpha1.Release:
			if typed.Name == testRelease {
				typed.Spec.ConfigSnapshot.Runtime = kitchenv1alpha1.RuntimeSpec{Port: 4000}
			}
		}
	}
	return objects
}
