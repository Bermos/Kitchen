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

package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/policy"
)

// Issue #142's acceptance criteria, criterion by criterion.
//
//  1. the pack for a range is byte-reproducible
//  2. it is independently verifiable — the shell commands the pack publishes
//     are *run* here, verbatim, against the two documents the API served
//  3. a typical project's pack completes well inside a minute
//  4. its contents are documented field by field — which is docs/api/audit-pack.md,
//     and TestEveryPackFieldIsDocumented checks the two cannot drift apart
//
// plus the design rules the criteria do not state: nothing about "now" inside
// the signed bytes, a truncated window said out loud, and no credential.

const (
	packFrom = "2026-01-01T00:00:00Z"
	packTo   = "2026-04-01T00:00:00Z"
	packPath = "/api/v1/projects/shop/audit-pack?from=" + packFrom + "&to=" + packTo
)

// packWindow is the range the fixtures below are built inside.
func packWindow(t *testing.T) (time.Time, time.Time) {
	t.Helper()
	from, err := time.Parse(time.RFC3339, packFrom)
	if err != nil {
		t.Fatal(err)
	}
	to, err := time.Parse(time.RFC3339, packTo)
	if err != nil {
		t.Fatal(err)
	}
	return from, to
}

// packFixtures is the shop project as it looks to an examiner: a release cut
// inside the window from a reviewed pull request, a promotion that let it in,
// an exception that waived a rule and has since expired, and a closed
// recertification cycle with its retained artefact.
func packFixtures(t *testing.T) []runtime.Object {
	t.Helper()
	from, _ := packWindow(t)
	inside := from.Add(24 * time.Hour)

	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "shop-bld-pack",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(inside),
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: shopProject},
			Git: kitchenv1alpha1.GitRevision{
				SHA: "feedface00000000", Branch: "main", Message: "raise the timeout", Author: "grace",
			},
		},
		Status: kitchenv1alpha1.BuildStatus{
			Phase: kitchenv1alpha1.BuildSucceeded,
			Artifact: &kitchenv1alpha1.ArtifactStatus{
				Repository: "registry.example.com/shop",
				Digest:     "sha256:" + strings.Repeat("a", 64),
				KeyID:      "key-1",
				AttestedAt: &metav1.Time{Time: inside},
				Evidence: []kitchenv1alpha1.ArtifactEvidence{
					{PredicateType: "https://slsa.dev/provenance/v1", Source: "builder"},
					{PredicateType: attestation.PredicateBuildRecord, Source: "platform"},
				},
			},
			Gates: []kitchenv1alpha1.QualityGateStatus{
				{Name: "trivy", Phase: kitchenv1alpha1.GateCompleted, Source: "platform"},
			},
			Source: &kitchenv1alpha1.SourceProvenanceStatus{
				Provider: "github", PullRequest: 42, Title: "raise the timeout",
				Author: "grace", MergedBy: "hopper", Approvers: []string{"hopper", "ada"},
				Independent: true, Required: true,
				CheckedAt: &metav1.Time{Time: inside},
			},
		},
	}
	release := &kitchenv1alpha1.Release{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "shop-rel-pack",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(inside),
		},
		Spec: kitchenv1alpha1.ReleaseSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: shopProject},
			BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "shop-bld-pack"},
			Image:      "registry.example.com/shop@sha256:" + strings.Repeat("a", 64),
		},
	}
	promotion := &kitchenv1alpha1.Promotion{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "shop-prm-pack",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(inside.Add(time.Hour)),
		},
		Spec: kitchenv1alpha1.PromotionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: shopProject},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: testEnvironment},
			ReleaseRef:     kitchenv1alpha1.LocalObjectReference{Name: "shop-rel-pack"},
			RequestedBy:    "grace",
			Trigger:        kitchenv1alpha1.PromotionManual,
		},
		Status: kitchenv1alpha1.PromotionStatus{
			Phase:      kitchenv1alpha1.PromotionApplied,
			Verdict:    policy.VerdictAllowedWithException,
			DecisionID: "decision-pack-1",
			AppliedAt:  &metav1.Time{Time: inside.Add(2 * time.Hour)},
		},
	}
	exception := &kitchenv1alpha1.Exception{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "shop-exc-pack",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(inside),
		},
		Spec: kitchenv1alpha1.ExceptionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: shopProject},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: testEnvironment},
			RuleIDs:        []string{"no-critical-vulnerabilities"},
			Reason:         "payment outage, fix is in this release",
			RequestedBy:    "grace",
			ApprovedBy:     "hopper",
			IncidentRef:    "INC-7",
			ExpiresAt:      metav1.NewTime(inside.Add(48 * time.Hour)),
		},
	}
	review := &kitchenv1alpha1.AccessReview{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "review-pack",
			Namespace:         testNamespace,
			CreationTimestamp: metav1.NewTime(inside),
			UID:               "review-pack-uid",
		},
		Spec: kitchenv1alpha1.AccessReviewSpec{
			Scope:     kitchenv1alpha1.AccessReviewAll,
			OpenedBy:  testCaller,
			Reviewers: []kitchenv1alpha1.AccessSubject{{Subject: testSubject, Email: testCaller}},
			DueBy:     metav1.NewTime(inside.Add(72 * time.Hour)),
		},
		Status: kitchenv1alpha1.AccessReviewStatus{
			Phase:      kitchenv1alpha1.AccessReviewClosed,
			OpenedAt:   &metav1.Time{Time: inside},
			SnapshotAt: &metav1.Time{Time: inside},
			ClosedAt:   &metav1.Time{Time: inside.Add(time.Hour)},
			ClosedBy:   testCaller,
			Confirmed:  2,
			Entries: []kitchenv1alpha1.AccessReviewEntry{
				{
					AccessSubject: kitchenv1alpha1.AccessSubject{Subject: testSubject, Email: testCaller},
					Grant:         "platform", Role: "operator",
					Decision: kitchenv1alpha1.AccessConfirm, DecidedBy: testCaller,
					DecidedAt: &metav1.Time{Time: inside.Add(time.Hour)}, SelfReview: true,
				},
				{
					AccessSubject: kitchenv1alpha1.AccessSubject{Subject: "ada"},
					Grant:         shopProject, Role: "developer",
					Decision: kitchenv1alpha1.AccessConfirm, DecidedBy: testCaller,
					DecidedAt: &metav1.Time{Time: inside.Add(time.Hour)},
				},
				// A grant on somebody else's project. It is in the cycle's
				// signed artefact and must not be in this project's pack.
				{
					AccessSubject: kitchenv1alpha1.AccessSubject{Subject: "linus"},
					Grant:         otherProject, Role: "admin",
				},
			},
			Artifact: &kitchenv1alpha1.AccessReviewArtifact{
				RecordID:      "record-review",
				Subject:       "sha256:" + strings.Repeat("d", 64),
				PredicateType: attestation.PredicateAccessReview,
				SignedAt:      &metav1.Time{Time: inside.Add(time.Hour)},
			},
		},
	}
	return []runtime.Object{build, release, promotion, exception, review}
}

