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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Which Secrets a `fromSecret` may name (#426).
//
// The application namespace holds the platform's own synced credentials as
// well as the project's — the registry's docker config and the git token,
// both shared by every project on their Connection — and a variable that
// could name one would be a developer reading a credential this API is
// careful never to answer with.

// The two the issue names by name, and the two the platform puts in the
// application namespace for the application itself.
func TestFromSecretRefusesThePlatformsOwnCredentialsAndKeepsTheProjectsOwn(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		secret  string
		refused bool
	}{
		{"the registry docker config the build syncs", registrySecretName("registry"), true},
		{"the registry docker config of another connection", registrySecretName("ghcr"), true},
		{"the git token the build syncs", gitSecretName("github"), true},
		{"anything else the platform writes there", "kitchen-something-later", true},
		{"the project's own secrets", ProjectSecretsName, false},
		{"the project's secret configuration files", ProjectFilesName, false},
		{"a resource claim's binding", claimSecretName("shop-db"), false},
		{"a Secret the project made for itself", "shop-api-key", false},
		{"a Secret an external operator synced in", "shop-secrets", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := CheckEnvSecretRef("API_KEY", testCase.secret)
			if testCase.refused && err == nil {
				t.Fatalf("%q may not be named by a fromSecret", testCase.secret)
			}
			if !testCase.refused && err != nil {
				t.Fatalf("%q is the project's own and has to keep working: %v", testCase.secret, err)
			}
		})
	}
}

// A refusal is only useful if it says what may be referenced instead: the
// name in the body plainly exists in the namespace, so "no" without a rule
// reads as a bug in the platform.
func TestTheRefusalNamesTheRuleAndWhatMayBeReferenced(t *testing.T) {
	err := CheckEnvSecretRef("GIT_TOKEN", gitSecretName("github"))
	if err == nil {
		t.Fatal("the git token may not be named by a fromSecret")
	}
	for _, want := range []string{"GIT_TOKEN", gitSecretName("github"), ProjectSecretsName, "claim"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want the refusal to name %q, got %q", want, err.Error())
		}
	}
}

// The list form is what the operator checks a stored spec with, and it names
// the variable that carries the reference rather than only the Secret.
func TestAStoredListIsRefusedByTheOneVariableThatNamesAPlatformSecret(t *testing.T) {
	err := CheckEnvSecretRefs([]kitchenv1alpha1.EnvVar{
		{Name: "PUBLIC_URL", Value: "https://shop.example.com"},
		{Name: "API_KEY", SecretRef: &kitchenv1alpha1.SecretKeySelector{Name: "shop-api-key", Key: "key"}},
		{Name: "STOLEN", SecretRef: &kitchenv1alpha1.SecretKeySelector{
			Name: registrySecretName("registry"), Key: ".dockerconfigjson"}},
	})
	if err == nil {
		t.Fatal("a stored list naming the registry credentials has to be refused")
	}
	if !strings.Contains(err.Error(), "STOLEN") {
		t.Fatalf("want the offending variable named, got %q", err.Error())
	}
	if err := CheckEnvSecretRefs([]kitchenv1alpha1.EnvVar{
		{Name: "API_KEY", SecretRef: &kitchenv1alpha1.SecretKeySelector{Name: "shop-api-key", Key: "key"}},
		{Name: "SMTP", SecretRef: &kitchenv1alpha1.SecretKeySelector{Name: ProjectSecretsName, Key: "SMTP"}},
	}); err != nil {
		t.Fatalf("a list of the project's own references has to pass: %v", err)
	}
}

// The API is the door, and it is not the only way in: a Project written with
// kubectl never passed it, and a reference stored before this rule existed
// passed a door that had no rule. So the environment that would materialize
// the reference refuses it — the Deployment is not written, and the
// environment says why.
func TestAnEnvironmentRefusesToMaterializeAPlatformSecretReference(t *testing.T) {
	const (
		projectName = "refuseshop"
		envName     = "refuseshop-production"
		releaseName = "refuseshop-rel-000001"
		namespace   = PlatformNamespace
	)
	ctx := context.Background()

	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name: envName, Namespace: namespace,
			Finalizers: []string{environmentFinalizer},
		},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
			Type:       kitchenv1alpha1.EnvironmentProduction,
			ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
		},
	}

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			&kitchenv1alpha1.Kitchen{
				ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
				Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com"},
			},
			&kitchenv1alpha1.Project{
				ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
				Spec: kitchenv1alpha1.ProjectSpec{
					Previews: kitchenv1alpha1.PreviewsSpec{Enabled: ptr.To(false), Protected: ptr.To(false)},
				},
			},
			&kitchenv1alpha1.Release{
				ObjectMeta: metav1.ObjectMeta{Name: releaseName, Namespace: namespace},
				Spec: kitchenv1alpha1.ReleaseSpec{
					ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
					Image:      "registry.example.com/kitchen/refuseshop@sha256:1111",
					ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
						Runtime: kitchenv1alpha1.RuntimeSpec{Port: 3000},
						Env: []kitchenv1alpha1.EnvVar{{
							Name: "STOLEN",
							SecretRef: &kitchenv1alpha1.SecretKeySelector{
								Name: gitSecretName("github"), Key: gitCredentialsTokenKey,
							},
						}},
					},
				},
			},
			env,
		).
		WithStatusSubresource(&kitchenv1alpha1.Environment{}).
		Build()

	reconciler := &EnvironmentReconciler{Client: c, Scheme: scheme}
	key := types.NamespacedName{Name: envName, Namespace: namespace}
	if _, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key}); err != nil {
		t.Fatalf("a refused reference is a condition, not a reconcile error: %v", err)
	}

	stored := &kitchenv1alpha1.Environment{}
	if err := c.Get(ctx, key, stored); err != nil {
		t.Fatal(err)
	}
	ready := meta.FindStatusCondition(stored.Status.Conditions, condReady)
	if ready == nil || ready.Status != metav1.ConditionFalse {
		t.Fatalf("want the environment held back, got %+v", stored.Status.Conditions)
	}
	if ready.Reason != "EnvSecretRefRefused" {
		t.Fatalf("want the refusal as the reason, got %q", ready.Reason)
	}
	if !strings.Contains(ready.Message, gitSecretName("github")) {
		t.Fatalf("want the message to name the secret, got %q", ready.Message)
	}
	if stored.Status.Phase != kitchenv1alpha1.EnvironmentPending {
		t.Fatalf("want the environment pending, got %q", stored.Status.Phase)
	}
}
