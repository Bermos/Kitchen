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
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
)

func complianceClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestEnsureSigningKeyGeneratesOnceAndReloadsTheSameKey(t *testing.T) {
	c := complianceClient(t)

	first, err := EnsureSigningKey(context.Background(), c, PlatformNamespace, SigningKeySecretName, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureSigningKey(context.Background(), c, PlatformNamespace, SigningKeySecretName, true)
	if err != nil {
		t.Fatal(err)
	}
	if first.KeyID() != second.KeyID() {
		t.Errorf("the key was regenerated: %s then %s — every attestation signed under the first is now orphaned",
			first.KeyID(), second.KeyID())
	}

	// The public half has to be stored beside the private one, because it is
	// what everybody outside the platform verifies with.
	secret := &corev1.Secret{}
	if err := c.Get(context.Background(), client.ObjectKey{
		Namespace: PlatformNamespace, Name: SigningKeySecretName,
	}, secret); err != nil {
		t.Fatal(err)
	}
	if len(secret.Data[attestation.SecretKeyPrivate]) == 0 || len(secret.Data[attestation.SecretKeyPublic]) == 0 {
		t.Errorf("the generated secret holds keys %v, want both halves", mapKeys(secret.Data))
	}
}

// An installation that named its own key must never be handed a generated
// one: a key that appeared because a secret was missing is a key nobody's
// custody rules cover.
func TestEnsureSigningKeyRefusesToInventOneItWasNotAskedFor(t *testing.T) {
	c := complianceClient(t)

	_, err := EnsureSigningKey(context.Background(), c, PlatformNamespace, "byo-key", false)
	if err == nil {
		t.Fatal("a missing named key was silently generated")
	}
	if !strings.Contains(err.Error(), "byo-key") {
		t.Errorf("the error does not name the secret it was looking for: %v", err)
	}

	secret := &corev1.Secret{}
	if getErr := c.Get(context.Background(), client.ObjectKey{
		Namespace: PlatformNamespace, Name: "byo-key",
	}, secret); getErr == nil {
		t.Error("a key was created despite the refusal")
	}
}

func TestSigningKeyForReportsNothingWhenAttestationIsOff(t *testing.T) {
	c := complianceClient(t)
	kitchen := &kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}}

	key, err := SigningKeyFor(context.Background(), c, kitchen)
	if err != nil {
		t.Fatalf("a platform with attestation off reported a fault: %v", err)
	}
	if key != nil {
		t.Error("a platform with attestation off produced a signing key")
	}
}

func TestSigningKeySecretHonoursAnExplicitReference(t *testing.T) {
	kitchen := &kitchenv1alpha1.Kitchen{
		Spec: kitchenv1alpha1.KitchenSpec{
			Compliance: kitchenv1alpha1.ComplianceSpec{
				Attestation: kitchenv1alpha1.AttestationSpec{
					Enabled:       true,
					SigningKeyRef: &kitchenv1alpha1.LocalObjectReference{Name: "hsm-backed"},
				},
			},
		},
	}
	if got := signingKeySecret(kitchen); got != "hsm-backed" {
		t.Errorf("resolved the key secret to %q, want the one the spec names", got)
	}
	if got := signingKeySecret(&kitchenv1alpha1.Kitchen{}); got != SigningKeySecretName {
		t.Errorf("resolved the default key secret to %q, want %q", got, SigningKeySecretName)
	}
}

// stubAttester stands in for the registry.
type stubAttester struct {
	attached  []attestation.Envelope
	predicate string
	err       error
}

func (s *stubAttester) Attach(
	_ context.Context, _ string, envelope attestation.Envelope, predicateType string,
) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	s.attached = append(s.attached, envelope)
	s.predicate = predicateType
	return "sha256:" + strings.Repeat("f", 64), nil
}

func attestFixtures(t *testing.T) (*BuildReconciler, *stubAttester, *kitchenv1alpha1.Build, *kitchenv1alpha1.Project, buildTarget) {
	t.Helper()
	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			Compliance: kitchenv1alpha1.ComplianceSpec{
				Attestation: kitchenv1alpha1.AttestationSpec{Enabled: true},
			},
		},
	}
	connection := &kitchenv1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ConnectionSpec{
			Provider:             "dockerRegistry",
			CredentialsSecretRef: kitchenv1alpha1.LocalObjectReference{Name: "registry-creds"},
		},
	}
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: PlatformNamespace},
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(
				`{"auths":{"registry.example.com":{"username":"robot","password":"hunter2"}}}`),
		},
	}
	_, privatePEM, publicPEM, err := attestation.GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SigningKeySecretName, Namespace: PlatformNamespace},
		Data: map[string][]byte{
			attestation.SecretKeyPrivate: privatePEM,
			attestation.SecretKeyPublic:  publicPEM,
		},
	}

	attester := &stubAttester{}
	reconciler := &BuildReconciler{
		Client: complianceClient(t, kitchen, connection, creds, keySecret),
		Attesters: func([]byte, string) (ArtifactAttester, error) {
			return attester, nil
		},
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-1", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: "shop"},
			Git:        kitchenv1alpha1.GitRevision{SHA: "abc123def456", Branch: "main"},
		},
		Status: kitchenv1alpha1.BuildStatus{DetectedFramework: "nextjs"},
	}
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ProjectSpec{
			Source: kitchenv1alpha1.GitSourceSpec{Repo: "acme/shop"},
		},
	}
	target := buildTarget{Connection: connection, Strategy: kitchenv1alpha1.BuildStrategyBuildpacks}
	return reconciler, attester, build, project, target
}