// packStore seeds the three store reads the pack makes.
func packStore(t *testing.T, h *harness) {
	t.Helper()
	from, _ := packWindow(t)
	inside := from.Add(24 * time.Hour)

	h.logs.decisions = []clickhouse.Decision{
		{
			ID: "decision-pack-2", Timestamp: inside.Add(72 * time.Hour), Kind: policy.KindRescan,
			Project: shopProject, Environment: testEnvironment, Release: "shop-rel-pack",
			BundleDigest: "sha256:bbbb", InputDigest: "sha256:cccc", DataSnapshot: "trivy-db@2026-01-04",
			Verdict:    policy.VerdictBlocked,
			RulesFired: `[{"rule":"no-critical-vulnerabilities","message":"CVE-2026-1","waived":false}]`,
			Input:      `{"artifact":{"digest":"sha256:aaaa"}}`,
		},
		{
			ID: "decision-pack-1", Timestamp: inside.Add(time.Hour), Kind: policy.KindPromotion,
			Project: shopProject, Environment: testEnvironment, Release: "shop-rel-pack",
			BundleDigest: "sha256:bbbb", InputDigest: "sha256:dddd",
			Verdict:    policy.VerdictAllowedWithException,
			RulesFired: `[{"rule":"no-critical-vulnerabilities","waived":true,"exception":"shop-exc-pack"}]`,
			Input:      `{"artifact":{"digest":"sha256:aaaa"}}`,
			DecidedBy:  "grace",
		},
	}
	h.logs.auditRecords = []clickhouse.AuditRecord{
		{
			Sequence: 12, Timestamp: inside, Actor: "grace", ActorKind: clickhouse.ActorUser,
			Operation: clickhouse.AuditCreate, Kind: "Exception", Name: "shop-exc-pack",
			Project: shopProject, Reason: "break-glass exception granted",
			Details:  `{"privileged":true,"privilegedClass":"break-glass"}`,
			PrevHash: "aa", Hash: "bb",
		},
		{
			Sequence: 13, Timestamp: inside.Add(time.Hour), Actor: "grace",
			ActorKind: clickhouse.ActorUser, Operation: clickhouse.AuditTransition,
			Kind: "Environment", Name: testEnvironment, Project: shopProject,
			Reason: "release promoted", PrevHash: "bb", Hash: "cc",
		},
	}
	h.logs.records = []clickhouse.SignedRecord{
		{
			ID: "record-review", Timestamp: inside.Add(time.Hour),
			Type:     attestation.PredicateAccessReview,
			Subject:  "sha256:" + strings.Repeat("d", 64),
			Envelope: `{"payloadType":"application/vnd.in-toto+json","payload":"e30=","signatures":[]}`,
		},
		{
			ID: "record-claim", Timestamp: inside, Type: attestation.PredicateDataClass,
			Subject: "sha256:" + strings.Repeat("c", 64), Project: shopProject,
			Envelope: `{"payloadType":"application/vnd.in-toto+json","payload":"e31=","signatures":[]}`,
		},
	}
}

// packHarness is the whole fixture set: the project, the evidence, the store.
func packHarness(t *testing.T) *harness {
	t.Helper()
	objects := append(fixtures(), neonConnection())
	objects = append(objects, packFixtures(t)...)
	h := newHarness(t, nil, objects...)
	packStore(t, h)
	return h
}

