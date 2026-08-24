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
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The cycle's own life: opening, coming due, and the two things closing is
// for — the revocations, and the retained artefact.

// fakeSignedRecords is somewhere for an artefact to land, without a store.
type fakeSignedRecords struct {
	records []clickhouse.SignedRecord
	err     error
}

func (s *fakeSignedRecords) InsertSignedRecord(_ context.Context, record clickhouse.SignedRecord) error {
	if s.err != nil {
		return s.err
	}
	s.records = append(s.records, record)
	return nil
}

func openReview(name string, due time.Time, entries ...kitchenv1alpha1.AccessReviewEntry) *kitchenv1alpha1.AccessReview {
	return &kitchenv1alpha1.AccessReview{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace, UID: types.UID("uid-" + name)},
		Spec: kitchenv1alpha1.AccessReviewSpec{
			Scope:     kitchenv1alpha1.AccessReviewAll,
			Reviewers: []kitchenv1alpha1.AccessSubject{{Subject: accessOperator}},
			OpenedBy:  accessOperator,
			DueBy:     metav1.NewTime(due),
		},
		Status: kitchenv1alpha1.AccessReviewStatus{
			Phase:      kitchenv1alpha1.AccessReviewOpen,
			OpenedAt:   &metav1.Time{Time: accessNow.Add(-time.Hour)},
			SnapshotAt: &metav1.Time{Time: accessNow.Add(-time.Hour)},
			Entries:    entries,
		},
	}
}

func entry(subject, grant, role string) kitchenv1alpha1.AccessReviewEntry {
	return kitchenv1alpha1.AccessReviewEntry{
		AccessSubject: kitchenv1alpha1.AccessSubject{Subject: subject},
		Grant:         grant,
		Role:          role,
	}
}

// signingAccessFixtures is a platform that can actually mint an artefact: a
// signing key in the platform namespace, and attestation turned on.
func signingAccessFixtures(t *testing.T, extra ...client.Object) *accessFixtures {
	t.Helper()
	_, privatePEM, publicPEM, err := attestation.GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	key := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SigningKeySecretName, Namespace: PlatformNamespace},
		Data: map[string][]byte{
			attestation.SecretKeyPrivate: privatePEM,
			attestation.SecretKeyPublic:  publicPEM,
		},
	}
	fixtures := newAccessFixtures(t, append(extra, key)...)

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Compliance.Attestation.Enabled = true
	if err := fixtures.client.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

func newAccessReconciler(fixtures *accessFixtures, records *fakeSignedRecords) *AccessReviewReconciler {
	return &AccessReviewReconciler{
		Client:  fixtures.client,
		Now:     func() time.Time { return accessNow },
		Records: func(clickhouse.Config) SignedRecordStore { return records },
	}
}

func reconcileReview(t *testing.T, r *AccessReviewReconciler, name string) ctrl.Result {
	t.Helper()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: PlatformNamespace, Name: name},
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	return result
}

// A cycle whose due date has passed is Overdue — and nothing else happens.
// The consequence of a late review is that somebody has to look, never that
// the platform refuses a deployment: a control that took a workload down
// would be switched off within the month.
func TestAnOverdueCycleIsStampedAndNothingIsRefused(t *testing.T) {
	review := openReview("access-review-late", accessNow.Add(-time.Hour),
		entry(accessSecond, accessProject, "admin"))
	review.Status.Pending = 1
	fixtures := newAccessFixtures(t, review)
	reconciler := newAccessReconciler(fixtures, &fakeSignedRecords{})

	result := reconcileReview(t, reconciler, review.Name)
	if result.RequeueAfter != 0 {
		t.Errorf("an overdue cycle needs no further wake-up, got RequeueAfter=%v", result.RequeueAfter)
	}

	stored := &kitchenv1alpha1.AccessReview{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Namespace: PlatformNamespace, Name: review.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Phase != kitchenv1alpha1.AccessReviewOverdue {
		t.Fatalf("want the cycle stamped Overdue, got %q", stored.Status.Phase)
	}
	// The grant is untouched. Overdue is a report, not a revocation.
	project := &kitchenv1alpha1.Project{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Namespace: PlatformNamespace, Name: accessProject}, project); err != nil {
		t.Fatal(err)
	}
	if len(project.Spec.Access) != 1 {
		t.Errorf("an overdue review must take nothing away, got %+v", project.Spec.Access)
	}
}

