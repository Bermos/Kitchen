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
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// fakeReporter is a git provider that records what the platform posted
// instead of talking to one. It implements both halves, because the operator
// resolves a Provider and narrows it to a StatusReporter.
type fakeReporter struct {
	statuses    []gitprovider.CommitStatus
	deployments []gitprovider.Deployment
	comments    []gitprovider.Comment
	// err, when set, is what every post fails with.
	err error
}

func (f *fakeReporter) EnsureWebhook(context.Context, string, gitprovider.WebhookSpec) (string, error) {
	return "1", nil
}

// The reporter reads source as well, because the operator resolves one
// Provider per Connection and narrows it to each half it needs: a build
// detects its framework through the same provider that posts its status.
func (f *fakeReporter) ListDir(ctx context.Context, repo, ref, dir string) ([]gitprovider.DirEntry, error) {
	return repoWithDockerfile().ListDir(ctx, repo, ref, dir)
}

func (f *fakeReporter) ReadFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	return repoWithDockerfile().ReadFile(ctx, repo, ref, path)
}

func (f *fakeReporter) DeleteWebhook(context.Context, string, string) error { return nil }

func (f *fakeReporter) SetCommitStatus(_ context.Context, _ string, status gitprovider.CommitStatus) error {
	if f.err != nil {
		return f.err
	}
	f.statuses = append(f.statuses, status)
	return nil
}

func (f *fakeReporter) PublishDeployment(_ context.Context, _ string, d gitprovider.Deployment) error {
	if f.err != nil {
		return f.err
	}
	f.deployments = append(f.deployments, d)
	return nil
}

func (f *fakeReporter) UpsertComment(_ context.Context, _ string, c gitprovider.Comment) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.comments = append(f.comments, c)
	return "4242", nil
}

// fakeCommenter is a provider that reports commit statuses and comments but
// keeps no deployment record — Gitea's shape, which has no deployments API.
// It deliberately does not implement DeploymentPublisher.
type fakeCommenter struct {
	comments []gitprovider.Comment
}

func (f *fakeCommenter) EnsureWebhook(context.Context, string, gitprovider.WebhookSpec) (string, error) {
	return "1", nil
}

func (f *fakeCommenter) DeleteWebhook(context.Context, string, string) error { return nil }

func (f *fakeCommenter) ListDir(ctx context.Context, repo, ref, dir string) ([]gitprovider.DirEntry, error) {
	return repoWithDockerfile().ListDir(ctx, repo, ref, dir)
}

func (f *fakeCommenter) ReadFile(ctx context.Context, repo, ref, path string) ([]byte, error) {
	return repoWithDockerfile().ReadFile(ctx, repo, ref, path)
}

func (f *fakeCommenter) SetCommitStatus(context.Context, string, gitprovider.CommitStatus) error {
	return nil
}

func (f *fakeCommenter) UpsertComment(_ context.Context, _ string, c gitprovider.Comment) (string, error) {
	f.comments = append(f.comments, c)
	return "4242", nil
}

func (f *fakeReporter) lastComment() gitprovider.Comment {
	return f.comments[len(f.comments)-1]
}