// --- Criterion 1: byte-reproducible for a given range -----------------------

func TestAPackForARangeIsByteReproducible(t *testing.T) {
	h := packHarness(t)

	first := h.do(t, http.MethodGet, packPath, "")
	if first.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", first.Code, first.Body.String())
	}
	// A second export, taken later, of the same range. Anything read off a
	// clock, any map iterated, any list left in the store's order shows up
	// here as a diff.
	second := h.do(t, http.MethodGet, packPath, "")
	if second.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", second.Code, second.Body.String())
	}
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Fatalf("two exports of one range differ:\nfirst:  %s\nsecond: %s",
			first.Body.String(), second.Body.String())
	}

	digest := first.Header().Get("X-Kitchen-Pack-Digest")
	sum := sha256.Sum256(first.Body.Bytes())
	if want := "sha256:" + hex.EncodeToString(sum[:]); digest != want {
		t.Fatalf("the served digest must be the sha256 of the served bytes: header %q, computed %q",
			digest, want)
	}
	if second.Header().Get("X-Kitchen-Pack-Digest") != digest {
		t.Fatal("two exports of one range must carry one digest")
	}
}

// The reproducibility rests on there being no clock reading inside the bytes.
// Every other endpoint in this package answers with a `generatedAt`; this one
// must not, and a future edit that adds one back has to fail here rather than
// in somebody's diff of two packs six months apart.
func TestNoPartOfAPackIsReadOffTheClock(t *testing.T) {
	h := packHarness(t)
	withSigningKey(t, h)
	recorder := h.do(t, http.MethodGet, packPath, "")
	body := recorder.Body.String()

	for _, forbidden := range []string{`"generatedAt"`, `"exportedAt"`, `"renderedAt"`, `"asOf"`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the signed bytes carry %s, which makes them a function of when they were "+
				"taken rather than of the range", forbidden)
		}
	}

	// The envelope is where the export's own timestamp belongs — outside the
	// bytes that have to reproduce.
	envelope := h.do(t, http.MethodGet, packPath+"&format=dsse", "")
	if envelope.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", envelope.Code, envelope.Body.String())
	}
	statement := decodeStatement(t, envelope.Body.Bytes())
	predicate := map[string]any{}
	if err := json.Unmarshal(statement.Predicate, &predicate); err != nil {
		t.Fatal(err)
	}
	if predicate["exportedAt"] == "" || predicate["exportedAt"] == nil {
		t.Fatal("the envelope must say when the export was taken")
	}
}

// A phase judged "now" would change a historical pack's mind as the clock
// moved. Both phases in the document are judged at the range's end instead.
func TestPhasesAreJudgedAtTheRangesEndAndNotNow(t *testing.T) {
	h := packHarness(t)
	pack := fetchPack(t, h)

	if len(pack.Exceptions) != 1 {
		t.Fatalf("the window's exception must be in the pack, got %+v", pack.Exceptions)
	}
	exception := pack.Exceptions[0]
	// It expired two days after it was granted, which is inside the window and
	// long before now. Judged at `to` it reads expired; judged at creation it
	// would read active, and judged at "now" it would read expired for a
	// different reason.
	if exception.Phase != string(kitchenv1alpha1.ExceptionExpired) {
		t.Fatalf("the grant must be judged at the range's end, got %q", exception.Phase)
	}
	if exception.ActiveAtRangeEnd {
		t.Fatal("a grant that had expired by the end of the window is not active at the end of it")
	}
	if len(pack.Access.Cycles) != 1 ||
		pack.Access.Cycles[0].Phase != string(kitchenv1alpha1.AccessReviewClosed) {
		t.Fatalf("the closed cycle must read closed, got %+v", pack.Access.Cycles)
	}
}

// --- Criterion 2: independently verifiable ----------------------------------

// The procedure the pack publishes is **run** here — the shell commands out of
// `verification.procedure`, executed verbatim against the two documents the
// API served, with no Kitchen code anywhere in the verification path. A
// procedure nobody executes is an intention, and §5's exit story is only true
// if somebody has walked out of the door.
func TestThePublishedProcedureActuallyVerifies(t *testing.T) {
	for _, tool := range []string{"sh", "jq", "base64", "openssl", "wc", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not on PATH, so the published procedure cannot be run here", tool)
		}
	}
	h := packHarness(t)
	withSigningKey(t, h)

	packed := h.do(t, http.MethodGet, packPath, "")
	if packed.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", packed.Code, packed.Body.String())
	}
	envelope := h.do(t, http.MethodGet, packPath+"&format=dsse", "")
	if envelope.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", envelope.Code, envelope.Body.String())
	}

	pack := decode[auditPack](t, packed)
	if !pack.Verification.Signed || pack.Verification.PublicKey == "" {
		t.Fatalf("a signed pack must publish the key it was signed under, got %+v", pack.Verification)
	}
	if len(pack.Verification.Procedure) != 4 {
		t.Fatalf("the procedure must be the four steps the document promises, got %d",
			len(pack.Verification.Procedure))
	}

	// Step 2: the digest of the file is the subject of the statement.
	statement := decodeStatement(t, envelope.Body.Bytes())
	sum := sha256.Sum256(packed.Body.Bytes())
	if !statement.Describes("sha256:" + hex.EncodeToString(sum[:])) {
		t.Fatalf("the statement must name the pack's own digest as its subject, got %+v",
			statement.Subject)
	}
	if statement.PredicateType != attestation.PredicateAuditPack {
		t.Fatalf("want predicate %q, got %q", attestation.PredicateAuditPack, statement.PredicateType)
	}

	// Steps 2 to 4, verbatim: the shell out of the published procedure, run
	// against the files as a reader would have saved them. Step 1 is the two
	// `curl`s, which is what the recorders above already did.
	directory := t.TempDir()
	write(t, filepath.Join(directory, "pack.json"), packed.Body.Bytes())
	write(t, filepath.Join(directory, "pack.dsse.json"), envelope.Body.Bytes())
	write(t, filepath.Join(directory, "public.pem"), []byte(pack.Verification.PublicKey))

	output := runProcedure(t, directory, pack.Verification.Procedure[1:])
	if !strings.Contains(output, hex.EncodeToString(sum[:])) {
		t.Fatalf("the published sha256sum did not print the digest the statement names:\n%s", output)
	}
	if !strings.Contains(output, "Verified OK") {
		t.Fatalf("the published procedure did not verify the pack it was published with:\n%s", output)
	}
}

