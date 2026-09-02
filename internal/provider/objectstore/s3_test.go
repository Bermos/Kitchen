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

package objectstore_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/objectstore"
	"github.com/Bermos/Kitchen/internal/provider/objectstore/objectstoretest"
)

const (
	endpoint = "http://kitchen-objectstore.kitchen-system.svc.cluster.local:9000"
	rootKey  = "root"
	rootPass = "hunter2hunter2"
)

func scoped(store *objectstoretest.Store, inCluster bool) *objectstore.S3 {
	return &objectstore.S3{
		Config:          objectstore.Config{Endpoint: endpoint, Region: "us-east-1", ForcePathStyle: true, InCluster: inCluster},
		AccessKeyID:     rootKey,
		SecretAccessKey: rootPass,
		Buckets:         store,
		Admin:           store,
	}
}

func TestProvisionIsABucketAUserAndAPolicyPerClaim(t *testing.T) {
	store := objectstoretest.New()
	s := scoped(store, true)

	instance, err := s.ProvisionWith(context.Background(), "shop-uploads", objectstore.Requirements{Versioning: true})
	if err != nil {
		t.Fatal(err)
	}
	bucket := objectstore.BucketName("shop-uploads")
	if instance.ID != bucket || instance.Binding.Bucket != bucket {
		t.Errorf("the instance is addressed by its bucket, got %q / %q", instance.ID, instance.Binding.Bucket)
	}
	if instance.Provenance != objectstore.ProvenanceProduction {
		t.Errorf("the claim's own bucket is production's, got %q", instance.Provenance)
	}
	b, ok := store.Buckets[bucket]
	if !ok || !b.Versioned {
		t.Fatalf("bucket %s should exist and be versioned: %+v", bucket, b)
	}

	binding := instance.Binding
	if binding.AccessKeyID == rootKey || binding.SecretAccessKey == rootPass {
		t.Error("the application must never be handed the store's root credential")
	}
	if binding.AccessKeyID != objectstore.AccessKeyFor(bucket) {
		t.Errorf("the access key is derived from the bucket, got %q", binding.AccessKeyID)
	}
	user, ok := store.Users[binding.AccessKeyID]
	if !ok || user.SecretKey != binding.SecretAccessKey {
		t.Fatalf("the user %s should exist with the bound secret", binding.AccessKeyID)
	}
	if len(user.Policies) != 1 || user.Policies[0] != objectstore.PolicyName(bucket) {
		t.Errorf("the user carries the bucket's policy and nothing else, got %v", user.Policies)
	}
	policy := string(store.Policies[objectstore.PolicyName(bucket)])
	if !strings.Contains(policy, "arn:aws:s3:::"+bucket+"/*") || strings.Contains(policy, "s3:*") ||
		strings.Contains(policy, "DeleteBucket") {
		t.Errorf("the policy is scoped to the one bucket and does not reach the bucket itself: %s", policy)
	}
	if !binding.ForcePathStyle || binding.Endpoint != endpoint || binding.Region != "us-east-1" {
		t.Errorf("the binding carries how the store is addressed: %+v", binding)
	}
	data := binding.Data()
	if string(data[objectstore.BindingKeyForcePathStyle]) != "true" || string(data[objectstore.BindingKeyBucket]) != bucket {
		t.Errorf("the Secret's keys are the binding's fields: %v", data)
	}
}

func TestProvisionAgainReissuesTheCredentialRatherThanADuplicate(t *testing.T) {
	store := objectstoretest.New()
	s := scoped(store, true)
	ctx := context.Background()

	first, err := s.Provision(ctx, "shop-uploads")
	if err != nil {
		t.Fatal(err)
	}
	store.Put(first.ID, "photo.jpg")
	second, err := s.Provision(ctx, "shop-uploads")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.Binding.AccessKeyID != first.Binding.AccessKeyID {
		t.Error("the same claim finds the same bucket and the same user")
	}
	if second.Binding.SecretAccessKey == first.Binding.SecretAccessKey {
		t.Error("a re-provision re-issues the secret, which is how a lost binding is recovered")
	}
	if len(store.Users) != 1 || len(store.Buckets) != 1 {
		t.Errorf("nothing was duplicated: %d users, %d buckets", len(store.Users), len(store.Buckets))
	}
	if store.Buckets[first.ID].Objects["photo.jpg"] != 1 {
		t.Error("finding the bucket again leaves its objects alone")
	}
}

