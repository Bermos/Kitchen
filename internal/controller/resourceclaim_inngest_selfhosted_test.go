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
	"fmt"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/inngest"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// fakeSelfHosted is a self-hosted Inngest as this controller sees it: a
// server per claim and one per preview, parkable and destroyable. What the
// server's objects look like is internal/provider/inngest's own tests; what
// is asserted here is the wiring — that the claim records the server, that a
// preview gets one of its own, that parking reaches it, and that deleting
// the claim takes it back.
type fakeSelfHosted struct {
	mu sync.Mutex
	// namespace stands in for kitchen-inngest, so that an instance ID here
	// has the namespace/name shape the real one has.
	namespace string
	// servers is every server that exists, by name, and serveURL is what
	// each was last told to call.
	servers map[string]string
	idled   map[string]bool
	deleted []string
}

func newFakeSelfHosted() *fakeSelfHosted {
	return &fakeSelfHosted{
		namespace: inngest.DefaultServerNamespace,
		servers:   map[string]string{},
		idled:     map[string]bool{},
	}
}

func (f *fakeSelfHosted) id(name string) string { return f.namespace + "/" + name }

func (f *fakeSelfHosted) binding(name string) inngest.Binding {
	return inngest.Binding{
		EventKey:          "ev-" + name,
		SigningKey:        "sign-" + name,
		BaseURL:           fmt.Sprintf("http://%s.%s.svc:8288", name, f.namespace),
		Dev:               "0",
		ConnectGatewayURL: fmt.Sprintf("ws://%s.%s.svc:8289/v0/connect", name, f.namespace),
	}
}

func (f *fakeSelfHosted) Provision(
	_ context.Context,
	res naming.Resource,
	req inngest.Requirements,
) (inngest.Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name := "kitchen-" + res.Project + "-" + res.Claim
	f.servers[name] = req.ServeURL
	return inngest.Instance{
		ID:      f.id(name),
		Name:    name,
		Binding: f.binding(name),
		Reason:  "ServerReady",
		Message: "Inngest " + name + " is serving",
	}, nil
}

func (f *fakeSelfHosted) CreateBranch(
	_ context.Context,
	instanceID, name string,
	req inngest.Requirements,
) (inngest.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	child := instanceID[len(f.namespace)+1:] + "-" + name
	f.servers[child] = req.ServeURL
	return inngest.Branch{ID: f.id(child), Binding: f.binding(child)}, nil
}

func (f *fakeSelfHosted) DeleteBranch(_ context.Context, _, branchID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, branchID)
	return nil
}

func (f *fakeSelfHosted) Deprovision(_ context.Context, instanceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, instanceID)
	return nil
}

func (f *fakeSelfHosted) IdleBranch(_ context.Context, branchID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idled[branchID] = true
	return nil
}

func (f *fakeSelfHosted) WakeBranch(_ context.Context, branchID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.idled[branchID] = false
	return nil
}

func (f *fakeSelfHosted) lastServeURL(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.servers[name]
}

