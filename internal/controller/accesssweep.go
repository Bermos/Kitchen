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
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/idp"
)

// The access sweep (issue #139): the cadence, the orphan survey, and the
// detection of writes the platform did not make.
//
// # Why a Runnable and not a reconciler
//
// The rescan sweep's first reason, and only its first: **a cycle opens once
// per interval across the platform**, and before the first one exists there
// is no object for a reconciler to reconcile. A Runnable that declares
// NeedLeaderElection says out loud that it is a singleton, which is what
// stops three replicas opening three cycles on the same morning. Everything
// that has an object — the due date, the close, the revocations — is the lean
// reconciler in accessreview_controller.go, exactly as the Exception
// reconciler is the clock for a grant that already exists.
//
// It also has no budget to spend: this sweep runs no pods and pulls no
// images. A step is three cached lists, one directory call and one store
// query, which is why the interval is minutes rather than the rescan's
// careful per-pair accounting.
//
// # What it detects, and what it cannot
//
// Out-of-band detection reads `metadata.managedFields`: the API server
// records which field manager last wrote each field of every object, and a
// manager Kitchen does not recognise on a Kitchen-managed object is a write
// no reconcile made. It is honest about being a heuristic — see
// docs/COMPLIANCE.md §11.4 for the blind spots, of which the flatly
// unavoidable one is that a caller may name themselves anything they like:
// `kubectl --field-manager=kitchen` is invisible here and always will be.
// That is why this is detection rather than prevention, and why the residual
// risk is documented rather than claimed away.

const (
	// accessSweepInterval is how often the sweep looks. A step is cheap and
	// the things it watches for are not urgent to the minute — an unnoticed
	// out-of-band write for five minutes is not a different incident from an
	// unnoticed one for one minute.
	accessSweepInterval = 5 * time.Minute

	// accessReviewNamePrefix is what a cadence-opened cycle is named.
	accessReviewNamePrefix = "access-review-"
)

// platformFieldManagers are the managers whose writes to Kitchen's objects
// are the platform's own or its installer's.
//
// The operator's own writes arrive under the manager the API server derives
// from its User-Agent — the binary's name — for every non-Apply request,
// which is what controller-runtime makes. Helm's are the chart's, and a
// chart's write to the singleton is an install rather than an intrusion.
// `kube-controller-manager` appears on objects Kubernetes itself touches.
//
// Anything else is reported, and an installation with a legitimate writer of
// its own names it in spec.compliance.access.expectedManagers rather than
// having the platform guess.
var platformFieldManagers = []string{
	"manager",
	"kitchen",
	"kitchen-operator",
	"helm",
	"kube-controller-manager",
}

// guardedKinds are the objects whose *content is a control*: change one and
// what the platform will allow changes with it.
//
// It is deliberately not every Kitchen kind. A Build or a Release edited
// behind the platform's back is a curiosity; the operator list, a project's
// grants, an environment's requirements, a break-glass grant, a credential's
// connection and the platform's own configuration are the six places somebody
// with cluster access would go to make the platform permit something it
// otherwise would not. Watching those and saying so is worth more than
// watching everything and being ignored.
func guardedKinds() []guardedKind {
	return []guardedKind{
		{audit.KindKitchen, func() client.ObjectList { return &kitchenv1alpha1.KitchenList{} }},
		{audit.KindProject, func() client.ObjectList { return &kitchenv1alpha1.ProjectList{} }},
		{audit.KindEnvironment, func() client.ObjectList { return &kitchenv1alpha1.EnvironmentList{} }},
		{audit.KindException, func() client.ObjectList { return &kitchenv1alpha1.ExceptionList{} }},
		{audit.KindConnection, func() client.ObjectList { return &kitchenv1alpha1.ConnectionList{} }},
		{audit.KindAccessReview, func() client.ObjectList { return &kitchenv1alpha1.AccessReviewList{} }},
	}
}

