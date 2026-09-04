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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The ceiling itself, without a cluster: which of the two numbers is in
// force, and when one more preview is one too many.
func TestPreviewCeilingReads(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		own      *int32
		platform int32
		live     int32
		max      int32
		reached  bool
	}{
		{"a project with no ceiling of its own takes the platform's", nil, 5, 2, 5, false},
		{"its own overrides the platform's", ptr.To(int32(2)), 5, 2, 2, true},
		{"zero on the project is no ceiling for it", ptr.To(int32(0)), 5, 9, 0, false},
		{"zero on the platform is no ceiling at all", nil, 0, 9, 0, false},
		{"at the ceiling is reached; one below is not", nil, 3, 3, 3, true},
		{"below it is not", nil, 3, 2, 3, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			project := &kitchenv1alpha1.Project{}
			project.Spec.Previews.Max = testCase.own
			ceiling := previewCapacity{
				Live: testCase.live,
				Max:  project.Spec.Previews.MaxOrPlatform(testCase.platform),
			}
			if ceiling.Max != testCase.max {
				t.Fatalf("ceiling in force is %d, want %d", ceiling.Max, testCase.max)
			}
			if ceiling.Reached() != testCase.reached {
				t.Fatalf("reached is %v, want %v", ceiling.Reached(), testCase.reached)
			}
		})
	}
}

// The refusal names the count, the ceiling and the setting that moves it —
// and which setting it names depends on which one is in force, because
// telling somebody to raise the platform's when their project overrides it is
// advice that does nothing.
func TestPreviewRefusalNamesTheSettingInForce(t *testing.T) {
	project := &kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	platform := previewRefusalMessage(project, previewCapacity{Live: 5, Max: 5})
	if !strings.Contains(platform, "spec.previews.maxPerProject") || !strings.Contains(platform, "5 of 5") {
		t.Fatalf("the platform's ceiling is not named: %q", platform)
	}
	project.Spec.Previews.Max = ptr.To(int32(2))
	own := previewRefusalMessage(project, previewCapacity{Live: 2, Max: 2})
	if !strings.Contains(own, "spec.previews.max on project shop") {
		t.Fatalf("the project's own ceiling is not named: %q", own)
	}
}

// The refusal list is a bounded record rather than a queue: one entry per
// pull request, refreshed on a new commit, silent on the same one, dropped
// when the request gets its preview after all.
func TestTheRefusalListIsARecord(t *testing.T) {
	status := &kitchenv1alpha1.PreviewCapacityStatus{}
	now := metav1.Now()

	if !status.RecordRefusedPreview(41, "aaa", now) {
		t.Fatal("the first refusal of a request is a change")
	}
	if status.RecordRefusedPreview(41, "aaa", now) {
		t.Fatal("refusing the same commit again is not a change, and must not write status")
	}
	if !status.RecordRefusedPreview(41, "bbb", now) {
		t.Fatal("a new commit on a refused request is a change")
	}
	if len(status.Refused) != 1 {
		t.Fatalf("one request, one entry: %+v", status.Refused)
	}

	if !status.ClearRefusedPreview(41) {
		t.Fatal("a request that got its preview comes off the list")
	}
	if status.ClearRefusedPreview(41) || len(status.Refused) != 0 {
		t.Fatalf("clearing twice is a no-op: %+v", status.Refused)
	}

	for i := range int32(kitchenv1alpha1.MaxRefusedPreviews + 5) {
		status.RecordRefusedPreview(i, "sha", now)
	}
	if len(status.Refused) != kitchenv1alpha1.MaxRefusedPreviews {
		t.Fatalf("the record is bounded at %d: %d", kitchenv1alpha1.MaxRefusedPreviews, len(status.Refused))
	}
	if status.Refused[0].PullRequest != 5 {
		t.Fatalf("the oldest entries are the ones dropped: %+v", status.Refused[0])
	}
}