var _ = Describe("Deploy status on the commit", func() {
	const (
		projectName = "gitshop"
		buildName   = "gitshop-bld-abc123abc123"
		sha         = "abc123abc123def456"
		releaseName = "gitshop-rel-abc123abc123"
		envName     = "gitshop-pr-7"
		namespace   = "default"
		repo        = "acme/shop"
	)

	ctx := context.Background()

	appNS := "kitchen-" + projectName
	buildKey := types.NamespacedName{Name: buildName, Namespace: namespace}
	envKey := types.NamespacedName{Name: envName, Namespace: namespace}

	var (
		reporter *fakeReporter
		builds   *BuildReconciler
		envs     *EnvironmentReconciler
	)

	// capabilities rewrites what the source Connection claims it can do.
	capabilities := func(caps ...kitchenv1alpha1.Capability) {
		conn := &kitchenv1alpha1.Connection{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: "gh", Namespace: namespace}, conn)).To(Succeed())
		conn.Status.Capabilities = caps
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, conn)).To(Succeed())
	}

	finishBuildJob := func() {
		job := &batchv1.Job{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: buildName, Namespace: appNS}, job)).To(Succeed())
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.CompletionTime = &now
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
		}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
	}

	// makeAvailable is what a real cluster's deployment controller would do,
	// and what turns the environment Live.
	makeAvailable := func() {
		deploy := &appsv1.Deployment{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: envName, Namespace: appNS}, deploy)).To(Succeed())
		deploy.Status.Conditions = []appsv1.DeploymentCondition{{
			Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue,
			LastUpdateTime: metav1.Now(), LastTransitionTime: metav1.Now(),
		}}
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, deploy)).To(Succeed())
	}

	BeforeEach(func() {
		reporter = &fakeReporter{}
		factory := func(*kitchenv1alpha1.Connection, string) (gitprovider.Provider, error) {
			return reporter, nil
		}
		builds = &BuildReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), GitProviders: factory}
		envs = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), GitProviders: factory}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Auth: kitchenv1alpha1.AuthSpec{
					Enabled:     true,
					PreviewGate: kitchenv1alpha1.PreviewGateSpec{Enabled: true},
				},
			},
		}
		ensureSingleton(ctx, kitchen)

		for _, secret := range []*corev1.Secret{
			{
				ObjectMeta: metav1.ObjectMeta{Name: "gh-creds", Namespace: namespace},
				Data:       map[string][]byte{gitCredentialsTokenKey: []byte("gh-token")},
			},
			{
				ObjectMeta: metav1.ObjectMeta{Name: "registry-creds-git", Namespace: namespace},
				Type:       corev1.SecretTypeDockerConfigJson,
				Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
			},
		} {
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, secret))).To(Succeed())
		}

		source := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "github",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "gh-creds"},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, source))).To(Succeed())
		capabilities(kitchenv1alpha1.CapabilityGitSource, kitchenv1alpha1.CapabilityStatusChecks)

		registry := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "registry-git", Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "dockerRegistry",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "registry-creds-git"},
				Config:               &runtime.RawExtension{Raw: []byte(`{"url":"harbor.example.com/kitchen"}`)},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, registry))).To(Succeed())

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:             repo,
					ProductionBranch: "main",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry-git"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		build := &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: namespace},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Git: kitchenv1alpha1.GitRevision{
					SHA: sha, Branch: "feature/checkout", PullRequest: ptr.To(int32(7)),
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, build))).To(Succeed())
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{}
		if err := k8sClient.Get(ctx, envKey, env); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
			_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
		}
		job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: appNS}}
		Expect(client.IgnoreNotFound(
			k8sClient.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground)))).To(Succeed())
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Build{ObjectMeta: metav1.ObjectMeta{Name: buildName, Namespace: namespace}},
			&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "gh", Namespace: namespace}},
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: "registry-git", Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "gh-creds", Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "registry-creds-git", Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kitchen-registry-registry-git", Namespace: appNS}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	Context("for a build", func() {
		It("posts a pending check when it starts and the verdict when it finishes", func() {
			_, err := builds.Reconcile(ctx, reconcile.Request{NamespacedName: buildKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(reporter.statuses).To(HaveLen(1))
			pending := reporter.statuses[0]
			Expect(pending.State).To(Equal(gitprovider.CommitPending))
			Expect(pending.SHA).To(Equal(sha))
			// The context carries the project: one repository can feed
			// several Kitchen projects, and a shared context would have them
			// overwrite each other's verdicts.
			Expect(pending.Context).To(Equal("kitchen/" + projectName))
			// The check links back to the build's page in the dashboard.
			Expect(pending.TargetURL).To(Equal("https://kitchen.apps.example.com/builds/" + buildName))

			finishBuildJob()
			_, err = builds.Reconcile(ctx, reconcile.Request{NamespacedName: buildKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(reporter.statuses).To(HaveLen(2))
			Expect(reporter.statuses[1].State).To(Equal(gitprovider.CommitSuccess))
		})

		It("reports a build the platform could not run as an error, not a failure", func() {
			build := &kitchenv1alpha1.Build{}
			Expect(k8sClient.Get(ctx, buildKey, build)).To(Succeed())
			project := &kitchenv1alpha1.Project{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, project)).To(Succeed())

			// A strategy the operator has no builder for is the platform
			// failing to run the build, not the repository failing to build.
			// It cannot be reached through a Project the API server accepts —
			// the CRD's enum names only strategies that are implemented — so
			// the reason goes in directly.
			_, err := builds.fail(ctx, build, project, "StrategyUnsupported",
				`build strategy "nixpacks" is not supported yet`)
			Expect(err).NotTo(HaveOccurred())

			Expect(reporter.statuses).To(HaveLen(1))
			Expect(reporter.statuses[0].State).To(Equal(gitprovider.CommitError))
		})

		It("stays quiet when the connection does not report statusChecks", func() {
			capabilities(kitchenv1alpha1.CapabilityGitSource)

			_, err := builds.Reconcile(ctx, reconcile.Request{NamespacedName: buildKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(reporter.statuses).To(BeEmpty())
		})

		It("finishes the build even when the provider refuses the status", func() {
			reporter.err = errors.New("401 Bad credentials")

			_, err := builds.Reconcile(ctx, reconcile.Request{NamespacedName: buildKey})
			Expect(err).NotTo(HaveOccurred())

			build := &kitchenv1alpha1.Build{}
			Expect(k8sClient.Get(ctx, buildKey, build)).To(Succeed())
			Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildRunning))
		})
	})

	Context("for a preview environment", func() {
		BeforeEach(func() {
			release := &kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: buildName},
					Image:      "harbor.example.com/kitchen/gitshop@sha256:feedface",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())

			env := &kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: namespace},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Type:       kitchenv1alpha1.EnvironmentPreview,
					ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
					Preview: &kitchenv1alpha1.PreviewInfo{
						PullRequest: 7, Branch: "feature/checkout",
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
		})

		It("publishes the deployment and comments the URL on the pull request", func() {
			_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
			makeAvailable()
			_, err = envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(reporter.deployments).To(HaveLen(2))
			deploying, live := reporter.deployments[0], reporter.deployments[1]
			Expect(deploying.State).To(Equal(gitprovider.DeploymentInProgress))
			Expect(live.State).To(Equal(gitprovider.DeploymentSuccess))
			Expect(live.Environment).To(Equal(envName))
			Expect(live.SHA).To(Equal(sha))
			// A preview goes away again, which is what tells the provider to
			// retire it rather than keep it in the environment's history.
			Expect(live.Transient).To(BeTrue())
			Expect(live.URL).To(Equal("https://gitshop-pr-7.apps.example.com"))

			Expect(reporter.comments).To(HaveLen(2))
			comment := reporter.lastComment()
			Expect(comment.PullRequest).To(Equal(int32(7)))
			Expect(comment.Body).To(ContainSubstring("https://gitshop-pr-7.apps.example.com"))
			Expect(comment.Body).To(ContainSubstring(comment.Marker))
			// A reviewer who is not a platform user meets a sign-in page, and
			// the comment has to say that is the gate rather than a dead link.
			Expect(comment.Body).To(ContainSubstring("gated behind Kitchen's login"))
			// The second write addresses the comment it already made, rather
			// than appending a second one.
			Expect(comment.ID).To(Equal("4242"))

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.GitReport).NotTo(BeNil())
			Expect(env.Status.GitReport.CommentID).To(Equal("4242"))
			Expect(env.Status.GitReport.State).To(Equal(string(gitprovider.DeploymentSuccess)))
			Expect(env.Status.GitReport.Error).To(BeEmpty())
		})

		It("still comments when the provider keeps no deployment record", func() {
			// Gitea has no deployments API. Publishing used to be the first
			// call and its failure returned early, so a provider without the
			// half would have lost the comment a reviewer actually reads.
			commenter := &fakeCommenter{}
			quiet := &EnvironmentReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(),
				GitProviders: func(*kitchenv1alpha1.Connection, string) (gitprovider.Provider, error) {
					return commenter, nil
				},
			}

			_, err := quiet.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
			makeAvailable()
			_, err = quiet.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(commenter.comments).NotTo(BeEmpty())
			last := commenter.comments[len(commenter.comments)-1]
			Expect(last.PullRequest).To(Equal(int32(7)))
			Expect(last.Body).To(ContainSubstring("https://gitshop-pr-7.apps.example.com"))

			// Not publishing is not a failure: nothing is recorded as one.
			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.GitReport).NotTo(BeNil())
			Expect(env.Status.GitReport.Error).To(BeEmpty())
			Expect(env.Status.GitReport.CommentID).To(Equal("4242"))
		})

		It("does not post again when nothing about the deployment moved", func() {
			_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(reporter.deployments).To(HaveLen(1))

			_, err = envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(reporter.deployments).To(HaveLen(1))
			Expect(reporter.comments).To(HaveLen(1))
		})

		It("retries a report the provider refused", func() {
			reporter.err = errors.New("502 Bad Gateway")
			_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.GitReport).NotTo(BeNil())
			Expect(env.Status.GitReport.Error).To(ContainSubstring("502"))

			reporter.err = nil
			_, err = envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(reporter.deployments).To(HaveLen(1))

			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.GitReport.Error).To(BeEmpty())
		})

		It("retires the deployment and closes the comment when the preview goes", func() {
			_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			_, err = envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			last := reporter.deployments[len(reporter.deployments)-1]
			Expect(last.State).To(Equal(gitprovider.DeploymentInactive))
			// The comment must stop advertising a URL that no longer answers.
			body := reporter.lastComment().Body
			Expect(body).To(ContainSubstring("has been removed"))
			Expect(body).NotTo(ContainSubstring("https://gitshop-pr-7.apps.example.com"))
		})
	})
})