// guardedKind is one watched kind: its name, because a typed object's
// TypeMeta is empty by the time the client is done with it, and a fresh list
// to read it into.
type guardedKind struct {
	Kind string
	List func() client.ObjectList
}

// AccessSweeper opens recertification cycles on the cadence, surveys who
// holds what, and watches Kitchen's own objects for writes it did not make.
type AccessSweeper struct {
	client.Client

	// Audit is fail-closed for the two things this sweep *records*: opening
	// a cycle, and an out-of-band write. A record the log refuses means the
	// sweep does neither and tries again next step.
	Audit *audit.Recorder

	// Namespace is where the platform's objects live.
	Namespace string

	// Interval overrides accessSweepInterval, for tests.
	Interval time.Duration

	// Now is the clock, for tests.
	Now func() time.Time

	// Accounts resolves the identity provider's account directory. Nil is the
	// real client, built from the singleton's auth secret.
	Accounts func(cfg idp.Config) AccountDirectory

	// Stores resolves the telemetry store the activity survey reads. Nil is
	// the real client.
	Stores func(cfg clickhouse.Config) ActivityStore

	// reported remembers which out-of-band writes have already been recorded,
	// so a foreign manager that stays on an object — and it does stay, until
	// the platform writes those fields again — is one audit record rather
	// than one every five minutes.
	//
	// It is in memory and not on the object, deliberately: writing a marker
	// onto the object would itself be a write to the thing being watched, and
	// the detection would start finding its own footprints. The cost is that
	// a restarted operator re-records what it already saw, which is
	// over-recording — §4.6's acceptable direction — rather than a hole.
	reported map[string]struct{}
}

// AccountDirectory is the slice of the identity provider the survey needs: an
// interface so a test needs no issuer.
type AccountDirectory interface {
	Accounts(ctx context.Context) ([]idp.Account, error)
}

// ActivityStore is the slice of the telemetry store the survey needs.
type ActivityStore interface {
	ActorActivity(ctx context.Context) (map[string]time.Time, error)
}

// NeedLeaderElection makes the sweep a singleton. Two replicas would open two
// cycles per interval and record every out-of-band write twice.
func (s *AccessSweeper) NeedLeaderElection() bool { return true }

