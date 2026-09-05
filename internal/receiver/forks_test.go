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

package receiver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// forkReporter records what the platform posted back to the forge instead of
// posting it. Only the two halves a refusal uses are interesting here.
type forkReporter struct {
	statuses []gitprovider.CommitStatus
	comments []gitprovider.Comment
}

func (f *forkReporter) EnsureWebhook(context.Context, string, gitprovider.WebhookSpec) (string, error) {
	return "1", nil
}
func (f *forkReporter) DeleteWebhook(context.Context, string, string) error { return nil }

func (f *forkReporter) SetCommitStatus(_ context.Context, _ string, s gitprovider.CommitStatus) error {
	f.statuses = append(f.statuses, s)
	return nil
}

func (f *forkReporter) UpsertComment(_ context.Context, _ string, c gitprovider.Comment) (string, error) {
	f.comments = append(f.comments, c)
	return "1", nil
}

// forkFixture is a receiver whose project asks for `policy` and whose
// Connection can report a status back, so the refusal is observable.
func forkFixture(
	t *testing.T,
	provider string,
	policy kitchenv1alpha1.ForkPolicy,
) (*GitWebhookReceiver, http.Handler, *forkReporter) {
	t.Helper()
	reporter := &forkReporter{}
	r, handler := newReceiverForProvider(t, provider, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gh-creds", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("t")},
	})
	r.Factory = func(*kitchenv1alpha1.Connection, string) (gitprovider.Provider, error) {
		return reporter, nil
	}

	ctx := context.Background()
	conn := &kitchenv1alpha1.Connection{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: "gh", Namespace: "default"}, conn); err != nil {
		t.Fatal(err)
	}
	conn.Status.Capabilities = []kitchenv1alpha1.Capability{
		kitchenv1alpha1.CapabilityGitSource, kitchenv1alpha1.CapabilityStatusChecks,
	}
	// A plain update rather than a status update: the fake client keeps no
	// status subresource for these types, and what matters here is only that
	// the Connection claims statusChecks when the refusal looks it up.
	if err := r.Client.Update(ctx, conn); err != nil {
		t.Fatal(err)
	}

	project := &kitchenv1alpha1.Project{}
	if err := r.Client.Get(ctx, types.NamespacedName{Name: "shop", Namespace: "default"}, project); err != nil {
		t.Fatal(err)
	}
	project.Spec.Previews.Forks = policy
	if err := r.Client.Update(ctx, project); err != nil {
		t.Fatal(err)
	}
	return r, handler, reporter
}

func buildsOf(t *testing.T, r *GitWebhookReceiver) []kitchenv1alpha1.Build {
	t.Helper()
	builds := &kitchenv1alpha1.BuildList{}
	if err := r.Client.List(context.Background(), builds); err != nil {
		t.Fatal(err)
	}
	return builds.Items
}

