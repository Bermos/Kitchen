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
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
)

// The continuous re-evaluation pass, acceptance criterion by acceptance
// criterion:
//
//   - it requires no rebuild and no redeploy;
//   - its findings are time-stamped and every scan is a stored decision, so
//     the history is kept;
//   - the vulnerability database snapshot is stored with each scan;
//   - an expired exception stops waiving, with no expiry engine anywhere;
//   - and the drift view's two failure modes are told apart (that half is in
//     internal/api).

const (
	rescanProject = "shop"
	rescanEnvName = "shop-production"
	rescanRelease = "shop-rel-a"
	rescanOldRel  = "shop-rel-old"
	rescanDigest  = "sha256:" + "1111222233334444" + "5555666677778888" +
		"9999aaaabbbbcccc" + "ddddeeee"
	rescanRepo  = "registry.example.com/kitchen/shop"
	rescanImage = rescanRepo + "@" + rescanDigest
)

// grypeSnapshot is the database identifier the grype report above names, and
// the one every layer of the pass has to carry through unchanged.
const grypeSnapshot = "grype-db:sha256:deadbeef"

// rescanScanner is the matcher an installation configured: pointed at the
// bill of materials, never at the image.
func rescanScanner() *kitchenv1alpha1.VulnerabilityScannerSpec {
	return &kitchenv1alpha1.VulnerabilityScannerSpec{
		Name:    "grype",
		Image:   "anchore/grype:v0.87.0",
		Version: "0.87.0",
		Format:  scanFormatGrype,
		Args:    []string{"-o", "json", "--file", "$(KITCHEN_FINDINGS)", "sbom:$(KITCHEN_SBOM)"},
	}
}

// rescanFixtures is a platform with the pass turned on, one environment
// running one release, and everything the sweep resolves along the way.
type rescanFixtures struct {
	sweeper  *RescanSweeper
	store    *fakeDecisionStore
	attester *stubAttester
	evidence *fakeEvidenceSetReader
	client   client.Client
}

func newRescanFixtures(t *testing.T, tweak func(*kitchenv1alpha1.Kitchen), extra ...client.Object) *rescanFixtures {
	t.Helper()

	kitchen := &kitchenv1alpha1.Kitchen{
		ObjectMeta: metav1.ObjectMeta{Name: KitchenSingletonName},
		Spec: kitchenv1alpha1.KitchenSpec{
			Observability: kitchenv1alpha1.ObservabilitySpec{
				ClickHouse: kitchenv1alpha1.ClickHouseSpec{
					SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: "clickhouse"},
				},
			},
			Compliance: kitchenv1alpha1.ComplianceSpec{
				Attestation: kitchenv1alpha1.AttestationSpec{Enabled: true},
				Rescan: kitchenv1alpha1.RescanSpec{
					Enabled: true,
					Scanner: rescanScanner(),
				},
			},
		},
	}
	if tweak != nil {
		tweak(kitchen)
	}

	_, privatePEM, publicPEM, err := attestation.GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	objects := []client.Object{
		kitchen,
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: SigningKeySecretName, Namespace: PlatformNamespace},
			Data: map[string][]byte{
				attestation.SecretKeyPrivate: privatePEM,
				attestation.SecretKeyPublic:  publicPEM,
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "clickhouse", Namespace: PlatformNamespace},
			Data: map[string][]byte{
				"host": []byte("clickhouse"), "httpPort": []byte("8123"),
				"database": []byte("kitchen"), "username": []byte("kitchen"), "password": []byte("s"),
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "registry-creds", Namespace: PlatformNamespace},
			Data: map[string][]byte{
				corev1.DockerConfigJsonKey: []byte(`{"auths":{"registry.example.com":{"username":"robot"}}}`),
			},
		},
		&kitchenv1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "registry", Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.ConnectionSpec{
				Provider:             "dockerRegistry",
				CredentialsSecretRef: kitchenv1alpha1.CredentialsReference{Name: "registry-creds"},
				Config:               &runtime.RawExtension{Raw: []byte(`{"url":"registry.example.com/kitchen"}`)},
			},
		},
		&kitchenv1alpha1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: rescanProject, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.ProjectSpec{
				Source: kitchenv1alpha1.ProjectSourceSpec{Git: &kitchenv1alpha1.GitSourceSpec{Repo: "acme/shop"}},
				Registry: &kitchenv1alpha1.RegistrySpec{
					ConnectionRef: kitchenv1alpha1.LocalObjectReference{Name: "registry"},
				},
			},
		},
		&kitchenv1alpha1.Build{
			ObjectMeta: metav1.ObjectMeta{Name: "shop-bld-a", Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.BuildSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: rescanProject},
				Git:        kitchenv1alpha1.GitRevision{SHA: "aaaa1111", Branch: "main"},
			},
			Status: kitchenv1alpha1.BuildStatus{
				Phase:    kitchenv1alpha1.BuildSucceeded,
				Artifact: &kitchenv1alpha1.ArtifactStatus{Repository: rescanRepo, Digest: rescanDigest},
			},
		},
		&kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: rescanRelease, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: rescanProject},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "shop-bld-a"},
				Image:      rescanImage,
			},
		},
		&kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{Name: rescanOldRel, Namespace: PlatformNamespace},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: rescanProject},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "shop-bld-a"},
				Image:      rescanImage,
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
			&kitchenv1alpha1.Environment{}, &kitchenv1alpha1.Kitchen{},
			&kitchenv1alpha1.Build{}, &kitchenv1alpha1.Exception{},
			&batchv1.Job{}, &corev1.Pod{},
		).
		Build()

	fixtures := &rescanFixtures{
		store:    &fakeDecisionStore{},
		attester: &stubAttester{blobs: map[string][]byte{}},
		evidence: &fakeEvidenceSetReader{},
		client:   c,
	}
	fixtures.sweeper = &RescanSweeper{
		Client:          c,
		OperatorImage:   "ghcr.io/bermos/kitchen:0.12.0",
		Stores:          func(clickhouse.Config) DecisionStore { return fixtures.store },
		Attesters:       func([]byte, string) (ArtifactAttester, error) { return fixtures.attester, nil },
		EvidenceReaders: func([]byte, string) (EvidenceSetReader, error) { return fixtures.evidence, nil },
	}
	return fixtures
}

