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
	"encoding/json"
	"fmt"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider"
)

// The project's own registry accounts, from the operator's side: what the
// token service checks a password against is written once and kept, and what
// the project's pods hold is that — never the platform's credential.
var _ = Describe("Per-project registry accounts", func() {
	const namespace = "default"

	ctx := context.Background()

	// A project and so an application namespace per spec, for the reason
	// projectsecrets_test.go gives: envtest runs no namespace controller.
	specs := 0

	var (
		reconciler  *ProjectReconciler
		projectName string
		projectKey  types.NamespacedName
		accountKey  types.NamespacedName
		appNS       string
	)

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// dockerAuth is the one entry a copied credential holds.
	dockerAuth := func(secret *corev1.Secret) (username, password string) {
		var cfg struct {
			Auths map[string]struct {
				Username string `json:"username"`
				Password string `json:"password"`
			} `json:"auths"`
		}
		ExpectWithOffset(1, json.Unmarshal(secret.Data[corev1.DockerConfigJsonKey], &cfg)).To(Succeed())
		ExpectWithOffset(1, cfg.Auths).To(HaveKey("registry.apps.example.com"))
		entry := cfg.Auths["registry.apps.example.com"]
		return entry.Username, entry.Password
	}

	connection := func(name string, perProject bool) {
		config, err := json.Marshal(map[string]string{"url": "registry.apps.example.com"})
		Expect(err).NotTo(HaveOccurred())
		conn := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "dockerRegistry",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "kitchen-connection-" + name},
				Config:               &runtime.RawExtension{Raw: config},
			},
		}
		if perProject {
			conn.Annotations = map[string]string{RegistryAccountsAnnotation: RegistryAccountsPerProject}
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, conn))).To(Succeed())
		platform, err := provider.DockerConfigJSON("registry.apps.example.com", "kitchen", "the-platforms-own")
		Expect(err).NotTo(HaveOccurred())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "kitchen-connection-" + name, Namespace: namespace},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: platform},
		}))).To(Succeed())
	}

	project := func() *kitchenv1alpha1.Project {
		p := &kitchenv1alpha1.Project{}
		ExpectWithOffset(1, k8sClient.Get(ctx, projectKey, p)).To(Succeed())
		return p
	}

	BeforeEach(func() {
		specs++
		projectName = fmt.Sprintf("larder-%d", specs)
		projectKey = types.NamespacedName{Name: projectName, Namespace: namespace}
		accountKey = types.NamespacedName{Name: RegistryAccountSecretName(projectName), Namespace: namespace}
		appNS = appNamespace(projectName)

		reconciler = &ProjectReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
		})
		connection("bundled", true)
		connection("harbor", false)

		Expect(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/" + projectName,
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "bundled"},
				},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		if err := k8sClient.Get(ctx, projectKey, &kitchenv1alpha1.Project{}); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, project()))).To(Succeed())
			Eventually(func() bool {
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
				Expect(err).NotTo(HaveOccurred())
				return errors.IsNotFound(k8sClient.Get(ctx, projectKey, &kitchenv1alpha1.Project{}))
			}).Should(BeTrue())
		}
		for _, obj := range []client.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: accountKey.Name, Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kitchen-webhook-" + projectName, Namespace: namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("gives a new project two accounts of its own, owned by it", func() {
		reconcileOnce()

		account := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, accountKey, account)).To(Succeed())
		push := string(account.Data[RegistryAccountPushPasswordKey])
		pull := string(account.Data[RegistryAccountPullPasswordKey])
		Expect(push).To(HaveLen(64))
		Expect(pull).To(HaveLen(64))
		Expect(push).NotTo(Equal(pull))
		Expect(account.OwnerReferences).To(HaveLen(1))
		Expect(account.OwnerReferences[0].Name).To(Equal(projectName),
			"the account has to go with the project even if the finalizer never reaches its own delete")

		By("putting them, and not the platform's, where the project's pods read them")
		copied := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: registrySecretName("bundled")}, copied)).To(Succeed())
		username, password := dockerAuth(copied)
		Expect(username).To(Equal(projectName + ".push"))
		Expect(password).To(Equal(push))
		Expect(copied.Type).To(Equal(corev1.SecretTypeDockerConfigJson),
			"the kubelet reads an image pull secret of this type and no other")

		read := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Namespace: appNS, Name: readCredentialSecretName(registrySecretName("bundled")),
		}, read)).To(Succeed())
		username, password = dockerAuth(read)
		Expect(username).To(Equal(projectName + ".pull"))
		Expect(password).To(Equal(pull))

		By("keeping the same passwords on every reconcile after")
		reconcileOnce()
		again := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, accountKey, again)).To(Succeed())
		Expect(string(again.Data[RegistryAccountPushPasswordKey])).To(Equal(push))
	})

	// The migration: an installation that upgrades into per-project accounts
	// has application namespaces holding copies of the platform's credential,
	// and environments that may not build again for a long time.
	It("replaces a copy of the platform's credential an older platform left behind", func() {
		reconcileOnce()
		stale, err := provider.DockerConfigJSON("registry.apps.example.com", "kitchen", "the-platforms-own")
		Expect(err).NotTo(HaveOccurred())
		copied := &corev1.Secret{}
		key := types.NamespacedName{Namespace: appNS, Name: registrySecretName("bundled")}
		Expect(k8sClient.Get(ctx, key, copied)).To(Succeed())
		copied.Data = map[string][]byte{corev1.DockerConfigJsonKey: stale}
		Expect(k8sClient.Update(ctx, copied)).To(Succeed())

		reconcileOnce()

		Expect(k8sClient.Get(ctx, key, copied)).To(Succeed())
		username, _ := dockerAuth(copied)
		Expect(username).To(Equal(projectName + ".push"))
	})

	It("still copies the connection's own credential for a registry with no per-project accounts", func() {
		reconcileOnce()
		p := project()
		push, read, err := writeRegistryCredentials(ctx, k8sClient, &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "harbor", Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "dockerRegistry",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "kitchen-connection-harbor"},
			},
		}, p, appNS)
		Expect(err).NotTo(HaveOccurred())
		Expect(read).To(BeEmpty(), "harbor issues no read-only credential here")
		copied := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: push}, copied)).To(Succeed())
		username, password := dockerAuth(copied)
		Expect(username).To(Equal("kitchen"))
		Expect(password).To(Equal("the-platforms-own"))
	})

	It("removes the account with the project", func() {
		reconcileOnce()
		Expect(k8sClient.Get(ctx, accountKey, &corev1.Secret{})).To(Succeed())

		Expect(k8sClient.Delete(ctx, project())).To(Succeed())
		Eventually(func() bool {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: projectKey})
			Expect(err).NotTo(HaveOccurred())
			return errors.IsNotFound(k8sClient.Get(ctx, projectKey, &kitchenv1alpha1.Project{}))
		}).Should(BeTrue())

		Expect(errors.IsNotFound(k8sClient.Get(ctx, accountKey, &corev1.Secret{}))).To(BeTrue())
	})
})