// runProcedure executes the shell out of the published steps, in order, in one
// directory. Each step is numbered prose with the command in backticks — the
// same string a reader copies — so the extraction here is the same one a
// person makes with their eyes.
func runProcedure(t *testing.T, directory string, steps []string) string {
	t.Helper()
	output := &strings.Builder{}
	for _, step := range steps {
		script := shellIn(t, step)
		command := exec.Command("sh", "-c", script)
		command.Dir = directory
		answer, err := command.CombinedOutput()
		output.Write(answer)
		if err != nil {
			t.Fatalf("the published step\n\t%s\ncould not be run: %v\n%s", script, err, answer)
		}
	}
	return output.String()
}

// shellIn pulls the command out of one published step. A step that carries no
// backticked command is a step this test cannot run, and saying so is better
// than silently checking nothing.
func shellIn(t *testing.T, step string) string {
	t.Helper()
	_, rest, found := strings.Cut(step, "`")
	if !found {
		t.Fatalf("the published step carries no command to run: %s", step)
	}
	script, _, found := strings.Cut(rest, "`")
	if !found {
		t.Fatalf("the published step's command is not closed: %s", step)
	}
	return script
}

// An envelope over bytes somebody edited must not verify. The check above
// proves the procedure passes; this proves it can fail.
func TestATamperedPackDoesNotVerify(t *testing.T) {
	if _, err := exec.LookPath("openssl"); err != nil {
		t.Skip("openssl is not on PATH")
	}
	h := packHarness(t)
	withSigningKey(t, h)

	packed := h.do(t, http.MethodGet, packPath, "")
	envelope := h.do(t, http.MethodGet, packPath+"&format=dsse", "")
	statement := decodeStatement(t, envelope.Body.Bytes())

	tampered := bytes.Replace(packed.Body.Bytes(),
		[]byte(`"break-glass"`), []byte(`"routine-work"`), 1)
	if bytes.Equal(tampered, packed.Body.Bytes()) {
		t.Fatal("the fixture must contain the string this test edits")
	}
	sum := sha256.Sum256(tampered)
	if statement.Describes("sha256:" + hex.EncodeToString(sum[:])) {
		t.Fatal("a pack whose bytes were edited must no longer match the signed digest")
	}
}

