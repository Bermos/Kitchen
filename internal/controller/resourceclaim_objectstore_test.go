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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/naming"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
	"github.com/Bermos/Kitchen/internal/provider/objectstore/objectstoretest"
)

// recordingStore wraps the in-memory store's provisioner so a test can see
// what the claim asked for, and refuse or delay on demand.
type recordingStore struct {
	*objectstore.S3
	asked  objectstore.Requirements
	refuse error
}

func (p *recordingStore) Provision(ctx context.Context, res naming.Resource) (objectstore.Instance, error) {
	return p.ProvisionWith(ctx, res, objectstore.Requirements{})
}

func (p *recordingStore) ProvisionWith(
	ctx context.Context,
	res naming.Resource,
	req objectstore.Requirements,
) (objectstore.Instance, error) {
	p.asked = req
	if p.refuse != nil {
		return objectstore.Instance{}, p.refuse
	}
	return p.S3.ProvisionWith(ctx, res, req)
}

var _ = Describe("An objectStore claim", func() {
	const (
		projectName    = "clfiles"
		claimName      = "clfiles-uploads"
		connectionName = "clstore"
		secretName     = "kitchen-connection-" + connectionName
		namespace      = "default"
		previewEnv     = "clfiles-pr-3"
	)

	ctx := context.Background()
	claimKey := types.NamespacedName{Name: claimName, Namespace: namespace}
	appNS := "kitchen-" + projectName

	var (
		reconciler *ResourceClaimReconciler
		store      *objectstoretest.Store
		provider   *recordingStore
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

	createClaim := func(config string, policy kitchenv1alpha1.ClaimDeletionPolicy) {
		claim := &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef:  &kitchenv1alpha1.LocalObjectReference{Name: connectionName},
				Type:           kitchenv1alpha1.ClaimTypeObjectStore,
				DeletionPolicy: policy,
			},
		}
		if config != "" {
			claim.Spec.Config = &runtime.RawExtension{Raw: []byte(config)}
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, claim)).To(Succeed())
	}

	deleteClaim := func() {
		claim := getClaim()
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, claim))).To(Succeed())
		reconcileOnce()
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, claimKey, &kitchenv1alpha1.ResourceClaim{}))).To(BeTrue())
	}

	BeforeEach(func() {
		store = objectstoretest.New()
		provider = &recordingStore{S3: &objectstore.S3{
			Config: objectstore.Config{
				Endpoint: "http://kitchen-objectstore.kitchen-system.svc.cluster.local:9000",
				Region:   "us-east-1", ForcePathStyle: true, InCluster: true,
			},
			AccessKeyID: "root", SecretAccessKey: "hunter2hunter2",
			Buckets: store, Admin: store,
		}}
		reconciler = &ResourceClaimReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
			Buckets: func(opts objectstore.Options) (objectstore.Provisioner, error) {
				Expect(opts.AccessKeyID).To(Equal("root"), "the credential comes off the connection's secret")
				return provider, nil
			},
		}

		project := &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/clfiles",
				},
				Registry: kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "reg"},
				},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, project))).To(Succeed())

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace},
			StringData: map[string]string{
				objectstore.CredentialKeyAccessKeyID:     "root",
				objectstore.CredentialKeySecretAccessKey: "hunter2hunter2",
			},
		}))).To(Succeed())

		connection := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: connectionName, Namespace: namespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             objectstore.ProviderS3,
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: secretName},
				Config:               &runtime.RawExtension{Raw: []byte(`{"endpoint": "http://store:9000", "forcePathStyle": true}`)},
			},
		}
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, connection))).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: connectionName, Namespace: namespace}, connection)).To(Succeed())
		connection.Status.Capabilities = []kitchenv1alpha1.Capability{kitchenv1alpha1.CapabilityObjectStore}
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
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: namespace}},
			&kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace}},
		} {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
	})

	It("binds a bucket with a credential scoped to it", func() {
		createClaim(`{"objectStore": {"versioning": true, "size": "1Gi"}}`, "")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		bucket := naming.Resource{Project: projectName, Claim: claimName}.Qualified(63)
		Expect(claim.Status.InstanceID).To(Equal(bucket))
		Expect(claim.Status.DataProvenance).To(Equal(string(objectstore.ProvenanceProduction)))
		Expect(claim.Status.PreviewMode).To(Equal("fresh"), "s3 declares a fresh bucket per preview")
		Expect(provider.asked).To(Equal(objectstore.Requirements{Versioning: true, Size: "1Gi"}))

		By("writing the binding in the keys an application reads")
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: claim.Status.SecretName}, secret)).To(Succeed())
		Expect(string(secret.Data[objectstore.BindingKeyBucket])).To(Equal(bucket))
		Expect(string(secret.Data[objectstore.BindingKeyForcePathStyle])).To(Equal("true"))
		Expect(string(secret.Data[objectstore.BindingKeyEndpoint])).To(HavePrefix("http://kitchen-objectstore"))
		accessKey := string(secret.Data[objectstore.BindingKeyAccessKeyID])
		Expect(accessKey).To(Equal(objectstore.AccessKeyFor(bucket)))
		Expect(accessKey).NotTo(Equal("root"), "the application never sees the store's root credential")
		Expect(store.Users[accessKey].SecretKey).To(Equal(string(secret.Data[objectstore.BindingKeySecretAccessKey])))
		Expect(store.Buckets[bucket].Versioned).To(BeTrue())
		Expect(store.Quotas[bucket]).To(Equal(uint64(1 << 30)))
	})

	It("fails the claim, with the reason, when the store cannot honour a requirement", func() {
		createClaim(`{"objectStore": {"publicRead": true}}`, "")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimFailed))
		Expect(claim.Status.InstanceID).To(BeEmpty(), "nothing was provisioned")
		ready := meta.FindStatusCondition(claim.Status.Conditions, condReady)
		Expect(ready.Reason).To(Equal("RequirementsUnsatisfiable"))
		Expect(ready.Message).To(ContainSubstring("inside the cluster"))
		Expect(store.Buckets).To(BeEmpty())
	})

	It("waits Pending while the store is still coming up", func() {
		provider.refuse = fmt.Errorf("%w: the store is not answering: connection refused", objectstore.ErrNotReady)
		createClaim("", "")
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimPending))
		Expect(meta.FindStatusCondition(claim.Status.Conditions, condReady).Reason).To(Equal("Provisioning"))
	})

	It("gives a preview its own empty bucket and removes it with the preview under Retain", func() {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: previewEnv, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentPreview,
				Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: 3, Branch: "feature/files"},
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: projectName + "-rel-1"},
			},
		}
		Expect(k8sClient.Create(ctx, env)).To(Succeed())

		createClaim(`{"objectStore": {"versioning": true}}`, kitchenv1alpha1.ClaimRetain)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		Expect(claim.Status.Branches).To(HaveLen(1))
		branch := claim.Status.Branches[0]
		Expect(branch.Environment).To(Equal(previewEnv))
		Expect(branch.Provenance).To(Equal(string(objectstore.ProvenanceSynthetic)),
			"an empty bucket never held production objects")
		Expect(branch.ID).NotTo(Equal(claim.Status.InstanceID))
		Expect(store.Buckets[branch.ID].Versioned).To(BeTrue(), "shaped like production's")

		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Namespace: appNS, Name: branch.SecretName}, secret)).To(Succeed())
		Expect(string(secret.Data[objectstore.BindingKeyBucket])).To(Equal(branch.ID))
		Expect(string(secret.Data[objectstore.BindingKeyAccessKeyID])).NotTo(Equal(objectstore.AccessKeyFor(claim.Status.InstanceID)),
			"a preview's credential reaches its own bucket, not production's")

		By("tearing the preview bucket down with the claim, and keeping production's under Retain")
		store.Put(claim.Status.InstanceID, "photo.jpg")
		production := claim.Status.InstanceID
		deleteClaim()
		Expect(store.Buckets).NotTo(HaveKey(branch.ID))
		Expect(store.Users).NotTo(HaveKey(objectstore.AccessKeyFor(branch.ID)))
		Expect(store.Buckets).To(HaveKey(production))
		Expect(store.Buckets[production].Objects).To(HaveKey("photo.jpg"))
		Expect(store.Users).To(HaveKey(objectstore.AccessKeyFor(production)), "Retain leaves the user too")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx,
			types.NamespacedName{Namespace: appNS, Name: branch.SecretName}, &corev1.Secret{}))).To(BeTrue())
	})

	It("removes the bucket, its objects and its credential under Delete", func() {
		createClaim("", kitchenv1alpha1.ClaimDelete)
		reconcileOnce()

		claim := getClaim()
		Expect(claim.Status.Phase).To(Equal(kitchenv1alpha1.ClaimBound))
		bucket := claim.Status.InstanceID
		store.Put(bucket, "photo.jpg")

		deleteClaim()
		Expect(store.Buckets).NotTo(HaveKey(bucket))
		Expect(store.Users).NotTo(HaveKey(objectstore.AccessKeyFor(bucket)))
		Expect(store.Policies).NotTo(HaveKey(objectstore.PolicyName(bucket)))
	})
})