func TestParseRegistryUsername(t *testing.T) {
	for _, tc := range []struct {
		username string
		project  string
		push     bool
		ok       bool
	}{
		{"shop.push", "shop", true, true},
		{"shop.pull", "shop", false, true},
		{"shop-api.push", "shop-api", true, true},
		// The platform's own accounts are not project accounts.
		{"kitchen", "", false, false},
		{"kitchen-read", "", false, false},
		{".push", "", false, false},
		// A project name is a DNS label: no dot, so no ambiguity about where
		// the project ends.
		{"a.b.push", "", false, false},
		{"shop.admin", "", false, false},
	} {
		project, push, ok := ParseRegistryUsername(tc.username)
		if project != tc.project || push != tc.push || ok != tc.ok {
			t.Errorf("ParseRegistryUsername(%q) = %q, %v, %v; want %q, %v, %v",
				tc.username, project, push, ok, tc.project, tc.push, tc.ok)
		}
		if tc.ok {
			want := RegistryPullUsername(tc.project)
			if tc.push {
				want = RegistryPushUsername(tc.project)
			}
			if want != tc.username {
				t.Errorf("username %q does not round-trip: %q", tc.username, want)
			}
		}
	}
}

func TestWorkloadRepositoryIsUnderTheProjectOnPerProjectRegistries(t *testing.T) {
	bundled := provider.RegistryTarget{Prefix: "registry.apps.example.com", PerProjectAccounts: true}
	if got := workloadRepository(bundled, "shop", "api"); got != "registry.apps.example.com/shop/api" {
		t.Errorf("on the bundled registry a workload's image goes under the project, got %q", got)
	}
	// Elsewhere the path segment is usually the account, so it stays beside.
	harbor := provider.RegistryTarget{Prefix: "harbor.example.com/kitchen"}
	if got := workloadRepository(harbor, "shop", "api"); got != "harbor.example.com/kitchen/shop-api" {
		t.Errorf("on another registry a workload's image goes beside the project, got %q", got)
	}
}
