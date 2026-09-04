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

package policy

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// Content addressing is what the whole scheme leans on: a requirement pins a
// digest, a decision cites two, and replay compares them. These tests pin the
// digests' properties rather than their values — with one exception, the
// shape, which is a wire contract.

func TestBundleDigestIsStableAndShaped(t *testing.T) {
	bundle := Bundle{"promotion.rego": "package kitchen.promotion\n", "data.json": "{}"}

	first := Digest(bundle)
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Fatalf("a bundle digest has the artifact digest form, got %q", first)
	}
	for range 10 {
		// Map iteration order must not leak in: the pairs are sorted.
		if again := Digest(Bundle{"data.json": "{}", "promotion.rego": "package kitchen.promotion\n"}); again != first {
			t.Fatalf("the digest is not stable: %q then %q", first, again)
		}
	}
}

func TestBundleDigestSeparatesPathFromContent(t *testing.T) {
	// Length-prefixing is what stops {"ab": "c"} colliding with {"a": "bc"}:
	// a digest over naive concatenation would read both as "abc".
	a := Digest(Bundle{"ab": "c"})
	b := Digest(Bundle{"a": "bc"})
	if a == b {
		t.Fatal("two different bundles share a digest")
	}
	if Digest(Bundle{"a": "b", "c": "d"}) == Digest(Bundle{"a": "bc", "": "d"}) {
		t.Fatal("pair boundaries are not separated")
	}
}

func TestInputDigestIsCanonical(t *testing.T) {
	input := Input{
		Kind:        KindPromotion,
		At:          time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Parameters:  map[string]string{"b": "2", "a": "1"},
		Project:     ProjectFacts{Name: "shop"},
		Environment: EnvironmentFacts{Name: "shop-production"},
		Release:     ReleaseFacts{Name: "shop-rel-1"},
	}
	first, err := input.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("an input digest has the artifact digest form, got %q", first)
	}
	// encoding/json sorts map keys and the struct fixes field order, so the
	// same facts digest the same however the maps were built.
	same := input
	same.Parameters = map[string]string{"a": "1", "b": "2"}
	again, err := same.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first != again {
		t.Fatalf("one input, two digests: %q and %q", first, again)
	}

	changed := input
	changed.Kind = KindRescan
	other, err := changed.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Fatal("a different input digests the same")
	}
}

func TestTheDefaultBundleNamesItsRules(t *testing.T) {
	rules := RuleIDs(DefaultBundle())
	want := []string{
		"data-provenance-preview",
		"dataclass-le-environment",
		"digest-approved-by-someone-else",
		"max-severity",
		"no-self-approval",
		"require-gate",
		"require-independent-review",
		"require-provenance",
		"require-pull-request",
		"require-sbom",
		"upstream-signature-verified",
	}
	if len(rules) != len(want) {
		t.Fatalf("the default bundle lists %v, want %v", rules, want)
	}
	for i := range want {
		if rules[i] != want[i] {
			t.Fatalf("the default bundle lists %v, want %v", rules, want)
		}
	}
}

func TestResolverServesBuiltInAndConfigMapBundles(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	institutional := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "finma-baseline",
			Namespace: "kitchen-system",
			Labels:    map[string]string{BundleLabel: "true"},
		},
		Data: map[string]string{
			"baseline.rego": `package kitchen.promotion

deny contains {"rule": "house-rule", "message": "no"} if { false }`,
		},
	}
	unlabelled := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-root-ca.crt", Namespace: "kitchen-system"},
		Data:       map[string]string{"ca.crt": "not a bundle"},
	}
	resolver := &Resolver{
		Client:    fake.NewClientBuilder().WithScheme(scheme).WithObjects(institutional, unlabelled).Build(),
		Namespace: "kitchen-system",
	}

	infos, err := resolver.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 2 {
		t.Fatalf("want the built-in bundle and one labelled ConfigMap, got %d: %+v", len(infos), infos)
	}
	if infos[0].Source != SourceBuiltIn || infos[0].Digest != Digest(DefaultBundle()) {
		t.Fatalf("the built-in bundle leads the list, got %+v", infos[0])
	}
	if infos[1].Source != "configmap/finma-baseline" {
		t.Fatalf("a ConfigMap bundle names its ConfigMap, got %q", infos[1].Source)
	}
	if len(infos[1].Rules) != 1 || infos[1].Rules[0] != "house-rule" {
		t.Fatalf("the listing names the bundle's rules, got %v", infos[1].Rules)
	}

	resolved, err := resolver.Resolve(context.Background(), infos[1].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Source != "configmap/finma-baseline" {
		t.Fatalf("resolve by digest found %+v", resolved)
	}

	_, err = resolver.Resolve(context.Background(), "sha256:"+strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "no policy bundle") {
		t.Fatalf("an unknown digest must say so, got %v", err)
	}
}

func TestAClientlessResolverStillServesTheBuiltInBundle(t *testing.T) {
	resolver := &Resolver{Namespace: "kitchen-system"}
	infos, err := resolver.List(context.Background())
	if err != nil || len(infos) != 1 || infos[0].Source != SourceBuiltIn {
		t.Fatalf("want the built-in bundle alone, got %+v (%v)", infos, err)
	}
}
