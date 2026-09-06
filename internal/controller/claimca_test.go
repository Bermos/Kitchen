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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
)

// A claim's certificate authority, mounted where its binding says it is
// (#456).
//
// The property under every case here is one sentence: a binding names a path
// exactly when that path is mounted. So the mount is asserted on every
// workload shape the claim's variables reach — the web process, a worker, a
// scheduled run and a deploy-time task — and asserted absent for a binding
// that hands over no authority at all.

var _ = Describe("A claim's certificate authority", func() {
	const (
		projectName = "cashop"
		namespace   = "default"
		image       = "registry.example.com/kitchen/cashop@sha256:0123456789abcdef"
		envName     = "cashop-production"
		verified    = "cashop-db"
		plain       = "cashop-neon"
		certificate = "-- the cluster's CA --"
	)
	appNS := "kitchen-" + projectName

	ctx := context.Background()
	envKey := types.NamespacedName{Name: envName, Namespace: namespace}

	var reconciler *EnvironmentReconciler
	var releases int

	reconcileOnce := func() {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: envKey})
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}

	// claim writes a bound claim and the binding Secret an application reads
	// it through. Neither reconciler runs here: what the Environment acts on
	// is the claim's status and the Secret's keys, and both are written as
	// the claim's own reconciler writes them.
	claim := func(name string, data map[string][]byte) {
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.ResourceClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ResourceClaimSpec{
				ProjectRef:    kitchenv1alpha1.LocalObjectReference{Name: projectName},
				ConnectionRef: &kitchenv1alpha1.LocalObjectReference{Name: "pg"},
				Type:          "postgres",
			},
		}))).To(Succeed())
		stored := &kitchenv1alpha1.ResourceClaim{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: namespace}, stored)).To(Succeed())
		stored.Status.SecretName = claimSecretName(name)
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, stored)).To(Succeed())

		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: claimSecretName(name), Namespace: appNS},
			Data:       data,
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, secret))).To(Succeed())
	}

	// release declares a unit of four workloads reading one claim: the web
	// process, a worker, a nightly job and a migration.
	release := func(claimName string) string {
		releases++
		name := "cashop-rel-" + string(rune('0'+releases))
		ExpectWithOffset(1, k8sClient.Create(ctx, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "cashop-bld-1"},
				Image:      image,
				ConfigSnapshot: kitchenv1alpha1.ConfigSnapshot{
					Runtime: kitchenv1alpha1.RuntimeSpec{Port: 8080},
					Env: []kitchenv1alpha1.EnvVar{{
						Name: "DATABASE_URL",
						FromResourceClaim: &kitchenv1alpha1.ResourceClaimKeySelector{
							Name: claimName, Key: "url",
						},
					}},
					Processes: []kitchenv1alpha1.ProcessSpec{
						{Name: "worker", Type: kitchenv1alpha1.ProcessWorker, Command: []string{"node", "w.js"}},
						{
							Name: "nightly", Type: kitchenv1alpha1.ProcessCron, Schedule: "0 3 * * *",
							Command: []string{"node", "report.js"},
						},
						{Name: "migrate", Type: kitchenv1alpha1.ProcessTask, Command: []string{"npm", "run", "migrate"}},
					},
				},
			},
		})).To(Succeed())
		return name
	}

	// deployOn points the environment at a release and runs the pass through
	// to the end: the first two passes add the finalizer and start the
	// migration, and nothing else of the release is applied until that run
	// has succeeded, so the case finishes it the way the Job controller would.
	deployOn := func(releaseName string) {
		env := &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: namespace},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: projectName},
				Type:       kitchenv1alpha1.EnvironmentProduction,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			},
		}
		ExpectWithOffset(1, client.IgnoreAlreadyExists(k8sClient.Create(ctx, env))).To(Succeed())
		for range 2 {
			reconcileOnce()
		}
	}

	// taskRun is the migration's Job, which is the whole of what the first
	// two passes applied.
	taskRun := func() *batchv1.Job {
		jobs := &batchv1.JobList{}
		ExpectWithOffset(1, k8sClient.List(ctx, jobs, client.InNamespace(appNS), client.MatchingLabels{
			labelEnvironment: envName, labelProcess: "migrate",
		})).To(Succeed())
		ExpectWithOffset(1, jobs.Items).To(HaveLen(1))
		return &jobs.Items[0]
	}

	// succeed ends the migration, so that the rest of the release is applied.
	succeed := func(job *batchv1.Job) {
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue,
				Reason: "Completed", LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
				Reason: "Completed", LastTransitionTime: now},
		}
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &now
		ExpectWithOffset(1, k8sClient.Status().Update(ctx, job)).To(Succeed())
		reconcileOnce()
	}

	deployment := func(name string) *appsv1.Deployment {
		deploy := &appsv1.Deployment{}
		ExpectWithOffset(1, k8sClient.Get(ctx,
			types.NamespacedName{Name: name, Namespace: appNS}, deploy)).To(Succeed())
		return deploy
	}

	// mountsCA asserts the whole of what a workload gets: the volume
	// projecting the one key of the binding Secret under the one file name,
	// and the read-only mount of it on the application container at the
	// directory the binding's URL names.
	// The claim is the one this environment reads, `verified`: what is being
	// asserted is that every workload shape gets the same mount, not that two
	// claims get two.
	mountsCA := func(spec corev1.PodSpec) {
		secretName, key := claimSecretName(verified), contract.BindingKeyCA
		volume := corev1.Volume{}
		found := false
		for _, v := range spec.Volumes {
			if v.Secret != nil && v.Secret.SecretName == secretName {
				volume, found = v, true
			}
		}
		ExpectWithOffset(1, found).To(BeTrue(), "no volume projects the binding Secret %s", secretName)
		ExpectWithOffset(1, volume.Secret.Items).To(Equal([]corev1.KeyToPath{
			{Key: key, Path: contract.CAFileName},
		}), "the volume must project the authority alone, never the password beside it")

		container := corev1.Container{}
		for _, c := range spec.Containers {
			if c.Name == AppContainerName {
				container = c
			}
		}
		mounted := false
		for _, m := range container.VolumeMounts {
			if m.Name != volume.Name {
				continue
			}
			mounted = true
			ExpectWithOffset(1, m.MountPath).To(Equal(contract.CADir(verified)))
			ExpectWithOffset(1, m.ReadOnly).To(BeTrue())
		}
		ExpectWithOffset(1, mounted).To(BeTrue(), "the application container does not mount %s", volume.Name)
	}

	BeforeEach(func() {
		reconciler = &EnvironmentReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}

		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: PlatformNamespace},
		}))).To(Succeed())
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: appNS},
		}))).To(Succeed())
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec:       kitchenv1alpha1.KitchenSpec{BaseDomain: "apps.example.com", TLS: acmeTLS()},
		})
		Expect(client.IgnoreAlreadyExists(k8sClient.Create(ctx, &kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: projectName, Namespace: namespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "gh"},
					Repo:          "acme/cashop",
				}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
			},
		}))).To(Succeed())
	})

	AfterEach(func() {
		env := &kitchenv1alpha1.Environment{}
		if err := k8sClient.Get(ctx, envKey, env); err == nil {
			Expect(k8sClient.Delete(ctx, env)).To(Succeed())
			reconcileOnce()
		}
	})

	It("is mounted in every workload the claim's variables reach", func() {
		claim(verified, map[string][]byte{
			"url": []byte("postgresql://app:pw@db:5432/app?sslmode=verify-full&sslrootcert=" +
				contract.CAFile(verified)),
			"ca": []byte(certificate),
		})
		deployOn(release(verified))

		// The deploy-time task runs before anything of the release takes
		// traffic, and it reads the same database through the same binding.
		run := taskRun()
		mountsCA(run.Spec.Template.Spec)
		succeed(run)

		mountsCA(deployment(envName).Spec.Template.Spec)
		mountsCA(deployment(ProcessWorkloadName(envName, "worker")).Spec.Template.Spec)

		cron := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{
			Name: ProcessWorkloadName(envName, "nightly"), Namespace: appNS}, cron)).To(Succeed())
		mountsCA(cron.Spec.JobTemplate.Spec.Template.Spec)
	})

	It("is nothing at all for a binding that hands over no authority", func() {
		claim(plain, map[string][]byte{
			"url": []byte("postgresql://app:pw@ep-cool.neon.tech/app?sslmode=require"),
		})
		deployOn(release(plain))
		succeed(taskRun())

		spec := deployment(envName).Spec.Template.Spec
		Expect(spec.Volumes).To(BeEmpty(),
			"a hosted database the public roots vouch for has nothing to mount")
		for _, mount := range spec.Containers[0].VolumeMounts {
			Expect(mount.MountPath).NotTo(HavePrefix(contract.CAMountRoot))
		}
	})
})

