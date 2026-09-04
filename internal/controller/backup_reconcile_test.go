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
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

func s3Destination() *kitchenv1alpha1.BackupDestination {
	return &kitchenv1alpha1.BackupDestination{
		Type: kitchenv1alpha1.BackupDestinationS3,
		S3: &kitchenv1alpha1.S3Destination{
			Bucket: "kitchen-backups",
			Prefix: "prod",
			Region: "us-east-1",
		},
	}
}

// What the CRD refuses outright. These are the `tls.mode: acme` rules again:
// a configuration that cannot produce a usable backup is a failed write,
// rather than an object the API server accepted and a condition somebody
// reads six weeks later.
var _ = Describe("Kitchen backup admission", func() {
	ctx := context.Background()

	const name = "backup-admission-probe"

	kitchenWith := func(backup kitchenv1alpha1.BackupSpec) *kitchenv1alpha1.Kitchen {
		return &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Backup:     backup,
			},
		}
	}

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx,
			&kitchenv1alpha1.Kitchen{ObjectMeta: metav1.ObjectMeta{Name: name}}))).To(Succeed())
	})

	It("refuses a schedule with nowhere to write to", func() {
		err := k8sClient.Create(ctx, kitchenWith(kitchenv1alpha1.BackupSpec{Schedule: "0 3 * * *"}))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected a validation failure, got %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.backup.destination is required when a schedule is set"))
	})

	It("refuses a retention with no destination to apply to", func() {
		err := k8sClient.Create(ctx, kitchenWith(kitchenv1alpha1.BackupSpec{
			Retention: kitchenv1alpha1.BackupRetentionSpec{KeepLast: ptr.To(int32(7))},
		}))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected a validation failure, got %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.backup.retention needs a destination"))
	})

	It("refuses an s3 destination with no s3 block", func() {
		err := k8sClient.Create(ctx, kitchenWith(kitchenv1alpha1.BackupSpec{
			Schedule:    "0 3 * * *",
			Destination: &kitchenv1alpha1.BackupDestination{Type: kitchenv1alpha1.BackupDestinationS3},
		}))
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "expected a validation failure, got %v", err)
		Expect(err.Error()).To(ContainSubstring("spec.backup.destination.s3 is required"))
	})

	It("admits an installation with no scheduled backup at all", func() {
		// The state every installation predating the field is in, and the
		// state a fresh install starts in. It has to be writable.
		Expect(k8sClient.Create(ctx, kitchenWith(kitchenv1alpha1.BackupSpec{}))).To(Succeed())
	})

	It("admits a schedule with a destination and a retention", func() {
		Expect(k8sClient.Create(ctx, kitchenWith(kitchenv1alpha1.BackupSpec{
			Schedule:    "0 3 * * *",
			Destination: s3Destination(),
			Retention:   kitchenv1alpha1.BackupRetentionSpec{KeepLast: ptr.To(int32(7))},
		}))).To(Succeed())
	})
})