func TestPublicReadIsRefusedInClusterAndHonouredElsewhere(t *testing.T) {
	ctx := context.Background()

	store := objectstoretest.New()
	_, err := scoped(store, true).ProvisionWith(ctx, "shop-cdn", objectstore.Requirements{PublicRead: true})
	if !errors.Is(err, objectstore.ErrUnsatisfiable) {
		t.Fatalf("the bundled store is reached inside the cluster alone; want ErrUnsatisfiable, got %v", err)
	}
	if !strings.Contains(err.Error(), "inside the cluster") {
		t.Errorf("the refusal says why: %v", err)
	}
	if len(store.Buckets) != 0 || len(store.Users) != 0 {
		t.Error("a refusal creates nothing")
	}

	store = objectstoretest.New()
	instance, err := scoped(store, false).ProvisionWith(ctx, "shop-cdn", objectstore.Requirements{PublicRead: true})
	if err != nil {
		t.Fatal(err)
	}
	policy := string(store.Buckets[instance.ID].PublicRead)
	if !strings.Contains(policy, `"AWS":["*"]`) || !strings.Contains(policy, "s3:GetObject") ||
		strings.Contains(policy, "PutObject") {
		t.Errorf("an internet-facing store gets an anonymous read policy and nothing more: %s", policy)
	}
}

func TestSizeIsAQuotaWhereThereIsAnAdminAPIAndARefusalWhereThereIsNot(t *testing.T) {
	ctx := context.Background()

	store := objectstoretest.New()
	instance, err := scoped(store, true).ProvisionWith(ctx, "shop-uploads", objectstore.Requirements{Size: "1Gi"})
	if err != nil {
		t.Fatal(err)
	}
	if store.Quotas[instance.ID] != 1<<30 {
		t.Errorf("1Gi is a quota of 2^30 bytes, got %d", store.Quotas[instance.ID])
	}

	unscoped := scoped(objectstoretest.New(), false)
	unscoped.Admin = nil
	_, err = unscoped.ProvisionWith(ctx, "shop-uploads", objectstore.Requirements{Size: "1Gi"})
	if !errors.Is(err, objectstore.ErrUnsatisfiable) || !strings.Contains(err.Error(), "scopedCredentials") {
		t.Errorf("a store with no admin API cannot be given a quota, and the refusal names the flag: %v", err)
	}

	_, err = scoped(objectstoretest.New(), true).ProvisionWith(ctx, "shop-uploads", objectstore.Requirements{Size: "lots"})
	if !errors.Is(err, objectstore.ErrUnsatisfiable) {
		t.Errorf("a size that is not a quantity is refused, got %v", err)
	}
}

func TestWithoutScopedCredentialsTheApplicationGetsTheConnectionsOwn(t *testing.T) {
	store := objectstoretest.New()
	s := scoped(store, false)
	s.Admin = nil

	instance, err := s.Provision(context.Background(), "shop-uploads")
	if err != nil {
		t.Fatal(err)
	}
	if instance.Binding.AccessKeyID != rootKey || instance.Binding.SecretAccessKey != rootPass {
		t.Error("a store with no admin API hands out the connection's credential; the bucket is the isolation")
	}
	if len(store.Users) != 0 || len(store.Policies) != 0 {
		t.Error("no user and no policy were minted")
	}
}

