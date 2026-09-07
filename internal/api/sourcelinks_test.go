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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The source links (#435). Everything the platform builds knows which commit
// it came from and none of it used to say where that commit is, so a build
// page showed a seven-character SHA nobody could click and a preview could
// not reach the pull request it exists for.

const shopCommitURL = "https://github.com/acme/shop/commit/" + testCommit

func TestABuildLinksItsCommitAndItsBranch(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/builds/"+testBuild, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	build := decode[buildView](t, recorder)
	if build.Git.CommitURL != shopCommitURL {
		t.Errorf("commitUrl = %q, want %q", build.Git.CommitURL, shopCommitURL)
	}
	if want := "https://github.com/acme/shop/tree/main"; build.Git.BranchURL != want {
		t.Errorf("branchUrl = %q, want %q", build.Git.BranchURL, want)
	}
	if build.Git.PullRequestURL != "" {
		t.Errorf("a build of a branch named a pull request: %q", build.Git.PullRequestURL)
	}
}

// A pull request from a fork has its head commit in the fork and nowhere
// else, so linking it in the project's own repository would be a 404 with a
// plausible-looking URL. The request itself is the base repository's.
func TestAForkedPullRequestLinksTheForkForItsCommit(t *testing.T) {
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-forked", Namespace: testNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git: kitchenv1alpha1.GitRevision{
				SHA:         "f00ba7f00ba7f00",
				Branch:      "patch-1",
				PullRequest: ptr.To(int32(7)),
				ForkRepo:    "stranger/shop",
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), build)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/builds/shop-bld-forked", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[buildView](t, recorder)
	if want := "https://github.com/stranger/shop/commit/f00ba7f00ba7f00"; view.Git.CommitURL != want {
		t.Errorf("commitUrl = %q, want the fork's %q", view.Git.CommitURL, want)
	}
	if want := "https://github.com/acme/shop/pull/7"; view.Git.PullRequestURL != want {
		t.Errorf("pullRequestUrl = %q, want the base repository's %q", view.Git.PullRequestURL, want)
	}
}

// The sharpest of the links: a preview exists because of a pull request, the
// platform is the only thing that knows the number, and reaching the
// discussion meant copying it into a URL bar.
func TestAPreviewLinksItsPullRequest(t *testing.T) {
	preview := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-pr-42", Namespace: testNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Type:       kitchenv1alpha1.EnvironmentPreview,
			Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 42, Branch: "feature/checkout"},
		},
	}
	h := newHarness(t, nil, append(fixtures(), preview)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/shop-pr-42", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[environmentView](t, recorder)
	if view.Preview == nil {
		t.Fatal("a preview environment answered with no preview")
	}
	if want := "https://github.com/acme/shop/pull/42"; view.Preview.PullRequestURL != want {
		t.Errorf("pullRequestUrl = %q, want %q", view.Preview.PullRequestURL, want)
	}
	if want := "https://github.com/acme/shop/tree/feature/checkout"; view.Preview.BranchURL != want {
		t.Errorf("branchUrl = %q, want %q", view.Preview.BranchURL, want)
	}
}

// What is actually running here, as a commit rather than as the name of a
// release — which is the question the environment screen could not answer.
func TestAnEnvironmentNamesTheCommitItIsRunning(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/"+testEnvironment, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[environmentView](t, recorder)
	if view.Git == nil {
		t.Fatal("a live environment did not say which commit it runs")
	}
	if view.Git.SHA != testCommit || view.Git.CommitURL != shopCommitURL {
		t.Errorf("the live commit is %+v, want the release's build", view.Git)
	}
}