var _ = Describe("Kitchen scheduled backup", func() {
	ctx := context.Background()

	singletonKey := types.NamespacedName{Name: KitchenSingletonName}
	cronKey := types.NamespacedName{Namespace: PlatformNamespace, Name: BackupCronJobName}

	var reconciler *KitchenReconciler

	singletonWith := func(backup kitchenv1alpha1.BackupSpec) {
		GinkgoHelper()
		ensureSingleton(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
			Spec: kitchenv1alpha1.KitchenSpec{
				BaseDomain: "apps.example.com",
				TLS:        acmeTLS(),
				Backup:     backup,
			},
		})
	}

	reconcileBackup := func() *kitchenv1alpha1.Kitchen {
		GinkgoHelper()
		kitchen := &kitchenv1alpha1.Kitchen{}
		Expect(k8sClient.Get(ctx, singletonKey, kitchen)).To(Succeed())
		reconciler.reconcileBackup(ctx, kitchen, func(
			condType string, status metav1.ConditionStatus, reason, message string,
		) {
			meta.SetStatusCondition(&kitchen.Status.Conditions, metav1.Condition{
				Type: condType, Status: status, Reason: reason, Message: message,
				ObservedGeneration: kitchen.Generation,
			})
		})
		return kitchen
	}

	BeforeEach(func() {
		reconciler = &KitchenReconciler{
			Client:               k8sClient,
			Scheme:               k8sClient.Scheme(),
			BackupImage:          "ghcr.io/bermos/kitchen:test",
			BackupServiceAccount: "kitchen-backup",
		}
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Namespace: PlatformNamespace, Name: BackupCronJobName},
		}))).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &kitchenv1alpha1.Kitchen{
			ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		}))).To(Succeed())
	})

	It("writes a CronJob of /backup --upload from the spec", func() {
		singletonWith(kitchenv1alpha1.BackupSpec{
			Schedule:    "0 3 * * *",
			Destination: s3Destination(),
			Timeout:     metav1.Duration{Duration: 20 * time.Minute},
		})
		kitchen := reconcileBackup()

		cron := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, cronKey, cron)).To(Succeed())
		Expect(cron.Spec.Schedule).To(Equal("0 3 * * *"))
		Expect(cron.Spec.ConcurrencyPolicy).To(Equal(batchv1.ForbidConcurrent))
		Expect(cron.Spec.StartingDeadlineSeconds).NotTo(BeNil())
		Expect(cron.Spec.Suspend).To(Equal(ptr.To(false)))
		Expect(cron.Labels).To(HaveKeyWithValue(labelPartOfKey, labelPartOfValue))

		By("running the operator's own image, so the archive and its reader are one release")
		container := cron.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
		Expect(container.Image).To(Equal("ghcr.io/bermos/kitchen:test"))
		Expect(container.Command).To(Equal([]string{"/backup"}))
		Expect(container.Args).To(ContainElement("--upload"))
		Expect(cron.Spec.JobTemplate.Spec.Template.Spec.ServiceAccountName).To(Equal("kitchen-backup"))

		By("bounding one run at the configured timeout")
		Expect(cron.Spec.JobTemplate.Spec.ActiveDeadlineSeconds).To(Equal(ptr.To(int64(1200))))

		By("reporting what is configured, and that nothing has run yet")
		Expect(kitchen.Status.Backup).NotTo(BeNil())
		Expect(kitchen.Status.Backup.Schedule).To(Equal("0 3 * * *"))
		Expect(kitchen.Status.Backup.Destination).To(Equal("s3://kitchen-backups/prod"))
		Expect(kitchen.Status.Backup.LastSuccess).To(BeNil())
	})

	It("moves the CronJob when the schedule changes, and suspends it when asked", func() {
		singletonWith(kitchenv1alpha1.BackupSpec{Schedule: "0 3 * * *", Destination: s3Destination()})
		reconcileBackup()

		singletonWith(kitchenv1alpha1.BackupSpec{
			Schedule: "30 1 * * *", Suspend: true, Destination: s3Destination(),
		})
		reconcileBackup()

		cron := &batchv1.CronJob{}
		Expect(k8sClient.Get(ctx, cronKey, cron)).To(Succeed())
		Expect(cron.Spec.Schedule).To(Equal("30 1 * * *"))
		Expect(cron.Spec.Suspend).To(Equal(ptr.To(true)))
	})

	It("removes the CronJob when the schedule is cleared", func() {
		singletonWith(kitchenv1alpha1.BackupSpec{Schedule: "0 3 * * *", Destination: s3Destination()})
		reconcileBackup()
		Expect(k8sClient.Get(ctx, cronKey, &batchv1.CronJob{})).To(Succeed())

		// A CronJob whose configuration has been taken away would keep
		// exporting every credential the platform holds to a destination
		// nobody is watching.
		singletonWith(kitchenv1alpha1.BackupSpec{})
		kitchen := reconcileBackup()

		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, cronKey, &batchv1.CronJob{}))).To(BeTrue())
		condition := meta.FindStatusCondition(kitchen.Status.Conditions, ConditionBackupReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal("NotScheduled"))
	})
})