func TestAttestBuildSignsAndAttachesTheBuildRecord(t *testing.T) {
	reconciler, attester, build, project, target := attestFixtures(t)
	image := "registry.example.com/shop@sha256:" + strings.Repeat("a", 64)

	status := reconciler.attestBuild(context.Background(), build, project, target, image)
	if status == nil {
		t.Fatal("no artifact status was produced")
	}
	if status.Message != "" {
		t.Fatalf("attesting reported %q", status.Message)
	}
	if status.Repository != "registry.example.com/shop" || status.Digest != "sha256:"+strings.Repeat("a", 64) {
		t.Errorf("the artifact was identified as %+v", status)
	}
	if status.AttestedAt == nil || status.KeyID == "" {
		t.Errorf("the artifact was not recorded as attested: %+v", status)
	}
	if len(attester.attached) != 1 {
		t.Fatalf("attached %d envelopes, want 1", len(attester.attached))
	}
	if attester.predicate != attestation.PredicateBuildRecord {
		t.Errorf("attached under predicate %q, want %q", attester.predicate, attestation.PredicateBuildRecord)
	}

	// The evidence has to say what it is about, and say it in the payload
	// rather than in a label somebody could change.
	statement, err := attester.attached[0].Statement()
	if err != nil {
		t.Fatal(err)
	}
	if !statement.Describes(status.Digest) {
		t.Error("the signed statement does not describe the artifact it was attached to")
	}
	if !strings.Contains(string(statement.Predicate), "abc123def456") {
		t.Errorf("the build record does not name the commit it was built from: %s", statement.Predicate)
	}
}

// A build that pushed but reported no digest has an image and nothing that can
// be said about it. That is a state worth naming, not a silent absence.
func TestAttestBuildSaysSoWhenThereIsNoDigest(t *testing.T) {
	reconciler, attester, build, project, target := attestFixtures(t)

	status := reconciler.attestBuild(context.Background(), build, project, target, "registry.example.com/shop:abc123")
	if status.Digest != "" {
		t.Errorf("a tag reference produced a digest: %q", status.Digest)
	}
	if status.Message == "" {
		t.Error("an artifact with no digest was left without an explanation")
	}
	if len(attester.attached) != 0 {
		t.Error("evidence was attached to something with no digest")
	}
}

// The image is real and the deployment that follows is honest; what an
// unattested artifact cannot do is satisfy a policy that requires evidence.
func TestAttestBuildRecordsAFailureWithoutLosingTheArtifact(t *testing.T) {
	reconciler, attester, build, project, target := attestFixtures(t)
	attester.err = errors.New("the registry refused the referrers write")
	image := "registry.example.com/shop@sha256:" + strings.Repeat("a", 64)

	status := reconciler.attestBuild(context.Background(), build, project, target, image)
	if status.Digest == "" {
		t.Error("a failed attestation lost the artifact's identity")
	}
	if status.AttestedAt != nil {
		t.Error("a failed attestation was recorded as attested")
	}
	if !strings.Contains(status.Message, "referrers") {
		t.Errorf("the failure does not carry the registry's own account: %q", status.Message)
	}
}

func TestAttestBuildDoesNothingWhenAttestationIsOff(t *testing.T) {
	reconciler, attester, build, project, target := attestFixtures(t)
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := reconciler.Get(context.Background(),
		client.ObjectKey{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Compliance.Attestation.Enabled = false
	if err := reconciler.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}

	image := "registry.example.com/shop@sha256:" + strings.Repeat("a", 64)
	status := reconciler.attestBuild(context.Background(), build, project, target, image)
	if status.Digest == "" {
		t.Error("turning attestation off also lost the artifact's identity, which is not the same setting")
	}
	if status.AttestedAt != nil || len(attester.attached) != 0 {
		t.Error("evidence was produced with attestation turned off")
	}
	// Off by choice is not a fault, and repeating it on every build would
	// read as one.
	if status.Message != "" {
		t.Errorf("a deliberately unattested build carries the message %q", status.Message)
	}
}

func mapKeys(data map[string][]byte) []string {
	keys := make([]string, 0, len(data))
	for key := range data {
		keys = append(keys, key)
	}
	return keys
}