// The type reaches every client verbatim, so a stage reads as a stage on the
// single environment, in the project's listing, and in the CLI's TYPE column
// that renders the same field — where every rung used to read `production`.
func TestAStageEnvironmentReadsAsItsOwnType(t *testing.T) {
	h := newHarness(t, nil, append(fixtures(), stageEnvironment())...)

	recorder := h.do(t, http.MethodGet, "/api/v1/environments/shop-staging", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[environmentView](t, recorder)
	stage := string(kitchenv1alpha1.EnvironmentStage)
	production := string(kitchenv1alpha1.EnvironmentProduction)
	if view.Type != stage {
		t.Errorf("type = %q, want %q", view.Type, stage)
	}
	if want := "https://shop-staging.apps.example.com"; view.URL != want {
		t.Errorf("url = %q, want %q — a stage does not answer at production's address", view.URL, want)
	}

	recorder = h.do(t, http.MethodGet, "/api/v1/projects/shop/environments", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	types := map[string]string{}
	for _, env := range decode[listBody[environmentView]](t, recorder).Items {
		types[env.Name] = env.Type
	}
	if types["shop-staging"] != stage || types[testEnvironment] != production {
		t.Errorf("the listing types are %+v, want the stage and production apart", types)
	}
}

func TestAReleaseNamesTheCommitItFroze(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/releases/"+testRelease, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[releaseView](t, recorder)
	if view.Git == nil {
		t.Fatal("a release did not say which commit it froze")
	}
	if view.Git.CommitURL != shopCommitURL {
		t.Errorf("commitUrl = %q, want %q", view.Git.CommitURL, shopCommitURL)
	}
}

func TestAProjectLinksItsRepository(t *testing.T) {
	h := newHarness(t, nil, fixtures()...)

	recorder := h.do(t, http.MethodGet, "/api/v1/projects/shop", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[projectView](t, recorder)
	if want := "https://github.com/acme/shop"; view.RepositoryURL != want {
		t.Errorf("repositoryUrl = %q, want %q", view.RepositoryURL, want)
	}
}

// A link is a convenience on an answer that was already correct: a connection
// that is gone, or one whose provider has no web routing, leaves the fields
// empty and changes nothing else.
func TestALinkIsAbsentRatherThanWrong(t *testing.T) {
	objects := []runtime.Object{}
	for _, object := range fixtures() {
		// Everything but the git connection the project names.
		if connection, ok := object.(*kitchenv1alpha1.Connection); ok && connection.Name == "gh" {
			continue
		}
		objects = append(objects, object)
	}
	h := newHarness(t, nil, objects...)

	recorder := h.do(t, http.MethodGet, "/api/v1/builds/"+testBuild, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[buildView](t, recorder)
	if view.Git.SHA != testCommit {
		t.Fatalf("the build stopped answering when its connection went: %+v", view.Git)
	}
	if view.Git.CommitURL != "" || view.Git.BranchURL != "" {
		t.Errorf("a connection that is gone produced links: %+v", view.Git)
	}
}

// The commit's date, which is the other half of the same complaint: a build
// had been running an eight-week-old commit for hours and every screen showed
// a seven-character SHA that looked exactly like a fresh one.
func TestABuildCarriesTheCommitsOwnDate(t *testing.T) {
	committed := time.Date(2026, 7, 11, 9, 30, 0, 0, time.UTC)
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-dated", Namespace: testNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git: kitchenv1alpha1.GitRevision{
				SHA:         testCommit,
				Branch:      defaultProductionBranch,
				CommittedAt: &metav1.Time{Time: committed},
			},
		},
	}
	h := newHarness(t, nil, append(fixtures(), build)...)

	recorder := h.do(t, http.MethodGet, "/api/v1/builds/shop-bld-dated", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	view := decode[buildView](t, recorder)
	if view.Git.CommittedAt == nil || !view.Git.CommittedAt.Equal(committed) {
		t.Fatalf("committedAt = %v, want %v", view.Git.CommittedAt, committed)
	}

	// A build whose provider never said carries no date rather than the date
	// of the build, which would be a different and untrue statement.
	recorder = h.do(t, http.MethodGet, "/api/v1/builds/"+testBuild, "")
	if undated := decode[buildView](t, recorder); undated.Git.CommittedAt != nil {
		t.Errorf("a build with no commit date invented one: %v", undated.Git.CommittedAt)
	}
}