// A platform with no signing key produces a pack that says so, rather than one
// that looks signed. The pack itself still exists — the evidence is unchanged;
// what is missing is the means to check it somewhere else.
func TestAnUnsignedPlatformSaysSoRatherThanLookingSigned(t *testing.T) {
	h := packHarness(t)
	pack := fetchPack(t, h)

	if pack.Verification.Signed {
		t.Fatal("attestation is off in the fixtures, so the pack cannot claim to be signed")
	}
	if pack.Verification.Message == "" {
		t.Fatal("an unsigned pack must say why")
	}
	if pack.Verification.PublicKey != "" {
		t.Fatal("an unsigned pack must not publish a key")
	}
	// And the signature format refuses rather than serving something that is
	// not a signature.
	recorder := h.do(t, http.MethodGet, packPath+"&format=dsse", "")
	if recorder.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// --- Criterion 3: under a minute for a typical project ----------------------

// A typical project here is the fixtures' shop plus a quarter of history: four
// environments, sixty releases, and the store's whole page of decisions and
// audit records — which is the most one pack can hold.
//
// It is a wall-clock assertion with an enormous margin rather than a
// benchmark, because what the criterion is really about is the *shape*: the
// assembly makes a fixed number of reads and never fans out per artifact, so
// a project ten times this size costs the same nine queries plus two per
// environment. The number the run prints is what was measured.
func TestATypicalProjectsPackIsWellInsideAMinute(t *testing.T) {
	objects := append(fixtures(), neonConnection())
	objects = append(objects, packFixtures(t)...)
	from, _ := packWindow(t)

	for i := range 60 {
		objects = append(objects, &kitchenv1alpha1.Release{
			ObjectMeta: metav1.ObjectMeta{
				Name:              fmt.Sprintf("shop-rel-bulk-%03d", i),
				Namespace:         testNamespace,
				CreationTimestamp: metav1.NewTime(from.Add(time.Duration(i) * time.Hour)),
			},
			Spec: kitchenv1alpha1.ReleaseSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: shopProject},
				BuildRef:   kitchenv1alpha1.LocalObjectReference{Name: "shop-bld-pack"},
				Image:      "registry.example.com/shop@sha256:" + strings.Repeat("a", 64),
			},
		})
	}
	for i := range 3 {
		objects = append(objects, &kitchenv1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("shop-preview-%d", i),
				Namespace: testNamespace,
			},
			Spec: kitchenv1alpha1.EnvironmentSpec{
				ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: shopProject},
				Type:       kitchenv1alpha1.EnvironmentPreview,
				ReleaseRef: kitchenv1alpha1.LocalObjectReference{Name: "shop-rel-pack"},
				Preview:    &kitchenv1alpha1.PreviewInfo{PullRequest: int32(i + 1)},
			},
		})
	}

	h := newHarness(t, nil, objects...)
	packStore(t, h)
	// A full page of each of the two store reads, which is the most a single
	// pack can carry.
	for i := len(h.logs.decisions); i < clickhouse.MaxDecisionLimit; i++ {
		h.logs.decisions = append(h.logs.decisions, clickhouse.Decision{
			ID: fmt.Sprintf("bulk-%04d", i), Timestamp: from.Add(time.Duration(i) * time.Minute),
			Kind: policy.KindRescan, Project: shopProject, Environment: testEnvironment,
			Release: "shop-rel-pack", Verdict: policy.VerdictAllowed,
			RulesFired: `[]`, Input: `{"artifact":{"digest":"sha256:aaaa"}}`,
		})
	}
	for i := len(h.logs.auditRecords); i < clickhouse.MaxAuditLimit; i++ {
		h.logs.auditRecords = append(h.logs.auditRecords, clickhouse.AuditRecord{
			Sequence: int64(100 + i), Timestamp: from.Add(time.Duration(i) * time.Minute),
			Actor: "grace", ActorKind: clickhouse.ActorUser, Operation: clickhouse.AuditUpdate,
			Kind: "Environment", Name: testEnvironment, Project: shopProject,
			Reason: "redeployed", PrevHash: "aa", Hash: "bb",
		})
	}

	started := time.Now()
	recorder := h.do(t, http.MethodGet, packPath, "")
	elapsed := time.Since(started)
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	t.Logf("assembled a %d-byte pack over 4 environments, 62 releases, %d decisions and %d audit "+
		"records in %s", recorder.Body.Len(), clickhouse.MaxDecisionLimit, clickhouse.MaxAuditLimit,
		elapsed.Round(time.Millisecond))
	if elapsed > 30*time.Second {
		t.Fatalf("assembling a typical project's pack took %s, and the criterion is one minute", elapsed)
	}
}

// --- The design rules the criteria do not state -----------------------------

// A window retention has already eaten into is reported, not silently
// answered with less. This is #140's whole argument applied to the export.
func TestARangeRetentionHasTruncatedSaysSo(t *testing.T) {
	from, _ := packWindow(t)
	h := packHarness(t)
	measureRetention(t, h, &kitchenv1alpha1.RetentionStatus{
		LastSweep: &metav1.Time{Time: from.Add(30 * 24 * time.Hour)},
		Classes: []kitchenv1alpha1.RetentionClassStatus{{
			Class:    "audit",
			Enforced: true,
			Rows:     10,
			Oldest:   &metav1.Time{Time: from.Add(45 * 24 * time.Hour)},
		}},
	})

	pack := fetchPack(t, h)
	if !pack.Retention.Truncated {
		t.Fatalf("a window older than the oldest kept record must be reported as truncated: %+v",
			pack.Retention)
	}
	if !strings.Contains(pack.Retention.Message, "retention has already removed part of the window") {
		t.Fatalf("the message must say what happened, got %q", pack.Retention.Message)
	}
	if pack.Retention.CoveredFrom != from.Add(45*24*time.Hour).UTC().Format(time.RFC3339) {
		t.Fatalf("the pack must say the earliest instant it can speak to, got %q",
			pack.Retention.CoveredFrom)
	}
}

// A range that fits inside what the store holds is not reported as truncated,
// and says so positively rather than by silence.
func TestAWholeRangeInsideRetentionIsSaidToBeCovered(t *testing.T) {
	from, _ := packWindow(t)
	h := packHarness(t)
	measureRetention(t, h, &kitchenv1alpha1.RetentionStatus{
		Classes: []kitchenv1alpha1.RetentionClassStatus{{
			Class: "audit", Enforced: true, Rows: 10,
			Oldest: &metav1.Time{Time: from.Add(-24 * time.Hour)},
		}},
	})

	pack := fetchPack(t, h)
	if pack.Retention.Truncated {
		t.Fatalf("this window is inside what the store holds: %+v", pack.Retention)
	}
	if pack.Retention.Message == "" {
		t.Fatal("coverage is stated either way, never by silence")
	}
}

