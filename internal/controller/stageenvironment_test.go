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
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What a project's promotion pipeline publishes at, one environment per
// stage.
//
// The spike behind #489 read the collision off the code rather than
// reproducing it: every non-preview environment took projectHost, so a
// project with a `staging` stage and a `production` stage applied two
// HTTPRoutes into one namespace claiming one hostname, and Gateway API
// resolved that by rule age. This is the reproduction, kept as the
// regression test: two non-preview environments of one project, two
// hostnames, two routes, no overlap.
var _ = Describe("A staged project's environments", func() {
	const (
		projectName = "stageshop"
		stageEnv    = "stageshop-staging"
		prodEnv     = "stageshop-production"
		releaseName = "stageshop-rel-000001"
		namespace   = "default"
		baseDomain  = "apps.example.com"
		image       = "registry.example.com/kitchen/stageshop@sha256:0123456789abcdef"
	)

	ctx := context.Background()
	appNS := "kitchen-" + projectName

	var reconciler *EnvironmentReconciler

	reconcileOnce := func(name string) {
		key := types.NamespacedName{Name: name, Namespace: namespace}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	environment := func(name string, envType kitchenv1alpha1.EnvironmentType) *kitchenv1alpha1.Environment {
		return &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       envType,
				ReleaseRef: kitchenv1alpha1.ReleaseReference{Name: releaseName},
			},
		}
	}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: baseDomain,
				TLS:        acmeTLS(),
			},
		})

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/shop",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				// The pipeline the two environments belong to: the last stage
				// is production, everything before it is a stage.
				Promotion: &kitchenv1alpha1.PromotionPolicySpec{Stages: []kitchenv1alpha1.PromotionStage{
					{Name: "staging", Environment: stageEnv},
					{Name: "production", Environment: prodEnv, AutoPromote: true},
				}},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		release := &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "stageshop-bld-1"},
				Image:      image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx,
			environment(stageEnv, kitchenv1alpha1.EnvironmentStage)))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx,
			environment(prodEnv, kitchenv1alpha1.EnvironmentProduction)))).To(Succeed())
	})

	AfterEach(func() {
		for _, name := range []string{stageEnv, prodEnv} {
			env := &kitchenv1alpha1.Environment{}
			key := types.NamespacedName{Name: name, Namespace: namespace}
			if err := k8sClient.Get(ctx, key, env); err == nil {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
				Expect(err).NotTo(HaveOccurred())
			}
		}
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("publishes each non-preview environment at a hostname of its own", func() {
		for _, name := range []string{stageEnv, prodEnv} {
			reconcileOnce(name)
			reconcileOnce(name)
		}

		hosts := map[string][]gatewayv1.Hostname{}
		for _, name := range []string{stageEnv, prodEnv} {
			route := &gatewayv1.HTTPRoute{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: appNS}, route)).To(Succeed())
			hosts[name] = route.Spec.Hostnames
		}

		Expect(hosts[stageEnv]).To(ConsistOf(gatewayv1.Hostname("stageshop-staging."+baseDomain)),
			"a stage carries its environment's name into its hostname")
		Expect(hosts[prodEnv]).To(ConsistOf(gatewayv1.Hostname("stageshop."+baseDomain)),
			"production keeps the project's own hostname, unchanged")
		Expect(hosts[stageEnv]).NotTo(Equal(hosts[prodEnv]),
			"two routes into one namespace claiming one hostname is resolved by rule age: one silently wins")

		stage := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stageEnv, Namespace: namespace}, stage)).To(Succeed())
		Expect(stage.Status.URL).To(Equal("https://stageshop-staging." + baseDomain))
		production := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: prodEnv, Namespace: namespace}, production)).To(Succeed())
		Expect(production.Status.URL).To(Equal("https://stageshop." + baseDomain))
	})

	// The upgrade. An installation that ran a staged pipeline before this
	// fix has its stage environments typed `production` — that was the only
	// value there was — so the collision is on the objects and not only in
	// the code that would create them. The reconciler re-derives the type
	// from the pipeline, which is what moves an existing stage off
	// production's hostname without waiting for somebody to deploy.
	// The two features meet here. `spec.exposure: internal` (#492) withholds
	// the route; the type (#490) says which environment this is. Neither
	// answers the other's question, so an internal project's stage is still a
	// stage — it is simply published nowhere, which is a statement about the
	// address and not about the rung.
	It("types a stage of an internal project the same, and still publishes nothing", func() {
		project := &kitchenv1alpha1.Project{}
		projectKey := types.NamespacedName{Name: projectName, Namespace: namespace}
		Expect(k8sClient.Get(ctx, projectKey, project)).To(Succeed())
		project.Spec.Exposure = kitchenv1alpha1.ExposureInternal
		Expect(k8sClient.Update(ctx, project)).To(Succeed())
		// A Project this suite's AfterEach deletes may still be there on the
		// next spec — nothing here runs the finalizer — so the exposure is put
		// back rather than left for a spec that expects a published route.
		DeferCleanup(func() {
			restored := &kitchenv1alpha1.Project{}
			if err := k8sClient.Get(ctx, projectKey, restored); err != nil {
				return
			}
			restored.Spec.Exposure = kitchenv1alpha1.ExposurePublic
			Expect(client.IgnoreNotFound(k8sClient.Update(ctx, restored))).To(Succeed())
		})

		// Created as production, the way a pre-#490 installation has it, so
		// this covers the re-typing on an internal project too.
		stage := &kitchenv1alpha1.Environment{}
		key := types.NamespacedName{Name: stageEnv, Namespace: namespace}
		Expect(k8sClient.Get(ctx, key, stage)).To(Succeed())
		stage.Spec.Type = kitchenv1alpha1.EnvironmentProduction
		Expect(k8sClient.Update(ctx, stage)).To(Succeed())

		reconcileOnce(stageEnv)
		reconcileOnce(stageEnv)

		Expect(k8sClient.Get(ctx, key, stage)).To(Succeed())
		Expect(stage.Spec.Type).To(Equal(kitchenv1alpha1.EnvironmentStage),
			"exposure decides whether it is published, not which rung it is")

		err := k8sClient.Get(ctx, types.NamespacedName{Name: stageEnv, Namespace: appNS}, &gatewayv1.HTTPRoute{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "an internal project publishes no environment, stage included")
		Expect(stage.Status.URL).To(BeEmpty(), "there is no address to move, so the visible break is not one here")
	})

	It("re-types an environment a pre-#490 installation created as production", func() {
		stage := &kitchenv1alpha1.Environment{}
		key := types.NamespacedName{Name: stageEnv, Namespace: namespace}
		Expect(k8sClient.Get(ctx, key, stage)).To(Succeed())
		stage.Spec.Type = kitchenv1alpha1.EnvironmentProduction
		Expect(k8sClient.Update(ctx, stage)).To(Succeed())

		reconcileOnce(stageEnv)
		reconcileOnce(stageEnv)

		Expect(k8sClient.Get(ctx, key, stage)).To(Succeed())
		Expect(stage.Spec.Type).To(Equal(kitchenv1alpha1.EnvironmentStage),
			"the pipeline says which rung this is, and the type follows it")

		route := &gatewayv1.HTTPRoute{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: stageEnv, Namespace: appNS}, route)).To(Succeed())
		Expect(route.Spec.Hostnames).To(ConsistOf(gatewayv1.Hostname("stageshop-staging."+baseDomain)),
			"the route moves with the type: this is the visible break on upgrade")
	})
})