// Where a pull request's head lives, per provider, and the one rule that runs
// through all three: a payload that does not say is a fork, never the
// project's own. That is the whole of issue #422 — the field was not decoded
// at all, so a stranger's commit was indistinguishable from a maintainer's.
func TestForkDetectionReadsEveryProvidersHead(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		provider string
		event    string
		body     string
		fork     string
	}{
		{
			name: "github: the same repository is not a fork", provider: "github", event: "pull_request",
			body: `{"action":"opened","number":42,
				"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd","repo":{"full_name":"acme/shop"}}},
				"repository":{"full_name":"acme/shop"}}`,
			fork: "",
		},
		{
			name: "github: another repository is", provider: "github", event: "pull_request",
			body: `{"action":"opened","number":42,
				"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd","repo":{"full_name":"stranger/shop"}}},
				"repository":{"full_name":"acme/shop"}}`,
			fork: "stranger/shop",
		},
		{
			name: "github: a different spelling of the same name is not", provider: "github", event: "pull_request",
			body: `{"action":"opened","number":42,
				"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd","repo":{"full_name":"ACME/Shop"}}},
				"repository":{"full_name":"acme/shop"}}`,
			fork: "",
		},
		{
			name: "github: no head repository at all is a fork", provider: "github", event: "pull_request",
			body: `{"action":"opened","number":42,
				"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd"}},
				"repository":{"full_name":"acme/shop"}}`,
			fork: kitchenv1alpha1.UnknownForkRepo,
		},
		{
			name: "gitea: the same repository is not a fork", provider: "gitea", event: "pull_request",
			body: `{"action":"opened","number":42,
				"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd","repo":{"full_name":"acme/shop"}}},
				"repository":{"full_name":"acme/shop"}}`,
			fork: "",
		},
		{
			name: "gitea: another repository is", provider: "gitea", event: "pull_request",
			body: `{"action":"opened","number":42,
				"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd","repo":{"full_name":"stranger/shop"}}},
				"repository":{"full_name":"acme/shop"}}`,
			fork: "stranger/shop",
		},
		{
			name: "gitea: no head repository at all is a fork", provider: "gitea", event: "pull_request",
			body: `{"action":"opened","number":42,
				"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd","repo":{}}},
				"repository":{"full_name":"acme/shop"}}`,
			fork: kitchenv1alpha1.UnknownForkRepo,
		},
		{
			name: "gitlab: one project id on both sides is not a fork", provider: "gitlab",
			event: "Merge Request Hook",
			body: `{"object_attributes":{"action":"open","iid":42,"source_branch":"f",
				"source_project_id":7,"target_project_id":7,
				"last_commit":{"id":"aaaabbbbccccdddd","message":"m"}},
				"project":{"path_with_namespace":"acme/shop"},"user":{"username":"bermos"}}`,
			fork: "",
		},
		{
			name: "gitlab: two project ids are", provider: "gitlab", event: "Merge Request Hook",
			body: `{"object_attributes":{"action":"open","iid":42,"source_branch":"f",
				"source_project_id":9,"target_project_id":7,
				"source":{"path_with_namespace":"stranger/shop"},
				"last_commit":{"id":"aaaabbbbccccdddd","message":"m"}},
				"project":{"path_with_namespace":"acme/shop"},"user":{"username":"bermos"}}`,
			fork: "stranger/shop",
		},
		{
			name: "gitlab: no project ids at all is a fork, not a match", provider: "gitlab",
			event: "Merge Request Hook",
			body: `{"object_attributes":{"action":"open","iid":42,"source_branch":"f",
				"last_commit":{"id":"aaaabbbbccccdddd","message":"m"}},
				"project":{"path_with_namespace":"acme/shop"},"user":{"username":"bermos"}}`,
			fork: kitchenv1alpha1.UnknownForkRepo,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// `full` so that every case builds and the Build is there to be
			// read: this test is about what was *detected*, not what it costs.
			r, handler, _ := forkFixture(t, testCase.provider, kitchenv1alpha1.ForkPolicyFull)
			body := []byte(testCase.body)
			signature := sign(body)
			if testCase.provider == "gitlab" {
				signature = webhookSecret
			}
			if rec := deliverAs(testCase.provider, handler, testCase.event, body, signature); rec.Code != http.StatusAccepted {
				t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
			}
			builds := buildsOf(t, r)
			if len(builds) != 1 {
				t.Fatalf("expected one build, got %d", len(builds))
			}
			revision := builds[0].Spec.Git
			if revision.ForkRepo != testCase.fork {
				t.Fatalf("forkRepo is %q, want %q", revision.ForkRepo, testCase.fork)
			}
			if revision.IsFork() != (testCase.fork != "") {
				t.Fatalf("isFork is %v for forkRepo %q", revision.IsFork(), revision.ForkRepo)
			}
		})
	}
}