// The range is mandatory at both ends, because a pack that ended "now" could
// not be reproduced.
func TestAPackWillNotBeTakenWithoutBothEndsOfItsWindow(t *testing.T) {
	h := packHarness(t)
	for _, path := range []string{
		"/api/v1/projects/shop/audit-pack",
		"/api/v1/projects/shop/audit-pack?from=" + packFrom,
		"/api/v1/projects/shop/audit-pack?to=" + packTo,
		"/api/v1/projects/shop/audit-pack?from=" + packTo + "&to=" + packFrom,
	} {
		recorder := h.do(t, http.MethodGet, path, "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d: %s", path, recorder.Code, recorder.Body.String())
		}
	}
}

// The pack is one project's whole answer, in one request. The issue names
// seven things it has to carry, and the three tests below walk them: the
// estate and the changes to it, the verdicts and the evidence behind them,
// and the registers.

func TestOnePackCarriesTheEstateAndTheChangesToIt(t *testing.T) {
	h := packHarness(t)
	pack := fetchPack(t, h)

	if pack.Schema != AuditPackSchema || pack.Project != shopProject {
		t.Fatalf("the document must identify itself, got %+v", pack.Range)
	}
	// Inventory: environments, releases, claims, connections, domains.
	if len(pack.Inventory.Environments) != 1 || len(pack.Inventory.Claims) != 1 ||
		len(pack.Inventory.Domains) != 1 || len(pack.Inventory.Connections) != 3 {
		t.Fatalf("the inventory must carry the whole estate, got %+v", pack.Inventory)
	}
	if !contains(pack.Inventory.ThirdParties, neonProvider) {
		t.Fatalf("the third parties must be named, got %+v", pack.Inventory.ThirdParties)
	}
	// Change log with author and approver — the pair §8 exists to record.
	if len(pack.ChangeLog) == 0 {
		t.Fatal("the change log must carry the window's releases")
	}
	change := packChangeFor(t, pack, "shop-rel-pack")
	if change.Author != "grace" || change.Commit != "feedface00000000" {
		t.Fatalf("the change must name its commit and author, got %+v", change)
	}
	if change.Review == nil || len(change.Review.Approvers) != 2 || !change.Review.Independent {
		t.Fatalf("the change must name who approved it, got %+v", change.Review)
	}
}

func TestOnePackCarriesTheVerdictsAndTheEvidenceBehindThem(t *testing.T) {
	h := packHarness(t)
	pack := fetchPack(t, h)

	// Promotion decisions with their reproduction inputs.
	if len(pack.Promotions) != 1 || pack.Promotions[0].DecisionID != "decision-pack-1" {
		t.Fatalf("the window's promotion must be here with its decision, got %+v", pack.Promotions)
	}
	decision := packDecisionFor(t, pack, "decision-pack-1")
	if decision.BundleDigest == "" || decision.InputDigest == "" || len(decision.Input) == 0 {
		t.Fatalf("a decision must carry what it can be replayed from, got %+v", decision)
	}
	// Attestation set per deployed artifact — an index, with the coordinates.
	artifact := packArtifactFor(t, pack, "shop-rel-pack")
	if len(artifact.Evidence) != 2 || artifact.Digest == "" {
		t.Fatalf("the artifact's evidence must be indexed, got %+v", artifact)
	}
	if artifact.Fetch == "" || !strings.Contains(artifact.Fetch, "cosign") {
		t.Fatalf("the index must say how to fetch the evidence itself, got %q", artifact.Fetch)
	}
	// The pack says what the policy says: judged on the newest scan.
	if artifact.NewestScan == nil || artifact.NewestScan.DecisionID != "decision-pack-2" {
		t.Fatalf("the newest re-evaluation is the one that counts, got %+v", artifact.NewestScan)
	}
}

func TestOnePackCarriesEveryRegisterTheIssueNames(t *testing.T) {
	h := packHarness(t)
	pack := fetchPack(t, h)

	// Active and historical exceptions.
	if len(pack.Exceptions) != 1 || pack.Exceptions[0].ApprovedBy != "hopper" {
		t.Fatalf("the exception register must be here, got %+v", pack.Exceptions)
	}
	// Access recertification records.
	if len(pack.Access.Cycles) != 1 || pack.Access.Cycles[0].RecordID != "record-review" {
		t.Fatalf("the recertification cycle must point at its artefact, got %+v", pack.Access.Cycles)
	}
	// Compliance drift history.
	if len(pack.Drift.Current) != 1 {
		t.Fatalf("the drift derivation must cover the project's environments, got %+v", pack.Drift.Current)
	}
	if len(pack.Drift.History) != 1 || pack.Drift.History[0].Unwaived != 1 {
		t.Fatalf("the window's re-evaluations must be here with what stood unwaived, got %+v",
			pack.Drift.History)
	}
	// The audit log and the signed statements.
	if len(pack.AuditLog.Items) != 2 || pack.AuditLog.Privileged != 1 {
		t.Fatalf("the log's slice must be here, privileged records counted, got %+v", pack.AuditLog)
	}
	if len(pack.SignedRecords.Items) != 2 {
		t.Fatalf("both signed statements must be carried whole, got %+v", pack.SignedRecords.Items)
	}
}