var _ = Describe("A self-hosted inngest claim", func() {
	const (
		projectName    = "shjobs"
		claimName      = "shjobs-inngest"
		connectionName = "shinngest"
		namespace      = "default"
		previewEnvName = "shjobs-pr-9"
		serverName     = "kitchen-shjobs-shjobs-inngest"
	)

	ctx := context.Background()
	claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}
	envKey := types.NamespacedName{Name: previewEnvName, Namespace: namespace}
	appNS := "kitchen-" + projectName

	var (
		server     *fakeSelfHosted
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

	createClaim := func(config string) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: connectionName},
				Type:          kitchenv1alpha1.ClaimTypeInngest,
			},
		}
		if config != "" {
			claim.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	// The production environment, whose URL is what a serve-mode server is
	// told to call, and one preview.
	createEnvironment := func(name string, envType kitchenv1alpha1.EnvironmentType, url string) {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       envType,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-rel-1"},
			},
		}
		if envType == kitchenv1alpha1.EnvironmentPreview {
			env.Spec.Preview = &kitchenv1alpha1.PreviewInfo{PullRequest: 9, Branch: "feature/jobs"}
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, env)).To(Succeed())
		env.Status.URL = url
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, env)).To(Succeed())
	}

	setIdle := func(name string, idle bool) {
		env := &kitchenv1alpha1.Environment{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, env)).To(Succeed())
		env.Status.Idle = idle
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, env)).To(Succeed())
	}

	BeforeEach(func() {
		server = newFakeSelfHosted()
		reconciler = &ResourceClaimReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Inngest: func(inngest.Options) (inngest.Provisioner, error) {
				return server, nil
			},
		}

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/shjobs",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
				Previews: kitchenv1alpha1.PreviewsSpec{Enabled: ptr.To(true), Protected: ptr.To(false)},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		// The provider with no credential at all: it runs the server itself,
		// with the operator's own account.
		connection := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace},
			Spec:       kitchenv1alpha1.ConnectionSpec{Provider: inngest.ProviderSelfHosted},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, connection))).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName, Namespace: namespace}, connection)).To(Succeed())
		connection.Status.Capabilities = []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityBackgroundJobs}
		Expect(k8sClient.Status().Update(ctx, connection)).To(Succeed())
	})

	AfterEach(func() {
		claim := &kitchenv1alpha1.ResourceClaim{}
		if err := k8sClient.Get(ctx, claimKey, claim); err == nil {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: claimKey})
			Expect(err).NotTo(HaveOccurred())
		}
		environments := &kitchenv1alpha1.EnvironmentList{}
		Expect(k8sClient.List(ctx, environments, client.InNamespace(namespace))).To(Succeed())
		for i := range environments.Items {
			env := &environments.Items[i]
			if env.Spec.ProjectRef.Name != projectName {
				continue
			}
			if controllerutil.RemoveFinalizer(env, claimBranchFinalizer) ||
				controllerutil.RemoveFinalizer(env, environmentFinalizer) {
				Expect(k8sClient.Update(ctx, env)).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, env))).To(Succeed())
		}
		for _, obj := range []client.Object{
			&kitchenv1alpha1.Connection{ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	// The binding is the server's: its address, its own keys, and the two
	// variables an SDK needs to be told a server is not Inngest Cloud.
	It("binds a server of the claim's own and says what it costs", func() {
		createClaim("")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.InstanceID).To(Equal(inngest.DefaultServerNamespace + "/" + serverName))
		Expect(claim.Status.InstanceName).To(Equal(serverName),
			"the server's name is recorded, so a later reconcile finds the same one")
		Expect(claim.Status.PreviewMode).To(Equal("fresh"), "a preview gets a server of its own")
		Expect(claim.Status.CanIdle).To(BeTrue(), "and it parks with the preview")
		Expect(claim.Status.KeepsPodsRunning).To(BeTrue(), "a connect worker holds the pods up wherever it dials")

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: claim.Status.SecretName}, secret)).To(Succeed())
		Expect(string(secret.Data[inngest.KeyBaseURL])).To(ContainSubstring(serverName))
		Expect(string(secret.Data[inngest.KeyDev])).To(Equal("0"))
		Expect(string(secret.Data[inngest.KeyConnectGatewayURL])).To(ContainSubstring(":8289/v0/connect"))
		Expect(string(secret.Data[inngest.KeyEnv])).To(Equal(""), "a self-hosted server has no environments")

		// A provider that publishes no app inventory says so rather than
		// reporting that nothing has connected.
		connected := meta.FindStatusCondition(claim.Status.Conditions, condAppConnected)
		Expect(connected).NotTo(BeNil())
		Expect(connected.Status).To(Equal(metav1.ConditionUnknown))
		Expect(connected.Reason).To(Equal("NotReported"))
		Expect(connected.Message).To(ContainSubstring("dashboard"))
	})

	// Serve mode is the one the platform had to build something for: the
	// server is in this cluster, so it can call the application — and the
	// address is the environment's own, which nothing but the platform could
	// have written down.
	It("tells a serve-mode server which URL to call, and gives the project back its scale to zero", func() {
		createEnvironment(projectName, kitchenv1alpha1.EnvironmentProduction, "https://shjobs.apps.example.com")
		createClaim(`{"inngest":{"mode":"serve","servePath":"/jobs/inngest"}}`)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.KeepsPodsRunning).To(BeFalse(),
			"nothing holds a connection open in serve mode, so the project keeps its scale to zero")
		Expect(server.lastServeURL(serverName)).To(Equal("https://shjobs.apps.example.com/jobs/inngest"))

		workers := meta.FindStatusCondition(claim.Status.Conditions, condConnectWorkers)
		Expect(workers.Reason).To(Equal("NotConnectMode"))
		Expect(workers.Message).To(ContainSubstring("protected preview"))
	})

	// The tenancy answer, end to end: the preview's binding is a different
	// server with different keys, so an event sent by production cannot
	// reach a preview's functions.
	It("gives each preview a server of its own, parks it with the preview, and takes it back", func() {
		createEnvironment(previewEnvName, kitchenv1alpha1.EnvironmentPreview, "https://shjobs-pr-9.apps.example.com")
		createClaim("")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Branches).To(HaveLen(1))
		branch := claim.Status.Branches[0]
		Expect(branch.Environment).To(Equal(previewEnvName))
		Expect(branch.Provenance).To(Equal("synthetic"))
		Expect(branch.ID).To(Equal(inngest.DefaultServerNamespace + "/" + serverName + "-" + previewEnvName))

		production := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: claim.Status.SecretName}, production)).To(Succeed())
		preview := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: branch.SecretName}, preview)).To(Succeed())
		Expect(preview.Data[inngest.KeyEventKey]).NotTo(Equal(production.Data[inngest.KeyEventKey]),
			"a preview with production's event key is the isolation hole this provider exists to close")
		Expect(preview.Data[inngest.KeyBaseURL]).NotTo(Equal(production.Data[inngest.KeyBaseURL]))

		// The preview parks: its server goes down with it, and comes back
		// when it wakes. That is what bounds the cost of a server each.
		setIdle(previewEnvName, true)
		reconcileOnce()
		Expect(server.idled[branch.ID]).To(BeTrue())
		Expect(getClaim().Status.Branches[0].Idle).To(BeTrue())

		setIdle(previewEnvName, false)
		reconcileOnce()
		Expect(server.idled[branch.ID]).To(BeFalse())
		Expect(getClaim().Status.Branches[0].Idle).To(BeFalse())

		// The pull request closes: the preview's server goes with it.
		env := &kitchenv1alpha1.Environment{}
		Expect(k8sClient.Get(ctx, envKey, env)).To(Succeed())
		Expect(k8sClient.Delete(ctx, env)).To(Succeed())
		reconcileOnce()
		Expect(getClaim().Status.Branches).To(BeEmpty())
		Expect(server.deleted).To(ContainElement(branch.ID))
	})

	// Deleting the claim destroys the server: the type carries no
	// deletionPolicy, because there is nothing here a third party holds.
	It("destroys the server with the claim", func() {
		createClaim("")
		reconcileOnce()
		instanceID := getClaim().Status.InstanceID

		claim := getClaim()
		Expect(k8sClient.Delete(ctx, claim)).To(Succeed())
		reconcileOnce()
		Expect(server.deleted).To(ContainElement(instanceID))
		Expect(k8sClient.Get(ctx, claimKey, &kitchenv1alpha1.ResourceClaim{})).NotTo(Succeed())
	})
})