// The default: a fork gets no Build at all, and the pull request is told so
// where its author is looking.
func TestForkPullRequestIsRefusedByDefault(t *testing.T) {
	for _, provider := range []string{"github", "gitea"} {
		t.Run(provider, func(t *testing.T) {
			r, handler, reporter := forkFixture(t, provider, "")
			body := []byte(`{"action":"opened","number":42,
				"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd","repo":{"full_name":"stranger/shop"}}},
				"repository":{"full_name":"acme/shop"}}`)

			rec := deliverAs(provider, handler, "pull_request", body, sign(body))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
			}
			if builds := buildsOf(t, r); len(builds) != 0 {
				t.Fatalf("a fork's commit was built anyway: %+v", builds)
			}
			if len(reporter.statuses) != 1 {
				t.Fatalf("expected one commit status, got %+v", reporter.statuses)
			}
			status := reporter.statuses[0]
			if status.Context != "kitchen/shop/preview" || status.State != gitprovider.CommitFailure {
				t.Fatalf("the refusal is reported under the wrong check: %+v", status)
			}
			if !strings.Contains(status.Description, "fork") {
				t.Fatalf("the check does not say why: %q", status.Description)
			}
			if len(reporter.comments) != 1 {
				t.Fatalf("expected one comment, got %+v", reporter.comments)
			}
			comment := reporter.comments[0]
			if comment.PullRequest != 42 || !strings.Contains(comment.Marker, "shop-pr-42") {
				t.Fatalf("the comment is not the preview's own: %+v", comment)
			}
			for _, want := range []string{"stranger/shop", "spec.previews.forks", "No preview environment"} {
				if !strings.Contains(comment.Body, want) {
					t.Fatalf("the comment does not say %q: %s", want, comment.Body)
				}
			}
		})
	}
}

// The middle setting: the commit is built and nothing else happens here. The
// build controller is what then declines the preview — see forks_test.go in
// internal/controller.
func TestForkPullRequestIsBuiltWhenTheProjectAsksForIt(t *testing.T) {
	r, handler, reporter := forkFixture(t, "github", kitchenv1alpha1.ForkPolicyBuild)
	body := []byte(`{"action":"opened","number":42,
		"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd","repo":{"full_name":"stranger/shop"}}},
		"repository":{"full_name":"acme/shop"}}`)

	if rec := deliverAs("github", handler, "pull_request", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	builds := buildsOf(t, r)
	if len(builds) != 1 {
		t.Fatalf("expected the fork's commit to be built, got %d builds", len(builds))
	}
	if builds[0].Spec.Git.ForkRepo != "stranger/shop" {
		t.Fatalf("the build does not record where the head came from: %+v", builds[0].Spec.Git)
	}
	if len(reporter.statuses) != 0 {
		t.Fatalf("nothing is refused at the receiver under `build`: %+v", reporter.statuses)
	}
}

// The platform's ceiling is applied to what the project asked for, so an
// estate that forbids fork builds forbids them however a project is set.
func TestThePlatformCeilingOverridesTheProject(t *testing.T) {
	r, handler, reporter := forkFixture(t, "github", kitchenv1alpha1.ForkPolicyFull)
	if err := r.Client.Create(context.Background(), &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: controller.KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			Previews: kitchenv1alpha1.PlatformPreviewsSpec{ForksMax: kitchenv1alpha1.ForkPolicyNone},
		},
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"action":"opened","number":42,
		"pull_request":{"head":{"ref":"f","sha":"aaaabbbbccccdddd","repo":{"full_name":"stranger/shop"}}},
		"repository":{"full_name":"acme/shop"}}`)

	if rec := deliverAs("github", handler, "pull_request", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if builds := buildsOf(t, r); len(builds) != 0 {
		t.Fatalf("the platform forbids fork builds and one happened: %+v", builds)
	}
	if len(reporter.statuses) != 1 {
		t.Fatalf("the refusal was not reported: %+v", reporter.statuses)
	}
}

// A push is never a fork's: no provider delivers a fork's pushes to the base
// repository's webhook, and a push payload has no head repository to read. It
// is asserted rather than assumed, because a push that were treated as a fork
// would stop building the project's own branches.
func TestAPushIsNeverAFork(t *testing.T) {
	r, handler, _ := forkFixture(t, "github", "")
	body := []byte(`{"ref":"refs/heads/main","after":"8f3a2c1d0abc456789ab",
		"repository":{"full_name":"acme/shop"},
		"head_commit":{"message":"Add checkout","author":{"username":"bermos"}}}`)

	if rec := deliver(handler, "push", body, sign(body)); rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	builds := buildsOf(t, r)
	if len(builds) != 1 {
		t.Fatalf("a push to the project's own repository must build: got %d", len(builds))
	}
	if builds[0].Spec.Git.IsFork() {
		t.Fatalf("a push was read as a fork: %+v", builds[0].Spec.Git)
	}
}