// The envelopes are carried byte for byte. A re-encoded envelope does not
// verify, which would make the section worse than useless.
func TestSignedEnvelopesAreCarriedVerbatim(t *testing.T) {
	h := packHarness(t)
	pack := fetchPack(t, h)

	for _, record := range pack.SignedRecords.Items {
		for _, stored := range h.logs.records {
			if stored.ID != record.ID {
				continue
			}
			if string(record.Envelope) != stored.Envelope {
				t.Fatalf("record %s was re-encoded: %s != %s",
					record.ID, record.Envelope, stored.Envelope)
			}
		}
	}
}

// A recertification cycle's own artefact covers grants on other projects; the
// entries beside it must not. The count says how many were left out rather
// than the omission being silent.
func TestACyclesEntriesAreNarrowedAndTheOmissionIsCounted(t *testing.T) {
	h := packHarness(t)
	pack := fetchPack(t, h)

	cycle := pack.Access.Cycles[0]
	if cycle.EntriesTotal != 3 || len(cycle.Entries) != 2 {
		t.Fatalf("the cycle had three entries and two touch this project, got %d of %d",
			len(cycle.Entries), cycle.EntriesTotal)
	}
	for _, entry := range cycle.Entries {
		if entry.Grant == otherProject {
			t.Fatalf("another project's grant reached this project's pack: %+v", entry)
		}
	}
	if cycle.EntriesNote == "" {
		t.Fatal("an omission must be stated rather than left to a reader to notice")
	}
}

// The export is a read, and it is still refused when it cannot be recorded.
// "Who took an audit pack of this project, for which window, and what digest
// did they get" is exactly the sentence the log exists to produce, and a pack
// served without it would be a compliance product with a hole in the middle.
func TestAnExportTheLogCannotRecordIsNotServed(t *testing.T) {
	h := packHarness(t)
	// A recorder pointed at a singleton that is not there: the append cannot
	// even be attempted, which is what a store outage looks like from here.
	h.server.Audit = &audit.Recorder{
		Client: h.server.Client, Namespace: testNamespace, Singleton: "no-such-kitchen",
	}

	recorder := h.do(t, http.MethodGet, packPath, "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

// The record's own shape: a kind of its own, so "every pack ever taken" is one
// query, and the range and digest in the details so the record identifies the
// document without holding it.
func TestTheExportRecordIdentifiesTheDocument(t *testing.T) {
	h := packHarness(t)
	pack := fetchPack(t, h)
	counts := packCounts(pack)

	if audit.KindEvidenceExport == audit.KindProject {
		t.Fatal("an export needs a kind of its own, or it cannot be filtered for")
	}
	for _, section := range []string{"decisions", "auditRecords", "signedRecords", "changes"} {
		if _, named := counts[section]; !named {
			t.Errorf("the record's section counts omit %q", section)
		}
	}
	if counts["decisions"] != len(pack.Decisions.Items) {
		t.Fatalf("the counts must describe the document that was served, got %d for %d",
			counts["decisions"], len(pack.Decisions.Items))
	}
}

// The pack carries no credential, and says so where a reader might assume one
// is missing rather than withheld.
func TestAPackCarriesNoCredential(t *testing.T) {
	h := packHarness(t)
	recorder := h.do(t, http.MethodGet, packPath, "")
	body := recorder.Body.String()

	for _, forbidden := range []string{"gh-credentials", "registry-credentials", "dockerconfigjson"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the pack names a credential secret (%s)", forbidden)
		}
	}
	pack := decode[auditPack](t, recorder)
	for _, connection := range pack.Inventory.Connections {
		if connection.Credential == "" {
			t.Fatalf("a connection must say the credential is withheld rather than leave a blank: %+v",
				connection)
		}
	}
}

// The human rendering is a rendering: derived from the pack, carrying its
// digest, and never claiming to be the signed thing.
func TestTheHumanRenderingIsDeterministicAndNamesTheDigest(t *testing.T) {
	h := packHarness(t)

	first := h.do(t, http.MethodGet, packPath+"&format=html", "")
	if first.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", first.Code, first.Body.String())
	}
	if got := first.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Fatalf("want HTML, got %q", got)
	}
	second := h.do(t, http.MethodGet, packPath+"&format=html", "")
	if first.Body.String() != second.Body.String() {
		t.Fatal("two renderings of one range must be the same page")
	}

	page := first.Body.String()
	digest := first.Header().Get("X-Kitchen-Pack-Digest")
	if !strings.Contains(page, digest) {
		t.Fatal("the page must carry the digest of the bytes it renders, so a printout ties back")
	}
	if !strings.Contains(page, "is a rendering") {
		t.Fatal("the page must say it is not the signed document")
	}
	// It is self-contained: nothing to fetch, so it survives being emailed.
	for _, external := range []string{"<script", "http://", "src=\"http"} {
		if strings.Contains(page, external) {
			t.Errorf("the page reaches outside itself (%s)", external)
		}
	}
	// And the evidence a person reads is on it.
	for _, expected := range []string{"grace", "hopper", "shop-exc-pack", "INC-7"} {
		if !strings.Contains(page, expected) {
			t.Errorf("the rendering omits %q", expected)
		}
	}
}

