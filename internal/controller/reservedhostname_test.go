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
	"net/url"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/platformhost"
)

// The hostnames the operator publishes are the other half of the reserved
// list — the chart's half is pinned in internal/platformhost. Between them,
// every address the platform answers on under the base domain is a name a
// project cannot be created with (#423).
func TestTheOperatorPublishesOnlyReservedHostnames(t *testing.T) {
	const base = "apps.example.com"
	kitchen := &kitchenv1alpha1.Kitchen{Spec: kitchenv1alpha1.KitchenSpec{
		BaseDomain: base,
		Auth:       kitchenv1alpha1.AuthSpec{Enabled: true, PreviewGate: kitchenv1alpha1.PreviewGateSpec{Enabled: true}},
	}}

	apiURL, err := url.Parse(apiExternalURL(kitchen))
	if err != nil {
		t.Fatalf("the operator's own API URL does not parse: %v", err)
	}
	for what, host := range map[string]string{
		"the preview gate's route": previewGateHost(kitchen),
		"the registry's route":     registryHost(kitchen),
		"the API and dashboard":    apiURL.Hostname(),
	} {
		if host == "" {
			t.Errorf("%s has no hostname in this configuration, so this test is checking nothing", what)
			continue
		}
		if !platformhost.IsReservedHost(host, base) {
			t.Errorf("%s is published at %s and that label is not reserved: "+
				"a project of that name would write a second HTTPRoute for it", what, host)
		}
	}

	// And the two constants the operator spells its subdomains with are the
	// package's own, so they cannot drift from the refusal.
	if PreviewGateHostPrefix != platformhost.PreviewGate || RegistryHostPrefix != platformhost.Registry {
		t.Errorf("the operator's subdomains are not platformhost's: %q and %q",
			PreviewGateHostPrefix, RegistryHostPrefix)
	}
}

var _ = Describe("An environment of a project whose name is reserved", func() {
	const (
		// `auth` is the identity provider's own hostname. A Project of that
		// name cannot be created through the API at all — this is a Project
		// written straight to the cluster.
		projectName = platformhost.Auth
		envName     = "auth-production"
		releaseName = "auth-rel-000001"
		namespace   = "default"
	)

	ctx := context.Background()
	envKey := types.NamespacedName{Name: envName, Namespace: namespace}
	appNS := AppNamespace(projectName)

	var reconciler *EnvironmentReconciler

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Auth:       kitchenv1alpha1.AuthSpec{Enabled: true},
			},
		})

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/auth",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
			},
		}))).To(Succeed())

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "auth-bld-1"},
				Image:      "registry.example.com/kitchen/auth@sha256:0123456789abcdef",
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080},
				},
			},
		}))).To(Succeed())

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef: kitchenv1alpha1.ReleaseReference{Name: releaseName},
			},
		}))).To(Succeed())
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{}
		if err := k8sClient.Get(ctx, envKey, env); err == nil {
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			Expect(err).NotTo(HaveOccurred())
		}
		project := &kitchenv1alpha1.Project{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: projectName, Namespace: namespace}, project); err == nil {
			Expect(k8sClient.Delete(ctx, project)).To(Succeed())
		}
	})

	It("publishes no route, and says why", func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
		Expect(err).NotTo(HaveOccurred())

		route := &gatewayv1.HTTPRoute{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: envName, Namespace: appNS}, route)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(),
			"the environment of a project named after the identity provider must not claim its hostname")

		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		Expect(env.Status.URL).To(BeEmpty())
		Expect(env.Status.Phase).To(Equal(kitchenv1alpha1.EnvironmentPending))
		for _, condType := range []string{condRouteProgrammed, condReady} {
			condition := meta.FindStatusCondition(env.Status.Conditions, condType)
			Expect(condition).NotTo(BeNil(), condType+" should say what happened")
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(reasonReservedHostname))
			// The message names the hostname that is taken, the way the
			// API's refusal does.
			Expect(strings.Contains(condition.Message, "auth.apps.example.com")).To(BeTrue(),
				"the condition should name the hostname: "+condition.Message)
		}
	})
})

// The Project itself says the same thing, so that somebody looking at the
// project rather than at one of its environments finds the reason.
var _ = Describe("A project whose name is reserved", func() {
	const (
		projectName = "shop-pr-7"
		namespace   = "default"
	)

	ctx := context.Background()
	key := types.NamespacedName{Name: projectName, Namespace: namespace}

	AfterEach(func() {
		project := &kitchenv1alpha1.Project{}
		if err := k8sClient.Get(ctx, key, project); err == nil {
			Expect(k8sClient.Delete(ctx, project)).To(Succeed())
		}
	})

	It("is not ready, and names the hostname it would have taken", func() {
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
			},
		})
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Image: &kitchenv1alpha1.ImageSourceSpec{
					Repository: "ghcr.io/acme/shop",
					Tag:        "v1",
				}},
			},
		}))).To(Succeed())

		reconciler := &ProjectReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		project := &kitchenv1alpha1.Project{}
		Expect(k8sClient.Get(ctx, key, project)).To(Succeed())
		for _, condType := range []string{condHostnameAvailable, condReady} {
			condition := meta.FindStatusCondition(project.Status.Conditions, condType)
			Expect(condition).NotTo(BeNil(), condType+" should say what happened")
			Expect(condition.Status).To(Equal(metav1.ConditionFalse))
			Expect(condition.Reason).To(Equal(reasonReservedHostname))
			Expect(strings.Contains(condition.Message, "shop-pr-7.apps.example.com")).To(BeTrue(),
				"the condition should name the hostname: "+condition.Message)
		}
	})
})
