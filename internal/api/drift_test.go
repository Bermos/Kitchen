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
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
)

// The drift view over HTTP. What is being tested is the distinction the
// endpoint exists for — a rule that started failing after promotion against a
// rule that fired at promotion and was waived by a grant that has since run
// out — plus the two honest absences: a pair nothing has re-evaluated, and a
// platform where nothing is re-evaluating anything.

// driftDecisions seeds a promotion and a rescan for the fixtures' deployed
// pair. `firedAtPromotion` is what the promotion decision recorded.
func driftDecisions(h *harness, rescanVerdict, rescanRules, firedAtPromotion string) {
	promotedAt := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	scannedAt := time.Date(2026, 8, 24, 3, 16, 0, 0, time.UTC)
	promotionVerdict := policy.VerdictAllowed
	if firedAtPromotion != "[]" {
		promotionVerdict = policy.VerdictAllowedWithException
	}
	h.logs.decisions = []clickhouse.Decision{
		{
			ID: "d-rescan", Timestamp: scannedAt, Kind: policy.KindRescan,
			Project: feedProject, Environment: testEnvironment, Release: testRelease,
			DataSnapshot: "grype-db:sha256:deadbeef",
			Verdict:      rescanVerdict, RulesFired: rescanRules, Input: "{}",
		},
		{
			ID: "d-promotion", Timestamp: promotedAt, Kind: policy.KindPromotion,
			Project: feedProject, Environment: testEnvironment, Release: testRelease,
			Verdict: promotionVerdict, RulesFired: firedAtPromotion, Input: "{}",
		},
	}
}

func driftBodyFor(t *testing.T, h *harness, query string) driftBody {
	t.Helper()
	recorder := h.do(t, http.MethodGet, "/api/v1/compliance/drift"+query, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", recorder.Code, recorder.Body.String())
	}
	return decode[driftBody](t, recorder)
}

func TestDriftTellsANewFailureFromAWaiverThatRanOut(t *testing.T) {
	// Newly failing: the rule did not fire when this release was promoted and
	// fires now. Nothing about the artifact changed — a vulnerability database
	// did, and this is the finding an institution cannot get anywhere else.
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	driftDecisions(h,
		policy.VerdictBlocked,
		`[{"rule":"max-severity","message":"CVE-2026-1 is critical","waived":false}]`,
		`[]`)

	body := driftBodyFor(t, h, "")
	if len(body.Items) != 1 {
		t.Fatalf("want one drifting pair, got %+v", body.Items)
	}
	item := body.Items[0]
	if item.Status != driftNewlyFailing {
		t.Fatalf("a rule that did not fire at promotion read as %q", item.Status)
	}
	if len(item.Rules) != 1 || item.Rules[0].Since != driftSinceRescan {
		t.Errorf("the rule does not say it started failing after promotion: %+v", item.Rules)
	}
	if item.DataSnapshot != "grype-db:sha256:deadbeef" {
		t.Errorf("the database the finding was produced against was lost: %q", item.DataSnapshot)
	}
	if item.PromotedVerdict != policy.VerdictAllowed {
		t.Errorf("what was decided at promotion is missing: %q", item.PromotedVerdict)
	}

	// Failing at promotion under exception: the same blocked verdict, and a
	// completely different thing. Reading it as a new vulnerability would send
	// somebody hunting for a CVE that was never there.
	h = asMember(t, kitchenv1alpha1.AccessRoleViewer)
	driftDecisions(h,
		policy.VerdictBlocked,
		`[{"rule":"require-provenance","message":"no provenance","waived":false}]`,
		`[{"rule":"require-provenance","message":"no provenance","waived":true,"exception":"exc-1"}]`)

	body = driftBodyFor(t, h, "")
	if len(body.Items) != 1 {
		t.Fatalf("want one drifting pair, got %+v", body.Items)
	}
	item = body.Items[0]
	if item.Status != driftWasWaived {
		t.Fatalf("an expired waiver read as %q", item.Status)
	}
	if len(item.Rules) != 1 || item.Rules[0].Since != driftSincePromotion {
		t.Errorf("the rule does not say it was already firing at promotion: %+v", item.Rules)
	}
	if item.Rules[0].WaivedAtPromotion != "exc-1" {
		t.Errorf("the grant that waived it at promotion is not named: %+v", item.Rules[0])
	}
	if item.Rules[0].Exception != "" {
		t.Errorf("a rule firing unwaived named a grant as waiving it: %+v", item.Rules[0])
	}
}

func TestDriftLeavesCompliantPairsOutUnlessAsked(t *testing.T) {
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	driftDecisions(h, policy.VerdictAllowed, "[]", "[]")

	body := driftBodyFor(t, h, "")
	if len(body.Items) != 0 || body.Drifting != 0 {
		t.Fatalf("a compliant pair was reported as drift: %+v", body)
	}
	if body.Counts[driftCompliant] != 1 {
		t.Errorf("the counts do not include what was left out: %+v", body.Counts)
	}

	body = driftBodyFor(t, h, "?all=true")
	if len(body.Items) != 1 || body.Items[0].Status != driftCompliant {
		t.Fatalf("--all did not include the compliant pair: %+v", body.Items)
	}
}

