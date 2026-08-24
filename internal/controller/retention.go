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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/retention"
)

// The retention sweep (issue #140): the pass that makes expiry a fact somebody
// can be shown rather than a setting somebody configured.
//
// Enforcement is not this pass's job. Retention is enforced where the data
// lives — a TTL on each table, kept in step with the model by the Kitchen
// reconcile — and it would go on being enforced if this never ran. What this
// pass does is produce the **evidence** that it happened: once a day it
// measures every class against the horizon its own configuration puts there,
// records what it found in the audit log, and publishes the same numbers on
// the singleton.
//
// It is a Runnable rather than a reconciler for the reason the rescan sweep
// gives at length: a sweep must happen once per interval across the platform
// rather than once per replica, and a Runnable that declares
// NeedLeaderElection says so about itself. Two replicas each writing a daily
// deletion record would produce a log that reads as if the platform swept
// twice, which is a small lie about a thing this whole feature exists to be
// truthful about.
//
// # Why it deletes so little
//
// It drops the partitions it can attribute to exactly one class and no others
// (see clickhouse.retentionTargets). Almost always it finds none, because the
// store's own TTL merge has already dropped them — which is the intended
// division of labour and not a failure. What the sweep adds over the TTL is
// two things the TTL cannot do: it deletes the case the TTL provably will not
// (a table left on part-drop mode whose partitions are wholly expired but not
// yet merged), and it writes down what the state of every class was at a
// stated moment under a stated rule.

const (
	// retentionSweepInterval is how often the pass runs. Daily, because the
	// unit of every retention here is a day: sweeping more often would
	// produce records that differ by rounding, and the log's value is in
	// somebody being able to read one line per class per day and see the
	// window sliding.
	retentionSweepInterval = 24 * time.Hour

	// retentionStepInterval is how often the sweeper wakes to decide whether
	// a sweep is due. It is not the sweep interval: an operator that has
	// just been restarted, or has just been given a store, should not wait
	// most of a day to make its first record.
	retentionStepInterval = 10 * time.Minute

	// actorRetentionSweep attributes the deletion evidence. Like every other
	// controller actor it is the pass's own name, so that "who recorded
	// this" answers with something an operator can go and read.
	actorRetentionSweep = "retention"
)

// RetentionSweeper measures every retention class on an interval and records
// what it found.
type RetentionSweeper struct {
	Client client.Client

	// Audit is where the evidence goes. Unlike the decision paths this is
	// **not** fail-closed, and the asymmetry is deliberate: a decision the
	// log refused to record is a decision that must not be acted on, but a
	// measurement the log refused is a measurement, and refusing to take the
	// next one because the last could not be written would turn a store
	// outage into a permanent hole. A failed append is logged and the pass
	// goes on. May be nil, which records nothing.
	Audit *audit.Recorder

	// Now is the clock. Nil is time.Now; tests move it to make an interval
	// elapse without waiting for one.
	Now func() time.Time

	// StepInterval and SweepInterval override the constants above. Tests
	// alone set them.
	StepInterval  time.Duration
	SweepInterval time.Duration

	// lastSweep is when this process last completed a pass. It is process
	// state rather than object state on purpose: a leader that has just
	// taken over should sweep, and reading the last sweep off the singleton
	// would make it wait out the rest of the interval instead.
	lastSweep time.Time
}

// NeedLeaderElection makes the sweep a singleton across replicas.
func (s *RetentionSweeper) NeedLeaderElection() bool { return true }

func (s *RetentionSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *RetentionSweeper) sweepInterval() time.Duration {
	if s.SweepInterval > 0 {
		return s.SweepInterval
	}
	return retentionSweepInterval
}