// rescanEnvironment is an environment running rescanRelease, with the release
// it moved off still in its history.
func rescanEnvironment(requirements *kitchenv1alpha1.EnvironmentRequirements) *kitchenv1alpha1.Environment {
	return &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: rescanEnvName, Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef:   kitchenv1alpha1.LocalObjectReference{Name: rescanProject},
			Type:         kitchenv1alpha1.EnvironmentProduction,
			ReleaseRef:   kitchenv1alpha1.LocalObjectReference{Name: rescanRelease},
			Requirements: requirements,
		},
		Status: kitchenv1alpha1.EnvironmentStatus{
			Phase: kitchenv1alpha1.EnvironmentLive,
			History: []kitchenv1alpha1.ReleaseHistoryEntry{{
				Release: rescanOldRel,
				From:    metav1.NewTime(time.Now().Add(-48 * time.Hour)),
				To:      metav1.NewTime(time.Now().Add(-24 * time.Hour)),
				Reason:  kitchenv1alpha1.ReleaseMovePromoted,
			}},
		},
	}
}

func (f *rescanFixtures) environment(t *testing.T) *kitchenv1alpha1.Environment {
	t.Helper()
	env := &kitchenv1alpha1.Environment{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: rescanEnvName}
	if err := f.client.Get(context.Background(), key, env); err != nil {
		t.Fatal(err)
	}
	return env
}