func (s *AccessSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Start runs the sweep until the context ends. Like every Runnable here, it
// never returns a non-nil error before then: a survey that could not run must
// not take the operator down with it.
func (s *AccessSweeper) Start(ctx context.Context) error {
	step := s.Interval
	if step <= 0 {
		step = accessSweepInterval
	}
	ticker := time.NewTicker(step)
	defer ticker.Stop()

	for {
		if _, err := s.SweepOnce(ctx); err != nil {
			logf.FromContext(ctx).V(1).Info("the access sweep could not complete", "reason", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// AccessSweepReport is what one pass found, which is what the singleton's
// status reports and what a test asserts on.
type AccessSweepReport struct {
	// Reviewing is whether the cadence is on and has somewhere to run.
	Reviewing bool
	// Opened names a cycle this pass opened, empty if none was due.
	Opened string
	// OpenReview names the cycle currently open, and DueBy when it is due.
	OpenReview string
	DueBy      *time.Time
	// LastClosed is when a cycle last closed.
	LastClosed *time.Time
	// Identities and Orphans are the survey's counts.
	Identities int
	Orphans    int
	// OutOfBand is how many guarded objects currently carry a manager the
	// platform does not recognise, and LastOutOfBand the newest such write.
	OutOfBand     int
	LastOutOfBand *time.Time
	// Message explains a pass that could not do all of its work.
	Message string
}

// SweepOnce advances everything by one step: it surveys who holds what, opens
// a cycle if one is due, scans the guarded kinds for foreign writes, and
// publishes what it found.
//
// It is exported because it is the unit of the pass, the way SweepOnce is for
// the rescan: a test drives it directly rather than waiting on a ticker.
func (s *AccessSweeper) SweepOnce(ctx context.Context) (AccessSweepReport, error) {
	report := AccessSweepReport{}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return report, client.IgnoreNotFound(err)
	}
	cfg := kitchen.Spec.Compliance.Access
	if !cfg.Enabled {
		report.Message = "the access controls are turned off in spec.compliance.access: no cycle opens on " +
			"its own, no identity is surveyed, and no out-of-band write is looked for"
		s.publish(ctx, report)
		return report, nil
	}

	survey, message := s.survey(ctx, kitchen, cfg)
	report.Identities, report.Orphans, report.Message = len(survey.Identities), survey.Orphans, message
	report.Reviewing = true

	if err := s.reviewCadence(ctx, kitchen, cfg, survey, &report); err != nil {
		s.publish(ctx, report)
		return report, err
	}

	if cfg.DetectOutOfBandWrites {
		if err := s.detectOutOfBand(ctx, kitchen, cfg, &report); err != nil {
			s.publish(ctx, report)
			return report, err
		}
	}

	s.publish(ctx, report)
	return report, nil
}

// survey materializes who holds what. Everything it reads is read here and
// handed to access.Survey, which is the suite's rule about materializing
// inputs applied to the one question this issue is about.
func (s *AccessSweeper) survey(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	cfg kitchenv1alpha1.AccessComplianceSpec,
) (access.IdentitySurvey, string) {
	projects := &kitchenv1alpha1.ProjectList{}
	if err := s.List(ctx, projects, client.InNamespace(s.Namespace)); err != nil {
		return access.IdentitySurvey{}, "the projects could not be listed: " + err.Error()
	}

	messages := []string{}
	activity, err := s.activity(ctx, kitchen)
	if err != nil {
		// Dormancy is judged against an empty answer, which makes every
		// identity look dormant — and because an orphan needs the directory
		// half too, no orphan is claimed on that alone. The message is what
		// stops a reader taking the number at face value.
		messages = append(messages, "the activity log could not be read, so dormancy is not judged: "+err.Error())
	}

	accounts, consulted, err := s.accounts(ctx, kitchen)
	if err != nil {
		messages = append(messages, "the account directory could not be read, so no grant is reported "+
			"as belonging to nobody: "+err.Error())
	}

	inactivity := cfg.InactivityDays
	if inactivity < 1 {
		inactivity = 90
	}
	return access.Survey(access.SurveyInput{
		Kitchen:            kitchen,
		Projects:           projects.Items,
		Activity:           activity,
		Accounts:           accounts,
		DirectoryConsulted: consulted,
		InactivityDays:     inactivity,
		At:                 s.now(),
		Message:            strings.Join(messages, "; "),
	}), strings.Join(messages, "; ")
}

// activity reads when each identity was last recorded doing something.
func (s *AccessSweeper) activity(
	ctx context.Context, kitchen *kitchenv1alpha1.Kitchen,
) (map[string]time.Time, error) {
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return nil, fmt.Errorf("this installation has no telemetry store")
	}
	secret := &corev1.Secret{}
	if err := s.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}, secret); err != nil {
		return nil, err
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return nil, err
	}
	factory := s.Stores
	if factory == nil {
		factory = func(cfg clickhouse.Config) ActivityStore { return clickhouse.New(cfg) }
	}
	return factory(cfg).ActorActivity(ctx)
}

// accounts reads the identity provider's directory. The second result says
// whether it answered at all, which is what stops a federated issuer — which
// serves no directory by design — turning every grant on the platform into an
// orphan.
func (s *AccessSweeper) accounts(
	ctx context.Context, kitchen *kitchenv1alpha1.Kitchen,
) (map[string]struct{}, bool, error) {
	ref := kitchen.Spec.Auth.SecretRef
	if ref == nil {
		return nil, false, fmt.Errorf("this installation runs no identity provider of Kitchen's")
	}
	secret := &corev1.Secret{}
	if err := s.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}, secret); err != nil {
		return nil, false, err
	}
	cfg, err := idp.ConfigFromSecret(secret)
	if err != nil {
		return nil, false, err
	}
	factory := s.Accounts
	if factory == nil {
		factory = func(cfg idp.Config) AccountDirectory { return idp.New(cfg) }
	}
	found, err := factory(cfg).Accounts(ctx)
	if err != nil {
		return nil, false, err
	}
	accounts := make(map[string]struct{}, len(found)*2)
	for _, account := range found {
		if account.Subject != "" {
			accounts[account.Subject] = struct{}{}
		}
		if account.Email != "" {
			accounts[account.Email] = struct{}{}
		}
	}
	return accounts, true, nil
}

