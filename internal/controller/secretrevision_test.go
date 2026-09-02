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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The digest that makes a rotated value reach a pod that is already running.
//
// Two claims are being made, and they are the same claim from both ends: a
// workload that reads a Secret is rolled when its content changes, and a
// workload that does not read it is not disturbed.
var _ = Describe("The secrets revision", func() {
	ctx := context.Background()

	const (
		sessionKey  = "session"
		namespace   = "kitchen-revision"
		credentials = "shop-credentials"
		binding     = "claim-binding"
		databaseURL = "DATABASE_URL"
	)

	// A Secret in the application namespace, written or replaced — which is
	// what a rotation is, whether the API wrote it or a claim's provider did.
	write := func(name string, data map[string][]byte) {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
		existing := &corev1.Secret{}
		key := client.ObjectKeyFromObject(secret)
		if err := k8sClient.Get(ctx, key, existing); err == nil {
			existing.Data = data
			ExpectWithOffset(1, k8sClient.Update(ctx, existing)).To(Succeed())
			return
		}
		secret.Data = data
		ExpectWithOffset(1, k8sClient.Create(ctx, secret)).To(Succeed())
	}

	// reads is a pod spec whose one container reads one key of one Secret.
	reads := func(secret, key string) *corev1.PodSpec {
		return &corev1.PodSpec{Containers: []corev1.Container{{
			Name: AppContainerName,
			Env: []corev1.EnvVar{{
				Name: "DB_PASS",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: secret},
					Key:                  key,
				}},
			}},
		}}}
	}

	BeforeEach(func() {
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		}))).To(Succeed())
	})

	It("is empty for a workload that reads no secret", func() {
		revision, names, err := secretsRevision(ctx, k8sClient, namespace, &corev1.PodSpec{
			Containers: []corev1.Container{{Name: AppContainerName, Env: []corev1.EnvVar{
				{Name: "LOG_LEVEL", Value: "debug"},
			}}},
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(revision).To(BeEmpty())
		Expect(names).To(BeEmpty())
	})

	// A reference to something that is not there is not an error: the
	// container will not start until it is, and the reconcile that creates it
	// is what brings the digest back.
	It("is empty for a secret that does not exist yet", func() {
		revision, _, err := secretsRevision(ctx, k8sClient, namespace, reads("absent", sessionKey))
		Expect(err).NotTo(HaveOccurred())
		Expect(revision).To(BeEmpty())
	})

	It("changes when the value it reads changes, and not otherwise", func() {
		write(credentials, map[string][]byte{sessionKey: []byte("first"), "unread": []byte("a")})

		before, names, err := secretsRevision(ctx, k8sClient, namespace, reads(credentials, sessionKey))
		Expect(err).NotTo(HaveOccurred())
		Expect(before).NotTo(BeEmpty())
		Expect(names).To(ConsistOf(credentials))

		By("changing a key this workload does not read")
		write(credentials, map[string][]byte{sessionKey: []byte("first"), "unread": []byte("b")})
		unmoved, _, err := secretsRevision(ctx, k8sClient, namespace, reads(credentials, sessionKey))
		Expect(err).NotTo(HaveOccurred())
		Expect(unmoved).To(Equal(before))

		By("rotating the one it does")
		write(credentials, map[string][]byte{sessionKey: []byte("rotated"), "unread": []byte("b")})
		after, _, err := secretsRevision(ctx, k8sClient, namespace, reads(credentials, sessionKey))
		Expect(err).NotTo(HaveOccurred())
		Expect(after).NotTo(Equal(before))
	})

	// The defect: the digest used to cover the project's own secrets and
	// nothing else, so a claim's binding — a Secret the provider writes, and
	// the one an application's database password arrives in — could be
	// replaced without a single pod hearing about it.
	It("covers a secret that is not the project's own", func() {
		write(binding, map[string][]byte{databaseURL: []byte("postgres://old")})

		before, _, err := secretsRevision(ctx, k8sClient, namespace, reads(binding, databaseURL))
		Expect(err).NotTo(HaveOccurred())
		Expect(before).NotTo(BeEmpty())

		write(binding, map[string][]byte{databaseURL: []byte("postgres://new")})
		after, _, err := secretsRevision(ctx, k8sClient, namespace, reads(binding, databaseURL))
		Expect(err).NotTo(HaveOccurred())
		Expect(after).NotTo(Equal(before))
	})

	It("covers every secret a workload reads at once", func() {
		write("one", map[string][]byte{"A": []byte("1")})
		write("two", map[string][]byte{"B": []byte("2")})

		spec := reads("one", "A")
		spec.Containers[0].EnvFrom = []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "two"}},
		}}
		before, names, err := secretsRevision(ctx, k8sClient, namespace, spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(names).To(Equal([]string{"one", "two"}), "named in a stable order, or the digest would be one")

		By("rotating the one taken whole")
		write("two", map[string][]byte{"B": []byte("rotated")})
		after, _, err := secretsRevision(ctx, k8sClient, namespace, spec)
		Expect(err).NotTo(HaveOccurred())
		Expect(after).NotTo(Equal(before))
	})

	Describe("what counts as reading a secret", func() {
		It("finds the three paths a secret reaches a pod by", func() {
			spec := &corev1.PodSpec{
				InitContainers: []corev1.Container{{Env: []corev1.EnvVar{{
					Name: "SEED",
					ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "variables"},
						Key:                  "seed",
					}},
				}}}},
				Containers: []corev1.Container{{
					EnvFrom: []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: "wholesale"},
					}}},
				}},
				Volumes: []corev1.Volume{{
					Name: "certs",
					VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
						SecretName: "files",
						Items:      []corev1.KeyToPath{{Key: "tls.crt", Path: "tls.crt"}},
					}},
				}},
			}
			Expect(secretsReadBy(spec)).To(Equal(map[string][]string{
				"variables": {"seed"},
				// Every key there is: envFrom takes the whole Secret, so a
				// key added to it changes what the container sees.
				"wholesale": nil,
				"files":     {"tls.crt"},
			}))
		})

		// The kubelet reads a pull credential at pull time rather than handing
		// it to the process, so a rotated registry password is in use the
		// moment it is written — and rolling on it would be a restart that
		// changes nothing.
		It("does not count the pull credential", func() {
			Expect(secretsReadBy(&corev1.PodSpec{
				ImagePullSecrets: []corev1.LocalObjectReference{{Name: registrySecretName("registry")}},
				Containers:       []corev1.Container{{Name: AppContainerName}},
			})).To(BeEmpty())
		})
	})

	Describe("the stamp on the pod template", func() {
		It("replaces the name the digest used to be written under", func() {
			template := &metav1.ObjectMeta{Annotations: map[string]string{
				projectSecretsRevisionAnnotation: "abcdef0123456789",
				"kitchen.bermos.dev/unrelated":   "kept",
			}}

			applySecretsRevision(template, "0123456789abcdef")
			Expect(template.Annotations).To(HaveKeyWithValue(SecretsRevisionAnnotation, "0123456789abcdef"))
			Expect(template.Annotations).NotTo(HaveKey(projectSecretsRevisionAnnotation),
				"two digests of one question is a question about which is current")
			Expect(template.Annotations).To(HaveKeyWithValue("kitchen.bermos.dev/unrelated", "kept"))
		})

		It("comes off a template that stops reading one", func() {
			template := &metav1.ObjectMeta{Annotations: map[string]string{
				SecretsRevisionAnnotation: "abcdef0123456789",
			}}
			applySecretsRevision(template, "")
			Expect(template.Annotations).NotTo(HaveKey(SecretsRevisionAnnotation))
		})
	})

	// What the activity entry is written from. A first deployment and a
	// workload that stops reading secrets are not rotations, and announcing
	// them would make the entry mean nothing.
	Describe("what counts as a rotation", func() {
		It("is a digest replaced by another", func() {
			Expect(secretRotation{from: "a", to: "b"}.rolled()).To(BeTrue())
			Expect(secretRotation{from: "a", to: "a"}.rolled()).To(BeFalse())
			Expect(secretRotation{to: "b"}.rolled()).To(BeFalse(), "a workload seen for the first time")
			Expect(secretRotation{from: "a"}.rolled()).To(BeFalse(), "a workload that stopped reading one")
		})

		It("names the secret where there is one, and the set where there are several", func() {
			one := secretRotation{from: "a", to: "b", secrets: []string{ProjectSecretsName}}
			Expect(one.cause()).To(Equal(ProjectSecretsName + " was rotated"))

			several := secretRotation{from: "a", to: "b", secrets: []string{"one", "two"}}
			Expect(several.cause()).To(ContainSubstring("(one, two)"))
		})
	})

	// Not part of the contract, only of the reading: the digest is short
	// enough to be an annotation value a person can compare by eye.
	It("is a short hex digest", func() {
		write("shape", map[string][]byte{"k": []byte("v")})
		revision, _, err := secretsRevision(ctx, k8sClient, namespace, reads("shape", "k"))
		Expect(err).NotTo(HaveOccurred())
		Expect(revision).To(HaveLen(16), fmt.Sprintf("got %q", revision))
	})
})