// finishScan marks the scanner Job complete and leaves the publisher's report
// on a pod, which is how the operator learns where the findings went.
// justNow is a scan finish time the sweep will read as recent.
//
// **The interval is measured against this, so it cannot be a fixed date.**
// A scan reported as finishing at a literal timestamp is a scan that recedes
// into the past as the calendar moves: a fixture that was "moments ago" when
// it was written silently becomes "yesterday", and every test that asserts a
// pair is *not* yet due starts failing on a commit nobody pushed. That is what
// happened to TestAPairIsNotRescannedBeforeItsIntervalIsUp — it passed until
// the wall clock drifted 24 hours past its hardcoded report and then failed on
// main with no change behind it.
//
// The one place a literal belongs is a test asserting the reported time is
// carried through verbatim, where the value is the subject rather than the
// clock; that one keeps its own constant.
func justNow() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func (f *rescanFixtures) finishScan(t *testing.T, report rescanReport) {
	t.Helper()
	f.jobStatus(t, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
	f.leaveReport(t, "publish", report)
}

// failScan marks the Job failed and leaves the fetch half's refusal on the
// init container, which is the shape of "this artifact has no SBOM".
func (f *rescanFixtures) failScan(t *testing.T, reason string) {
	t.Helper()
	f.jobStatus(t, batchv1.JobCondition{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "BackoffLimitExceeded",
	})
	f.leaveReport(t, "sbom", rescanReport{Error: reason})
}

func (f *rescanFixtures) jobStatus(t *testing.T, condition batchv1.JobCondition) {
	t.Helper()
	job := &batchv1.Job{}
	key := types.NamespacedName{
		Namespace: appNamespace(rescanProject),
		Name:      rescanJobName(rescanEnvName, rescanRelease),
	}
	if err := f.client.Get(context.Background(), key, job); err != nil {
		t.Fatalf("no scan job was created: %v", err)
	}
	job.Status.Conditions = []batchv1.JobCondition{condition}
	if err := f.client.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func (f *rescanFixtures) leaveReport(t *testing.T, container string, report rescanReport) {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	name := rescanJobName(rescanEnvName, rescanRelease)
	status := corev1.ContainerStatus{
		Name: container,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{Message: string(body)},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name + "-pod", Namespace: appNamespace(rescanProject),
			Labels: map[string]string{"job-name": name},
		},
	}
	if container == "publish" {
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{status}
	} else {
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{status}
	}
	ctx := context.Background()
	if err := f.client.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

func TestRescanScansTheBillOfMaterialsAndNeitherRebuildsNorRedeploys(t *testing.T) {
	f := newRescanFixtures(t, nil, rescanEnvironment(nil))
	ctx := context.Background()

	release := &kitchenv1alpha1.Release{}
	releaseKey := types.NamespacedName{Namespace: PlatformNamespace, Name: rescanRelease}
	if err := f.client.Get(ctx, releaseKey, release); err != nil {
		t.Fatal(err)
	}
	untouched := release.ResourceVersion

	if _, err := f.sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}

	job := &batchv1.Job{}
	key := types.NamespacedName{
		Namespace: appNamespace(rescanProject),
		Name:      rescanJobName(rescanEnvName, rescanRelease),
	}
	if err := f.client.Get(ctx, key, job); err != nil {
		t.Fatalf("no scan job was created: %v", err)
	}
	pod := job.Spec.Template.Spec

	// Three containers, in one order: fetch the bill of materials, match it,
	// carry the findings out. The artifact itself is never pulled, which is
	// what makes "no rebuild and no redeploy" literal rather than nearly true.
	if len(pod.InitContainers) != 2 {
		t.Fatalf("want the fetch and the scanner as init containers, got %+v", pod.InitContainers)
	}
	if got := pod.InitContainers[0].Command; len(got) != 2 || got[0] != "/rescan" || got[1] != "fetch" {
		t.Errorf("the bill of materials is not fetched first: %v", got)
	}
	if pod.InitContainers[1].Image != "anchore/grype:v0.87.0" {
		t.Errorf("the scanner is not the second init container: %+v", pod.InitContainers[1])
	}
	if len(pod.Containers) != 1 || pod.Containers[0].Image != "ghcr.io/bermos/kitchen:0.12.0" {
		t.Fatalf("the publisher is not the operator's own image: %+v", pod.Containers)
	}

	assertScannerIsGivenNothingButAFile(t, job)

	// Nothing was rebuilt and nothing was redeployed.
	if err := f.client.Get(ctx, releaseKey, release); err != nil {
		t.Fatal(err)
	}
	if release.ResourceVersion != untouched || release.Spec.Image != rescanImage {
		t.Error("the release was touched by a rescan")
	}
	env := f.environment(t)
	if env.Spec.ReleaseRef.Name != rescanRelease {
		t.Error("the environment was moved by a rescan")
	}
	if env.Status.Rescan == nil || env.Status.Rescan.Phase != kitchenv1alpha1.RescanScanning {
		t.Fatalf("the environment does not say a scan is in flight: %+v", env.Status.Rescan)
	}
	if env.Status.Rescan.StartedAt == nil {
		t.Error("the scan is not time-stamped")
	}
}

// assertScannerIsGivenNothingButAFile holds §7.3's contract up to the light:
// the scanner is an image somebody else wrote, and it gets a bill of
// materials, a place to write, and nothing that would let it touch anything.
func assertScannerIsGivenNothingButAFile(t *testing.T, job *batchv1.Job) {
	t.Helper()
	pod := job.Spec.Template.Spec
	environment := map[string]string{}
	for _, variable := range pod.InitContainers[1].Env {
		environment[variable.Name] = variable.Value
	}
	if environment["KITCHEN_SBOM"] == "" || environment["KITCHEN_FINDINGS"] == "" {
		t.Errorf("the scanner was not told what to read or where to write: %+v", environment)
	}
	if environment["KITCHEN_DATA_SNAPSHOT"] == "" {
		t.Error("the scanner has nowhere to name the database it matched against")
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the scanner pod was given a service account token")
	}
	security := pod.InitContainers[1].SecurityContext
	if security == nil || security.RunAsNonRoot == nil || !*security.RunAsNonRoot ||
		security.Capabilities == nil || len(security.Capabilities.Drop) != 1 {
		t.Errorf("the scanner is not unprivileged with every capability dropped: %+v", security)
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Error("a scan that failed to run will be retried; the retry is the next interval")
	}
}

func TestRescanSignsTheFindingsWithTheDatabaseSnapshotAndStoresTheDecision(t *testing.T) {
	f := newRescanFixtures(t, nil, rescanEnvironment(nil))
	ctx := context.Background()

	if _, err := f.sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	blob := "sha256:" + strings.Repeat("b", 64)
	f.attester.blobs[blob] = []byte(grypeReport)
	f.finishScan(t, rescanReport{
		Blob: blob,
		SBOM: "https://spdx.dev/Document", SBOMDigest: "sha256:" + strings.Repeat("c", 64),
		FinishedAt: "2026-08-24T03:16:11Z",
	})

	if _, err := f.sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}

	// The findings became evidence in the operator, under the reserved
	// predicate the default bundle already reads.
	if len(f.attester.predicates) != 1 ||
		f.attester.predicates[0] != attestation.PredicateVulnerabilityScan {
		t.Fatalf("the scan was not attested: %+v", f.attester.predicates)
	}
	predicate := attestedPredicate(t, f.attester)
	for _, forbidden := range []string{"pass", "passed", "verdict", "ok", "allowed"} {
		if _, present := predicate[forbidden]; present {
			t.Errorf("a scan emitted a verdict (%q); whether a finding is disqualifying is the "+
				"environment's question", forbidden)
		}
	}
	if predicate["dataSnapshot"] != grypeSnapshot {
		t.Errorf("the scan does not name the database it was produced against: %+v", predicate["dataSnapshot"])
	}
	if predicate["scannedAt"] != "2026-08-24T03:16:11Z" {
		t.Errorf("the findings are not time-stamped with the scan: %+v", predicate["scannedAt"])
	}
	findings, _ := predicate["findings"].([]any)
	if len(findings) != 2 {
		t.Fatalf("the normalized findings the rules judge are missing: %+v", predicate["findings"])
	}
	first, _ := findings[0].(map[string]any)
	if first["vulnerability"] == nil || first["severity"] == nil {
		t.Errorf("a finding does not carry what max-severity matches on: %+v", first)
	}
	// The scanner's own bytes are signed beside the platform's reading of them.
	if predicate["report"] == nil {
		t.Error("the scanner's own report was not carried verbatim")
	}

	// A re-evaluation is a decision, and a decision is a stored record.
	if len(f.store.decisions) != 1 {
		t.Fatalf("the rescan stored %d decisions, want 1", len(f.store.decisions))
	}
	decision := f.store.decisions[0]
	if decision.Kind != policy.KindRescan {
		t.Errorf("the decision was stored as %q", decision.Kind)
	}
	if decision.DataSnapshot != grypeSnapshot {
		t.Errorf("the snapshot is not stored with the decision: %q", decision.DataSnapshot)
	}
	if decision.Environment != rescanEnvName || decision.Release != rescanRelease {
		t.Errorf("the decision is not about the deployed pair: %+v", decision)
	}

	env := f.environment(t)
	state := env.Status.Rescan
	if state == nil || state.Phase != kitchenv1alpha1.RescanEvaluated {
		t.Fatalf("the environment does not say the scan was evaluated: %+v", state)
	}
	if state.FinishedAt == nil || state.DataSnapshot == "" || state.Findings != 2 {
		t.Errorf("the environment's answer is incomplete: %+v", state)
	}
	if state.DecisionID != decision.ID {
		t.Error("the environment does not lead back to the stored decision")
	}
	if state.Verdict != policy.VerdictAllowed {
		t.Errorf("an environment with no bar was judged %q", state.Verdict)
	}
}

