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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/idp"
)

// Access recertification and the privileged surface (#139), acceptance
// criterion by acceptance criterion:
//
//   - recertification produces a retained, timestamped artefact per cycle —
//     the signed record in accessreview_artifact_test.go;
//   - out-of-band mutation of a Kitchen-managed object raises an alert — the
//     foreign-manager tests below plus the audit record they produce;
//   - privileged actions are separable from ordinary ones in the audit log —
//     internal/audit/privilege_test.go, and every record here carrying its
//     class;
//   - the residual risk of cluster-admin bypass is documented rather than
//     implied — docs/COMPLIANCE.md §11, and the blind spot the last test
//     below pins so that nobody quietly starts claiming otherwise.

const (
	accessOperator = "grace@example.com"
	accessSecond   = "heidi@example.com"
	accessProject  = "shop"
)

var accessNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

type stubAccountDirectory struct {
	accounts []idp.Account
	err      error
}

func (d *stubAccountDirectory) Accounts(context.Context) ([]idp.Account, error) {
	return d.accounts, d.err
}

type fakeActivityStore struct {
	activity map[string]time.Time
	err      error
}

func (s *fakeActivityStore) ActorActivity(context.Context) (map[string]time.Time, error) {
	return s.activity, s.err
}

type accessFixtures struct {
	client    client.Client
	sweeper   *AccessSweeper
	directory *stubAccountDirectory
	activity  *fakeActivityStore
}

// accessSingleton is a platform with the access controls on, one operator and
// one project with a grant.
func accessSingleton() *kitchenv1alpha1.Kitchen {
	return &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			Access: kitchenv1alpha1.AccessSpec{
				Operators: []kitchenv1alpha1.AccessSubject{{Subject: accessOperator}},
			},
			Auth: kitchenv1alpha1.AuthSpec{
				SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: "auth"},
			},
			Observability: kitchenv1alpha1.ObservabilitySpec{
				ClickHouse: kitchenv1alpha1.ClickHouseSpec{
					SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: "clickhouse"},
				},
			},
			Compliance: kitchenv1alpha1.ComplianceSpec{
				Access: kitchenv1alpha1.AccessComplianceSpec{
					Enabled:               true,
					IntervalDays:          90,
					DueDays:               14,
					InactivityDays:        90,
					DetectOutOfBandWrites: true,
				},
			},
		},
	}
}

func newAccessFixtures(t *testing.T, extra ...client.Object) *accessFixtures {
	t.Helper()

	objects := []client.Object{
		accessSingleton(),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "clickhouse", Namespace: PlatformNamespace},
			Data: map[string][]byte{
				"host": []byte("clickhouse"), "httpPort": []byte("8123"),
				"database": []byte("kitchen"), "username": []byte("kitchen"), "password": []byte("s"),
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "auth", Namespace: PlatformNamespace},
			Data: map[string][]byte{
				"issuer": []byte("https://auth.example.com"),
				"url":    []byte("http://auth.kitchen-system.svc:3000"),
				"token":  []byte("service-token"),
			},
		},
		&kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: accessProject, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Access: []kitchenv1alpha1.AccessGrant{
					{
						AccessSubject: kitchenv1alpha1.AccessSubject{Subject: accessSecond},
						Role:          kitchenv1alpha1.AccessRoleAdmin,
					},
				},
			},
		},
	}
	objects = append(objects, extra...)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kitchenv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		WithStatusSubresource(
			&kitchenv1alpha1.Kitchen{}, &kitchenv1alpha1.AccessReview{},
			&kitchenv1alpha1.Project{},
		).
		Build()

	fixtures := &accessFixtures{
		client: c,
		directory: &stubAccountDirectory{accounts: []idp.Account{
			{Subject: accessOperator, Email: accessOperator},
			{Subject: accessSecond, Email: accessSecond},
		}},
		activity: &fakeActivityStore{activity: map[string]time.Time{
			accessOperator: accessNow.Add(-time.Hour),
			accessSecond:   accessNow.Add(-2 * time.Hour),
		}},
	}
	fixtures.sweeper = &AccessSweeper{
		Client:    c,
		Namespace: PlatformNamespace,
		Now:       func() time.Time { return accessNow },
		Accounts:  func(idp.Config) AccountDirectory { return fixtures.directory },
		Stores:    func(clickhouse.Config) ActivityStore { return fixtures.activity },
	}
	return fixtures
}