// Start implements manager.Runnable. Like every other sweep here it never
// returns an error before the context ends: a store that cannot be reached is
// a thing the platform reports rather than dies of.
func (s *RetentionSweeper) Start(ctx context.Context) error {
	step := s.StepInterval
	if step <= 0 {
		step = retentionStepInterval
	}
	ticker := time.NewTicker(step)
	defer ticker.Stop()

	for {
		if _, err := s.SweepIfDue(ctx); err != nil {
			logf.FromContext(ctx).V(1).Info("the retention sweep could not complete", "reason", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// SweepIfDue runs a pass when one is due and does nothing otherwise.
func (s *RetentionSweeper) SweepIfDue(ctx context.Context) (RetentionSweepReport, error) {
	now := s.now()
	if !s.lastSweep.IsZero() && now.Sub(s.lastSweep) < s.sweepInterval() {
		return RetentionSweepReport{}, nil
	}
	report, err := s.SweepOnce(ctx)
	if err == nil {
		s.lastSweep = now
	}
	return report, err
}

// RetentionSweepReport is what one pass did, which is what a test asserts on.
type RetentionSweepReport struct {
	// Measured is how many classes were measured, Removed how many rows the
	// pass deleted itself, and Recorded whether the audit record was
	// appended.
	Measured int
	Removed  int64
	Recorded bool

	// Observations are the per-class answers, in the model's own order.
	Observations []clickhouse.RetentionObservation

	// Message explains a pass that measured nothing.
	Message string
}

// SweepOnce measures every class once, records the result and publishes it.
//
// It is exported because it is the unit of the pass: a test drives it directly
// rather than waiting on a ticker.
func (s *RetentionSweeper) SweepOnce(ctx context.Context) (RetentionSweepReport, error) {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return RetentionSweepReport{}, err
	}

	model := retention.Resolve(kitchen)
	store, err := s.store(ctx, kitchen)
	if err != nil {
		report := RetentionSweepReport{Message: err.Error()}
		s.publishStatus(ctx, model, nil, err.Error())
		return report, nil
	}

	now := s.now()
	observations := store.SweepRetention(ctx, model, now)

	report := RetentionSweepReport{Observations: observations}
	for _, observation := range observations {
		if observation.Measured() {
			report.Measured++
		}
		report.Removed += observation.Removed
	}
	report.Recorded = s.record(ctx, kitchen, model, observations, now)
	s.publishStatus(ctx, model, observations, "")
	return report, nil
}

// store resolves the telemetry store, or says why there is none.
func (s *RetentionSweeper) store(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) (*clickhouse.Client, error) {
	ref := kitchen.Spec.Observability.ClickHouse.SecretRef
	if ref == nil {
		return nil, fmt.Errorf("retention is configured but nothing enforces it: this installation has " +
			"no telemetry store, so there is no data to keep and nothing to record about keeping it. " +
			"Set spec.observability.clickhouse.secretRef")
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	cfg, err := clickhouse.ConfigFromSecret(secret)
	if err != nil {
		return nil, err
	}
	return clickhouse.New(cfg), nil
}

// record appends the deletion evidence.
//
// One record per pass rather than one per class, because the pass is the
// event: nine records saying "swept, found nothing to remove" would be nine
// lines of noise a day for the same fact. The details carry every class, so
// the record is complete without being repetitive — and it goes in through the
// same chained, single-appender log every other piece of evidence does, which
// is the point of the instruction not to invent a second ledger for it.
//
// The audit class is in the details like any other, and the record itself
// lives in the audit table it is describing. That is a circularity worth
// naming rather than engineering around: the evidence of audit expiry ages out
// under the audit retention, which is exactly what the floor bounds. A record
// of what was deleted 400 days ago is not evidence anybody is looking for; a
// record of what was deleted last month is, and the floor guarantees it.
func (s *RetentionSweeper) record(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	model retention.Model,
	observations []clickhouse.RetentionObservation,
	now time.Time,
) bool {
	if s.Audit == nil {
		return false
	}

	classes := make([]any, 0, len(observations))
	var removed int64
	for _, observation := range observations {
		entry := map[string]any{
			"class":   string(observation.Class),
			"table":   observation.Table,
			"days":    observation.Days,
			"horizon": observation.Horizon.UTC().Format(time.RFC3339),
			"rows":    observation.Rows,
			"expired": observation.Expired,
			"removed": observation.Removed,
			"source":  model.Source(observation.Class),
		}
		if observation.Oldest != nil {
			entry["oldest"] = observation.Oldest.UTC().Format(time.RFC3339)
		}
		if observation.Error != "" {
			entry["error"] = observation.Error
		}
		classes = append(classes, entry)
		removed += observation.Removed
	}

	transition := audit.Transition{
		Object:     kitchen,
		Kind:       audit.KindRetention,
		Operation:  clickhouse.AuditDelete,
		Controller: actorRetentionSweep,
		Reason:     describeSweep(observations, removed),
		Details: map[string]any{
			"change":  audit.ChangeRetentionSweep,
			"sweptAt": now.UTC().Format(time.RFC3339),
			"removed": removed,
			"classes": classes,
		},
	}
	if err := s.Audit.Record(ctx, transition); err != nil {
		logf.FromContext(ctx).V(1).Info("the retention sweep was not recorded", "reason", err.Error())
		return false
	}
	return true
}

// describeSweep is the one line a person reads in the log.
func describeSweep(observations []clickhouse.RetentionObservation, removed int64) string {
	oldest := ""
	for _, observation := range observations {
		if observation.Oldest == nil {
			continue
		}
		stamp := observation.Oldest.UTC().Format("2006-01-02")
		if oldest == "" || stamp < oldest {
			oldest = stamp
		}
	}
	sentence := fmt.Sprintf("retention was swept across %d classes", len(observations))
	if removed > 0 {
		sentence += fmt.Sprintf(" and removed %d expired rows", removed)
	}
	if oldest != "" {
		sentence += "; nothing the platform holds is older than " + oldest
	}
	return sentence
}

// publishStatus keeps the singleton honest about what is actually retained.
// Best effort and quiet, like every other sweep's: the pass's job is the pass.
func (s *RetentionSweeper) publishStatus(
	ctx context.Context,
	model retention.Model,
	observations []clickhouse.RetentionObservation,
	message string,
) {
	current := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, current); err != nil {
		return
	}
	current.Status.Retention = RetentionStatusFrom(model, observations, message, ptr.To(metav1.NewTime(s.now())))
	if err := s.Client.Status().Update(ctx, current); err != nil {
		logf.FromContext(ctx).V(1).Info("the retention status was not published", "reason", err.Error())
	}
}

// RetentionStatusFrom turns a model and a pass's observations into the status
// the singleton publishes.
//
// It is exported and pure so that the Kitchen reconcile can publish the
// configured half — every class, its days and where the number came from —
// before any sweep has run. An installation should be able to see what its
// retention *is* within a reconcile of setting it, rather than within a day.
func RetentionStatusFrom(
	model retention.Model,
	observations []clickhouse.RetentionObservation,
	message string,
	sweptAt *metav1.Time,
) *kitchenv1alpha1.RetentionStatus {
	measured := make(map[retention.Class]clickhouse.RetentionObservation, len(observations))
	for _, observation := range observations {
		measured[observation.Class] = observation
	}

	classes := make([]kitchenv1alpha1.RetentionClassStatus, 0, len(model.Classes()))
	for _, class := range retention.Sorted(model.Classes()) {
		setting, ok := model.Setting(class)
		if !ok {
			continue
		}
		entry := kitchenv1alpha1.RetentionClassStatus{
			Class:  string(class),
			Days:   setting.Days,
			Source: setting.Source,
		}
		observation, seen := measured[class]
		switch {
		case !seen:
			entry.Message = message
		case observation.Error != "":
			entry.Message = observation.Error
		default:
			entry.Enforced = true
			entry.Rows = observation.Rows
			entry.Expired = observation.Expired
			entry.Removed = observation.Removed
			if observation.Oldest != nil {
				entry.Oldest = ptr.To(metav1.NewTime(observation.Oldest.UTC()))
			}
		}
		classes = append(classes, entry)
	}

	status := &kitchenv1alpha1.RetentionStatus{
		Classes:              classes,
		AuditFloorOverridden: model.AuditBelowFloor(),
		Message:              message,
	}
	if len(observations) > 0 {
		status.LastSweep = sweptAt
	}
	return status
}

// applyRetentionStatus refreshes the configured half of status.retention on
// every reconcile: every class, the days in force, and where that number came
// from.
//
// It exists because the sweep runs daily and configuration changes do not. An
// operator who has just raised the container-log retention should see the new
// number on the object within a reconcile; waiting most of a day for the next
// sweep to report it would make the status read as if the change had not
// landed.
//
// What the last sweep measured is carried across **only for a class whose
// number has not moved**. A measurement is a claim about a horizon, so a
// horizon that has just changed invalidates it — reporting yesterday's oldest
// row beside today's new retention would be reporting a claim nothing has
// checked.
func applyRetentionStatus(kitchen *kitchenv1alpha1.Kitchen, model retention.Model, message string) {
	previous := map[string]kitchenv1alpha1.RetentionClassStatus{}
	var lastSweep *metav1.Time
	if existing := kitchen.Status.Retention; existing != nil {
		lastSweep = existing.LastSweep
		for _, entry := range existing.Classes {
			previous[entry.Class] = entry
		}
	}

	classes := make([]kitchenv1alpha1.RetentionClassStatus, 0, len(model.Classes()))
	for _, class := range retention.Sorted(model.Classes()) {
		setting, ok := model.Setting(class)
		if !ok {
			continue
		}
		entry := kitchenv1alpha1.RetentionClassStatus{
			Class:   string(class),
			Days:    setting.Days,
			Source:  setting.Source,
			Message: message,
		}
		if was, seen := previous[string(class)]; seen && was.Days == setting.Days {
			entry.Enforced = was.Enforced
			entry.Rows = was.Rows
			entry.Oldest = was.Oldest
			entry.Expired = was.Expired
			entry.Removed = was.Removed
			if message == "" {
				entry.Message = was.Message
			}
		}
		classes = append(classes, entry)
	}

	kitchen.Status.Retention = &kitchenv1alpha1.RetentionStatus{
		Classes:              classes,
		AuditFloorOverridden: model.AuditBelowFloor(),
		LastSweep:            lastSweep,
		Message:              message,
	}
}

// describeTelemetryRetention is the TelemetrySchemaReady condition's message:
// the retention in force, grouped so that the common case — every class the
// same — is one number rather than nine.
func describeTelemetryRetention(model retention.Model) string {
	byDays := map[int32][]string{}
	for _, class := range model.Classes() {
		if class == retention.ClassAudit {
			// The audit log is not part of the telemetry schema and never
			// has been: it answers to the compliance condition instead.
			continue
		}
		byDays[model.Days(class)] = append(byDays[model.Days(class)], string(class))
	}
	if len(byDays) == 1 {
		for days := range byDays {
			return fmt.Sprintf("telemetry schema is in place, retaining %d days", days)
		}
	}

	parts := make([]string, 0, len(byDays))
	for days, classes := range byDays {
		sort.Strings(classes)
		parts = append(parts, fmt.Sprintf("%s %dd", strings.Join(classes, "/"), days))
	}
	sort.Strings(parts)
	return "telemetry schema is in place, retaining " + strings.Join(parts, ", ")
}