// reviewCadence opens the next cycle when one is due, and reports where the
// current one stands.
//
// Due is counted from the *last cycle's close* rather than from a fixed grid,
// so an installation that recertified late does not immediately owe another
// one — the rescan's self-spreading interval, applied to a quarterly control.
func (s *AccessSweeper) reviewCadence(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	cfg kitchenv1alpha1.AccessComplianceSpec,
	survey access.IdentitySurvey,
	report *AccessSweepReport,
) error {
	reviews := &kitchenv1alpha1.AccessReviewList{}
	if err := s.List(ctx, reviews, client.InNamespace(s.Namespace)); err != nil {
		return err
	}

	now := s.now()
	var lastClosed *time.Time
	for i := range reviews.Items {
		review := &reviews.Items[i]
		switch review.EffectivePhase(now) {
		case kitchenv1alpha1.AccessReviewClosed:
			at := review.Status.ClosedAt
			if at != nil && (lastClosed == nil || at.Time.After(*lastClosed)) {
				closed := at.Time.UTC()
				lastClosed = &closed
			}
		default:
			// A cycle is open. One at a time is the whole rule: two open
			// cycles over the same grants would be two reviewers deciding
			// the same question, and a close that applied one set of
			// revocations while the other cycle still showed the grant.
			report.OpenReview = review.Name
			due := review.Spec.DueBy.Time.UTC()
			report.DueBy = &due
		}
	}
	report.LastClosed = lastClosed
	if report.OpenReview != "" {
		return nil
	}

	if cfg.IntervalDays <= 0 {
		// The cadence is off. The surface stays: an operator opens a cycle
		// through the API when they need one.
		return nil
	}
	if len(survey.Identities) == 0 {
		// Nothing to review. A fresh install before anybody has signed up
		// looks exactly like this, and opening an empty cycle would be a
		// recertification of nobody.
		return nil
	}
	interval := time.Duration(cfg.IntervalDays) * 24 * time.Hour
	if lastClosed != nil && now.Sub(*lastClosed) < interval {
		return nil
	}

	name, err := s.openCycle(ctx, kitchen, cfg, survey, now)
	if err != nil {
		return err
	}
	report.Opened, report.OpenReview = name, name
	due := now.Add(time.Duration(dueDays(cfg)) * 24 * time.Hour)
	report.DueBy = &due
	return nil
}

func dueDays(cfg kitchenv1alpha1.AccessComplianceSpec) int32 {
	if cfg.DueDays < 1 {
		return 14
	}
	return cfg.DueDays
}

