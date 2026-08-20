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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The requirements bundle digest is pinned at admission, through the CRD's own
// pattern rather than anything a client has to remember to check: a bundle
// named by anything loose — a tag, a truncated hash — is a decision that
// cannot be replayed, so the API server refuses it before it exists.
var _ = Describe("Environment requirements validation", func() {
	const namespace = "default"

	ctx := context.Background()

	environment := func(name string, requirements *kitchenv1alpha1.EnvironmentRequirements) *kitchenv1alpha1.Environment {
		return &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef:   kitchenv1alpha1.LocalObjectReference{Name: "reqshop"},
				Type:         kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef:   kitchenv1alpha1.LocalObjectReference{Name: "reqshop-rel-1"},
				Owners:       []string{"user_owner", "owner@example.com"},
				Requirements: requirements,
			},
		}
	}

	It("admits a well-formed bundle digest with parameters and owners", func() {
		env := environment("reqshop-well-formed", &kitchenv1alpha1.EnvironmentRequirements{
			BundleDigest: "sha256:" + strings.Repeat("ab", 32),
			Parameters:   map[string]string{"maxSeverity": "high"},
		})
		Expect(k8sClient.Create(ctx, env)).To(Succeed())
		Expect(k8sClient.Delete(ctx, env)).To(Succeed())
	})

	It("refuses a digest that is not sha256:<64 hex>", func() {
		for _, digest := range []string{
			"latest",
			"sha256:" + strings.Repeat("a", 63),
			"sha512:" + strings.Repeat("a", 64),
			"sha256:" + strings.Repeat("A", 64), // upper case is not the digest form
		} {
			env := environment("reqshop-malformed", &kitchenv1alpha1.EnvironmentRequirements{
				BundleDigest: digest,
			})
			err := k8sClient.Create(ctx, env)
			Expect(err).To(HaveOccurred(), "digest %q must be refused", digest)
			Expect(err.Error()).To(ContainSubstring("bundleDigest"))
		}
	})

	It("refuses requirements that name no bundle at all", func() {
		env := environment("reqshop-empty", &kitchenv1alpha1.EnvironmentRequirements{})
		Expect(k8sClient.Create(ctx, env)).NotTo(Succeed())
	})
})
