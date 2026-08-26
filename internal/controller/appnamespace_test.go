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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The Pod Security level of an application namespace is what decides whether
// the Dockerfile build strategy works at all: the BuildKit builder runs
// rootless and asks for an unconfined seccomp profile and an unconfined
// AppArmor profile, and Pod Security admits neither below `privileged`. A Job
// whose pods it refuses creates no pod, so the failure is silent — which is
// why the label is asserted here rather than left to the cluster default.
var _ = Describe("Application namespaces", func() {
	ctx := context.Background()

	const (
		enforce = "pod-security.kubernetes.io/enforce"
		audit   = "pod-security.kubernetes.io/audit"
		warn    = "pod-security.kubernetes.io/warn"
	)

	singletonKey := types.NamespacedName{Name: KitchenSingletonName}

	// setLevel puts a platform singleton in the cluster carrying the given
	// level, or none at all when the level is empty.
	setLevel := func(level kitchenv1alpha1.PodSecurityLevel) {
		kitchen := &kitchenv1alpha1.Kitchen{}
		err := k8sClient.Get(ctx, singletonKey, kitchen)
		if err == nil {
			ExpectWithOffset(1, k8sClient.Delete(ctx, kitchen)).To(Succeed())
		}
		if level == "" {
			return
		}
		kitchen = &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain:    "apps.example.com",
				TLS:           acmeTLS(),
				AppNamespaces: kitchenv1alpha1.AppNamespacesSpec{PodSecurity: level},
			},
		}
		ExpectWithOffset(1, k8sClient.Create(ctx, kitchen)).To(Succeed())
	}

	labelsOf := func(name string) map[string]string {
		ns := &corev1.Namespace{}
		ExpectWithOffset(1, k8sClient.Get(ctx, types.NamespacedName{Name: name}, ns)).To(Succeed())
		return ns.Labels
	}

	AfterEach(func() {
		kitchen := &kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName}}
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, kitchen))).To(Succeed())
	})

	It("labels a namespace it creates with the configured level", func() {
		setLevel(kitchenv1alpha1.PodSecurityBaseline)

		const name = "kitchen-psslabelled"
		Expect(ensureNamespace(ctx, k8sClient, name, "psslabelled")).To(Succeed())

		labels := labelsOf(name)
		Expect(labels).To(HaveKeyWithValue(enforce, "baseline"))
		Expect(labels).To(HaveKeyWithValue(audit, "baseline"))
		Expect(labels).To(HaveKeyWithValue(warn, "baseline"))
		Expect(labels).To(HaveKeyWithValue(labelProject, "psslabelled"))
		Expect(labels).To(HaveKeyWithValue(labelManagedByKey, labelManagedByValue))
	})

	It("falls back to privileged when the platform has no singleton yet", func() {
		setLevel("")

		const name = "kitchen-pssnosingleton"
		Expect(ensureNamespace(ctx, k8sClient, name, "pssnosingleton")).To(Succeed())

		Expect(labelsOf(name)).To(HaveKeyWithValue(enforce, "privileged"))
	})

	// Every installation upgrading into this has namespaces already, and a
	// namespace is created once and found by every reconcile after that. A
	// level written at creation alone would therefore never reach them, and
	// their Dockerfile builds would go on never starting.
	It("relabels a namespace that already exists", func() {
		setLevel("")

		const name = "kitchen-pssrelabelled"
		existing := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{labelProject: "pssrelabelled", "example.com/kept": "yes"},
		}}
		Expect(k8sClient.Create(ctx, existing)).To(Succeed())

		setLevel(kitchenv1alpha1.PodSecurityRestricted)
		Expect(ensureNamespace(ctx, k8sClient, name, "pssrelabelled")).To(Succeed())

		labels := labelsOf(name)
		Expect(labels).To(HaveKeyWithValue(enforce, "restricted"))
		Expect(labels).To(HaveKeyWithValue(audit, "restricted"))
		Expect(labels).To(HaveKeyWithValue(warn, "restricted"))
		// A merge patch of the labels the operator owns leaves everything
		// else on the namespace where it was.
		Expect(labels).To(HaveKeyWithValue("example.com/kept", "yes"))

		By("following the level when it changes")
		setLevel(kitchenv1alpha1.PodSecurityPrivileged)
		Expect(ensureNamespace(ctx, k8sClient, name, "pssrelabelled")).To(Succeed())
		Expect(labelsOf(name)).To(HaveKeyWithValue(enforce, "privileged"))
	})
})
