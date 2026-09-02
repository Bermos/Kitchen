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
	"net/http"
	"net/http/httptest"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/idp"
)

// clientIssuer is an identity provider that registers clients and maintains
// them at the operator's own prefix — which is what the identity provider the
// chart ships does, since the OAuth provider plugin implements RFC 7591 and
// not RFC 7592.
type clientIssuer struct {
	*httptest.Server

	mu            sync.Mutex
	registrations int
	redirects     []string
	deleted       []string
	refuseUpdates bool
}

func newClientIssuer() *clientIssuer {
	issuer := &clientIssuer{}
	mux := http.NewServeMux()
	issuer.Server = httptest.NewServer(mux)

	mux.HandleFunc(idp.DiscoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                issuer.URL,
			"token_endpoint":        issuer.URL + "/oauth2/token",
			"registration_endpoint": issuer.URL + "/oauth2/register",
		})
	})
	mux.HandleFunc("/oauth2/register", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body struct {
			RedirectURIs []string `json:"redirect_uris"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)

		issuer.mu.Lock()
		issuer.registrations++
		issuer.redirects = body.RedirectURIs
		issuer.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]string{
			"client_id":     "app-client",
			"client_secret": "app-secret",
		})
	})
	mux.HandleFunc(idp.ClientsPath, func(w http.ResponseWriter, r *http.Request) {
		issuer.mu.Lock()
		defer issuer.mu.Unlock()
		if issuer.refuseUpdates {
			// An issuer that has never heard of the prefix, which is every
			// issuer but the one the chart ships.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodPut:
			var body struct {
				RedirectURIs []string `json:"redirectURIs"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			issuer.redirects = body.RedirectURIs
			_ = json.NewEncoder(w).Encode(map[string]string{"clientId": "app-client"})
		case http.MethodDelete:
			issuer.deleted = append(issuer.deleted, r.URL.Query().Get("clientId"))
			_ = json.NewEncoder(w).Encode(map[string]string{"clientId": "app-client"})
		}
	})
	return issuer
}

func (i *clientIssuer) state() (int, []string, []string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.registrations, append([]string(nil), i.redirects...), append([]string(nil), i.deleted...)
}