// The ceiling as the platform actually applies it, against a real API server:
// what is counted, what is refused, what it says, and what frees a slot.
var _ = Describe("The preview ceiling", func() {
	const (
		namespace   = "default"
		projectName = "ceiling-shop"
	)

	var reconciler *BuildReconciler

	// previewNumbered is a preview Environment of this project, as the build
	// controller would have made one.
	previewNumbered := func(number int32) *kitchenv1alpha1.Environment {
		return &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      PreviewEnvironmentName(projectName, number),
				Namespace: namespace,
			},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentPreview,
				Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: number, Branch: "feature"},
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "rel"},
			},
		}
	}

	// buildFor is a finished build of a pull request's head, which is what
	// routePreview is handed.
	buildFor := func(number int32) *kitchenv1alpha1.Build {
		return &kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-bld-%d", projectName, number),
				Namespace: namespace,
			},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Git: kitchenv1alpha1.GitRevision{
					SHA: fmt.Sprintf("%040d", number), Branch: "feature", PullRequest: ptr.To(number),
				},
			},
		}
	}

	project := func() *kitchenv1alpha1.Project {
		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: projectName}, project)).To(Succeed())
		return project
	}

	BeforeEach(func() {
		reconciler = &BuildReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Previews:   kitchenv1alpha1.PlatformPreviewsSpec{MaxPerProject: ptr.To(int32(2))},
			},
		})

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef:    kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:             "acme/ceiling-shop",
					ProductionBranch: "main",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
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
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
		}))).To(Succeed())
	})

	It("refuses the one past the ceiling, and says so on the project", func() {
		for _, number := range []int32{41, 42} {
			Expect(k8sClient.Create(ctx, previewNumbered(number))).To(Succeed())
		}

		build := buildFor(43)
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, project(), 43, "rel")).To(Succeed())

		By("creating no environment for it")
		Expect(build.Status.Preview).To(BeEmpty())
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: namespace, Name: PreviewEnvironmentName(projectName, 43),
		}, &kitchenv1alpha1.Environment{})).NotTo(Succeed())

		By("recording the refusal where the dashboard and the CLI read it")
		Expect(project().Status.Previews).NotTo(BeNil())
		Expect(project().Status.Previews.Live).To(Equal(int32(2)))
		Expect(project().Status.Previews.Max).To(Equal(int32(2)))
		Expect(project().Status.Previews.Refused).To(HaveLen(1))
		Expect(project().Status.Previews.Refused[0].PullRequest).To(Equal(int32(43)))
	})

	It("does not count production, or anything promoted", func() {
		Expect(k8sClient.Create(ctx, &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: projectName + "-production", Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "rel"},
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: projectName + "-staging", Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "rel"},
			},
		})).To(Succeed())
		Expect(k8sClient.Create(ctx, previewNumbered(41))).To(Succeed())

		build := buildFor(44)
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, project(), 44, "rel")).To(Succeed())

		Expect(build.Status.Preview).To(Equal(PreviewEnvironmentName(projectName, 44)))
	})

	It("frees the slot when a preview goes, and the next push takes it", func() {
		for _, number := range []int32{41, 42} {
			Expect(k8sClient.Create(ctx, previewNumbered(number))).To(Succeed())
		}
		refused := buildFor(45)
		Expect(k8sClient.Create(ctx, refused)).To(Succeed())
		Expect(reconciler.routePreview(ctx, refused, project(), 45, "rel")).To(Succeed())
		Expect(refused.Status.Preview).To(BeEmpty())

		By("closing a pull request")
		Expect(k8sClient.Delete(ctx, previewNumbered(41))).To(Succeed())

		By("pushing again to the request that was refused")
		pushed := buildFor(45)
		pushed.Name += "-again"
		pushed.Spec.Git.SHA = strings.Repeat("b", 40)
		Expect(k8sClient.Create(ctx, pushed)).To(Succeed())
		Expect(reconciler.routePreview(ctx, pushed, project(), 45, "rel")).To(Succeed())

		Expect(pushed.Status.Preview).To(Equal(PreviewEnvironmentName(projectName, 45)))
		Expect(project().Status.Previews.Refused).To(BeEmpty())
	})

	It("never refuses a preview that already exists", func() {
		for _, number := range []int32{41, 42} {
			Expect(k8sClient.Create(ctx, previewNumbered(number))).To(Succeed())
		}
		build := buildFor(41)
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, project(), 41, "rel")).To(Succeed())
		Expect(build.Status.Preview).To(Equal(PreviewEnvironmentName(projectName, 41)))
	})

	It("bounds nothing when the platform's ceiling is zero", func() {
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Previews:   kitchenv1alpha1.PlatformPreviewsSpec{MaxPerProject: ptr.To(int32(0))},
			},
		})
		for _, number := range []int32{41, 42, 46} {
			Expect(k8sClient.Create(ctx, previewNumbered(number))).To(Succeed())
		}
		build := buildFor(47)
		Expect(k8sClient.Create(ctx, build)).To(Succeed())
		Expect(reconciler.routePreview(ctx, build, project(), 47, "rel")).To(Succeed())
		Expect(build.Status.Preview).To(Equal(PreviewEnvironmentName(projectName, 47)))
	})
})
