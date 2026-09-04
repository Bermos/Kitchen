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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The digest poll (#308): what corresponds to a push, for software this
// platform did not build.
//
// The cases here are that issue's acceptance criteria in the order it states
// them — a moved tag produces a Build, a Release and a deploy with no builder
// Job; a digest-pinned project produces nothing and is asked nothing; an
// unreachable registry is a failed Build with a stated reason that leaves
// what is running alone; and a rollback is the ordinary rollback, which is
// true because the Release froze a digest.

// movingResolver is a registry whose answer changes, and which can be taken
// away entirely. It records what it was asked, so a case can assert that a
// pinned project costs no request at all.
type movingResolver struct {
	digest string
	fail   error
	asked  []string
}

func (m *movingResolver) Resolve(_ context.Context, ref string) (string, error) {
	m.asked = append(m.asked, ref)
	if m.fail != nil {
		return "", m.fail
	}
	return repositoryOf(ref) + "@" + m.digest, nil
}

// repositoryOf is a reference with its tag or digest taken off. The cut is
// looked for after the last slash, because a registry host may carry a port
// and a digest carries a colon of its own.
func repositoryOf(ref string) string {
	slash := strings.LastIndex(ref, "/")
	if cut := strings.IndexAny(ref[slash+1:], ":@"); cut >= 0 {
		return ref[:slash+1+cut]
	}
	return ref
}

var _ = Describe("The digest poll", func() {
	const (
		project = "vendorpoll"
		repo    = "ghcr.io/vendor/app"
		tag     = "stable"

		first = "sha256:" +
			"aaaa111111111111111111111111111111111111111111111111111111111111"
		second = "sha256:" +
			"bbbb222222222222222222222222222222222222222222222222222222222222"
	)

	ctx := context.Background()
	reference := repo + ":" + tag
	firstImage := repo + "@" + first
	secondImage := repo + "@" + second

	var (
		created  []client.Object
		resolver *movingResolver
		builds   *BuildReconciler
		poll     *ImagePollSweeper
		unit     *kitchenv1alpha1.Project
	)

	create := func(obj client.Object) {
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, obj))).To(Succeed())
		created = append(created, obj)
	}

	// reconcileBuild drives one Build to whatever it becomes, which for an
	// acquisition is one pass: there is no Job to wait for.
	reconcileBuild := func(name string) *kitchenv1alpha1.Build {
		GinkgoHelper()
		key := types.NamespacedName{Name: name, Namespace: PlatformNamespace}
		_, err := builds.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		build := &kitchenv1alpha1.Build{}
		Expect(k8sClient.Get(ctx, key, build)).To(Succeed())
		return build
	}

	// acquisitions is every Build of this project that names no commit,
	// which is the whole of what the poll can have created.
	acquisitions := func() []kitchenv1alpha1.Build {
		GinkgoHelper()
		list := &kitchenv1alpha1.BuildList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace(PlatformNamespace),
			client.MatchingLabels{kitchenv1alpha1.ProjectLabel: project})).To(Succeed())
		var mine []kitchenv1alpha1.Build
		for i := range list.Items {
			if list.Items[i].Spec.ProjectRef.Name == project {
				mine = append(mine, list.Items[i])
				created = append(created, &list.Items[i])
			}
		}
		return mine
	}

	BeforeEach(func() {
		resolver = &movingResolver{digest: first}
		factory := func([]byte, string) (ImageResolver, error) { return resolver, nil }
		builds = &BuildReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Resolvers: factory,
		}
		poll = &ImagePollSweeper{Client: k8sClient, Resolvers: factory}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
		})

		unit = &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: project, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Image: &kitchenv1alpha1.ImageSourceSpec{
					Repository: repo, Tag: tag,
				}},
				Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080},
			},
		}
		create(unit)
	})

	AfterEach(func() {
		for i := len(created) - 1; i >= 0; i-- {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, created[i]))).To(Succeed())
		}
		created = nil
	})

	// acquired is the project's first acquisition, taken and released: the
	// state every case below starts from, because a poll compares what it
	// finds against what the platform last took.
	acquired := func() *kitchenv1alpha1.Build {
		GinkgoHelper()
		seed := &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{
				Name:      AcquisitionNameFor(project, reference),
				Namespace: PlatformNamespace,
				Labels:    map[string]string{kitchenv1alpha1.ProjectLabel: project},
			},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project},
				Acquire: &kitchenv1alpha1.AcquisitionSpec{
					Reference: reference,
					Trigger:   kitchenv1alpha1.AcquisitionSeeded,
				},
			},
		}
		create(seed)
		build := reconcileBuild(seed.Name)
		Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildSucceeded))
		Expect(build.Status.Acquisition).NotTo(BeNil())
		Expect(build.Status.Acquisition.Image).To(Equal(firstImage))
		Expect(build.Status.Acquisition.Previous).To(BeEmpty(), "the first acquisition replaced nothing")
		created = append(created, &kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{
			Name: acquisitionReleaseName(project, firstImage), Namespace: PlatformNamespace}})
		created = append(created, &kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{
			Name: ProductionTargetEnvironmentName(unit), Namespace: PlatformNamespace}})
		return build
	}

	It("makes a Build, a Release and a deploy out of a moved tag, and runs no builder", func() {
		acquired()

		By("finding nothing while the tag names what was taken")
		report, err := poll.PollOnce(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Running).To(BeTrue())
		Expect(report.Polled).To(Equal(1))
		Expect(report.Acquired).To(BeZero())
		Expect(acquisitions()).To(HaveLen(1))

		By("stamping the project, so the next step does not ask again inside the interval")
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: project, Namespace: PlatformNamespace}, unit)).To(Succeed())
		Expect(unit.Status.ImagePoll).NotTo(BeNil())
		Expect(unit.Status.ImagePoll.LastPolledAt).NotTo(BeNil())
		Expect(unit.Status.ImagePoll.Message).To(BeEmpty())

		again, err := poll.PollOnce(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.Polled).To(BeZero(), "the interval is what bounds the cost")

		By("acquiring the new digest once the tag has moved and the interval has passed")
		resolver.digest = second
		poll.Now = func() time.Time { return time.Now().Add(time.Hour) }
		report, err = poll.PollOnce(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Acquired).To(Equal(1))

		list := acquisitions()
		Expect(list).To(HaveLen(2))
		var moved *kitchenv1alpha1.Build
		for i := range list {
			if list[i].Spec.Acquire != nil &&
				list[i].Spec.Acquire.Trigger == kitchenv1alpha1.AcquisitionPolled {
				moved = &list[i]
			}
		}
		Expect(moved).NotTo(BeNil())
		Expect(moved.Spec.Acquire.Digest).To(Equal(second),
			"the poll has already asked, so the Build takes the digest that made it exist")
		Expect(moved.Spec.Acquire.Reference).To(Equal(reference))
		Expect(moved.FromRepository()).To(BeFalse(), "nothing fakes a commit")

		By("saying what it resolved, from which reference, when, and what it replaced")
		build := reconcileBuild(moved.Name)
		Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildSucceeded))
		Expect(build.Status.Acquisition.Reference).To(Equal(reference))
		Expect(build.Status.Acquisition.Image).To(Equal(secondImage))
		Expect(build.Status.Acquisition.Previous).To(Equal(firstImage))
		Expect(build.Status.Acquisition.Trigger).To(Equal(kitchenv1alpha1.AcquisitionPolled))
		Expect(build.Status.Acquisition.ResolvedAt).NotTo(BeNil())

		By("freezing the digest onto a Release and deploying it")
		release := &kitchenv1alpha1.Release{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: acquisitionReleaseName(project, secondImage), Namespace: PlatformNamespace,
		}, release)).To(Succeed())
		created = append(created, release)
		Expect(release.Spec.Image).To(Equal(secondImage))

		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: ProductionTargetEnvironmentName(unit), Namespace: PlatformNamespace,
		}, env)).To(Succeed())
		Expect(env.Spec.ReleaseRef.Name).To(Equal(release.Name),
			"the new digest is what production is pointed at")

		By("running no builder Job: there was nothing to build")
		jobs := &batchv1.JobList{}
		Expect(k8sClient.List(ctx, jobs, client.InNamespace(appNamespace(project)))).To(Succeed())
		Expect(jobs.Items).To(BeEmpty())
	})

	It("asks nothing about a digest-pinned project, and never moves one", func() {
		acquired()
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: project, Namespace: PlatformNamespace}, unit)).To(Succeed())
		unit.Spec.Source.Image.Tag = ""
		unit.Spec.Source.Image.Digest = first
		Expect(k8sClient.Update(ctx, unit)).To(Succeed())

		resolver.asked = nil
		resolver.digest = second
		report, err := poll.PollOnce(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Watched).To(BeZero(), "a pinned reference cannot move, so it is not watched")
		Expect(report.Acquired).To(BeZero())
		Expect(resolver.asked).To(BeEmpty(), "the cheapest possible poll of a project that opted out")
		Expect(acquisitions()).To(HaveLen(1))
	})

	It("fails a Build for a stated reason when the registry cannot be read, and leaves what is running alone", func() {
		acquired()
		env := &kitchenv1alpha1.Environment{}
		envKey := types.NamespacedName{Name: ProductionTargetEnvironmentName(unit), Namespace: PlatformNamespace}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		serving := env.Spec.ReleaseRef.Name
		Expect(serving).NotTo(BeEmpty())

		resolver.fail = errors.New("dial tcp: connection refused")
		report, err := poll.PollOnce(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(report.Acquired).To(BeZero(), "nothing was acquired, because nothing could be read")
		Expect(report.Unreadable).To(Equal(1))

		By("saying so on the project rather than silently doing nothing")
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: project, Namespace: PlatformNamespace}, unit)).To(Succeed())
		Expect(unit.Status.ImagePoll).NotTo(BeNil())
		Expect(unit.Status.ImagePoll.Message).To(ContainSubstring("connection refused"))

		By("and as a Build that failed for that reason")
		list := acquisitions()
		Expect(list).To(HaveLen(2))
		var attempted *kitchenv1alpha1.Build
		for i := range list {
			if list[i].Spec.Acquire != nil &&
				list[i].Spec.Acquire.Trigger == kitchenv1alpha1.AcquisitionPolled {
				attempted = &list[i]
			}
		}
		Expect(attempted).NotTo(BeNil())
		build := reconcileBuild(attempted.Name)
		Expect(build.Status.Phase).To(Equal(kitchenv1alpha1.BuildFailed))
		ready := meta.FindStatusCondition(build.Status.Conditions, condReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Reason).To(Equal(reasonImageUnresolved))
		Expect(ready.Message).To(ContainSubstring("connection refused"))
		Expect(build.Status.Acquisition).NotTo(BeNil(),
			"a failed acquisition still says which reference it was following")
		Expect(build.Status.Acquisition.Reference).To(Equal(reference))
		Expect(build.Status.Acquisition.Image).To(BeEmpty(), "nothing was resolved")

		By("leaving the environment on the digest it is already serving")
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		Expect(env.Spec.ReleaseRef.Name).To(Equal(serving))

		By("saying it once: a registry that stays down is one failed Build, not one an interval")
		poll.Now = func() time.Time { return time.Now().Add(time.Hour) }
		_, err = poll.PollOnce(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(acquisitions()).To(HaveLen(2))
	})
})