var _ = Describe("ResourceClaim of type oidcClient", func() {
	const (
		projectName = "oidcshop"
		claimName   = "oidcshop-auth"
		namespace   = "default"
		authSecret  = "claim-auth-secret"
		baseDomain  = "apps.example.com"
	)

	ctx := context.Background()
	claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}
	appNS := "kitchen-" + projectName
	bindingKey := types.NamespacedName{Name: claimName + "-binding", Namespace: appNS}
	recordKey := types.NamespacedName{Name: claimName + "-oidc-client", Namespace: namespace}

	var (
		issuer     *clientIssuer
		reconciler *ResourceClaimReconciler
	)

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	getClaim := func() *kitchenv1alpha1.ResourceClaim {
		claim := &kitchenv1alpha1.ResourceClaim{}
		ExpectWithOffset(1, k8sClient.Get(ctx, claimKey, claim)).To(Succeed())
		return claim
	}

	// createEnvironment publishes one environment of the project at a URL,
	// the way the environment reconciler does once its route is programmed.
	createEnvironment := func(name string, envType kitchenv1alpha1.EnvironmentType, pullRequest int32, url string) {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: name + "-release"},
				Type:       envType,
			},
		}
		if envType == kitchenv1alpha1.EnvironmentPreview {
			env.Spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: pullRequest, Branch: "feature"}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, env)).To(Succeed())
		env.Status.URL = url
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, env)).To(Succeed())
	}

	BeforeEach(func() {
		issuer = newClientIssuer()
		reconciler = &ResourceClaimReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: authSecret, Namespace: PlatformNamespace},
			StringData: map[string]string{
				idp.SecretKeyIssuer:     issuer.URL,
				idp.SecretKeyServiceKey: "the-service-key",
			},
		}))).To(Succeed())
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: baseDomain, TLS: acmeTLS()},
		})

		// Written rather than assumed: the singleton is one object for the
		// whole suite, and a spec that inherited another's platform would be
		// asserting against a base domain it did not choose.
		kitchen := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen)).To(Succeed())
		kitchen.Spec.BaseDomain = baseDomain
		kitchen.Spec.TLS = acmeTLS()
		kitchen.Spec.Auth = kitchenv1alpha1.AuthSpec{
			Enabled:   true,
			SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: authSecret},
		}
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		Expect(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/oidcshop",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		claim := &kitchenv1alpha1.ResourceClaim{}
		if err := k8sClient.Get(ctx, claimKey, claim); err == nil {
			Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey})
			Expect(err).NotTo(HaveOccurred())
		}
		environments := &kitchenv1alpha1.EnvironmentList{}
		Expect(k8sClient.List(ctx, environments, client.InNamespace(namespace))).To(Succeed())
		for i := range environments.Items {
			if environments.Items[i].Spec.ProjectRef.Name == projectName {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &environments.Items[i]))).To(Succeed())
			}
		}
		domains := &kitchenv1alpha1.DomainList{}
		Expect(k8sClient.List(ctx, domains, client.InNamespace(namespace))).To(Succeed())
		for i := range domains.Items {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &domains.Items[i]))).To(Succeed())
		}
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: authSecret, Namespace: PlatformNamespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: recordKey.Name, Namespace: recordKey.Namespace}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: bindingKey.Name, Namespace: bindingKey.Namespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		issuer.Close()
	})

	createClaim := func(config map[string]any) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.ClaimTypeOIDCClient,
			},
		}
		if config != nil {
			raw, err := json.Marshal(config)
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
			claim.Spec.Config = &runtime.RawExtension{Raw: raw}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	It("registers a client and binds it before the project has ever been deployed", func() {
		createClaim(nil)
		reconcileOnce()

		By("binding the claim to a secret the application's env vars can read")
		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.SecretName).To(Equal(claimName + "-binding"))
		Expect(claim.Status.InstanceID).To(Equal("app-client"))

		binding := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, bindingKey, binding)).To(Succeed())
		Expect(string(binding.Data[bindingKeyIssuer])).To(Equal(issuer.URL))
		Expect(string(binding.Data[bindingKeyClientID])).To(Equal("app-client"))
		Expect(string(binding.Data[bindingKeyClientSecret])).To(Equal("app-secret"))

		By("registering production's callback, which is knowable without an environment")
		registrations, redirects, _ := issuer.state()
		Expect(registrations).To(Equal(1))
		Expect(redirects).To(ConsistOf(
			"https://oidcshop.apps.example.com/auth/callback",
			"https://oidcshop.apps.example.com/api/auth/callback/kitchen",
		))

		By("keeping the credentials the application must never see out of its namespace")
		record := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, recordKey, record)).To(Succeed())
		Expect(string(record.Data[oidcKeyClientSecret])).To(Equal("app-secret"))
		Expect(binding.Data).NotTo(HaveKey(oidcKeyRegistrationToken))
	})

	It("adds a preview's callback when it appears and drops it when the pull request closes", func() {
		createClaim(map[string]any{"callbackPaths": []string{"/auth/callback"}})
		reconcileOnce()

		_, redirects, _ := issuer.state()
		Expect(redirects).To(ConsistOf("https://oidcshop.apps.example.com/auth/callback"))

		By("following the preview environment's own URL")
		createEnvironment(projectName+"-pr-42", kitchenv1alpha1.EnvironmentPreview, 42,
			"https://oidcshop-pr-42.apps.example.com")
		reconcileOnce()

		registrations, redirects, _ := issuer.state()
		Expect(registrations).To(Equal(1), "a redirect list changing must not cost the application a new client id")
		Expect(redirects).To(ConsistOf(
			"https://oidcshop.apps.example.com/auth/callback",
			"https://oidcshop-pr-42.apps.example.com/auth/callback",
		))
		Expect(getClaim().Status.RedirectURIs).To(HaveLen(2))

		By("taking it away again with the environment")
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx,
			types.NamespacedName{Name: projectName + "-pr-42", Namespace: namespace}, env)).To(Succeed())
		Expect(k8sClient.Delete(ctx, env)).To(Succeed())
		reconcileOnce()

		_, redirects, _ = issuer.state()
		Expect(redirects).To(ConsistOf("https://oidcshop.apps.example.com/auth/callback"))
	})

	It("sends nothing to the issuer when the world has not moved", func() {
		createClaim(map[string]any{"callbackPaths": []string{"/auth/callback"}})
		reconcileOnce()
		reconcileOnce()
		reconcileOnce()

		registrations, _, _ := issuer.state()
		Expect(registrations).To(Equal(1))
		condition := meta.FindStatusCondition(getClaim().Status.Conditions, condRedirectURIs)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionTrue))
	})

	It("registers a verified custom domain and the claim's own verbatim URIs", func() {
		createClaim(map[string]any{
			"callbackPaths": []string{"/auth/callback"},
			"redirectURIs":  []string{"http://localhost:3000/auth/callback"},
		})
		createEnvironment(projectName+"-production", kitchenv1alpha1.EnvironmentProduction, 0,
			"https://oidcshop.apps.example.com")

		verified := &kitchenv1alpha1.Domain{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-example-com", Namespace: namespace},
			Spec: kitchenv1alpha1.DomainSpec{
				Hostname:       "shop.example.com",
				EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-production"},
			},
		}
		Expect(k8sClient.Create(ctx, verified)).To(Succeed())
		verified.Status.Verified = true
		verified.Status.TLSMode = kitchenv1alpha1.TLSModeACME
		Expect(k8sClient.Status().Update(ctx, verified)).To(Succeed())

		unverified := &kitchenv1alpha1.Domain{
			ObjectMeta: metav1.ObjectMeta{Name: "not-mine-example-com", Namespace: namespace},
			Spec: kitchenv1alpha1.DomainSpec{
				Hostname:       "not-mine.example.com",
				EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-production"},
			},
		}
		Expect(k8sClient.Create(ctx, unverified)).To(Succeed())

		reconcileOnce()

		_, redirects, _ := issuer.state()
		Expect(redirects).To(ConsistOf(
			"https://oidcshop.apps.example.com/auth/callback",
			"https://shop.example.com/auth/callback",
			"http://localhost:3000/auth/callback",
		))
	})

	It("stays bound and says so when the issuer cannot be asked to change a client", func() {
		createClaim(map[string]any{"callbackPaths": []string{"/auth/callback"}})
		reconcileOnce()

		issuer.mu.Lock()
		issuer.refuseUpdates = true
		issuer.mu.Unlock()

		createEnvironment(projectName+"-pr-9", kitchenv1alpha1.EnvironmentPreview, 9,
			"https://oidcshop-pr-9.apps.example.com")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound),
			"the client still works everywhere it was registered for")
		condition := meta.FindStatusCondition(claim.Status.Conditions, condRedirectURIs)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Message).To(ContainSubstring("https://oidcshop-pr-9.apps.example.com/auth/callback"))
	})

	It("deregisters the client when the claim is deleted, whatever the deletion policy says", func() {
		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Spec.DeletionPolicy).To(Equal(kitchenv1alpha1.ClaimRetain),
			"Retain is the CRD's default, and it has no say over an OAuth client")
		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		reconcileOnce()

		_, _, deleted := issuer.state()
		Expect(deleted).To(ConsistOf("app-client"))
		Expect(k8sClient.Get(ctx, claimKey, &kitchenv1alpha1.ResourceClaim{})).NotTo(Succeed())
		Expect(k8sClient.Get(ctx, recordKey, &corev1.Secret{})).NotTo(Succeed())
		Expect(k8sClient.Get(ctx, bindingKey, &corev1.Secret{})).NotTo(Succeed())
	})

	It("waits, rather than registering, on a platform with no identity provider", func() {
		kitchen := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen)).To(Succeed())
		kitchen.Spec.Auth.SecretRef = nil
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		createClaim(nil)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimPending))
		condition := meta.FindStatusCondition(claim.Status.Conditions, condReady)
		Expect(condition.Reason).To(Equal("IdentityProviderMissing"))
		registrations, _, _ := issuer.state()
		Expect(registrations).To(BeZero())
	})
})
