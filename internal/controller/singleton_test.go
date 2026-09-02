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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// ensureSingleton creates the platform singleton, or brings the one that is
// already there to this suite's shape.
//
// The Kitchen object is cluster-scoped and a dozen suites create one, so
// tolerating whichever got there first makes a suite's result depend on the
// spec order — a singleton left behind with a feature switched off, and a
// reconcile that does nothing for a reason the suite never chose. Ginkgo
// randomises that order per run, which is what turns it into a test that
// passes locally and fails in CI.
func ensureSingleton(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) {
	GinkgoHelper()
	if err := k8sClient.Create(ctx, kitchen); apierrors.IsAlreadyExists(err) {
		existing := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: kitchen.Name}, existing)).To(Succeed())
		existing.Spec = kitchen.Spec
		Expect(k8sClient.Update(ctx, existing)).To(Succeed())
	} else {
		Expect(err).NotTo(HaveOccurred())
	}
}