func TestAPairNothingHasReEvaluatedIsNeverCountedAsCompliant(t *testing.T) {
	// The most important row and the easiest to leave out. It is a finding
	// about the platform rather than about the release, and it is said out
	// loud rather than read as silence.
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	h.logs.decisions = []clickhouse.Decision{{
		ID: "d-promotion", Timestamp: time.Now().UTC(), Kind: policy.KindPromotion,
		Project: feedProject, Environment: testEnvironment, Release: testRelease,
		Verdict: policy.VerdictAllowed, RulesFired: "[]", Input: "{}",
	}}

	body := driftBodyFor(t, h, "")
	if len(body.Items) != 1 || body.Items[0].Status != driftUnknown {
		t.Fatalf("an unevaluated pair read as %+v", body.Items)
	}
	if body.Drifting != 1 {
		t.Error("an unevaluated pair was counted as compliant")
	}
	if body.Rescanning {
		t.Error("the pass reported itself running with rescan off in the fixtures")
	}
	if body.Message == "" {
		t.Error("a platform that is not re-evaluating anything did not say so")
	}
}

func TestAPairWhoseLastScanFailedDoesNotReadAsCompliant(t *testing.T) {
	// Monday's scan allowed the release. Tuesday's scanner pod could not pull
	// its image. The newest *stored decision* is still Monday's — the last
	// scan that succeeded — so a verdict switch reading it alone answers
	// "re-evaluated and still clears its environment's bar" with a scannedAt
	// from before the scanner started failing. §9.8 is explicit that
	// not-evaluated is never counted as compliant, and this is the case that
	// produces a stale answer rather than none.
	h := asMember(t, kitchenv1alpha1.AccessRoleViewer)
	driftDecisions(h, policy.VerdictAllowed, "[]", "[]")
	failLastScan(t, h, "the scan did not run: ImagePullBackOff")

	body := driftBodyFor(t, h, "")
	if len(body.Items) != 1 {
		t.Fatalf("a pair whose last scan failed was left out of the drift view: %+v", body)
	}
	item := body.Items[0]
	if item.Status != driftUnknown {
		t.Fatalf("a pair whose last scan failed read as %q", item.Status)
	}
	if item.ScanFailed == "" || !strings.Contains(item.ScanFailed, "ImagePullBackOff") {
		t.Errorf("why the scan did not run was dropped: %+v", item)
	}
	if body.Drifting != 1 || body.Counts[driftCompliant] != 0 {
		t.Errorf("a pair nothing currently vouches for was counted as compliant: %+v", body)
	}
	// The stale answer is still shown beside the failure rather than hidden:
	// "allowed, as of Monday" is what makes the failure legible.
	if item.Verdict != policy.VerdictAllowed || item.ScannedAt == nil {
		t.Errorf("the newest answer that did stand was dropped: %+v", item)
	}

	// A blocked pair whose last scan failed is still blocked — the failure
	// does not soften a finding, it only refuses to invent a clean one.
	h = asMember(t, kitchenv1alpha1.AccessRoleViewer)
	driftDecisions(h, policy.VerdictBlocked,
		`[{"rule":"max-severity","message":"CVE-2026-1 is critical","waived":false}]`, "[]")
	failLastScan(t, h, "the scan did not run: ImagePullBackOff")

	item = driftBodyFor(t, h, "").Items[0]
	if item.Status != driftNewlyFailing || item.ScanFailed == "" {
		t.Errorf("a blocked pair whose scan then failed reads as %q with %q",
			item.Status, item.ScanFailed)
	}
}

// failLastScan records on the deployed environment that the most recent scan
// attempt did not run, which is the cluster half of the join.
func failLastScan(t *testing.T, h *harness, reason string) {
	t.Helper()
	env := &kitchenv1alpha1.Environment{}
	if err := h.server.Client.Get(context.Background(),
		types.NamespacedName{Namespace: testNamespace, Name: testEnvironment}, env); err != nil {
		t.Fatal(err)
	}
	env.Status.Rescan = &kitchenv1alpha1.EnvironmentRescanStatus{
		Phase:      kitchenv1alpha1.RescanFailed,
		Release:    testRelease,
		FinishedAt: &metav1.Time{Time: time.Now().UTC()},
		Message:    reason,
	}
	if err := h.server.Client.Status().Update(context.Background(), env); err != nil {
		t.Fatal(err)
	}
}

func TestDriftIsFilteredToWhatTheCallerMaySee(t *testing.T) {
	// A member sees their projects' pairs and nothing else, like every
	// cross-project read here — the environments of a project they hold no
	// role on are not rows they get to know about.
	h := asMember(t, "")
	driftDecisions(h, policy.VerdictBlocked,
		`[{"rule":"max-severity","message":"x","waived":false}]`, "[]")

	body := driftBodyFor(t, h, "")
	if len(body.Items) != 0 {
		t.Fatalf("a caller with no role was answered about %+v", body.Items)
	}
}