// The cadence opens the first cycle on an installation that has never had
// one, and the cycle it opens carries the snapshot — every grant as it stood
// at that instant, which is what the review is a review *of*.
func TestTheCadenceOpensACycleWithItsSnapshotFrozen(t *testing.T) {
	fixtures := newAccessFixtures(t)
	ctx := context.Background()

	report, err := fixtures.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if report.Opened == "" {
		t.Fatal("an installation that has never recertified is due a cycle, and none was opened")
	}

	reviews := &kitchenv1alpha1.AccessReviewList{}
	if err := fixtures.client.List(ctx, reviews); err != nil {
		t.Fatal(err)
	}
	if len(reviews.Items) != 1 {
		t.Fatalf("want one cycle, got %d", len(reviews.Items))
	}
	review := reviews.Items[0]
	if review.Spec.Scope != kitchenv1alpha1.AccessReviewAll {
		t.Errorf("the cadence reviews the whole install, got scope %q", review.Spec.Scope)
	}
	if review.Status.SnapshotAt == nil {
		t.Error("a cycle without a snapshot instant is a review of nothing in particular")
	}
	if len(review.Status.Entries) != 2 {
		t.Fatalf("want the operator grant and the project grant, got %d: %+v",
			len(review.Status.Entries), review.Status.Entries)
	}
	if review.Status.Pending != 2 {
		t.Errorf("every grant starts undecided, got pending=%d", review.Status.Pending)
	}
	if review.Status.Entries[0].Grant != access.PlatformGrant {
		t.Errorf("the platform's own grant leads the snapshot: %+v", review.Status.Entries[0])
	}
	if review.Spec.DueBy.Time.Sub(accessNow) != 14*24*time.Hour {
		t.Errorf("dueDays is 14, got a deadline of %v", review.Spec.DueBy.Time.Sub(accessNow))
	}
}

// One cycle at a time. Two open cycles over the same grants would be two
// reviewers deciding the same question, and a close that applied one set of
// revocations while the other still showed the grant.
func TestTheCadenceOpensNoSecondCycleWhileOneIsOpen(t *testing.T) {
	fixtures := newAccessFixtures(t)
	ctx := context.Background()

	if _, err := fixtures.sweeper.SweepOnce(ctx); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	report, err := fixtures.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if report.Opened != "" {
		t.Errorf("a second cycle was opened while one was already open: %q", report.Opened)
	}
	if report.OpenReview == "" {
		t.Error("the sweep must report the cycle that is open")
	}
}