// attestedPredicate decodes the predicate of the last statement the sweep
// signed, so a test can hold the claim up to the light.
func attestedPredicate(t *testing.T, attester *stubAttester) map[string]any {
	t.Helper()
	if len(attester.attached) == 0 {
		t.Fatal("nothing was attached")
	}
	envelope := attester.attached[len(attester.attached)-1]
	statement, err := envelope.Statement()
	if err != nil {
		t.Fatal(err)
	}
	predicate := map[string]any{}
	if err := json.Unmarshal(statement.Predicate, &predicate); err != nil {
		t.Fatal(err)
	}
	return predicate
}

// requiringProvenance is a bar the artifact does not clear: the registry will
// answer that it carries nothing.
func requiringProvenance() *kitchenv1alpha1.EnvironmentRequirements {
	return &kitchenv1alpha1.EnvironmentRequirements{
		BundleDigest: policy.Digest(policy.DefaultBundle()),
		Parameters:   map[string]string{"require-provenance": "true"},
	}
}

func rescanException(name string, expiresIn time.Duration, autoRollback bool) *kitchenv1alpha1.Exception {
	return &kitchenv1alpha1.Exception{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.ExceptionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: rescanProject},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: rescanEnvName},
			RuleIDs:        []string{"require-provenance"},
			Reason:         "hotfix for the checkout outage",
			RequestedBy:    "grace@example.com",
			ApprovedBy:     "heidi@example.com",
			ExpiresAt:      metav1.NewTime(time.Now().Add(expiresIn).Truncate(time.Second)),
			AutoRollback:   autoRollback,
		},
	}
}