// An open cycle wakes itself at the moment it comes due — the Exception
// reconciler's RequeueAfter, which is the cheap alternative to a ticker.
func TestAnOpenCycleWakesItselfWhenItComesDue(t *testing.T) {
	review := openReview("access-review-open", accessNow.Add(48*time.Hour))
	review.Status.Phase = ""
	fixtures := newAccessFixtures(t, review)
	reconciler := newAccessReconciler(fixtures, &fakeSignedRecords{})

	result := reconcileReview(t, reconciler, review.Name)
	if result.RequeueAfter < 47*time.Hour || result.RequeueAfter > 49*time.Hour {
		t.Fatalf("want a wake-up at the due moment, got %v", result.RequeueAfter)
	}
}

// The overdue record is privileged and classified `access`: "the platform's
// access went unreviewed past its own deadline" is a fact about the control
// environment, and it is exactly what an examiner asks for evidence of.
func TestTheOverdueRecordIsAPrivilegedAccessRecord(t *testing.T) {
	review := openReview("access-review-late", accessNow.Add(-time.Hour))
	review.Status.Pending = 3
	transition := accessReviewOverdueTransition(review)

	if transition.Kind != audit.KindAccessReview {
		t.Errorf("the record is about the cycle, got kind %q", transition.Kind)
	}
	if transition.Privileged != audit.PrivilegeAccess {
		t.Errorf("an unreviewed deadline is a privileged access record, got %q", transition.Privileged)
	}
	if transition.Details["pending"] != int32(3) {
		t.Errorf("the record must say how much went undecided: %+v", transition.Details)
	}
}

// Closing carries out what was decided. A `revoke` that left the role in
// place would be a form somebody filled in, not a control.
func TestClosingACycleTakesTheRevokedGrantsOff(t *testing.T) {
	revoked := entry(accessSecond, accessProject, "admin")
	revoked.Decision = kitchenv1alpha1.AccessRevoke
	revoked.DecidedBy = accessOperator
	revoked.DecidedAt = &metav1.Time{Time: accessNow}

	review := openReview("access-review-closing", accessNow.Add(time.Hour), revoked)
	review.Status.Phase = kitchenv1alpha1.AccessReviewClosed
	review.Status.ClosedBy = accessOperator
	review.Status.ClosedAt = &metav1.Time{Time: accessNow}
	review.Status.Revoked = 1

	fixtures := newAccessFixtures(t, review)
	records := &fakeSignedRecords{}
	reconcileReview(t, newAccessReconciler(fixtures, records), review.Name)

	project := &kitchenv1alpha1.Project{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Namespace: PlatformNamespace, Name: accessProject}, project); err != nil {
		t.Fatal(err)
	}
	if len(project.Spec.Access) != 0 {
		t.Fatalf("the revoked grant must be gone, got %+v", project.Spec.Access)
	}

	stored := &kitchenv1alpha1.AccessReview{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Namespace: PlatformNamespace, Name: review.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Status.Entries[0].Applied {
		t.Errorf("a revocation that was carried out must say so: %+v", stored.Status.Entries[0])
	}
}