// The cadence is counted from the last cycle's close, not from a fixed grid:
// an installation that recertified late does not immediately owe another.
func TestTheCadenceIsCountedFromTheLastClose(t *testing.T) {
	closed := &kitchenv1alpha1.AccessReview{
		ObjectMeta: metav1.ObjectMeta{Name: "access-review-old", Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.AccessReviewSpec{
			Scope:     kitchenv1alpha1.AccessReviewAll,
			Reviewers: []kitchenv1alpha1.AccessSubject{{Subject: accessOperator}},
			OpenedBy:  accessOperator,
			DueBy:     metav1.NewTime(accessNow.Add(-60 * 24 * time.Hour)),
		},
		Status: kitchenv1alpha1.AccessReviewStatus{
			Phase:    kitchenv1alpha1.AccessReviewClosed,
			ClosedAt: &metav1.Time{Time: accessNow.Add(-30 * 24 * time.Hour)},
		},
	}
	fixtures := newAccessFixtures(t, closed)

	report, err := fixtures.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if report.Opened != "" {
		t.Errorf("a cycle closed 30 days ago on a 90-day cadence is not due, and one was opened: %q",
			report.Opened)
	}
	if report.LastClosed == nil {
		t.Error("the sweep must report when access was last recertified: it is the question that gets asked")
	}
}

// A platform with no operators has nobody to ask, and a cycle with no
// addressee is a form nobody receives. The OperatorsConfigured condition is
// already saying what is wrong.
func TestNoOperatorsOpensNoCycle(t *testing.T) {
	fixtures := newAccessFixtures(t)
	ctx := context.Background()

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := fixtures.client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Access.Operators = nil
	if err := fixtures.client.Update(ctx, kitchen); err != nil {
		t.Fatal(err)
	}

	report, err := fixtures.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if report.Opened != "" {
		t.Errorf("a platform with no operators has nobody to review it, and a cycle was opened: %q",
			report.Opened)
	}
}

// Turned off, the sweep opens nothing and watches nothing — and says which of
// the two states it is in, rather than reporting a clean zero.
func TestTurnedOffTheSweepSaysSoRatherThanReportingNothing(t *testing.T) {
	fixtures := newAccessFixtures(t)
	ctx := context.Background()

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := fixtures.client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Compliance.Access.Enabled = false
	if err := fixtures.client.Update(ctx, kitchen); err != nil {
		t.Fatal(err)
	}

	report, err := fixtures.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if report.Reviewing {
		t.Error("the controls are off and the sweep claimed to be reviewing")
	}
	if report.Message == "" {
		t.Error("a control that is off must say so: the failure mode of evidence is silence")
	}

	if err := fixtures.client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if kitchen.Status.Compliance == nil || kitchen.Status.Compliance.Access == nil {
		t.Fatal("the singleton must carry the access posture even when it is off")
	}
	if kitchen.Status.Compliance.Access.Message == "" {
		t.Error("the published status must explain a survey that is not running")
	}
}

// The acceptance criterion, at the level the detection is a pure function:
// a manager the platform does not recognise on a guarded object is a finding.
func TestAForeignFieldManagerIsAFinding(t *testing.T) {
	expected := expectedManagers(kitchenv1alpha1.AccessComplianceSpec{})
	written := metav1.NewTime(accessNow.Add(-time.Minute))
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: accessProject, Namespace: PlatformNamespace,
			ManagedFields: []metav1.ManagedFieldsEntry{
				{Manager: "manager", Operation: metav1.ManagedFieldsOperationUpdate, Time: &written},
				{
					Manager: "kubectl-edit", Operation: metav1.ManagedFieldsOperationUpdate,
					Time: &written,
					FieldsV1: &metav1.FieldsV1{
						Raw: []byte(`{"f:spec":{"f:access":{}}}`),
					},
				},
			},
		},
	}

	findings := ForeignWrites(audit.KindProject, project, expected)
	if len(findings) != 1 {
		t.Fatalf("want one finding for the one foreign manager, got %d: %+v", len(findings), findings)
	}
	finding := findings[0]
	if finding.Manager != "kubectl-edit" || finding.Kind != audit.KindProject || finding.Name != accessProject {
		t.Fatalf("the finding must name what was written and by what: %+v", finding)
	}
	if finding.Fields == "" {
		t.Error("the finding must say what that manager owns, or a reader cannot tell a spec edit from a label")
	}
}

// A status write is every controller's, including ones that legitimately
// watch Kitchen's objects, and it cannot change what the platform allows.
// What is being looked for is a write to the spec.
func TestAStatusWriteIsNotAnOutOfBandWrite(t *testing.T) {
	written := metav1.NewTime(accessNow)
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: accessProject, Namespace: PlatformNamespace,
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "some-other-controller", Operation: metav1.ManagedFieldsOperationUpdate,
				Subresource: "status", Time: &written,
			}},
		},
	}
	if findings := ForeignWrites(audit.KindProject, project, expectedManagers(
		kitchenv1alpha1.AccessComplianceSpec{})); len(findings) != 0 {
		t.Fatalf("a status write must not be reported as an out-of-band change: %+v", findings)
	}
}

