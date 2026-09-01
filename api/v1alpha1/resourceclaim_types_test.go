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

package v1alpha1

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

// The CRD's enum and CEL rules are kubebuilder markers, and a marker cannot
// read a Go value. These tests are what keeps the markers level with the
// tables they are written against: a claim type added to ClaimTypes without
// the enum and both rules moving fails here, and so does a rule that names a
// type the table does not.

// celSet is the `['a', 'b']` literal a rule tests membership against.
var celSet = regexp.MustCompile(`in \[([^\]]*)\]`)

func TestClaimTypeTableMatchesCRD(t *testing.T) {
	crd := loadCRD(t, "kitchen.bermos.dev_resourceclaims.yaml")
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	enum := enumValues(spec.Properties["type"].Enum)
	if want := ClaimTypeNames(); !slices.Equal(enum, want) {
		t.Errorf("spec.type enum is %v, ClaimTypes says %v: move the +kubebuilder:validation:Enum marker on "+
			"ResourceClaimSpec.Type with the table", enum, want)
	}

	if len(spec.XValidations) != 2 {
		t.Fatalf("expected the two connectionRef rules on ResourceClaimSpec, found %d", len(spec.XValidations))
	}
	want := ClaimTypesWithoutConnection()
	for _, rule := range spec.XValidations {
		if !strings.Contains(rule.Rule, "self.type in [") {
			t.Errorf("rule %q does not test spec.type against a set: write it as `self.type in [...]` so "+
				"that a type is added to the set rather than to a chain of exceptions", rule.Rule)
			continue
		}
		if got := setMembers(rule.Rule); !slices.Equal(got, want) {
			t.Errorf("rule %q names %v as the types without a Connection; ClaimTypes says %v", rule.Rule, got, want)
		}
		if rule.MessageExpression == "" || !strings.Contains(rule.MessageExpression, "self.type") {
			t.Errorf("rule %q refuses without naming the type: give it a messageExpression that reads self.type",
				rule.Rule)
		}
		if strings.Contains(rule.Rule, "|| has(") {
			// The requiring rule lists the exceptions in its message, so a
			// developer knows which types to reach for instead.
			for _, name := range want {
				if !strings.Contains(rule.MessageExpression, name) {
					t.Errorf("the message of rule %q does not list %q among the types that take no Connection",
						rule.Rule, name)
				}
			}
		}
	}
}

func TestConnectionProviderSetMatchesCRD(t *testing.T) {
	crd := loadCRD(t, "kitchen.bermos.dev_connections.yaml")
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]

	enum := enumValues(spec.Properties["provider"].Enum)
	for _, name := range ConnectionProvidersWithoutCredential {
		if !slices.Contains(enum, name) {
			t.Errorf("ConnectionProvidersWithoutCredential names %q, which the provider enum %v does not admit",
				name, enum)
		}
	}

	if len(spec.XValidations) != 2 {
		t.Fatalf("expected the two credentialsSecretRef rules on ConnectionSpec, found %d", len(spec.XValidations))
	}
	for _, rule := range spec.XValidations {
		if !strings.Contains(rule.Rule, "self.provider in [") {
			t.Errorf("rule %q does not test spec.provider against a set", rule.Rule)
			continue
		}
		if got := setMembers(rule.Rule); !slices.Equal(got, ConnectionProvidersWithoutCredential) {
			t.Errorf("rule %q names %v as the providers without a credential; the table says %v",
				rule.Rule, got, ConnectionProvidersWithoutCredential)
		}
		if !strings.Contains(rule.MessageExpression, "self.provider") {
			t.Errorf("rule %q refuses without naming the provider", rule.Rule)
		}
	}
}

func TestProviderNeedsCredential(t *testing.T) {
	if ProviderNeedsCredential("cnpg") {
		t.Error("cnpg provisions with the operator's own account and has no credential to need")
	}
	if !ProviderNeedsCredential("neon") {
		t.Error("neon is reached with a token, and needs it")
	}
}

func TestLookupClaimType(t *testing.T) {
	postgres, ok := LookupClaimType(ClaimTypePostgres)
	if !ok || !postgres.TakesConnection() || postgres.Capability != CapabilityDatabase || !postgres.HoldsData {
		t.Errorf("postgres should be a data-holding type provisioned through a database Connection, got %+v", postgres)
	}
	oidc, ok := LookupClaimType(ClaimTypeOIDCClient)
	if !ok || oidc.TakesConnection() || oidc.HoldsData {
		t.Errorf("oidcClient should take no Connection and hold no data, got %+v", oidc)
	}
	if _, ok := LookupClaimType("mainframe"); ok {
		t.Error("a type the table does not know must not be found")
	}
	for _, claimType := range ClaimTypes {
		if claimType.Resource == "" {
			t.Errorf("claim type %q names no resource noun; every message about it needs one", claimType.Name)
		}
	}
}

func TestDecodeConfigIsTheOneDoorToSpecConfig(t *testing.T) {
	claim := &ResourceClaim{}
	claim.Spec.Type = ClaimTypePostgres
	var cfg struct{ Any string }
	if claim.DecodeConfig(&cfg) {
		t.Error("a claim with no config has nothing to decode")
	}
	claim.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"previewMode": "shared", "postgres": {"version": "17"}}`)}
	if claim.PreviewChoice() != "shared" {
		t.Error("previewMode is the platform's own slice of the config")
	}
	if got := claim.Postgres().Version; got != "17" {
		t.Errorf("the postgres slice is read through the same door, got version %q", got)
	}
	claim.Spec.Config = &runtime.RawExtension{Raw: []byte(`{"previewBranching": true}`)}
	if claim.PreviewChoice() != "" {
		t.Error("the old previewBranching flag asked for the preview's own resource, which is now the provider's default")
	}
	claim.Spec.Config = &runtime.RawExtension{Raw: []byte(`not json`)}
	if claim.PreviewChoice() != "" || claim.Postgres().Version != "" {
		t.Error("a config the platform cannot read counts as asking for nothing")
	}
}

func loadCRD(t *testing.T, file string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "crd", "bases", file))
	if err != nil {
		t.Fatalf("reading the generated CRD: %v (run `make manifests`)", err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(raw, crd); err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	return crd
}

func enumValues(enum []apiextensionsv1.JSON) []string {
	values := make([]string, 0, len(enum))
	for _, v := range enum {
		values = append(values, strings.Trim(string(v.Raw), `"`))
	}
	return values
}

func setMembers(rule string) []string {
	match := celSet.FindStringSubmatch(rule)
	if match == nil {
		return nil
	}
	members := []string{}
	for _, member := range strings.Split(match[1], ",") {
		if member = strings.Trim(strings.TrimSpace(member), `'`); member != "" {
			members = append(members, member)
		}
	}
	return members
}
