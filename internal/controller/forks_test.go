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
	"fmt"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// The policy algebra, without a cluster: what a project asks for, what the
// platform allows, and what a fork therefore gets. The row that matters most
// is the last: anything this operator does not recognize reads as `none`,
// because the only safe reading of an unknown value in a security default is
// the strictest one.
func TestForkPolicyIsBoundedByThePlatform(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		own     kitchenv1alpha1.ForkPolicy
		ceiling kitchenv1alpha1.ForkPolicy
		want    kitchenv1alpha1.ForkPolicy
		builds  bool
		preview bool
	}{
		{"a project that has said nothing gets nothing", "", kitchenv1alpha1.ForkPolicyFull,
			kitchenv1alpha1.ForkPolicyNone, false, false},
		{"build under an open ceiling builds and publishes nothing", kitchenv1alpha1.ForkPolicyBuild,
			kitchenv1alpha1.ForkPolicyFull, kitchenv1alpha1.ForkPolicyBuild, true, false},
		{"full under an open ceiling is the project's own branch", kitchenv1alpha1.ForkPolicyFull,
			kitchenv1alpha1.ForkPolicyFull, kitchenv1alpha1.ForkPolicyFull, true, true},
		{"the platform's ceiling wins over the project's ask", kitchenv1alpha1.ForkPolicyFull,
			kitchenv1alpha1.ForkPolicyBuild, kitchenv1alpha1.ForkPolicyBuild, true, false},
		{"a platform that forbids forks forbids them", kitchenv1alpha1.ForkPolicyFull,
			kitchenv1alpha1.ForkPolicyNone, kitchenv1alpha1.ForkPolicyNone, false, false},
		{"a ceiling never raises what a project asked for", kitchenv1alpha1.ForkPolicyNone,
			kitchenv1alpha1.ForkPolicyFull, kitchenv1alpha1.ForkPolicyNone, false, false},
		{"a spelling nobody here knows is none", kitchenv1alpha1.ForkPolicy("everything"),
			kitchenv1alpha1.ForkPolicyFull, kitchenv1alpha1.ForkPolicyNone, false, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			project := &kitchenv1alpha1.Project{}
			project.Spec.Previews.Forks = testCase.own
			got := project.Spec.Previews.ForksUnder(testCase.ceiling)
			if got != testCase.want {
				t.Fatalf("fork policy in force is %q, want %q", got, testCase.want)
			}
			if got.BuildsForks() != testCase.builds {
				t.Fatalf("builds is %v, want %v", got.BuildsForks(), testCase.builds)
			}
			if got.PreviewsForks() != testCase.preview {
				t.Fatalf("previews is %v, want %v", got.PreviewsForks(), testCase.preview)
			}
		})
	}
}

// A singleton written before the field existed forbids nothing, rather than
// forbidding everything: the safe default lives on the project, and a ceiling
// that read absent as `none` would silently override the installations that
// had deliberately turned forks on.
func TestThePlatformForkCeilingDefaultsToForbiddingNothing(t *testing.T) {
	var previews kitchenv1alpha1.PlatformPreviewsSpec
	if got := previews.EffectiveForksMax(); got != kitchenv1alpha1.ForkPolicyFull {
		t.Fatalf("an unset ceiling is %q, want full", got)
	}
	previews.ForksMax = kitchenv1alpha1.ForkPolicyBuild
	if got := previews.EffectiveForksMax(); got != kitchenv1alpha1.ForkPolicyBuild {
		t.Fatalf("the ceiling as set is %q, want build", got)
	}
}