// An installation with a legitimate writer of its own names it, and naming it
// is the operator putting "this one is expected" on the record — the
// alternative being an alert everybody learns to ignore. Matched exactly.
func TestAnExpectedManagerIsNotAFinding(t *testing.T) {
	written := metav1.NewTime(accessNow)
	entry := func(manager string) metav1.ManagedFieldsEntry {
		return metav1.ManagedFieldsEntry{
			Manager: manager, Operation: metav1.ManagedFieldsOperationApply, Time: &written,
		}
	}
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: accessProject, Namespace: PlatformNamespace,
			ManagedFields: []metav1.ManagedFieldsEntry{entry("argocd-controller"), entry("argocd")},
		},
	}
	expected := expectedManagers(kitchenv1alpha1.AccessComplianceSpec{
		ExpectedManagers: []string{"argocd"},
	})
	findings := ForeignWrites(audit.KindProject, project, expected)
	if len(findings) != 1 || findings[0].Manager != "argocd-controller" {
		t.Fatalf("the named manager is expected and nothing near it is: %+v", findings)
	}
}

// The blind spot, pinned so that nobody starts claiming otherwise: a caller
// may name their own field manager anything they like, and one that names
// itself as the platform is invisible here. This is why the detection is
// documented as detection rather than prevention, and why the residual risk
// in docs/COMPLIANCE.md §11 is stated rather than argued away.
func TestAWriterThatNamesItselfKitchenIsNotDetected(t *testing.T) {
	written := metav1.NewTime(accessNow)
	project := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: accessProject, Namespace: PlatformNamespace,
			ManagedFields: []metav1.ManagedFieldsEntry{{
				// `kubectl edit --field-manager=kitchen`.
				Manager: "kitchen", Operation: metav1.ManagedFieldsOperationUpdate, Time: &written,
			}},
		},
	}
	if findings := ForeignWrites(audit.KindProject, project, expectedManagers(
		kitchenv1alpha1.AccessComplianceSpec{})); len(findings) != 0 {
		t.Fatalf("this is the documented blind spot; if it now produces a finding, "+
			"docs/COMPLIANCE.md §11.4 needs rewriting rather than this test deleting: %+v", findings)
	}
}

// The acceptance criterion end to end: a Kitchen-managed object carrying a
// manager the platform does not recognise is counted, timestamped and
// published — the alert, in the one place an operator looks.
func TestASweepCountsAndPublishesAnOutOfBandWrite(t *testing.T) {
	written := metav1.NewTime(accessNow.Add(-time.Minute))
	edited := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shop-production", Namespace: PlatformNamespace,
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "kubectl-edit", Operation: metav1.ManagedFieldsOperationUpdate, Time: &written,
				FieldsV1: &metav1.FieldsV1{Raw: []byte(`{"f:spec":{"f:requirements":{}}}`)},
			}},
		},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: accessProject},
		},
	}
	fixtures := newAccessFixtures(t, edited)
	ctx := context.Background()

	report, err := fixtures.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if report.OutOfBand != 1 {
		t.Fatalf("want one out-of-band write, got %d", report.OutOfBand)
	}
	if report.LastOutOfBand == nil || !report.LastOutOfBand.Equal(written.Time.UTC()) {
		t.Errorf("the newest such write must be dated, got %v", report.LastOutOfBand)
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := fixtures.client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	status := kitchen.Status.Compliance.Access
	if status == nil || status.OutOfBandWrites != 1 || status.LastOutOfBand == nil {
		t.Fatalf("the finding must reach the singleton, or it is an alert nobody can see: %+v", status)
	}

	// And it is recorded once, not once every five minutes: a foreign manager
	// stays on the object until the platform writes those fields again.
	second, err := fixtures.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("second SweepOnce: %v", err)
	}
	if second.OutOfBand != 1 {
		t.Errorf("the standing count must not grow on a second pass, got %d", second.OutOfBand)
	}
}