// hostname is the one derivation of where an environment answers, and each
// type answers somewhere else: production at the project's own host — which
// projectHost promises to anything that has to know it before the
// Environment exists — a stage at its own name under the base domain, and a
// preview at its pull request's.
func TestTheHostnameOfEachEnvironmentType(t *testing.T) {
	const baseDomain = "apps.example.com"
	for name, tc := range map[string]struct {
		env  kitchenv1alpha1.Environment
		want string
	}{
		"production": {
			env:  kitchenv1alpha1.Environment{Spec: kitchenv1alpha1.EnvironmentSpec{Type: kitchenv1alpha1.EnvironmentProduction}},
			want: "shop." + baseDomain,
		},
		"a stage": {
			env: kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: "shop-staging"},
				Spec:       kitchenv1alpha1.EnvironmentSpec{Type: kitchenv1alpha1.EnvironmentStage},
			},
			want: "shop-staging." + baseDomain,
		},
		// A stage environment named without the project's prefix still
		// publishes under it: the host has to name the project, or two
		// projects with a stage called "staging" would collide the way the
		// stage and production used to.
		"a stage named for the rung alone": {
			env: kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: "staging"},
				Spec:       kitchenv1alpha1.EnvironmentSpec{Type: kitchenv1alpha1.EnvironmentStage},
			},
			want: "shop-staging." + baseDomain,
		},
		"a preview": {
			env: kitchenv1alpha1.Environment{
				ObjectMeta: metav1.ObjectMeta{Name: "shop-pr-42"},
				Spec: kitchenv1alpha1.EnvironmentSpec{
					Type:    kitchenv1alpha1.EnvironmentPreview,
					Preview: &kitchenv1alpha1.PreviewInfo{PullRequest: 42, Branch: "feat/checkout"},
				},
			},
			want: "shop-pr-42." + baseDomain,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := hostname("shop", &tc.env, baseDomain); got != tc.want {
				t.Errorf("hostname = %q, want %q", got, tc.want)
			}
		})
	}
}
