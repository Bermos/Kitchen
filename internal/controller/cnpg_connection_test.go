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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The credential exception, enforced where it has to be: at admission, so a
// Connection that reached the cluster another way cannot be shaped wrongly
// either.
var _ = Describe("A connection to the platform's own Postgres", func() {
	ctx := context.Background()

	It("is admitted with no credentials secret at all", func() {
		conn := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "cl-cnpg-admit", Namespace: "default"},
			Spec:       kitchenv1alpha1.ConnectionSpec{Provider: "cnpg"},
		}
		Expect(k8sClient.Create(ctx, conn)).To(Succeed())
		Expect(k8sClient.Delete(ctx, conn)).To(Succeed())
	})

	It("is refused when it names one, because nothing would read it", func() {
		conn := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "cl-cnpg-refused", Namespace: "default"},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "cnpg",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "somewhere"},
			},
		}
		err := k8sClient.Create(ctx, conn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("takes no credentialsSecretRef"))
	})

	It("leaves every other provider requiring one", func() {
		conn := &kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "cl-neon-refused", Namespace: "default"},
			Spec:       kitchenv1alpha1.ConnectionSpec{Provider: "neon"},
		}
		err := k8sClient.Create(ctx, conn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("credentialsSecretRef is required"))
	})
})