// The one revocation the platform will not perform. A platform with no
// operators refuses every operator-only route to everybody — including the
// one that names an operator — and there is no way back that does not involve
// kubectl. A compliance control that can lock an institution out of its own
// platform is one that gets turned off.
func TestTheLastOperatorIsNeverRevoked(t *testing.T) {
	revoked := entry(accessOperator, access.PlatformGrant, "operator")
	revoked.Decision = kitchenv1alpha1.AccessRevoke
	revoked.DecidedBy = accessSecond

	review := openReview("access-review-lockout", accessNow.Add(time.Hour), revoked)
	review.Status.Phase = kitchenv1alpha1.AccessReviewClosed
	review.Status.ClosedAt = &metav1.Time{Time: accessNow}

	fixtures := newAccessFixtures(t, review)
	reconcileReview(t, newAccessReconciler(fixtures, &fakeSignedRecords{}), review.Name)

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if len(kitchen.Spec.Access.Operators) != 1 {
		t.Fatalf("the platform's last operator must survive a revocation, got %+v",
			kitchen.Spec.Access.Operators)
	}

	stored := &kitchenv1alpha1.AccessReview{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Namespace: PlatformNamespace, Name: review.Name}, stored); err != nil {
		t.Fatal(err)
	}
	got := stored.Status.Entries[0]
	if got.Applied {
		t.Error("a revocation that was declined must not read as one that was performed")
	}
	if !strings.Contains(got.ApplyMessage, "last operator") {
		t.Errorf("the entry must say why it was declined, got %q", got.ApplyMessage)
	}
}

// A grant somebody removed during the cycle is the outcome the reviewer
// wanted, and must not read as a revocation the platform performed.
func TestARevocationOfAGrantThatIsAlreadyGoneSaysSo(t *testing.T) {
	revoked := entry("nobody@example.com", accessProject, "developer")
	revoked.Decision = kitchenv1alpha1.AccessRevoke
	revoked.DecidedBy = accessOperator

	review := openReview("access-review-gone", accessNow.Add(time.Hour), revoked)
	review.Status.Phase = kitchenv1alpha1.AccessReviewClosed
	review.Status.ClosedAt = &metav1.Time{Time: accessNow}

	fixtures := newAccessFixtures(t, review)
	reconcileReview(t, newAccessReconciler(fixtures, &fakeSignedRecords{}), review.Name)

	stored := &kitchenv1alpha1.AccessReview{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Namespace: PlatformNamespace, Name: review.Name}, stored); err != nil {
		t.Fatal(err)
	}
	got := stored.Status.Entries[0]
	if got.Applied || got.ApplyMessage == "" {
		t.Fatalf("a grant that was already gone must be reported, not claimed: %+v", got)
	}
}