// reliedOn is the register's record that a promotion of `release` into the
// environment actually went out under this grant, which is what an
// auto-rollback is allowed to act on. It returns the Promotion the status row
// names, because the sweep reads the object rather than trusting the name.
func reliedOn(
	exception *kitchenv1alpha1.Exception, promotion, release string,
) *kitchenv1alpha1.Promotion {
	exception.Status.UsedBy = append(exception.Status.UsedBy, promotion)
	return &kitchenv1alpha1.Promotion{
		ObjectMeta: metav1.ObjectMeta{Name: promotion, Namespace: PlatformNamespace},
		Spec: kitchenv1alpha1.PromotionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: rescanProject},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: rescanEnvName},
			ReleaseRef:     kitchenv1alpha1.LocalObjectReference{Name: release},
			RequestedBy:    "grace@example.com",
			Trigger:        kitchenv1alpha1.PromotionManual,
		},
	}
}

// scanThrough drives one pair from due to evaluated.
func (f *rescanFixtures) scanThrough(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	blob := "sha256:" + strings.Repeat("b", 64)
	f.attester.blobs[blob] = []byte(grypeReport)
	f.finishScan(t, rescanReport{Blob: blob, FinishedAt: justNow()})
	if _, err := f.sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAnExpiredExceptionStopsWaivingWithNoExpiryEngine(t *testing.T) {
	// Nothing tells the sweep an exception has expired. ActiveExceptionsFor
	// stops listing it, the rule fires unwaived, and the verdict is blocked —
	// which is the whole of "exception expiry is evaluated by this pass".
	live := newRescanFixtures(t, nil,
		rescanEnvironment(requiringProvenance()), rescanException("exc-live", time.Hour, false))
	live.evidence.set = attestation.EvidenceSet{Attestations: []attestation.Evidence{}}
	live.scanThrough(t)

	state := live.environment(t).Status.Rescan
	if state.Verdict != policy.VerdictAllowedWithException {
		t.Fatalf("an active grant did not carry the deployed release: %+v", state)
	}
	if len(state.UnmetRules) != 0 {
		t.Errorf("a waived rule was reported unmet: %+v", state.UnmetRules)
	}

	lapsed := newRescanFixtures(t, nil,
		rescanEnvironment(requiringProvenance()), rescanException("exc-lapsed", -time.Minute, false))
	lapsed.evidence.set = attestation.EvidenceSet{Attestations: []attestation.Evidence{}}
	lapsed.scanThrough(t)

	state = lapsed.environment(t).Status.Rescan
	if state.Verdict != policy.VerdictBlocked {
		t.Fatalf("an expired grant still waived: %+v", state)
	}
	if len(state.UnmetRules) != 1 || state.UnmetRules[0] != "require-provenance" {
		t.Errorf("the rules that fire unwaived are not named: %+v", state.UnmetRules)
	}
	// Nothing was rolled back: this grant did not ask for it.
	if lapsed.environment(t).Spec.ReleaseRef.Name != rescanRelease {
		t.Error("an expiry yanked a running workload nobody asked it to")
	}
}

func TestAnExpiredExceptionThatAskedForARollbackGetsOne(t *testing.T) {
	exception := rescanException("exc-rollback", -time.Minute, true)
	promotion := reliedOn(exception, "shop-prm-1", rescanRelease)
	f := newRescanFixtures(t, nil, rescanEnvironment(requiringProvenance()), exception, promotion)
	f.evidence.set = attestation.EvidenceSet{Attestations: []attestation.Evidence{}}
	f.scanThrough(t)

	env := f.environment(t)
	if env.Spec.ReleaseRef.Name != rescanOldRel {
		t.Fatalf("the environment did not retreat to the release before it: %q", env.Spec.ReleaseRef.Name)
	}
	if len(env.Status.History) == 0 || env.Status.History[0].Release != rescanRelease ||
		env.Status.History[0].Reason != kitchenv1alpha1.ReleaseMoveRolledBack {
		t.Errorf("the retreat is not in the history as a rollback: %+v", env.Status.History)
	}
	// The answer went with the artifact that stopped running: carrying it
	// forward would report a scan of something that was never scanned.
	if env.Status.Rescan != nil {
		t.Errorf("the rescan state survived a release move: %+v", env.Status.Rescan)
	}
	// The decision still stands: the rollback is a consequence of it, never a
	// substitute for recording it.
	if len(f.store.decisions) != 1 || f.store.decisions[0].Verdict != policy.VerdictBlocked {
		t.Errorf("the blocked re-evaluation was not recorded: %+v", f.store.decisions)
	}
}

func TestALongDeadGrantDoesNotYankAReleaseItNeverLetOut(t *testing.T) {
	// March: a 24-hour waiver on production to ship a hotfix, autoRollback on,
	// naming no release. Nobody ever resolved it, and the register keeps it —
	// Covers() answers true for any release and EffectivePhase() answers
	// Expired forever. August: an unrelated release is deployed and a rescan
	// blocks it on the same rule id, for a CVE nobody involved has heard of.
	//
	// Rolling production back here would be exactly the failure the narrowness
	// exists to prevent: a hotfix grant turned into a mechanism that yanks a
	// running workload for an unrelated reason.
	stale := rescanException("exc-march", -5*30*24*time.Hour, true)
	promotion := reliedOn(stale, "shop-prm-march", rescanOldRel)
	f := newRescanFixtures(t, nil, rescanEnvironment(requiringProvenance()), stale, promotion)
	f.evidence.set = attestation.EvidenceSet{Attestations: []attestation.Evidence{}}
	f.scanThrough(t)

	env := f.environment(t)
	if env.Spec.ReleaseRef.Name != rescanRelease {
		t.Fatalf("a five-month-old grant this release never went out under rolled it back to %q",
			env.Spec.ReleaseRef.Name)
	}
	// The non-compliance is still recorded and still visible — the rollback is
	// what is refused, never the finding.
	state := env.Status.Rescan
	if state == nil || state.Verdict != policy.VerdictBlocked {
		t.Fatalf("the blocked re-evaluation was not recorded on the environment: %+v", state)
	}
	if len(f.store.decisions) != 1 || f.store.decisions[0].Verdict != policy.VerdictBlocked {
		t.Errorf("the blocked re-evaluation was not stored: %+v", f.store.decisions)
	}
}

func TestAGrantWhoseUseNothingRecordsRollsNothingBack(t *testing.T) {
	// The same shape with the link missing altogether: an expired grant that
	// asked for a rollback, covering this pair, waiving the rule that is
	// firing — and no promotion in status.usedBy. An unmatchable record is not
	// a link, and a workload left running is the recoverable direction.
	orphan := rescanException("exc-unused", -time.Minute, true)
	f := newRescanFixtures(t, nil, rescanEnvironment(requiringProvenance()), orphan)
	f.evidence.set = attestation.EvidenceSet{Attestations: []attestation.Evidence{}}
	f.scanThrough(t)

	if env := f.environment(t); env.Spec.ReleaseRef.Name != rescanRelease {
		t.Fatalf("a grant nothing recorded a use of rolled the environment back to %q",
			env.Spec.ReleaseRef.Name)
	}
}

func TestARescanThatCouldNotRunIsFailedRatherThanClean(t *testing.T) {
	// An artifact with no bill of materials cannot be rescanned, and the
	// difference between "nothing was found" and "nothing was looked at" is
	// the difference a compliance system dies of.
	f := newRescanFixtures(t, nil, rescanEnvironment(nil))
	ctx := context.Background()

	if _, err := f.sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	f.failScan(t, "the artifact carries no bill of materials attestation")

	if _, err := f.sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	state := f.environment(t).Status.Rescan
	if state == nil || state.Phase != kitchenv1alpha1.RescanFailed {
		t.Fatalf("a scan that never ran is not Failed: %+v", state)
	}
	if !strings.Contains(state.Message, "bill of materials") {
		t.Errorf("the reason the scan did not run was lost: %q", state.Message)
	}
	if len(f.store.decisions) != 0 {
		t.Error("a scan that did not happen produced a decision about the artifact")
	}
	if len(f.attester.predicates) != 0 {
		t.Error("a scan that did not happen was attested")
	}
}

func TestTheSweepIsInertAndSaysSoWhenNothingIsConfigured(t *testing.T) {
	for _, spec := range []struct {
		name  string
		tweak func(*kitchenv1alpha1.Kitchen)
		want  string
	}{
		{"off", func(k *kitchenv1alpha1.Kitchen) {
			k.Spec.Compliance.Rescan.Enabled = false
		}, "is off"},
		{"no scanner", func(k *kitchenv1alpha1.Kitchen) {
			k.Spec.Compliance.Rescan.Scanner = nil
		}, "no scanner is configured"},
		{"nothing to sign with", func(k *kitchenv1alpha1.Kitchen) {
			k.Spec.Compliance.Attestation.Enabled = false
		}, "could not be signed"},
	} {
		t.Run(spec.name, func(t *testing.T) {
			f := newRescanFixtures(t, spec.tweak, rescanEnvironment(nil))
			report, err := f.sweeper.SweepOnce(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if report.Running {
				t.Fatal("the pass reported itself running with nothing to run")
			}
			if !strings.Contains(report.Message, spec.want) {
				t.Errorf("the pass did not say why it is inert: %q", report.Message)
			}
			// Said on the singleton too: the failure mode of evidence is
			// silence, so the platform owns up on its own object.
			kitchen := &kitchenv1alpha1.Kitchen{}
			if err := f.client.Get(context.Background(),
				types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
				t.Fatal(err)
			}
			status := kitchen.Status.Compliance
			if status == nil || status.Rescan == nil || status.Rescan.Running {
				t.Fatalf("the singleton does not report an inert pass: %+v", status)
			}
			job := &batchv1.Job{}
			key := types.NamespacedName{
				Namespace: appNamespace(rescanProject),
				Name:      rescanJobName(rescanEnvName, rescanRelease),
			}
			if err := f.client.Get(context.Background(), key, job); err == nil {
				t.Error("an inert pass started a scan anyway")
			}
		})
	}
}

func TestTheSweepDoesNotStampedeAnEstateThatIsAllDueAtOnce(t *testing.T) {
	// Every environment is due on the first pass after an upgrade. The budget
	// is what stops that being one image pull per environment at the same
	// instant, and a per-object requeue would have no vantage point to count
	// from.
	extra := []client.Object{rescanEnvironment(nil)}
	for _, name := range []string{"shop-staging", "shop-canary", "shop-edge"} {
		env := rescanEnvironment(nil)
		env.Name = name
		extra = append(extra, env)
	}
	f := newRescanFixtures(t, func(k *kitchenv1alpha1.Kitchen) {
		k.Spec.Compliance.Rescan.Concurrency = 2
	}, extra...)

	report, err := f.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Environments != 4 {
		t.Fatalf("the pass considered %d pairs, want 4", report.Environments)
	}
	if report.Started != 2 {
		t.Errorf("the pass started %d scans against a budget of 2", report.Started)
	}
	jobs := &batchv1.JobList{}
	if err := f.client.List(context.Background(), jobs,
		client.InNamespace(appNamespace(rescanProject))); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 2 {
		t.Errorf("%d scanner pods went out at once", len(jobs.Items))
	}
}

func TestAPairIsNotRescannedBeforeItsIntervalIsUp(t *testing.T) {
	f := newRescanFixtures(t, func(k *kitchenv1alpha1.Kitchen) {
		k.Spec.Compliance.Rescan.Interval = metav1.Duration{Duration: 24 * time.Hour}
	}, rescanEnvironment(nil))
	f.scanThrough(t)

	// The scan finished; the Job is left behind for its TTL. A second pass
	// inside the interval starts nothing, because the interval is counted from
	// this pair's own last finished scan rather than from a platform-wide tick.
	if err := f.client.Delete(context.Background(), &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Namespace: appNamespace(rescanProject),
		Name:      rescanJobName(rescanEnvName, rescanRelease),
	}}); err != nil {
		t.Fatal(err)
	}
	report, err := f.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Started != 0 {
		t.Fatalf("a pair scanned moments ago was scanned again: %+v", report)
	}

	// Move the clock past the interval and it is due again.
	f.sweeper.Now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	report, err = f.sweeper.SweepOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Started != 1 {
		t.Errorf("a pair past its interval was not rescanned: %+v", report)
	}
}