// openCycle creates a cadence cycle over the whole installation, with the
// snapshot already on it. Audit record first, because a cycle the log could
// not record is one the platform does not open.
func (s *AccessSweeper) openCycle(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	cfg kitchenv1alpha1.AccessComplianceSpec,
	survey access.IdentitySurvey,
	now time.Time,
) (string, error) {
	review := &kitchenv1alpha1.AccessReview{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: accessReviewNamePrefix,
			Namespace:    s.Namespace,
		},
		Spec: kitchenv1alpha1.AccessReviewSpec{
			Scope: kitchenv1alpha1.AccessReviewAll,
			// The platform's operators are who the cadence asks, because
			// they are the only accounts that can see every grant. A cycle
			// somebody opens by hand may name anybody.
			Reviewers: kitchen.Spec.Access.Operators,
			OpenedBy:  audit.ControllerActor(actorAccessReviewController),
			DueBy:     metav1.NewTime(now.Add(time.Duration(dueDays(cfg)) * 24 * time.Hour)),
			Reason: fmt.Sprintf("the %d-day recertification cadence in spec.compliance.access",
				cfg.IntervalDays),
		},
	}
	if len(review.Spec.Reviewers) == 0 {
		// Nobody to ask. An installation with no operators cannot review
		// itself, and opening a cycle with no reviewer would be a form with
		// no addressee. The condition on the singleton (OperatorsConfigured)
		// is already saying what is wrong.
		return "", nil
	}

	if err := s.Audit.Record(ctx, accessReviewOpenedTransition(review, survey)); err != nil {
		return "", err
	}
	if err := s.Create(ctx, review); err != nil {
		return "", err
	}
	review.Status.Phase = kitchenv1alpha1.AccessReviewOpen
	review.Status.OpenedAt = &metav1.Time{Time: now}
	review.Status.SnapshotAt = &metav1.Time{Time: survey.At}
	review.Status.Entries = SnapshotEntries(survey)
	review.Status.Pending = int32(len(review.Status.Entries)) //nolint:gosec // a grant count is not a security boundary
	review.Status.Orphaned = int32(survey.Orphans)            //nolint:gosec // ditto
	if err := s.Status().Update(ctx, review); err != nil {
		return review.Name, err
	}

	logf.FromContext(ctx).Info("opened an access recertification cycle", "review", review.Name,
		"grants", len(review.Status.Entries), "orphaned", survey.Orphans,
		"dueBy", review.Spec.DueBy.UTC().Format(time.RFC3339))
	return review.Name, nil
}

// SnapshotEntries freezes a survey into a cycle's entries. It is the one
// conversion, so a snapshot and the live read are the same rows.
func SnapshotEntries(survey access.IdentitySurvey) []kitchenv1alpha1.AccessReviewEntry {
	entries := make([]kitchenv1alpha1.AccessReviewEntry, 0, len(survey.Identities))
	for _, identity := range survey.Identities {
		entry := kitchenv1alpha1.AccessReviewEntry{
			AccessSubject: kitchenv1alpha1.AccessSubject{
				Subject: identity.Subject,
				Email:   identity.Email,
			},
			Grant:    identity.Grant,
			Role:     identity.Role,
			Inactive: identity.Inactive,
			Unknown:  identity.Unknown,
			Orphaned: identity.Orphaned,
		}
		if identity.LastActive != nil {
			entry.LastActive = &metav1.Time{Time: *identity.LastActive}
		}
		entries = append(entries, entry)
	}
	return entries
}

// accessReviewOpenedTransition is the cycle's opening audit record.
func accessReviewOpenedTransition(
	review *kitchenv1alpha1.AccessReview, survey access.IdentitySurvey,
) audit.Transition {
	return audit.Transition{
		Object:     review,
		Kind:       audit.KindAccessReview,
		Operation:  clickhouse.AuditCreate,
		Controller: actorAccessReviewController,
		Privileged: audit.PrivilegeAccess,
		To:         string(kitchenv1alpha1.AccessReviewOpen),
		Reason: fmt.Sprintf(
			"access recertification opened on the cadence: %d grant(s) to review by %s",
			len(survey.Identities), review.Spec.DueBy.UTC().Format(time.RFC3339)),
		Details: map[string]any{
			"scope":              string(review.Spec.Scope),
			"grants":             len(survey.Identities),
			"orphaned":           survey.Orphans,
			"dueBy":              review.Spec.DueBy.UTC().Format(time.RFC3339),
			"reviewers":          subjectsOfReviewers(review),
			"directoryConsulted": survey.DirectoryConsulted,
		},
	}
}

// OutOfBandWrite is one Kitchen-managed object carrying a field manager the
// platform does not recognise.
type OutOfBandWrite struct {
	Kind      string
	Namespace string
	Name      string
	// Manager is the field manager the API server recorded, Operation how it
	// wrote (Update or Apply), and At when.
	Manager   string
	Operation string
	At        time.Time
	// Fields is a readable summary of what that manager owns on the object.
	Fields string
}