// The first acceptance criterion: a closed cycle leaves a retained,
// timestamped artefact. It is a signed envelope in the store rather than a
// copy on the object, so it survives the object being deleted.
func TestClosingACycleLeavesARetainedTimestampedArtefact(t *testing.T) {
	confirmed := entry(accessSecond, accessProject, "admin")
	confirmed.Decision = kitchenv1alpha1.AccessConfirm
	confirmed.DecidedBy = accessOperator
	confirmed.DecidedAt = &metav1.Time{Time: accessNow}
	selfReviewed := entry(accessOperator, access.PlatformGrant, "operator")
	selfReviewed.Decision = kitchenv1alpha1.AccessConfirm
	selfReviewed.DecidedBy = accessOperator
	selfReviewed.SelfReview = true

	review := openReview("access-review-closed", accessNow.Add(time.Hour), confirmed, selfReviewed)
	review.Status.Phase = kitchenv1alpha1.AccessReviewClosed
	review.Status.ClosedBy = accessOperator
	review.Status.ClosedAt = &metav1.Time{Time: accessNow}
	review.Status.Confirmed = 2
	review.Status.SelfReviewed = 1

	fixtures := signingAccessFixtures(t, review)
	records := &fakeSignedRecords{}
	reconcileReview(t, newAccessReconciler(fixtures, records), review.Name)

	if len(records.records) != 1 {
		t.Fatalf("want one retained artefact, got %d", len(records.records))
	}
	record := records.records[0]
	if record.Type != attestation.PredicateAccessReview {
		t.Errorf("the artefact must carry its own predicate type, got %q", record.Type)
	}
	if record.Subject != AccessReviewIdentityDigest(review) {
		t.Errorf("the artefact's subject must be the cycle's identity, got %q", record.Subject)
	}
	if record.Timestamp.IsZero() {
		t.Error("a retained artefact with no timestamp is not evidence of when anything was reviewed")
	}

	// And the envelope actually says what was decided, by whom.
	predicate := decodeEnvelopePredicate(t, record.Envelope)
	if predicate["closedBy"] != accessOperator {
		t.Errorf("the artefact must name the reviewer who closed it: %+v", predicate)
	}
	entries, _ := predicate["entries"].([]any)
	if len(entries) != 2 {
		t.Fatalf("the artefact must carry every decision, got %d", len(entries))
	}
	if predicate["selfReviewed"] != float64(1) {
		t.Errorf("self-review is carried into the artefact rather than hidden: %+v", predicate)
	}

	stored := &kitchenv1alpha1.AccessReview{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Namespace: PlatformNamespace, Name: review.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Artifact == nil || stored.Status.Artifact.RecordID != record.ID {
		t.Fatalf("the cycle must point at the artefact it produced: %+v", stored.Status.Artifact)
	}
	if stored.Status.Artifact.SignedAt == nil {
		t.Error("the artefact pointer must say when it was minted")
	}
}

// A platform with no signing key still closes the cycle and still applies the
// revocations. What it cannot do is leave portable evidence, and status says
// so rather than leaving a blank field for a reader to interpret generously.
func TestACycleThatCouldNotBeAttestedSaysSo(t *testing.T) {
	review := openReview("access-review-unsigned", accessNow.Add(time.Hour))
	review.Status.Phase = kitchenv1alpha1.AccessReviewClosed
	review.Status.ClosedAt = &metav1.Time{Time: accessNow}

	fixtures := newAccessFixtures(t, review)
	// No signing key secret, and attestation is off on this singleton.
	records := &fakeSignedRecords{}
	reconcileReview(t, newAccessReconciler(fixtures, records), review.Name)

	stored := &kitchenv1alpha1.AccessReview{}
	if err := fixtures.client.Get(context.Background(),
		types.NamespacedName{Namespace: PlatformNamespace, Name: review.Name}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Artifact == nil {
		t.Fatal("a closed cycle must always carry an artefact block, even to say there is none")
	}
	if stored.Status.Artifact.Message == "" {
		t.Error("a cycle that closed unattested must say so: a blank field invites a generous reading")
	}
	if len(records.records) != 0 {
		t.Errorf("nothing should have been stored, got %d records", len(records.records))
	}
}

// The predicate is built apart from the signing, so this is a unit test of
// what the artefact actually claims: an undecided grant is *in* the record.
// "Nobody looked at this one" is exactly the finding an examiner is reading
// for, and omitting it would make the artefact read better than the review
// was.
func TestTheArtefactRecordsUndecidedGrantsRatherThanOmittingThem(t *testing.T) {
	review := openReview("access-review-partial", accessNow.Add(time.Hour),
		entry(accessSecond, accessProject, "admin"))
	review.Status.Pending = 1

	predicate := AccessReviewPredicate(review, accessNow)
	entries, _ := predicate["entries"].([]map[string]any)
	if len(entries) != 1 {
		t.Fatalf("want the undecided grant in the artefact, got %+v", predicate["entries"])
	}
	if entries[0]["decision"] != "undecided" {
		t.Errorf("an undecided grant must say so in words, got %v", entries[0]["decision"])
	}
}

func decodeEnvelopePredicate(t *testing.T, envelope string) map[string]any {
	t.Helper()
	wrapper := struct {
		Payload string `json:"payload"`
	}{}
	if err := json.Unmarshal([]byte(envelope), &wrapper); err != nil {
		t.Fatalf("the envelope is not JSON: %v", err)
	}
	payload, err := base64.StdEncoding.DecodeString(wrapper.Payload)
	if err != nil {
		t.Fatalf("the envelope's payload is not base64: %v", err)
	}
	statement := struct {
		Predicate map[string]any `json:"predicate"`
	}{}
	if err := json.Unmarshal(payload, &statement); err != nil {
		t.Fatalf("the payload is not a statement: %v", err)
	}
	return statement.Predicate
}