func TestAFinishedJobIsNotRescannedAgainst(t *testing.T) {
	// The Job's name is deterministic on the pair and its TTL is the same hour
	// as the interval floor, so a pair can come due again in the moment before
	// the TTL controller has collected the last scan's Job. Adopting it would
	// stamp the pair Scanning against a scan that is over, read the old pod's
	// termination message, and re-sign yesterday's findings under yesterday's
	// snapshot — and, because scannedAt comes off the report, stamp the result
	// with the old scan's time and be immediately due again.
	f := newRescanFixtures(t, func(k *kitchenv1alpha1.Kitchen) {
		k.Spec.Compliance.Rescan.Interval = metav1.Duration{Duration: 24 * time.Hour}
	}, rescanEnvironment(nil))
	ctx := context.Background()
	f.scanThrough(t)
	published := len(f.store.decisions)

	f.sweeper.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	report, err := f.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Started != 0 || report.Scanning != 0 {
		t.Fatalf("the finished job was adopted as an in-flight scan: %+v", report)
	}
	if state := f.environment(t).Status.Rescan; state.Phase == kitchenv1alpha1.RescanScanning {
		t.Errorf("the pair is scanning against a job that already finished: %+v", state)
	}
	if len(f.store.decisions) != published {
		t.Errorf("a stale scan was published again: %d decisions, was %d",
			len(f.store.decisions), published)
	}
	key := types.NamespacedName{
		Namespace: appNamespace(rescanProject),
		Name:      rescanJobName(rescanEnvName, rescanRelease),
	}
	if err := f.client.Get(ctx, key, &batchv1.Job{}); err == nil {
		t.Error("the previous scan's job was left where the next step will find it again")
	}

	// With it gone the pair starts cleanly, which is what makes this a delay
	// of one step rather than a pair that never scans again.
	report, err = f.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Started != 1 {
		t.Errorf("the pair did not start once the stale job was gone: %+v", report)
	}
}