// backupHealth is where "six weeks of no archive" becomes a sentence
// somebody reads, so it is tested against a status rather than a cluster.
func TestBackupHealth(t *testing.T) {
	now := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	at := func(offset time.Duration) *metav1.Time {
		return &metav1.Time{Time: now.Add(offset)}
	}

	for _, tc := range []struct {
		name    string
		status  kitchenv1alpha1.BackupStatus
		healthy bool
		reason  string
	}{
		{
			name:    "a schedule that has never run yet is not a failure",
			status:  kitchenv1alpha1.BackupStatus{Schedule: "0 3 * * *", Destination: "s3://b"},
			healthy: true, reason: "Scheduled",
		},
		{
			name: "a recent archive is the healthy case",
			status: kitchenv1alpha1.BackupStatus{
				Destination: "s3://b", LastSuccess: at(-6 * time.Hour), LastRun: at(-6 * time.Hour),
			},
			healthy: true, reason: "BackedUp",
		},
		{
			name: "a suspended schedule is a decision, not a fault",
			status: kitchenv1alpha1.BackupStatus{
				Suspended: true, LastSuccess: at(-30 * 24 * time.Hour),
			},
			healthy: true, reason: "Suspended",
		},
		{
			name: "a failure after the last success is unhealthy",
			status: kitchenv1alpha1.BackupStatus{
				Destination: "s3://b", LastSuccess: at(-48 * time.Hour), LastFailure: at(-2 * time.Hour),
				Message: "the destination refused the credential",
			},
			healthy: false, reason: "RunFailed",
		},
		{
			name: "a run that started and never finished is unhealthy",
			status: kitchenv1alpha1.BackupStatus{
				Destination: "s3://b", LastSuccess: at(-72 * time.Hour), LastRun: at(-24 * time.Hour),
			},
			healthy: false, reason: "RunLate",
		},
		{
			name: "a schedule that has been running for days and never succeeded is unhealthy",
			status: kitchenv1alpha1.BackupStatus{
				Destination: "s3://b", LastRun: at(-24 * time.Hour),
			},
			healthy: false, reason: "NeverSucceeded",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			healthy, reason, message := backupHealth(&tc.status, DefaultBackupTimeout, now)
			if healthy != tc.healthy || reason != tc.reason {
				t.Fatalf("wanted healthy=%v reason=%q, got healthy=%v reason=%q (%s)",
					tc.healthy, tc.reason, healthy, reason, message)
			}
			if message == "" {
				t.Error("every judgement has to say what it is judging, in words somebody can act on")
			}
		})
	}
}

// The component survey is where the absence of a backup becomes visible in
// the list an operator already reads.
func TestBackupComponent(t *testing.T) {
	unconfigured := &kitchenv1alpha1.Kitchen{}
	if row := backupComponent(unconfigured); row != nil {
		t.Errorf("an installation with no scheduled backup has no CronJob to report on, got %+v", row)
	}

	configured := &kitchenv1alpha1.Kitchen{
		Spec: kitchenv1alpha1.KitchenSpec{Backup: kitchenv1alpha1.BackupSpec{
			Schedule:    "0 3 * * *",
			Destination: s3Destination(),
		}},
		Status: kitchenv1alpha1.KitchenStatus{Backup: &kitchenv1alpha1.BackupStatus{
			Destination: "s3://kitchen-backups/prod",
			LastRun:     &metav1.Time{Time: time.Now().Add(-6 * 24 * time.Hour)},
			LastFailure: &metav1.Time{Time: time.Now().Add(-6 * 24 * time.Hour)},
			Message:     "the destination refused the credential",
		}},
	}
	row := backupComponent(configured)
	if row == nil {
		t.Fatal("a configured backup is a row in the survey, whatever state it is in")
	}
	if row.Healthy || row.Kind != "CronJob" || row.Name != backupComponentName {
		t.Fatalf("wanted an unhealthy CronJob row named %q, got %+v", backupComponentName, row)
	}
}