// The volume a claim's authority is projected through is named after the
// claim, so two claims of one project cannot collide — and a claim name is a
// DNS label of its own, so the name has to be cut to fit one.
func TestClaimCAVolumeNameFitsADNSLabel(t *testing.T) {
	short := claimCA{claim: "shop-db"}.volumeName()
	if short != claimCAVolumePrefix+"shop-db" {
		t.Errorf("volume name is %q, want %q", short, claimCAVolumePrefix+"shop-db")
	}

	long := claimCA{claim: strings.Repeat("a", 63)}.volumeName()
	if len(long) > dnsLabelBudget {
		t.Errorf("volume name %q is %d characters, want at most %d", long, len(long), dnsLabelBudget)
	}
	if other := (claimCA{claim: strings.Repeat("a", 62) + "b"}).volumeName(); other == long {
		t.Error("two claims whose names differ only past the cut share a volume name")
	}
}

// Which key of a binding holds an authority, and the one reading that keeps a
// mount from ever being empty: present-and-empty is not an authority. The
// object store's address refresh writes exactly that when a store loses its
// private certificate, and a file mounted from it would vouch for nothing.
func TestBindingCAKeyReadsPresentAndEmptyAsNone(t *testing.T) {
	for name, tc := range map[string]struct {
		data map[string][]byte
		want string
	}{
		"a database's":      {map[string][]byte{"url": []byte("postgresql://"), "ca": []byte("--")}, "ca"},
		"an object store's": {map[string][]byte{"caCert": []byte("--")}, "caCert"},
		"none":              {map[string][]byte{"url": []byte("postgresql://")}, ""},
		"present and empty": {map[string][]byte{"caCert": {}}, ""},
	} {
		if got := contract.BindingCAKey(tc.data); got != tc.want {
			t.Errorf("%s binding: key is %q, want %q", name, got, tc.want)
		}
	}
}