func TestAPairStuckInPublishIsStillCountedAsScanning(t *testing.T) {
	// The budget pre-pass counts a pair by inFlight(); the report has to count
	// it the same way. A publish that fails before it records — an unreadable
	// registry credential, a store that is down — leaves the pair Scanning and
	// holding its slot, and a singleton reporting `running: true,
	// environments: 1, scanning: 0` would describe a sweep that is idle and
	// healthy while it is neither.
	f := newRescanFixtures(t, nil, rescanEnvironment(nil))
	ctx := context.Background()

	if _, err := f.sweeper.SweepOnce(ctx); err != nil {
		t.Fatal(err)
	}
	blob := "sha256:" + strings.Repeat("b", 64)
	f.attester.blobs[blob] = []byte(grypeReport)
	f.finishScan(t, rescanReport{Blob: blob, FinishedAt: justNow()})

	f.sweeper.Attesters = func([]byte, string) (ArtifactAttester, error) {
		return nil, errors.New("the registry connection's credential could not be read")
	}
	report, err := f.sweeper.SweepOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scanning != 1 {
		t.Fatalf("a pair holding a concurrency slot was reported as %d scanning: %+v",
			report.Scanning, report)
	}
	if report.Evaluated != 0 {
		t.Errorf("a publish that failed was counted as an evaluation: %+v", report)
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := f.client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	if status := kitchen.Status.Compliance.Rescan; status.Scanning != 1 {
		t.Errorf("the singleton reports %d scanning while the budget is spent: %+v",
			status.Scanning, status)
	}
}