// key identifies one finding, so a manager that stays on an object is one
// record rather than one every five minutes. The time is in the key because
// a *second* write by the same manager is a second event worth recording.
func (w OutOfBandWrite) key() string {
	return strings.Join([]string{w.Kind, w.Namespace, w.Name, w.Manager,
		w.At.UTC().Format(time.RFC3339)}, "\x00")
}

// detectOutOfBand scans the guarded kinds and records what it finds.
func (s *AccessSweeper) detectOutOfBand(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	cfg kitchenv1alpha1.AccessComplianceSpec,
	report *AccessSweepReport,
) error {
	expected := expectedManagers(cfg)
	findings := []OutOfBandWrite{}
	for _, guarded := range guardedKinds() {
		list := guarded.List()
		if err := s.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
			return err
		}
		objects, err := apimeta.ExtractList(list)
		if err != nil {
			return err
		}
		for _, item := range objects {
			object, ok := item.(client.Object)
			if !ok {
				continue
			}
			findings = append(findings, ForeignWrites(guarded.Kind, object, expected)...)
		}
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].At.Before(findings[j].At) })

	report.OutOfBand = len(findings)
	if len(findings) > 0 {
		newest := findings[len(findings)-1].At
		report.LastOutOfBand = &newest
	}

	if s.reported == nil {
		s.reported = map[string]struct{}{}
	}
	for _, finding := range findings {
		if _, seen := s.reported[finding.key()]; seen {
			continue
		}
		if err := s.Audit.Record(ctx, outOfBandTransition(finding, kitchen)); err != nil {
			// Fail closed and try again next step. A detection the log could
			// not keep is one the platform has not made: reporting it on the
			// status without a record would be the one shape this suite
			// refuses — a fact with no evidence behind it.
			return err
		}
		s.reported[finding.key()] = struct{}{}
		logf.FromContext(ctx).Info("a Kitchen-managed object was written by something that is not Kitchen",
			"kind", finding.Kind, "name", finding.Name, "manager", finding.Manager,
			"operation", finding.Operation, "at", finding.At.UTC().Format(time.RFC3339))
	}
	return nil
}

// expectedManagers is the platform's own plus whatever the installation
// named. Matched exactly: a prefix or a glob here would eventually excuse
// more than whoever wrote it meant, which is the one kind of surprise a
// detection must not have.
func expectedManagers(cfg kitchenv1alpha1.AccessComplianceSpec) map[string]struct{} {
	expected := make(map[string]struct{}, len(platformFieldManagers)+len(cfg.ExpectedManagers))
	for _, manager := range platformFieldManagers {
		expected[manager] = struct{}{}
	}
	for _, manager := range cfg.ExpectedManagers {
		if trimmed := strings.TrimSpace(manager); trimmed != "" {
			expected[trimmed] = struct{}{}
		}
	}
	return expected
}

// ForeignWrites reads one object's managedFields and reports every manager
// the platform does not recognise.
//
// It is a pure function of the object so that a test can hold a
// hand-assembled managedFields list up to the light — which is what the
// acceptance criterion "out-of-band mutation of a Kitchen-managed object
// raises an alert" is actually about.
//
// Status subresource entries are skipped. A controller writing status is
// every controller, including ones that legitimately watch Kitchen's objects,
// and a status write cannot change what the platform allows — which is the
// whole subject. What is being looked for is a write to the *spec*.
func ForeignWrites(kind string, object client.Object, expected map[string]struct{}) []OutOfBandWrite {
	findings := []OutOfBandWrite{}
	for _, entry := range object.GetManagedFields() {
		if entry.Subresource != "" {
			continue
		}
		if _, ok := expected[entry.Manager]; ok {
			continue
		}
		at := time.Time{}
		if entry.Time != nil {
			at = entry.Time.Time
		}
		findings = append(findings, OutOfBandWrite{
			Kind:      kind,
			Namespace: object.GetNamespace(),
			Name:      object.GetName(),
			Manager:   entry.Manager,
			Operation: string(entry.Operation),
			At:        at.UTC(),
			Fields:    summariseFields(entry.FieldsV1),
		})
	}
	return findings
}