// The detection's audit record is privileged and classified `integrity`, is
// about the platform rather than about the project whose object was written,
// and puts the field manager in the details rather than in the actor — a
// manager name is a string the writer chose, and the log must not present it
// as an authenticated identity.
func TestTheOutOfBandRecordIsAboutThePlatformAndNamesTheManagerAsData(t *testing.T) {
	kitchen := accessSingleton()
	transition := outOfBandTransition(OutOfBandWrite{
		Kind: audit.KindProject, Namespace: PlatformNamespace, Name: accessProject,
		Manager: "kubectl-edit", Operation: "Update", At: accessNow,
		Fields: `{"f:spec":{"f:access":{}}}`,
	}, kitchen)

	if transition.Kind != audit.KindKitchen {
		t.Errorf("the record is a statement about the platform's integrity, got kind %q", transition.Kind)
	}
	if transition.Project != "" {
		t.Errorf("it must not land in a project's audit view as though the project did it, got %q",
			transition.Project)
	}
	if transition.Privileged != audit.PrivilegeIntegrity {
		t.Errorf("a write the platform did not make is a privileged integrity record, got %q",
			transition.Privileged)
	}
	if transition.Actor != "" {
		t.Errorf("the actor is the reconciler that noticed, never the manager that wrote: %q", transition.Actor)
	}
	if transition.Details["fieldManager"] != "kubectl-edit" {
		t.Errorf("the manager belongs in the details, where an unverified claim belongs: %+v",
			transition.Details)
	}
	for _, key := range []string{"objectKind", "objectName", "operation", "detection", "fields"} {
		if _, ok := transition.Details[key]; !ok {
			t.Errorf("the record must carry %q: %+v", key, transition.Details)
		}
	}
}

// Turned off, the detection looks for nothing — an installation that decided
// its cluster access is managed elsewhere is not nagged about it.
func TestDetectionOffLooksForNothing(t *testing.T) {
	written := metav1.NewTime(accessNow)
	edited := &kitchenv1alpha1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: "blog", Namespace: PlatformNamespace,
			ManagedFields: []metav1.ManagedFieldsEntry{{
				Manager: "kubectl-edit", Operation: metav1.ManagedFieldsOperationUpdate, Time: &written,
			}},
		},
	}
	fixtures := newAccessFixtures(t, edited)
	ctx := context.Background()

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := fixtures.client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Compliance.Access.DetectOutOfBandWrites = false
	if err := fixtures.client.Update(ctx, kitchen); err != nil {
		t.Fatal(err)
	}

	report, err := fixtures.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if report.OutOfBand != 0 {
		t.Errorf("detection is off and something was reported: %d", report.OutOfBand)
	}
}

// The sweep publishes what it found on the singleton, so an alert nobody can
// see is not what this is.
func TestTheSweepPublishesTheAccessPostureOnTheSingleton(t *testing.T) {
	fixtures := newAccessFixtures(t)
	ctx := context.Background()

	if _, err := fixtures.sweeper.SweepOnce(ctx); err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := fixtures.client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	status := kitchen.Status.Compliance.Access
	if status == nil || !status.Reviewing {
		t.Fatalf("the access posture must be published and reviewing: %+v", status)
	}
	if status.Identities != 2 {
		t.Errorf("two grants were surveyed, the status says %d", status.Identities)
	}
	if status.OpenReview == "" || status.DueBy == nil {
		t.Errorf("an open cycle and its deadline must be visible on the singleton: %+v", status)
	}
}

// A directory that would not answer must not turn every grant into an orphan.
// "We could not ask" and "nobody is behind it" are different sentences, and
// only one of them is evidence.
func TestADirectoryThatWillNotAnswerClaimsNoOrphans(t *testing.T) {
	fixtures := newAccessFixtures(t)
	fixtures.directory.err = idp.ErrNoDirectory

	report, err := fixtures.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatalf("SweepOnce: %v", err)
	}
	if report.Orphans != 0 {
		t.Errorf("no directory means no ownership claim, got %d orphans", report.Orphans)
	}
	if report.Message == "" {
		t.Error("the survey must say what it could not read")
	}
}
