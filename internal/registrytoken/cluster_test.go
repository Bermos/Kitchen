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

package registrytoken

import (
	"crypto/x509"
	"encoding/pem"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

const platformNS = "kitchen-system"

func clusterFixture(t *testing.T, objects ...client.Object) *ClusterAccounts {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	base := []client.Object{
		&kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: controller.KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        kitchenv1alpha1.TLSSpec{Mode: kitchenv1alpha1.TLSModeACME},
				Registry:   kitchenv1alpha1.ImageRegistrySpec{Enabled: true},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "kitchen-registry", Namespace: platformNS},
			Data: map[string][]byte{
				"username": []byte("kitchen"), "password": []byte("platform"),
				"readUsername": []byte("kitchen-read"), "readPassword": []byte("reader"),
				"tokenKey": keyPEM,
			},
		},
		project("shop"),
		account("shop", "shop-push", "shop-pull"),
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(base, objects...)...).Build()
	return &ClusterAccounts{Client: c, Namespace: platformNS}
}

func project(name string) *kitchenv1alpha1.Project {
	return &kitchenv1alpha1.Project{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNS}}
}

func account(project, push, pull string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: controller.RegistryAccountSecretName(project), Namespace: platformNS},
		Data: map[string][]byte{
			controller.RegistryAccountPushPasswordKey: []byte(push),
			controller.RegistryAccountPullPasswordKey: []byte(pull),
		},
	}
}

func release(name, project string, workloads ...kitchenv1alpha1.WorkloadImage) *kitchenv1alpha1.Release {
	return &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: platformNS},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project},
			Image:      "registry.apps.example.com/" + project + "@sha256:aa",
			Workloads:  workloads,
		},
	}
}

func TestClusterAccountsAnswerThePlatformsOwnTwo(t *testing.T) {
	accounts := clusterFixture(t)
	for _, tc := range []struct {
		username, password string
		want               []string
	}{
		{"kitchen", "platform", []string{ActionPull, ActionPush, ActionDelete}},
		{"kitchen-read", "reader", []string{ActionPull}},
	} {
		grant, err := accounts.Authenticate(t.Context(), tc.username, tc.password)
		if err != nil || grant == nil {
			t.Fatalf("%s: %v %v", tc.username, grant, err)
		}
		if !reflect.DeepEqual(grant.Everywhere, tc.want) {
			t.Errorf("%s may %v, want %v", tc.username, grant.Everywhere, tc.want)
		}
	}
	if grant, _ := accounts.Authenticate(t.Context(), "kitchen", "reader"); grant != nil {
		t.Error("the read account's password opened the pushing account")
	}
}

func TestClusterAccountsScopeAProjectsAccountToTheProject(t *testing.T) {
	accounts := clusterFixture(t)

	grant, err := accounts.Authenticate(t.Context(), "shop.push", "shop-push")
	if err != nil || grant == nil {
		t.Fatalf("%v %v", grant, err)
	}
	if grant.Project != "shop" || !reflect.DeepEqual(grant.ProjectActions, []string{ActionPull, ActionPush}) {
		t.Errorf("push account: %+v", grant)
	}
	grant, _ = accounts.Authenticate(t.Context(), "shop.pull", "shop-pull")
	if grant == nil || !reflect.DeepEqual(grant.ProjectActions, []string{ActionPull}) {
		t.Errorf("pull account: %+v", grant)
	}
	if grant, _ := accounts.Authenticate(t.Context(), "shop.push", "shop-pull"); grant != nil {
		t.Error("the pull account's password opened the push account")
	}
	if grant, _ := accounts.Authenticate(t.Context(), "pantry.push", "shop-push"); grant != nil {
		t.Error("an account for a project that has none")
	}
}

func TestClusterAccountsCloseAProjectThatIsBeingDeleted(t *testing.T) {
	deleting := project("shop")
	deleting.Name = "attic"
	now := metav1.NewTime(time.Now())
	deleting.DeletionTimestamp = &now
	deleting.Finalizers = []string{"kitchen.bermos.dev/project"}
	accounts := clusterFixture(t, deleting, account("attic", "a", "b"))
	if grant, _ := accounts.Authenticate(t.Context(), "attic.push", "a"); grant != nil {
		t.Error("a project being deleted kept a working registry account")
	}
}

// The repositories a project pushed before its workloads moved under its
// name: read-only, and read off the Releases that run them — never matched by
// a pattern, which would hand project shop the images of project shop-api.
func TestClusterAccountsKeepPullingWhatAnOlderReleaseRuns(t *testing.T) {
	accounts := clusterFixture(t,
		release("shop-1", "shop",
			kitchenv1alpha1.WorkloadImage{Name: "api", Image: "registry.apps.example.com/shop-api@sha256:bb",
				Strategy: kitchenv1alpha1.BuildStrategyDockerfile},
			// Vendored: the project named it, the platform did not push it.
			kitchenv1alpha1.WorkloadImage{Name: "cache", Image: "registry.apps.example.com/shop-cache@sha256:cc"},
			// Built after the move, under the project already.
			kitchenv1alpha1.WorkloadImage{Name: "web", Image: "registry.apps.example.com/shop/web@sha256:dd",
				Strategy: kitchenv1alpha1.BuildStrategyBuildpacks},
			// Not named after the workload it is the image of.
			kitchenv1alpha1.WorkloadImage{Name: "jobs", Image: "registry.apps.example.com/billing@sha256:ee",
				Strategy: kitchenv1alpha1.BuildStrategyDockerfile},
		),
		release("other-1", "other",
			kitchenv1alpha1.WorkloadImage{Name: "x", Image: "registry.apps.example.com/other-x@sha256:ff",
				Strategy: kitchenv1alpha1.BuildStrategyDockerfile}),
	)
	grant, err := accounts.Authenticate(t.Context(), "shop.pull", "shop-pull")
	if err != nil || grant == nil {
		t.Fatalf("%v %v", grant, err)
	}
	if !reflect.DeepEqual(grant.Legacy, map[string]bool{"shop-api": true}) {
		t.Errorf("legacy = %v", grant.Legacy)
	}
}

func TestClusterAccountsSignWithTheChartsKey(t *testing.T) {
	accounts := clusterFixture(t)
	first, err := accounts.SigningKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second, _ := accounts.SigningKey(t.Context())
	if first != second {
		t.Error("the key is parsed once per version of its Secret")
	}
}

func TestRepositoryOn(t *testing.T) {
	for image, want := range map[string]string{
		"registry.apps.example.com/shop-api@sha256:bb":   "shop-api",
		"registry.apps.example.com/shop/api:abc1234":     "shop/api",
		"registry.apps.example.com/shop/api:1@sha256:bb": "shop/api",
		"ghcr.io/acme/shop-api@sha256:bb":                "",
	} {
		got, _ := repositoryOn("registry.apps.example.com", image)
		if got != want {
			t.Errorf("repositoryOn(%q) = %q, want %q", image, got, want)
		}
	}
}