// The refusal says what was refused, where the head came from, and the
// setting that changes it — and it distinguishes the two refusals, because
// "nothing was built" and "it was built and not published" are different
// facts about the same pull request.
func TestForkRefusalSaysWhereAndWhichSetting(t *testing.T) {
	project := &kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	project.Spec.Source.Git = &kitchenv1alpha1.GitSourceSpec{Repo: "acme/shop"}

	refused := ForkRefusalMessage(project, kitchenv1alpha1.ForkPolicyNone, "stranger/shop")
	for _, want := range []string{"stranger/shop", "acme/shop", "nothing was built", "spec.previews.forks"} {
		if !strings.Contains(refused, want) {
			t.Fatalf("the refusal does not say %q: %s", want, refused)
		}
	}

	built := ForkRefusalMessage(project, kitchenv1alpha1.ForkPolicyBuild, "stranger/shop")
	if !strings.Contains(built, "the commit was built and no preview environment was created") {
		t.Fatalf("a fork that was built is not said to have been: %s", built)
	}

	// A payload that would not say where the head is still refuses in words
	// somebody can act on.
	unknown := ForkRefusalMessage(project, kitchenv1alpha1.ForkPolicyNone, kitchenv1alpha1.UnknownForkRepo)
	if !strings.Contains(unknown, "a fork") || strings.Contains(unknown, "the fork unknown") {
		t.Fatalf("an unnamed fork reads badly: %s", unknown)
	}
}

