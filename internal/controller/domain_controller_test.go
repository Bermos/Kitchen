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
	"net"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// fakeResolver answers verification lookups from maps instead of the network.
// A name with no entry answers the way a real resolver does for a record that
// is not there: NXDOMAIN.
type fakeResolver struct {
	txt   map[string][]string
	cname map[string]string
	// err, when set, fails every lookup — the resolver-down case.
	err error
}

func (f *fakeResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	values, ok := f.txt[name]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}
	return values, nil
}

func (f *fakeResolver) LookupCNAME(_ context.Context, host string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	target, ok := f.cname[host]
	if !ok {
		return "", &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	return target, nil
}

var _ = Describe("Domain Controller", func() {
	const (
		domainName     = "shop-example-com"
		domainHostname = "shop.example.com"
		projectName    = "domshop"
		envName        = "domshop-production"
		releaseName    = "domshop-rel-000001"
	)

	ctx := context.Background()

	domainKey := types.NamespacedName{Name: domainName, Namespace: PlatformNamespace}
	certKey := types.NamespacedName{Name: domainCertificateName(domainName), Namespace: PlatformNamespace}
	secretKey := types.NamespacedName{Name: DomainTLSSecretName(domainName), Namespace: PlatformNamespace}
	routeKey := types.NamespacedName{Name: envName, Namespace: appNamespace(projectName)}

	var (
		resolver   *fakeResolver
		reconciler *DomainReconciler
	)

	domainCertificateObject := func() *unstructured.Unstructured {
		return certManagerObject("Certificate", domainCertificateName(domainName), PlatformNamespace)
	}

	reconcileDomain := func() reconcile.Result {
		result, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: domainKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
		return result
	}

	getDomain := func() *kitchenv1alpha1.Domain {
		domain := &kitchenv1alpha1.Domain{}
		ExpectWithOffset(1, k8sClient.Get(ctx, domainKey, domain)).To(Succeed())
		return domain
	}

	condition := func(condType string) *metav1.Condition {
		return meta.FindStatusCondition(getDomain().Status.Conditions, condType)
	}

	// markVerified is DNS having been set up: the token is only known after
	// the first reconcile wrote the instructions.
	markVerified := func() {
		result := reconcileDomain()
		domain := getDomain()
		ExpectWithOffset(1, domain.Status.Verification).NotTo(BeNil())
		resolver.txt[domain.Status.Verification.TXTRecord] = []string{domain.Status.Verification.TXTValue}
		ExpectWithOffset(1, result.RequeueAfter).To(Equal(time.Minute))
		reconcileDomain()
		ExpectWithOffset(1, getDomain().Status.Verified).To(BeTrue())
	}

	issuedSecret := func() *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name:      secretKey.Name,
			Namespace: secretKey.Namespace,
			Labels:    map[string]string{labelManagedByKey: labelManagedByValue},
		}}
	}

	BeforeEach(func() {
		resolver = &fakeResolver{txt: map[string][]string{}, cname: map[string]string{}}
		reconciler = &DomainReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Resolver: resolver}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())

		kitchen := &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
			},
		}
		ensureSingleton(ctx, kitchen)

		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())

		domain := &kitchenv1alpha1.Domain{
			ObjectMeta: metav1.ObjectMeta{Name: domainName, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.DomainSpec{
				Hostname:       domainHostname,
				EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: envName},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, domain))).To(Succeed())
	})

	AfterEach(func() {
		// The domain's finalizer has to run for the object to actually go.
		domain := &kitchenv1alpha1.Domain{}
		if err := k8sClient.Get(ctx, domainKey, domain); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, domain))).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: domainKey})
			Expect(err).NotTo(HaveOccurred())
		}
		for _, obj := range []client.Object{
			domainCertificateObject(),
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretKey.Name, Namespace: secretKey.Namespace}},
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: routeKey.Name, Namespace: routeKey.Namespace}},
			&gatewayv1.Gateway{ObjectMeta: metav1.ObjectMeta{Name: SharedGatewayName, Namespace: PlatformNamespace}},
			&gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: httpsRedirectRouteName, Namespace: PlatformNamespace}},
			certManagerObject("ClusterIssuer", acmeClusterIssuerName, ""),
			certManagerObject("ClusterIssuer", acmeHTTP01ClusterIssuerName, ""),
			certManagerObject("Certificate", wildcardCertificateName, PlatformNamespace),
			&kitchenv1alpha1.Environment{ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: PlatformNamespace}},
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("answers with the exact records to create while the domain is unverified", func() {
		result := reconcileDomain()
		Expect(result.RequeueAfter).To(Equal(time.Minute), "an absent record is polled, not watched")

		domain := getDomain()
		Expect(domain.Status.Verified).To(BeFalse())
		Expect(domain.Status.TLSMode).To(Equal(kitchenv1alpha1.TLSModeACME),
			"an empty spec.tls inherits the platform mode")

		By("spelling out the TXT record and the CNAME alternative")
		verification := domain.Status.Verification
		Expect(verification).NotTo(BeNil())
		Expect(verification.TXTRecord).To(Equal("_kitchen-challenge." + domainHostname))
		Expect(verification.TXTValue).To(HavePrefix("kitchen-verify="))
		Expect(verification.CNAMETarget).To(Equal(projectName+".apps.example.com"),
			"the CNAME suggestion is the environment's own generated hostname — the record routing needs anyway")

		By("saying the record is absent, not merely that verification failed")
		cond := condition(condVerified)
		Expect(cond).NotTo(BeNil())
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("RecordMissing"))
		Expect(cond.Message).To(ContainSubstring(verification.TXTRecord))

		By("holding the certificate and the route back until then")
		Expect(condition(condCertificateReady).Reason).To(Equal("AwaitingVerification"))
		Expect(condition(condRouteProgrammed).Reason).To(Equal("AwaitingVerification"))
		Expect(errors.IsNotFound(k8sClient.Get(ctx, certKey, domainCertificateObject()))).To(BeTrue(),
			"requesting a certificate for an unproven name would burn ACME rate limits on hopeless challenges")
	})

	It("tells a wrong value from an absent record from a failed lookup", func() {
		reconcileDomain()
		record := getDomain().Status.Verification.TXTRecord

		By("a record that exists with the wrong value")
		resolver.txt[record] = []string{"kitchen-verify=not-the-token"}
		reconcileDomain()
		cond := condition(condVerified)
		Expect(cond.Reason).To(Equal("RecordMismatch"))
		Expect(cond.Message).To(ContainSubstring("wrong value"))
		Expect(getDomain().Status.Verified).To(BeFalse())

		By("a lookup that could not complete at all")
		resolver.err = &net.DNSError{Err: "connection timed out", Name: record, IsTimeout: true}
		reconcileDomain()
		cond = condition(condVerified)
		Expect(cond.Reason).To(Equal("LookupFailed"))
		Expect(cond.Message).To(ContainSubstring("connection timed out"))
	})

	It("verifies through the TXT record and requests the certificate over HTTP-01", func() {
		markVerified()

		Expect(condition(condVerified).Reason).To(Equal("TXTRecordFound"))

		By("requesting the hostname's certificate from the HTTP-01 issuer")
		cert := domainCertificateObject()
		Expect(k8sClient.Get(ctx, certKey, cert)).To(Succeed())
		spec, _, err := unstructured.NestedMap(cert.Object, "spec")
		Expect(err).NotTo(HaveOccurred())
		Expect(spec).To(HaveKeyWithValue("secretName", secretKey.Name))
		Expect(spec).To(HaveKeyWithValue("dnsNames", []any{domainHostname}))
		Expect(spec).To(HaveKeyWithValue("issuerRef", map[string]any{
			"name":  acmeHTTP01ClusterIssuerName,
			"kind":  "ClusterIssuer",
			"group": "cert-manager.io",
		}), "the DNS-01 issuer's token cannot write to a zone the platform does not own")

		By("labelling the issued secret so the platform recognizes it as its own")
		labels, _, err := unstructured.NestedStringMap(cert.Object, "spec", "secretTemplate", "labels")
		Expect(err).NotTo(HaveOccurred())
		Expect(labels).To(HaveKeyWithValue(labelManagedByKey, labelManagedByValue))

		By("reporting issuance as pending, with the HTTP-01 dependency spelled out")
		cond := condition(condCertificateReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("Issuing"))
		Expect(cond.Message).To(ContainSubstring("must resolve to the platform"))
	})

	It("verifies through a CNAME pointing into the base domain", func() {
		resolver.cname[domainHostname] = projectName + ".apps.example.com."
		reconcileDomain()

		domain := getDomain()
		Expect(domain.Status.Verified).To(BeTrue())
		Expect(condition(condVerified).Reason).To(Equal("CNAMEPointsAtPlatform"))
	})

	It("stays verified when the record later disappears", func() {
		markVerified()
		resolver.txt = map[string][]string{}

		reconcileDomain()
		Expect(getDomain().Status.Verified).To(BeTrue(),
			"routing must not flap with a record owners typically delete after setup")
		Expect(condition(condVerified).Status).To(Equal(metav1.ConditionTrue))
	})

	It("mirrors what cert-manager says about the certificate", func() {
		markVerified()

		cert := domainCertificateObject()
		Expect(k8sClient.Get(ctx, certKey, cert)).To(Succeed())
		Expect(unstructured.SetNestedSlice(cert.Object, []any{
			map[string]any{
				"type":               "Ready",
				"status":             "True",
				"reason":             "TestWritten",
				"message":            certManagerReadyMessage,
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			},
		}, "status", "conditions")).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, cert)).To(Succeed())

		reconcileDomain()
		cond := condition(condCertificateReady)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Issued"))
		Expect(cond.Message).To(Equal(certManagerReadyMessage))
	})

	It("says what an acme domain on a non-acme platform is missing", func() {
		kitchen := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen)).To(Succeed())
		kitchen.Spec.TLS = kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeNone}
		Expect(k8sClient.Update(ctx, kitchen)).To(Succeed())

		domain := getDomain()
		domain.Spec.TLS = kitchenv1alpha1.TLSModeACME
		Expect(k8sClient.Update(ctx, domain)).To(Succeed())

		markVerified()
		cond := condition(condCertificateReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("NoACMEAccount"))
		Expect(cond.Message).To(ContainSubstring("no ACME account is configured"))
	})

	It("is honest about the manual step a cloudflared domain needs", func() {
		domain := getDomain()
		domain.Spec.TLS = kitchenv1alpha1.TLSModeCloudflared
		Expect(k8sClient.Update(ctx, domain)).To(Succeed())

		markVerified()

		By("neither requesting a certificate nor claiming the tunnel route exists")
		Expect(errors.IsNotFound(k8sClient.Get(ctx, certKey, domainCertificateObject()))).To(BeTrue())
		cond := condition(condCertificateReady)
		Expect(cond.Status).To(Equal(metav1.ConditionUnknown),
			"the tunnel is token-managed: the operator can neither add the hostname nor see whether someone did")
		Expect(cond.Reason).To(Equal("TunnelManagedExternally"))
		Expect(cond.Message).To(ContainSubstring("Cloudflare dashboard"))
		Expect(getDomain().Status.TLSMode).To(Equal(kitchenv1alpha1.TLSModeCloudflared))
	})

	It("reports no certificate at all for tls none", func() {
		domain := getDomain()
		domain.Spec.TLS = kitchenv1alpha1.TLSModeNone
		Expect(k8sClient.Update(ctx, domain)).To(Succeed())

		markVerified()
		Expect(condition(condCertificateReady)).To(BeNil(),
			"a domain served over plain HTTP by choice has nothing to report on")
	})

	It("observes the route rather than owning it", func() {
		markVerified()
		Expect(condition(condRouteProgrammed).Reason).To(Equal("RouteMissing"))

		By("noticing a route that does not carry the hostname yet")
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: routeKey.Namespace},
		}))).To(Succeed())
		route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: routeKey.Name, Namespace: routeKey.Namespace}}
		route.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: SharedGatewayName}}
		route.Spec.Hostnames = []gatewayv1.Hostname{gatewayv1.Hostname(projectName + ".apps.example.com")}
		Expect(k8sClient.Create(ctx, route)).To(Succeed())
		reconcileDomain()
		Expect(condition(condRouteProgrammed).Reason).To(Equal("HostnamePending"))

		By("waiting for the gateway once the hostname is on the route")
		Expect(k8sClient.Get(ctx, routeKey, route)).To(Succeed())
		route.Spec.Hostnames = append(route.Spec.Hostnames, domainHostname)
		Expect(k8sClient.Update(ctx, route)).To(Succeed())
		reconcileDomain()
		Expect(condition(condRouteProgrammed).Reason).To(Equal("AwaitingGatewayAcceptance"))

		By("counting only acceptance on the listener this domain's traffic uses")
		Expect(k8sClient.Get(ctx, routeKey, route)).To(Succeed())
		accepted := func(section string) []gatewayv1.RouteParentStatus {
			return []gatewayv1.RouteParentStatus{{
				ParentRef: gatewayv1.ParentReference{
					Name:        SharedGatewayName,
					SectionName: ptrSection(section),
				},
				ControllerName: "io.cilium/gateway-controller",
				Conditions: []metav1.Condition{{
					Type:               string(gatewayv1.RouteConditionAccepted),
					Status:             metav1.ConditionTrue,
					Reason:             "Accepted",
					Message:            "route accepted",
					LastTransitionTime: metav1.Now(),
				}},
			}}
		}
		route.Status.Parents = accepted(gatewayListenerHTTPS)
		Expect(k8sClient.Status().Update(ctx, route)).To(Succeed())
		reconcileDomain()
		Expect(condition(condRouteProgrammed).Reason).To(Equal("AwaitingGatewayAcceptance"),
			"the wildcard listener accepting the route says nothing about the custom hostname")

		By("reporting acceptance from the route's own status")
		Expect(k8sClient.Get(ctx, routeKey, route)).To(Succeed())
		route.Status.Parents = accepted(domainListenerName(domainName))
		Expect(k8sClient.Status().Update(ctx, route)).To(Succeed())
		reconcileDomain()
		cond := condition(condRouteProgrammed)
		Expect(cond.Status).To(Equal(metav1.ConditionTrue))
		Expect(cond.Reason).To(Equal("Accepted"))
	})

	It("tears its children down through the finalizer", func() {
		markVerified()
		Expect(k8sClient.Get(ctx, certKey, domainCertificateObject())).To(Succeed())

		By("standing in for cert-manager: the issued secret exists")
		Expect(k8sClient.Create(ctx, issuedSecret())).To(Succeed())

		By("deleting the domain")
		Expect(k8sClient.Delete(ctx, getDomain())).To(Succeed())
		reconcileDomain()

		Expect(errors.IsNotFound(k8sClient.Get(ctx, domainKey, &kitchenv1alpha1.Domain{}))).To(BeTrue())
		Expect(errors.IsNotFound(k8sClient.Get(ctx, certKey, domainCertificateObject()))).To(BeTrue(),
			"the certificate goes with the domain")
		Expect(errors.IsNotFound(k8sClient.Get(ctx, secretKey, &corev1.Secret{}))).To(BeTrue(),
			"cert-manager does not delete a certificate's secret, so the finalizer must")
	})

	It("leaves a secret the platform did not issue alone on teardown", func() {
		secret := issuedSecret()
		secret.Labels = nil
		Expect(k8sClient.Create(ctx, secret)).To(Succeed())

		Expect(k8sClient.Delete(ctx, getDomain())).To(Succeed())
		reconcileDomain()

		Expect(k8sClient.Get(ctx, secretKey, &corev1.Secret{})).To(Succeed(),
			"a same-named secret something else wrote is not the platform's to delete")
	})

	Context("the shared Gateway", func() {
		var kitchenReconciler *KitchenReconciler

		BeforeEach(func() {
			kitchenReconciler = &KitchenReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
		})

		reconcileKitchen := func() {
			_, err := kitchenReconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: KitchenSingletonName},
			})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		gatewayListeners := func() []gatewayv1.Listener {
			gw := &gatewayv1.Gateway{}
			ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{
				Name: SharedGatewayName, Namespace: PlatformNamespace,
			}, gw)).To(Succeed())
			return gw.Spec.Listeners
		}

		listenerNamed := func(name string) *gatewayv1.Listener {
			for _, listener := range gatewayListeners() {
				if string(listener.Name) == name {
					return &listener
				}
			}
			return nil
		}

		It("gains a listener when the domain is ready and loses it with the domain", func() {
			By("no listener while the domain is unverified")
			reconcileKitchen()
			Expect(listenerNamed(domainListenerName(domainName))).To(BeNil())

			By("no listener while the certificate secret does not exist")
			markVerified()
			reconcileKitchen()
			Expect(listenerNamed(domainListenerName(domainName))).To(BeNil(),
				"a listener referencing a secret that is not there would break the whole Gateway")

			By("the listener appears once the secret is issued")
			Expect(k8sClient.Create(ctx, issuedSecret())).To(Succeed())
			reconcileKitchen()
			listener := listenerNamed(domainListenerName(domainName))
			Expect(listener).NotTo(BeNil())
			Expect(listener.Port).To(Equal(gatewayv1.PortNumber(443)))
			Expect(string(*listener.Hostname)).To(Equal(domainHostname))
			Expect(string(listener.TLS.CertificateRefs[0].Name)).To(Equal(secretKey.Name))

			By("the listener goes when the domain does")
			Expect(k8sClient.Delete(ctx, getDomain())).To(Succeed())
			reconcileKitchen()
			Expect(listenerNamed(domainListenerName(domainName))).To(BeNil(),
				"a deleting domain loses its listener before the finalizer removes the secret")
			reconcileDomain()
		})
	})

	Context("the environment's route", func() {
		var envReconciler *EnvironmentReconciler

		envKey := types.NamespacedName{Name: envName, Namespace: PlatformNamespace}

		BeforeEach(func() {
			envReconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

			project := &kitchenv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: PlatformNamespace},
				Spec: kitchenv1alpha1.ProjectSpec{
					Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
						Repo:          "acme/shop",
					}},
					Registry: &kitchenv1alpha1.RegistrySpec{
						ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
					},
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

			release := &kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: PlatformNamespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "domshop-bld-1"},
					Image:      "registry.example.com/domshop@sha256:0123",
				},
			}
			Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, release))).To(Succeed())
		})

		AfterEach(func() {
			env := &kitchenv1alpha1.Environment{}
			if err := k8sClient.Get(ctx, envKey, env); err == nil {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
				_, err := envReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
				Expect(err).NotTo(HaveOccurred())
			}
			for _, obj := range []client.Object{
				&kitchenv1alpha1.Release{ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: PlatformNamespace}},
				&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: PlatformNamespace}},
			} {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
			}
		})

		reconcileEnv := func() {
			_, err := envReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
			ExpectWithOffset(1, err).NotTo(HaveOccurred())
		}

		getRoute := func() *gatewayv1.HTTPRoute {
			route := &gatewayv1.HTTPRoute{}
			ExpectWithOffset(1, k8sClient.Get(ctx, routeKey, route)).To(Succeed())
			return route
		}

		It("carries a verified domain's hostname and drops it with the domain", func() {
			By("an unverified domain stays off the route")
			reconcileEnv()
			reconcileEnv()
			Expect(getRoute().Spec.Hostnames).To(ConsistOf(
				gatewayv1.Hostname(projectName + ".apps.example.com")))

			By("a verified domain's hostname joins, without its listener yet")
			markVerified()
			reconcileEnv()
			route := getRoute()
			Expect(route.Spec.Hostnames).To(ConsistOf(
				gatewayv1.Hostname(projectName+".apps.example.com"),
				gatewayv1.Hostname(domainHostname)))
			Expect(routeSectionNames(route)).To(ConsistOf(gatewayListenerHTTPS),
				"the per-domain listener cannot exist before its secret, so the route does not name it")

			By("binding the per-domain listener once the certificate secret exists")
			Expect(k8sClient.Create(ctx, issuedSecret())).To(Succeed())
			reconcileEnv()
			Expect(routeSectionNames(getRoute())).To(ConsistOf(
				gatewayListenerHTTPS, domainListenerName(domainName)))

			By("dropping the hostname when the domain is deleted")
			Expect(k8sClient.Delete(ctx, getDomain())).To(Succeed())
			reconcileEnv()
			Expect(getRoute().Spec.Hostnames).To(ConsistOf(
				gatewayv1.Hostname(projectName + ".apps.example.com")))
			reconcileDomain()
		})

		It("routes a tls-none domain over the shared HTTP listener", func() {
			domain := getDomain()
			domain.Spec.TLS = kitchenv1alpha1.TLSModeNone
			Expect(k8sClient.Update(ctx, domain)).To(Succeed())
			markVerified()

			reconcileEnv()
			reconcileEnv()

			route := getRoute()
			Expect(route.Spec.Hostnames).To(ContainElement(gatewayv1.Hostname(domainHostname)))
			Expect(routeSectionNames(route)).To(ConsistOf(gatewayListenerHTTPS, gatewayListenerHTTP),
				"on an edge-TLS platform the plain-HTTP domain needs port 80 bound explicitly")
		})
	})
})

// ptrSection and routeSectionNames keep the assertions readable.
func ptrSection(name string) *gatewayv1.SectionName {
	section := gatewayv1.SectionName(name)
	return &section
}

func routeSectionNames(route *gatewayv1.HTTPRoute) []string {
	var names []string
	for _, parent := range route.Spec.ParentRefs {
		if parent.SectionName != nil {
			names = append(names, string(*parent.SectionName))
		}
	}
	return names
}
