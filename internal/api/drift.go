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
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/policy"
)

// Compliance drift: what is running right now that no longer meets its
// environment's bar.
//
// It is a join and not a table. The current state is the cluster's — which
// release each environment runs, and what the last rescan of it found — and
// the history is the decision store's. Nothing here is recorded anywhere: a
// drift row is derived on the request, from a rescan decision and the
// promotion decision that let the release in, and the derivation is the whole
// value of the endpoint.
//
// The distinction the answer exists to draw is between two kinds of failure
// that look identical on a blocked verdict and mean completely different
// things:
//
//   - **newly failing** — a rule that did not fire when this release was
//     promoted and fires now. The artifact did not change; a vulnerability
//     database did. This is the finding an institution cannot get out of any
//     other system it owns.
//   - **failing at promotion under exception** — a rule that fired at
//     promotion too and was waived by a break-glass grant that has since run
//     out. Nothing new was discovered; a decision somebody made deliberately,
//     with an expiry, reached its expiry.
//
// Collapsing them would make an expired waiver read as a new vulnerability
// and send somebody hunting for a CVE that was never there.

// Drift statuses. They are words rather than a boolean because "is this
// release compliant" has more than two answers and an export read by an
// auditor must not leave the difference to a reader's generosity.
const (
	// driftCompliant: the last rescan cleared the bar outright.
	driftCompliant = "compliant"
	// driftWaived: the last rescan cleared it only because an exception
	// waived every rule that fired. Compliant by grace, and dated.
	driftWaived = "waived"
	// driftNewlyFailing: blocked now, and at least one of the rules standing
	// in the way did not fire at promotion.
	driftNewlyFailing = "newly-failing"
	// driftWasWaived: blocked now, and every rule standing in the way fired at
	// promotion too and was waived there. The exception ran out.
	driftWasWaived = "waived-at-promotion"
	// driftUnknown: nothing has re-evaluated this pair. It is not a finding
	// about the release; it is a finding about the platform, and it is said
	// out loud rather than counted as compliant.
	driftUnknown = "not-evaluated"
)

// driftRuleView is one rule standing in the way, and where it came from.
type driftRuleView struct {
	Rule    string `json:"rule"`
	Message string `json:"message,omitempty"`
	// Since is "rescan" for a rule that started failing after promotion, and
	// "promotion" for one that fired at promotion and was waived there.
	Since string `json:"since"`
	// Exception names the grant that waived it at promotion, where there was
	// one. It is the reader's next stop: renew it, resolve it, or fix the
	// finding.
	Exception string `json:"exception,omitempty"`
}

// driftItemView is one deployed (environment, release) pair.
type driftItemView struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	Release     string `json:"release"`
	Artifact    string `json:"artifact,omitempty"`

	// Status is one of the five words above.
	Status string `json:"status"`

	// Verdict is the last rescan's verdict, and ScannedAt when it was
	// reached. DataSnapshot is the vulnerability database it was reached
	// against — the field that makes the finding reproducible rather than
	// merely repeatable.
	Verdict      string     `json:"verdict,omitempty"`
	ScannedAt    *time.Time `json:"scannedAt,omitempty"`
	DataSnapshot string     `json:"dataSnapshot,omitempty"`
	Findings     int32      `json:"findings,omitempty"`
	DecisionID   string     `json:"decisionID,omitempty"`

	// PromotedVerdict and PromotedAt are what was decided when this release
	// was let in, which is the other half of every comparison here.
	PromotedVerdict string     `json:"promotedVerdict,omitempty"`
	PromotedAt      *time.Time `json:"promotedAt,omitempty"`

	// Rules are what stands in the way now, each saying since when.
	Rules []driftRuleView `json:"rules"`

	// Message is the one line a person reads.
	Message string `json:"message,omitempty"`
}

// driftBody is the whole answer, exportable as it is.
type driftBody struct {
	GeneratedAt time.Time `json:"generatedAt"`
	// Rescanning says whether the continuous re-evaluation pass is running at
	// all. A drift view with nothing in it means two very different things
	// depending on this, and a reader who cannot tell them apart will
	// eventually read "nothing is being checked" as "nothing is wrong".
	Rescanning bool   `json:"rescanning"`
	Message    string `json:"message,omitempty"`
	// Drifting is how many rows are not compliant, counted before any
	// filtering the query asked for, so a page that shows nothing still says
	// whether there is anything.
	Drifting int             `json:"drifting"`
	Items    []driftItemView `json:"items"`
	// Counts is every status against how many pairs carry it, including the
	// compliant ones the default page leaves out.
	Counts map[string]int `json:"counts"`
}

