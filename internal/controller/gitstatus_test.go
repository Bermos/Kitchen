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
	"k8s.io/apimachinery/pkg/api/meta"
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
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:             repo,
					ProductionBranch: "main",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
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
					ReleaseRef: kitchenv1alpha1.ReleaseReference{Name: releaseName},
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

		// #597: a failed deploy used to read "<env> could not be deployed" and
		// nothing else, on the deployment status and in the comment alike —
		// the one thing the reviewer already knew, while the sentence saying
		// what happened sat on the Environment two fields away.
		It("says why a deploy failed, in the same words on the status and in the comment", func() {
			_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			// A container the kubelet will not create. Nothing above the pod
			// carries its reason, which is the whole point: the platform is
			// the only thing that can carry it to the pull request.
			const kubelet = `secret "gitshop-db-pr-7" not found`
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      envName + "-59f4c",
					Namespace: appNS,
					Labels:    webLabels(map[string]string{labelEnvironment: envName}),
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: AppContainerName, Image: "harbor.example.com/kitchen/gitshop@sha256:feedface"},
				}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod,
					client.GracePeriodSeconds(0)))).To(Succeed())
			})
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: AppContainerName,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CreateContainerConfigError", Message: kubelet,
				}},
			}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			_, err = envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.Phase).To(Equal(kitchenv1alpha1.EnvironmentDegraded))

			// The sentence the platform wrote down, whole. Asserting the exact
			// string rather than "not the old one" is deliberate: a substring
			// check passes against half a fix and an absence check passes
			// against the nothing the unfixed code writes.
			const why = "the container of web could not be started: " +
				`CreateContainerConfigError: secret "gitshop-db-pr-7" not found`

			// The dashboard draws the first unhealthy condition's message, so
			// the words on the screen and the words in the pull request are
			// the same words only if the operator wrote one sentence onto
			// both conditions.
			ready := meta.FindStatusCondition(env.Status.Conditions, condReady)
			workload := meta.FindStatusCondition(env.Status.Conditions, condWorkloadAvailable)
			Expect(ready).NotTo(BeNil())
			Expect(workload).NotTo(BeNil())
			Expect(ready.Message).To(Equal(why))
			Expect(workload.Message).To(Equal(why))

			last := reporter.deployments[len(reporter.deployments)-1]
			Expect(last.State).To(Equal(gitprovider.DeploymentFailure))
			Expect(last.Description).To(Equal(envName + " could not be deployed: " + why))

			// The comment is the only surface an anonymous reviewer has: the
			// description is cut at a hundred and forty characters by the
			// provider and the dashboard is behind the platform's login, so
			// this one carries the sentence whole.
			body := reporter.lastComment().Body
			Expect(body).To(ContainSubstring("| **Status** | Failed |"))
			Expect(body).To(ContainSubstring("**This deploy did not finish** — " + why + ".\n"))
			// That this surface carries no instruction to a reader who may be
			// unable to act is asserted where the sentence is built and a
			// posture is actually declared — this release declares none, so
			// the clause is not in `why` at all: see
			// TestAContainerRefusedUnderThePostureIsReportedInWords.
		})

		// The blocker found reviewing #605: the description and the comment
		// now depend on a reason, and the de-duplication key did not. A
		// failure posted for a commit stood for ever while the dashboard,
		// which reads the conditions, moved on to the second cause.
		It("posts again when the reason changes under the same commit and state", func() {
			_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      envName + "-2f0ba",
					Namespace: appNS,
					Labels:    webLabels(map[string]string{labelEnvironment: envName}),
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: AppContainerName, Image: "harbor.example.com/kitchen/gitshop@sha256:feedface"},
				}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod,
					client.GracePeriodSeconds(0)))).To(Succeed())
			})

			refuseWith := func(message string) {
				Expect(k8sClient.Get(ctx,
					types.NamespacedName{Name: pod.Name, Namespace: appNS}, pod)).To(Succeed())
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
					Name: AppContainerName,
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: "CreateContainerConfigError", Message: message,
					}},
				}}
				ExpectWithOffset(1, k8sClient.Status().Update(ctx, pod)).To(Succeed())
				_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
				ExpectWithOffset(1, err).NotTo(HaveOccurred())
			}

			refuseWith(`secret "gitshop-db-pr-7" not found`)
			published := len(reporter.deployments)
			Expect(reporter.deployments[published-1].Description).To(ContainSubstring("gitshop-db-pr-7"))

			// The Secret is created, and the pod is then refused for a second
			// cause. Same commit, same `failure`, same URL — a different
			// thing to say.
			refuseWith(`configmap "gitshop-files-pr-7" not found`)

			Expect(reporter.deployments).To(HaveLen(published + 1))
			last := reporter.deployments[len(reporter.deployments)-1]
			Expect(last.State).To(Equal(gitprovider.DeploymentFailure))
			Expect(last.Description).To(ContainSubstring("gitshop-files-pr-7"))
			Expect(last.Description).NotTo(ContainSubstring("gitshop-db-pr-7"))

			// And the comment a reviewer is looking at, which is the surface
			// the first cause would have been frozen on.
			body := reporter.lastComment().Body
			Expect(body).To(ContainSubstring("gitshop-files-pr-7"))
			Expect(body).NotTo(ContainSubstring("gitshop-db-pr-7"))

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.GitReport.Description).To(Equal(last.Description))
		})

		// Criterion (5) for the other path to Degraded, read off the writer
		// rather than off a fixture: `awaitingDeployTasks` puts one sentence
		// onto the DeployTasks condition and onto Ready, and the report has
		// to be carrying that same sentence — not a third wording of it.
		It("carries a failed deploy task's own sentence, in the words on both conditions", func() {
			const jobSaid = `relation "orders" already exists`
			withTask := releaseName + "-task"
			Expect(k8sClient.Create(ctx, &kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: withTask, Namespace: namespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: buildName},
					Image:      "harbor.example.com/kitchen/gitshop@sha256:feedface",
					ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
						Runtime: kitchenv1alpha1.RuntimeSpec{Port: 3000},
						Processes: []kitchenv1alpha1.ProcessSpec{{
							Name:    "migrate",
							Type:    kitchenv1alpha1.ProcessTask,
							Command: []string{"npm", "run", "migrate"},
						}},
					},
				},
			})).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &kitchenv1alpha1.Release{
					ObjectMeta: metav1.ObjectMeta{Name: withTask, Namespace: namespace},
				}))).To(Succeed())
			})

			env := &kitchenv1alpha1.Environment{}
			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			env.Spec.ReleaseRef = kitchenv1alpha1.ReleaseReference{Name: withTask}
			Expect(k8sClient.Update(ctx, env)).To(Succeed())

			// One pass starts the run and reports the deploy as in flight.
			_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
			jobs := &batchv1.JobList{}
			Expect(k8sClient.List(ctx, jobs, client.InNamespace(appNS),
				client.MatchingLabels{labelEnvironment: envName, labelProcess: "migrate"})).To(Succeed())
			Expect(jobs.Items).To(HaveLen(1))
			run := jobs.Items[0]
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &run,
					client.PropagationPolicy(metav1.DeletePropagationBackground)))).To(Succeed())
			})

			// envtest runs no Job controller, so the run ends because this
			// says it did — the way the controller ends one, interim
			// condition first.
			now := metav1.Now()
			run.Status.StartTime = &now
			run.Status.Conditions = []batchv1.JobCondition{
				{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue,
					Reason: "BackoffLimitExceeded", LastTransitionTime: now},
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
					Reason: "BackoffLimitExceeded", Message: jobSaid, LastTransitionTime: now},
			}
			Expect(k8sClient.Status().Update(ctx, &run)).To(Succeed())

			_, err = envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
			Expect(env.Status.Phase).To(Equal(kitchenv1alpha1.EnvironmentDegraded))
			blocked := meta.FindStatusCondition(env.Status.Conditions, condDeployTasks)
			ready := meta.FindStatusCondition(env.Status.Conditions, condReady)
			Expect(blocked).NotTo(BeNil())
			Expect(ready).NotTo(BeNil())

			// Anchored to what actually happened, so the three assertions
			// below cannot all agree on nothing.
			Expect(blocked.Reason).To(Equal(reasonTaskFailed))
			Expect(blocked.Message).To(ContainSubstring("migrate failed"))
			Expect(blocked.Message).To(ContainSubstring(run.Name))
			Expect(blocked.Message).To(ContainSubstring(jobSaid))

			// The property: one sentence, written by the reconciler onto both
			// conditions, and the report carrying that one rather than a
			// third wording.
			Expect(ready.Message).To(Equal(blocked.Message))
			last := reporter.deployments[len(reporter.deployments)-1]
			Expect(last.State).To(Equal(gitprovider.DeploymentFailure))
			Expect(last.Description).To(Equal(envName + " could not be deployed: " + blocked.Message))
			Expect(reporter.lastComment().Body).To(
				ContainSubstring("**This deploy did not finish** — " + blocked.Message))
		})

		// #597 asks whether an environment that dips through Degraded on its
		// way to Live leaves a `failure` behind that the later `success` never
		// erases. The recovery *is* published, and this is what says so: the
		// de-duplication key carries the state, so a success does not match
		// the failure already recorded — which makes it the deployment's
		// newest status and rewrites the comment in place. So a preview found
		// at `Failed` in its comment, at rest, is one that is still Degraded
		// rather than one that flickered.
		//
		// The `failure` does stay in that deployment's status history under
		// the success, and a recovery that lands after a newer push is posted
		// against the *newer* deployment, leaving the older record's final
		// status a failure for ever. Neither is what this pins; #604 carries
		// what that means for the numbers in #597.
		It("publishes the recovery, so a dip through Degraded does not read as a failed deploy", func() {
			_, err := envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      envName + "-7cb81",
					Namespace: appNS,
					Labels:    webLabels(map[string]string{labelEnvironment: envName}),
				},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: AppContainerName, Image: "harbor.example.com/kitchen/gitshop@sha256:feedface"},
				}},
			}
			Expect(k8sClient.Create(ctx, pod)).To(Succeed())
			DeferCleanup(func() {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, pod,
					client.GracePeriodSeconds(0)))).To(Succeed())
			})
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
				Name: AppContainerName,
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: "CreateContainerConfigError", Message: "configmap not found",
				}},
			}}
			Expect(k8sClient.Status().Update(ctx, pod)).To(Succeed())

			_, err = envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
			Expect(reporter.deployments[len(reporter.deployments)-1].State).
				To(Equal(gitprovider.DeploymentFailure))

			// The kubelet gets what it was waiting for and the container
			// starts, which on a cluster is a pod watch and one more pass.
			Expect(k8sClient.Delete(ctx, pod, client.GracePeriodSeconds(0))).To(Succeed())
			makeAvailable()
			_, err = envs.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())

			last := reporter.deployments[len(reporter.deployments)-1]
			Expect(last.State).To(Equal(gitprovider.DeploymentSuccess))
			Expect(last.Description).To(Equal(envName + " is live"))
			// And the comment the reviewer reads is the recovered one, in
			// place, rather than the failure with a success underneath it.
			body := reporter.lastComment().Body
			Expect(body).To(ContainSubstring("| **Status** | Ready |"))
			Expect(body).NotTo(ContainSubstring("This deploy did not finish"))
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