func TestABranchIsAnEmptyBucketShapedLikeItsParent(t *testing.T) {
	store := objectstoretest.New()
	s := scoped(store, true)
	ctx := context.Background()

	parent, err := s.ProvisionWith(ctx, "shop-uploads", objectstore.Requirements{Versioning: true})
	if err != nil {
		t.Fatal(err)
	}
	store.Put(parent.ID, "production.jpg")

	branch, err := s.CreateBranch(ctx, parent.ID, "shop-pr-9")
	if err != nil {
		t.Fatal(err)
	}
	if branch.ID == parent.ID || branch.Binding.Bucket != branch.ID {
		t.Errorf("a preview's bucket is its own, got %q", branch.ID)
	}
	if branch.Provenance != objectstore.ProvenanceSynthetic {
		t.Errorf("an empty bucket never held production objects: %q", branch.Provenance)
	}
	b := store.Buckets[branch.ID]
	if !b.Versioned || len(b.Objects) != 0 {
		t.Errorf("versioned like the parent and empty: %+v", b)
	}
	if branch.Binding.AccessKeyID == parent.Binding.AccessKeyID {
		t.Error("a preview's credential is its own, not production's")
	}

	if err := s.DeleteBranch(ctx, parent.ID, branch.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Buckets[branch.ID]; ok {
		t.Error("the preview's bucket went with the preview")
	}
	if _, ok := store.Users[branch.Binding.AccessKeyID]; ok {
		t.Error("and so did its credential")
	}
	if _, ok := store.Buckets[parent.ID]; !ok || store.Buckets[parent.ID].Objects["production.jpg"] != 1 {
		t.Error("production's bucket and its objects were left alone")
	}
}

func TestDeprovisionRemovesEverythingAndTolerantesAbsence(t *testing.T) {
	store := objectstoretest.New()
	s := scoped(store, true)
	ctx := context.Background()

	instance, err := s.ProvisionWith(ctx, "shop-uploads", objectstore.Requirements{Size: "1Gi"})
	if err != nil {
		t.Fatal(err)
	}
	store.Put(instance.ID, "a.jpg")
	store.Put(instance.ID, "b.jpg")

	if err := s.Deprovision(ctx, instance.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Buckets[instance.ID]; ok {
		t.Error("the bucket and its objects are gone")
	}
	if _, ok := store.Users[instance.Binding.AccessKeyID]; ok {
		t.Error("the user is gone")
	}
	if _, ok := store.Policies[objectstore.PolicyName(instance.ID)]; ok {
		t.Error("the policy is gone")
	}
	if err := s.Deprovision(ctx, instance.ID); err != nil {
		t.Errorf("deprovisioning what is already gone is success: %v", err)
	}
}

func TestAStoreThatIsNotAnsweringIsNotReady(t *testing.T) {
	store := objectstoretest.New()
	store.NotReady = errors.Join(objectstore.ErrNotReady, errors.New("connection refused"))

	_, err := scoped(store, true).Provision(context.Background(), "shop-uploads")
	if !errors.Is(err, objectstore.ErrNotReady) {
		t.Errorf("want ErrNotReady, got %v", err)
	}
}

func TestBucketNamesFitAndStayApart(t *testing.T) {
	long := strings.Repeat("a", 60)
	if got := objectstore.BucketName(long); len(got) > 63 || got == objectstore.BucketName(long+"b") {
		t.Errorf("a long claim name is truncated with a digest rather than cut: %q", got)
	}
	if got := objectstore.BucketName("Shop-Uploads"); got != "kitchen-shop-uploads" {
		t.Errorf("bucket names are lowercase: %q", got)
	}
	branch := objectstore.BranchBucketName(objectstore.BucketName(long), "project-pr-1234")
	if len(branch) > 63 || !strings.Contains(branch, "project-pr-1234") {
		t.Errorf("the environment half keeps the most room: %q", branch)
	}
	key := objectstore.AccessKeyFor("kitchen-shop-uploads")
	if len(key) > 20 || key == objectstore.AccessKeyFor("kitchen-shop-upload") {
		t.Errorf("an access key fits MinIO's limit and differs per bucket: %q", key)
	}
}

func TestConfigIsReadOnceForEverybody(t *testing.T) {
	cfg, err := objectstore.ParseConfig([]byte(`{"endpoint": " https://s3.eu-central-1.amazonaws.com ", "region": "eu-central-1"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Endpoint != "https://s3.eu-central-1.amazonaws.com" || cfg.Region != "eu-central-1" || !cfg.Scoped() {
		t.Errorf("trimmed, and scoped by default: %+v", cfg)
	}
	host, secure, err := cfg.Host()
	if err != nil || host != "s3.eu-central-1.amazonaws.com" || !secure {
		t.Errorf("the host is the endpoint without its scheme: %q %v %v", host, secure, err)
	}
	cfg, err = objectstore.ParseConfig([]byte(`{"endpoint": "http://minio:9000", "scopedCredentials": false}`))
	if err != nil || cfg.Region != objectstore.DefaultRegion || cfg.Scoped() {
		t.Errorf("a region defaults and scoping can be switched off: %+v %v", cfg, err)
	}
	for _, raw := range []string{``, `{}`, `{"endpoint": "minio:9000"}`, `{"endpoint": "ftp://x"}`, `not json`} {
		if _, err := objectstore.ParseConfig([]byte(raw)); err == nil {
			t.Errorf("%q should be refused", raw)
		}
	}

	conn := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "store"},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider: objectstore.ProviderS3,
			Config:   &runtime.RawExtension{Raw: []byte(`{"endpoint": "http://minio:9000", "forcePathStyle": true}`)},
		},
	}
	s, err := objectstore.NewS3(objectstore.Options{Connection: conn, AccessKeyID: "a", SecretAccessKey: "b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Admin == nil || !s.Config.ForcePathStyle {
		t.Error("a scoped connection gets an admin client")
	}
	conn.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"endpoint": "http://minio:9000", "scopedCredentials": false}`)}
	s, err = objectstore.NewS3(objectstore.Options{Connection: conn, AccessKeyID: "a", SecretAccessKey: "b"}, nil)
	if err != nil || s.Admin != nil {
		t.Errorf("an unscoped connection gets none: %v", err)
	}
	if _, err := objectstore.NewS3(objectstore.Options{Connection: conn}, nil); err == nil {
		t.Error("a connection without both halves of its credential is refused")
	}
	if _, err := objectstore.Default(objectstore.Options{Connection: &kitchenv1alpha1.Connection{
		Spec: kitchenv1alpha1.ConnectionSpec{Provider: "neon"},
	}}); !errors.Is(err, objectstore.ErrUnsupportedProvider) {
		t.Errorf("a provider this package does not know: %v", err)
	}
	_ = ptr.To(true)
}