// firedRule is the engine's own encoding of one fired rule, as the decision
// store keeps it. It is decoded here rather than passed through because this
// endpoint compares two decisions' rules against each other.
type firedRule struct {
	Rule      string `json:"rule"`
	Message   string `json:"message"`
	Waived    bool   `json:"waived"`
	Exception string `json:"exception,omitempty"`
}

// complianceDrift answers GET /api/v1/compliance/drift.
//
// An operator reads the whole install; a member reads their own projects'
// rows, like every cross-project read here. `?project=` and `?environment=`
// narrow it, and `?all=true` includes the pairs that are compliant — the
// default is the question the endpoint exists for, which is what is *not*.
func (s *Server) complianceDrift(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	scope := scopeFrom(ctx)

	project := strings.TrimSpace(req.URL.Query().Get("project"))
	if !s.visibleProject(w, req, project) {
		return
	}
	environment := strings.TrimSpace(req.URL.Query().Get("environment"))
	all := strings.EqualFold(strings.TrimSpace(req.URL.Query().Get("all")), "true")

	body := driftBody{GeneratedAt: time.Now().UTC(), Items: []driftItemView{}, Counts: map[string]int{}}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err == nil {
		if status := kitchen.Status.Compliance; status != nil && status.Rescan != nil {
			body.Rescanning = status.Rescan.Running
			body.Message = status.Rescan.Message
		} else if !kitchen.Spec.Compliance.Rescan.Enabled {
			body.Message = "continuous re-evaluation is off, so nothing here has been re-checked since it was promoted"
		}
	}

	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := s.Client.List(ctx, environments, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	store := s.openLogStore(w, req)
	if store == nil {
		return
	}

	items := environments.Items
	sort.Slice(items, func(i, j int) bool {
		if items[i].Spec.ProjectRef.Name != items[j].Spec.ProjectRef.Name {
			return items[i].Spec.ProjectRef.Name < items[j].Spec.ProjectRef.Name
		}
		return items[i].Name < items[j].Name
	})

	for i := range items {
		env := &items[i]
		if env.Spec.ReleaseRef.Name == "" || !scope.allows(env.Spec.ProjectRef.Name) {
			continue
		}
		if project != "" && env.Spec.ProjectRef.Name != project {
			continue
		}
		if environment != "" && env.Name != environment {
			continue
		}
		item, err := s.driftFor(req, store, env)
		if err != nil {
			s.writeStoreError(w, err, "the drift query")
			return
		}
		body.Counts[item.Status]++
		if item.Status != driftCompliant {
			body.Drifting++
		}
		if all || item.Status != driftCompliant {
			body.Items = append(body.Items, item)
		}
	}
	writeJSON(w, http.StatusOK, body)
}

// driftFor derives one pair's row.
//
// Two narrow reads rather than one wide one: a pair rescanned daily for a year
// has three hundred rescan decisions between it and its promotion, so a single
// page of its history would answer the second question with whatever fell
// inside the page. Asking each kind for its newest row is exact and stays
// exact however long the release has been up.
func (s *Server) driftFor(
	req *http.Request, store logReader, env *kitchenv1alpha1.Environment,
) (driftItemView, error) {
	ctx := req.Context()
	release := env.Spec.ReleaseRef.Name

	item := driftItemView{
		Project:     env.Spec.ProjectRef.Name,
		Environment: env.Name,
		Release:     release,
		Status:      driftUnknown,
		Rules:       []driftRuleView{},
		Message: "this release has not been re-evaluated since it was promoted, " +
			"so nothing here is a statement about it today",
	}
	if state := env.Status.Rescan; state != nil && state.Release == release {
		item.Artifact = state.Artifact
		item.DataSnapshot = state.DataSnapshot
		item.Findings = state.Findings
		if state.Phase == kitchenv1alpha1.RescanFailed {
			item.Message = "the last scan of this release did not run: " + state.Message
		}
	}

	base := clickhouse.DecisionQuery{
		Project: env.Spec.ProjectRef.Name, Environment: env.Name, Release: release, Limit: 1,
	}
	rescans, err := store.QueryDecisions(ctx, withKind(base, policy.KindRescan))
	if err != nil {
		return item, err
	}
	promotions, err := store.QueryDecisions(ctx, withKind(base, policy.KindPromotion))
	if err != nil {
		return item, err
	}

	if len(promotions) == 1 {
		promoted := promotions[0].Timestamp
		item.PromotedVerdict = promotions[0].Verdict
		item.PromotedAt = &promoted
	}
	if len(rescans) == 0 {
		return item, nil
	}

	latest := rescans[0]
	scanned := latest.Timestamp
	item.Verdict = latest.Verdict
	item.ScannedAt = &scanned
	item.DecisionID = latest.ID
	if latest.DataSnapshot != "" {
		item.DataSnapshot = latest.DataSnapshot
	}
	if latest.Artifact != "" {
		item.Artifact = latest.Artifact
	}

	now := decodeFired(latest.RulesFired)
	then := map[string]firedRule{}
	if len(promotions) == 1 {
		for _, rule := range decodeFired(promotions[0].RulesFired) {
			then[rule.Rule] = rule
		}
	}

	switch latest.Verdict {
	case policy.VerdictAllowed:
		item.Status = driftCompliant
		item.Message = "re-evaluated and still clears its environment's bar"
		return item, nil
	case policy.VerdictAllowedWithException:
		item.Status = driftWaived
		item.Message = "re-evaluated as blocked, with every rule waived by an exception that has not " +
			"yet expired — compliant by grace, and dated"
		for _, rule := range now {
			item.Rules = append(item.Rules, driftRuleView{
				Rule: rule.Rule, Message: rule.Message, Since: sinceOf(rule, then), Exception: rule.Exception,
			})
		}
		return item, nil
	}

	newly := 0
	for _, rule := range now {
		if rule.Waived {
			continue
		}
		since := sinceOf(rule, then)
		if since == driftSinceRescan {
			newly++
		}
		waivedBy := ""
		if earlier, found := then[rule.Rule]; found {
			waivedBy = earlier.Exception
		}
		item.Rules = append(item.Rules, driftRuleView{
			Rule: rule.Rule, Message: rule.Message, Since: since, Exception: waivedBy,
		})
	}
	switch {
	case newly > 0:
		item.Status = driftNewlyFailing
		item.Message = "no longer meets its environment's bar: rules that did not fire when this release " +
			"was promoted fire now, against a newer vulnerability database"
	case len(item.Rules) > 0:
		item.Status = driftWasWaived
		item.Message = "no longer meets its environment's bar: every rule standing in the way fired at " +
			"promotion too and was waived by an exception that has since expired"
	default:
		// Blocked with nothing unwaived is not a shape the engine produces;
		// answering it honestly costs one branch and beats answering it as
		// compliant.
		item.Status = driftNewlyFailing
		item.Message = "re-evaluated as blocked, and the decision names no unwaived rule — read the " +
			"decision itself"
	}
	return item, nil
}

// Where a failing rule came from.
const (
	driftSinceRescan    = "rescan"
	driftSincePromotion = "promotion"
)

// sinceOf answers whether a rule was already firing at promotion. A rule that
// fired then — waived or not — is not news; one that did not is.
func sinceOf(rule firedRule, atPromotion map[string]firedRule) string {
	if _, found := atPromotion[rule.Rule]; found {
		return driftSincePromotion
	}
	return driftSinceRescan
}

// withKind narrows a decision query to one kind without the caller building
// two structs.
func withKind(query clickhouse.DecisionQuery, kind string) clickhouse.DecisionQuery {
	query.Kind = kind
	return query
}

// decodeFired reads the engine's stored encoding of what fired. An unreadable
// column answers with nothing rather than failing the whole view: a row the
// platform cannot parse is one pair's detail lost, not the estate's answer.
func decodeFired(stored string) []firedRule {
	if strings.TrimSpace(stored) == "" {
		return nil
	}
	rules := []firedRule{}
	if err := json.Unmarshal([]byte(stored), &rules); err != nil {
		return nil
	}
	return rules
}