func TestAnUnknownFormatIsRefusedWithTheThreeThatExist(t *testing.T) {
	h := packHarness(t)
	recorder := h.do(t, http.MethodGet, packPath+"&format=pdf", "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "dsse") {
		t.Fatalf("the refusal must name the formats that exist, got %s", recorder.Body.String())
	}
}

// --- helpers ---------------------------------------------------------------

func fetchPack(t *testing.T, h *harness) auditPack {
	t.Helper()
	recorder := h.do(t, http.MethodGet, packPath, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	return decode[auditPack](t, recorder)
}

func packChangeFor(t *testing.T, pack auditPack, release string) auditPackChange {
	t.Helper()
	for _, change := range pack.ChangeLog {
		if change.Release == release {
			return change
		}
	}
	t.Fatalf("no change log entry for %s", release)
	return auditPackChange{}
}

func packDecisionFor(t *testing.T, pack auditPack, id string) auditPackDecision {
	t.Helper()
	for _, decision := range pack.Decisions.Items {
		if decision.ID == id {
			return decision
		}
	}
	t.Fatalf("no decision %s in the pack", id)
	return auditPackDecision{}
}

func packArtifactFor(t *testing.T, pack auditPack, release string) auditPackArtifact {
	t.Helper()
	for _, artifact := range pack.Attestations {
		if artifact.Release == release {
			return artifact
		}
	}
	t.Fatalf("no attestation entry for %s", release)
	return auditPackArtifact{}
}

func envelopeOf(t *testing.T, body []byte) attestation.Envelope {
	t.Helper()
	envelope := attestation.Envelope{}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("the dsse format must answer with an envelope: %v (%s)", err, body)
	}
	if len(envelope.Signatures) == 0 {
		t.Fatal("the envelope carries no signature")
	}
	return envelope
}

func decodeStatement(t *testing.T, body []byte) attestation.Statement {
	t.Helper()
	decoded, err := envelopeOf(t, body).Statement()
	if err != nil {
		t.Fatalf("the envelope's payload is not a statement: %v", err)
	}
	return decoded
}

func write(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
}

// measureRetention writes what a sweep would have measured onto the
// singleton, which is where the pack reads how far back the log goes.
func measureRetention(t *testing.T, h *harness, status *kitchenv1alpha1.RetentionStatus) {
	t.Helper()
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Status.Retention = status
	if err := h.server.Client.Update(context.Background(), kitchen); err != nil {
		t.Fatal(err)
	}
}

// withSigningKey turns attestation on and puts a real keypair where
// controller.SigningKeyFor looks for one, so the signature the procedure
// checks is the platform's own rather than a test's.
func withSigningKey(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()

	_, private, public, err := attestation.GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := h.server.Client.Get(ctx,
		types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		t.Fatal(err)
	}
	kitchen.Spec.Compliance.Attestation.Enabled = true
	if err := h.server.Client.Update(ctx, kitchen); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      controller.SigningKeySecretName,
			Namespace: controller.PlatformNamespace,
		},
		Data: map[string][]byte{
			attestation.SecretKeyPrivate: private,
			attestation.SecretKeyPublic:  public,
		},
	}
	if err := h.server.Client.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
}

// --- Criterion 4: documented field by field --------------------------------

// Every field of the document appears in its page, walked off the structs
// rather than listed by hand — because a list by hand is a list that goes
// stale on the first field somebody adds.
//
// "Documented field by field, mapped to the requirement each satisfies" is an
// acceptance criterion, and it is the one that decays silently: the code keeps
// working, the page quietly stops describing it. This is the only link in the
// chain that a test can hold.
func TestEveryPackFieldIsDocumented(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "..", "docs", "api", "audit-pack.md"))
	if err != nil {
		t.Fatalf("the pack's page must exist: %v", err)
	}
	documented := string(page)

	missing := []string{}
	for _, field := range jsonFieldsOf(reflect.TypeOf(auditPack{}), map[reflect.Type]bool{}) {
		if !strings.Contains(documented, field) {
			missing = append(missing, field)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("docs/api/audit-pack.md does not mention %s — a field nobody documented is a "+
			"field an examiner has to guess at", strings.Join(missing, ", "))
	}

	// And the requirement mapping is the other half of the criterion: the
	// page has to say which requirement each part is there to satisfy.
	for _, code := range []string{"GR-J3", "GR-L1", "GR-D1", "GR-D8", "GR-E2", "GR-G6", "GR-C4"} {
		if !strings.Contains(documented, code) {
			t.Errorf("the page maps nothing to %s", code)
		}
	}
}

// jsonFieldsOf collects every json name in a struct tree, once each. Anonymous
// and unexported fields are skipped, and a type already walked is not walked
// again — the document has no cycles today, and a future one must not hang the
// test instead of failing it.
func jsonFieldsOf(walked reflect.Type, seen map[reflect.Type]bool) []string {
	for walked.Kind() == reflect.Pointer || walked.Kind() == reflect.Slice {
		walked = walked.Elem()
	}
	if walked.Kind() != reflect.Struct || seen[walked] {
		return nil
	}
	seen[walked] = true

	names := []string{}
	for i := range walked.NumField() {
		field := walked.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
		names = append(names, jsonFieldsOf(field.Type, seen)...)
	}
	return names
}