// What a fork pull request actually gets, against a real API server: the
// preview, the refusal, and the commit status and comment that carry it.
var _ = Describe("A pull request from a fork", func() {
	const (
		namespace   = "default"
		projectName = "fork-shop"
		repo        = "acme/fork-shop"
		fork        = "stranger/fork-shop"
	)

	var (
		reconciler *BuildReconciler
		reporter   *fakeReporter
	)

	project := func() *kitchenv1alpha1.Project {
		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: projectName}, project)).To(Succeed())
		return project
	}

	// forkBuild is a finished build of a fork's head, as the receiver creates
	// one when the project allows fork builds at all.
	forkBuild := func(number int32, forkRepo string) *kitchenv1alpha1.Build {
		return &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-bld-%04d", projectName, number),
				Namespace: namespace,
			},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Git: kitchenv1alpha1.GitRevision{
					SHA:         strings.Repeat("f", 40),
					Branch:      "feature",
					PullRequest: ptr.To(number),
					ForkRepo:    forkRepo,
				},
			},
		}
	}

	// forks sets what this project asks for a fork to be given.
	forks := func(policy kitchenv1alpha1.ForkPolicy) *kitchenv1alpha1.Project {
		p := project()
		p.Spec.Previews.Forks = policy
		ExpectWithOffset(1, k8sClient.Update(ctx, p)).To(Succeed())
		return project()
	}

	BeforeEach(func() {
		reporter = &fakeReporter{}
		reconciler = &BuildReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(),
			GitProviders: func(*kitchenv1alpha1.Connection, string) (gitprovider.Provider, error) {
				return reporter, nil
			},
		}

		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
			},
		})

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "fork-gh-creds", Namespace: namespace},
			Data:       map[string][]byte{gitCredentialsTokenKey: []byte("token")},
		}))).To(Succeed())
		conn := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "fork-gh", Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "github",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "fork-gh-creds"},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, conn))).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "fork-gh", Namespace: namespace}, conn)).To(Succeed())
		conn.Status.Capabilities = []kitchenv1alpha1.Capability{
			kitchenv1alpha1.CapabilityGitSource, kitchenv1alpha1.CapabilityStatusChecks,
		}
		Expect(k8sClient.Status().Update(ctx, conn)).To(Succeed())

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "fork-registry", Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "dockerRegistry",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "fork-gh-creds"},
				Config:               &runtime.RawExtension{Raw: []byte(`{"url":"harbor.example.com/kitchen"}`)},
			},
		}))).To(Succeed())

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: "fork-gh"},
					Repo:             repo,
					ProductionBranch: "main",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "fork-registry"},
				},
				Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080},
			},
		}))).To(Succeed())
	})

	AfterEach(func() {
		environments := &kitchenv1alpha1.EnvironmentList{}
		Expect(k8sClient.List(ctx, environments, client.InNamespace(namespace))).To(Succeed())
		for i := range environments.Items {
			env := &environments.Items[i]
			if env.Spec.ProjectRef.Name != projectName {
				continue
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
		}
		builds := &kitchenv1alpha1.BuildList{}
		Expect(k8sClient.List(ctx, builds, client.InNamespace(namespace))).To(Succeed())
		for i := range builds.Items {
			build := &builds.Items[i]
			if build.Spec.ProjectRef.Name != projectName {
				continue
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, build))).To(Succeed())
		}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
		}))).To(Succeed())
	})

	It("gets no environment under the default, and is told so on the request", func() {
		build := forkBuild(7, fork)
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, project(), 7, "rel")).To(Succeed())

		By("creating no environment, so no secret of this project's can reach it")
		Expect(build.Status.Preview).To(BeEmpty())
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: PreviewEnvironmentName(projectName, 7),
		}, &kitchenv1alpha1.Environment{})).NotTo(Succeed())

		By("posting the refusal under the preview's own context, not the build's")
		Expect(reporter.statuses).To(HaveLen(1))
		Expect(reporter.statuses[0].Context).To(Equal("kitchen/" + projectName + "/preview"))
		Expect(reporter.statuses[0].State).To(Equal(gitprovider.CommitFailure))
		Expect(reporter.statuses[0].Description).To(ContainSubstring("fork"))

		By("writing the comment under the marker a preview would have used")
		Expect(reporter.comments).To(HaveLen(1))
		Expect(reporter.comments[0].PullRequest).To(Equal(int32(7)))
		Expect(reporter.comments[0].Marker).To(ContainSubstring(PreviewEnvironmentName(projectName, 7)))
		Expect(reporter.comments[0].Body).To(ContainSubstring(fork))
		Expect(reporter.comments[0].Body).To(ContainSubstring("spec.previews.forks"))
	})

	It("still gets no environment when the project asks only for a build", func() {
		build := forkBuild(8, fork)
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, forks(kitchenv1alpha1.ForkPolicyBuild), 8, "rel")).To(Succeed())

		Expect(build.Status.Preview).To(BeEmpty())
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: PreviewEnvironmentName(projectName, 8),
		}, &kitchenv1alpha1.Environment{})).NotTo(Succeed())
		Expect(reporter.comments[0].Body).To(ContainSubstring("the commit was built"))
	})

	It("is the project's own branch under full, and records where it came from", func() {
		build := forkBuild(9, fork)
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, forks(kitchenv1alpha1.ForkPolicyFull), 9, "rel")).To(Succeed())

		Expect(build.Status.Preview).To(Equal(PreviewEnvironmentName(projectName, 9)))
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: PreviewEnvironmentName(projectName, 9),
		}, env)).To(Succeed())
		Expect(env.Spec.Preview).NotTo(BeNil())
		Expect(env.Spec.Preview.ForkRepo).To(Equal(fork))
		Expect(reporter.statuses).To(BeEmpty())
	})

	It("is refused under full when the platform's ceiling forbids it", func() {
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Previews:   kitchenv1alpha1.PlatformPreviewsSpec{ForksMax: kitchenv1alpha1.ForkPolicyBuild},
			},
		})
		build := forkBuild(6, fork)
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, forks(kitchenv1alpha1.ForkPolicyFull), 6, "rel")).To(Succeed())

		Expect(build.Status.Preview).To(BeEmpty())
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: PreviewEnvironmentName(projectName, 6),
		}, &kitchenv1alpha1.Environment{})).NotTo(Succeed())
	})

	// The upgrade case: a preview created before this gate existed is not
	// torn down, and stops being deployed to.
	It("does not deploy to a preview a fork already had", func() {
		existing := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PreviewEnvironmentName(projectName, 5),
				Namespace: namespace,
			},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentPreview,
				Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 5, Branch: "feature"},
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "old-rel"},
			},
		}
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())

		build := forkBuild(5, fork)
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, project(), 5, "new-rel")).To(Succeed())

		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: PreviewEnvironmentName(projectName, 5),
		}, existing)).To(Succeed())
		Expect(existing.Spec.ReleaseRef.Name).To(Equal("old-rel"))
	})

	It("leaves a pull request from the project's own branch alone", func() {
		build := forkBuild(4, "")
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, project(), 4, "rel")).To(Succeed())

		Expect(build.Status.Preview).To(Equal(PreviewEnvironmentName(projectName, 4)))
		Expect(reporter.statuses).To(BeEmpty())
	})
})