// summariseFields turns the API server's fieldsV1 blob into something a
// person reads. It is the raw JSON trimmed rather than parsed: the shape is
// a nested set of `f:` keys, and a record that named the top-level paths is
// enough to say "they wrote the spec" without this file growing a parser for
// somebody else's encoding.
func summariseFields(fields *metav1.FieldsV1) string {
	if fields == nil || len(fields.Raw) == 0 {
		return ""
	}
	const cap = 512
	raw := string(fields.Raw)
	if len(raw) > cap {
		return raw[:cap] + "…"
	}
	return raw
}

// outOfBandTransition is the detection's audit record. The object it is about
// is the Kitchen singleton rather than the written object, because the record
// is a statement about the *platform's* integrity and because attributing it
// to the written object would put it in that project's audit view as though
// the project had done something.
//
// The actor is the reconciler that noticed, not the manager that wrote: the
// manager is a name a caller chose for itself and the log must not present it
// as an authenticated identity. It is in the details, which is where an
// unverified claim belongs.
func outOfBandTransition(
	finding OutOfBandWrite, kitchen *kitchenv1alpha1.Kitchen,
) audit.Transition {
	details := map[string]any{
		"objectKind":      finding.Kind,
		"objectName":      finding.Name,
		"objectNamespace": finding.Namespace,
		"fieldManager":    finding.Manager,
		"operation":       finding.Operation,
		"detection":       "managedFields",
	}
	if !finding.At.IsZero() {
		details["writtenAt"] = finding.At.UTC().Format(time.RFC3339)
	}
	if finding.Fields != "" {
		details["fields"] = finding.Fields
	}
	return audit.Transition{
		Object:     kitchen,
		Kind:       audit.KindKitchen,
		Controller: actorAccessReviewController,
		Privileged: audit.PrivilegeIntegrity,
		Reason: fmt.Sprintf(
			"out-of-band write: %s/%s carries field manager %q, which is not the platform's — "+
				"this change was not made by a reconcile or by the API",
			finding.Kind, finding.Name, finding.Manager),
		Details: details,
	}
}

// publish keeps the singleton honest about where the access controls stand.
// Best-effort and quiet, exactly as the rescan sweep's is: the sweep's job is
// the sweep, and a status write that failed is not a reason to skip the next
// one.
func (s *AccessSweeper) publish(ctx context.Context, report AccessSweepReport) {
	status := &kitchenv1alpha1.AccessComplianceStatus{
		Reviewing:       report.Reviewing,
		OpenReview:      report.OpenReview,
		Identities:      int32(report.Identities), //nolint:gosec // a grant count is not a security boundary
		Orphaned:        int32(report.Orphans),    //nolint:gosec // ditto
		OutOfBandWrites: int32(report.OutOfBand),  //nolint:gosec // ditto
		Message:         report.Message,
	}
	if report.DueBy != nil {
		status.DueBy = &metav1.Time{Time: *report.DueBy}
	}
	if report.LastClosed != nil {
		status.LastClosed = &metav1.Time{Time: *report.LastClosed}
	}
	if report.LastOutOfBand != nil {
		status.LastOutOfBand = &metav1.Time{Time: *report.LastOutOfBand}
	}

	current := &kitchenv1alpha1.Kitchen{}
	if err := s.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, current); err != nil {
		return
	}
	if current.Status.Compliance == nil {
		current.Status.Compliance = &kitchenv1alpha1.ComplianceStatus{}
	}
	current.Status.Compliance.Access = status
	if err := s.Status().Update(ctx, current); err != nil {
		logf.FromContext(ctx).V(1).Info("the access status was not published", "reason", err.Error())
	}
}
