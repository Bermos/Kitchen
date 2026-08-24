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
	"fmt"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
	"github.com/Bermos/Kitchen/internal/version"
)

// The continuous re-evaluation pass (issue #134): the difference between a
// gate and a control.
//
// A promotion asks "may this artifact land here" once, against the world as it
// was that afternoon. This asks the same question, of the artifact that is
// already running, against the world as it is today — and it asks it through
// the same PolicyEvaluator, so the two cannot drift apart in the one direction
// that would matter. What it needs from the artifact is nothing: the bill of
// materials the build already attested is matched against a current
// vulnerability database, the findings are signed onto the artifact's digest
// beside everything else, and the environment's own bar is re-run over the
// enlarged evidence set. **No rebuild and no redeploy**, which is the first
// acceptance criterion and also the reason the whole scheme is arranged around
// an artifact that is built once.
//
// # Why a Runnable and not a reconciler
//
// The obvious shape is an Environment reconciler with a RequeueAfter of the
// interval, and it is the shape this repository reaches for first (see the
// Connection and Exception reconcilers). It is the wrong one here for two
// reasons, and both are about the word "sweep":
//
//   - A rescan must happen **once per interval across the platform**, not once
//     per replica. A reconciler's requeue is leader-elected today only because
//     the whole manager is; a Runnable that declares NeedLeaderElection says
//     so about itself, which is the honest place for the claim.
//   - The pass has a **platform-wide budget**. Concurrency is bounded across
//     every environment at once — the first sweep after an upgrade has two
//     hundred environments due at the same instant, and two hundred scanner
//     pods pulling two hundred images is a denial of service the platform
//     performed on itself. A per-object requeue has no vantage point from
//     which to count.
//
// So this is the ticker-plus-Runnable pattern the event recorder and the flow
// collector already use, including its rule: Start never returns a non-nil
// error before the context ends. A scan that cannot be run must not take the
// operator down with it.
//
// The per-(environment, release) *state* still lives on the object, on
// status.rescan, and the sweep is stateless between passes. That is what makes
// the interval self-spreading: it is counted from each pair's own last
// finished scan, so an estate scanned four at a time stays four at a time
// rather than re-converging on one minute of the day.
//
// # Exception expiry, and no expiry engine
//
// This pass is also the only thing that judges exception expiry, and it needs
// no machinery for it: ActiveExceptionsFor excludes an expired grant, the
// rules it waived fire unwaived, and the verdict is Blocked. What arrives
// here is the *consequence* — a visible, recorded non-compliance, and, for a
// grant that asked for it, the rollback.
//
// # What it does about spec.autoRollback
//
// The exception reconciler deliberately left the acting to this pass, and the
// answer is: roll back, narrowly. Only when the verdict is Blocked, only when
// an expired-and-unresolved exception covering this pair asked for it, only
// when that exception waived a rule that is now firing unwaived, and only when
// there is a previous release to go back to. Anything looser would turn a
// grant somebody took out to ship a hotfix into a mechanism that yanks a
// running workload for an unrelated reason, which is exactly the failure this
// suite's design rules call worse than the disease.

const (
	// rescanStepInterval is how often the sweep looks at the estate. It is
	// not the rescan interval: a step decides whether anything is *due*,
	// starts what it may, and collects what has finished. A minute is cheap
	// (one cached list) and keeps a finished scanner pod from sitting on the
	// concurrency budget.
	rescanStepInterval = time.Minute

	// rescanJobTTLSeconds is how long a finished scanner Job sticks around.
	// Its output is in the registry by then, so this is a window for the
	// sweep to read the termination message and for a person to look.
	rescanJobTTLSeconds = 3600

	// defaultRescanTimeoutSeconds bounds one scan when the scanner names no
	// timeout. It mirrors the CRD default, which a singleton written before
	// the field existed does not carry.
	defaultRescanTimeoutSeconds = 900

	// labelRescan marks the Jobs this pass creates, so they are told apart
	// from a build's and a gate's without parsing names.
	labelRescan = "kitchen.bermos.dev/rescan"

	// The pod's layout. Two volumes: one the bill of materials is fetched
	// onto and the scanner reads, one the scanner writes its findings to and
	// the publisher reads.
	rescanSBOMDir      = "/kitchen/sbom"
	rescanSBOMFile     = rescanSBOMDir + "/sbom.json"
	rescanFindingsDir  = "/kitchen/findings"
	rescanFindingsFile = rescanFindingsDir + "/findings.json"
	rescanSnapshotFile = rescanFindingsDir + "/data-snapshot.txt"
)

// rescanReport is what cmd/rescan leaves on the pod: where the findings are,
// which bill of materials they are about, and whatever the scanner was able
// to say about its own database.
type rescanReport struct {
	Scanner      string `json:"scanner"`
	Blob         string `json:"blob"`
	Bytes        int    `json:"bytes"`
	DataSnapshot string `json:"dataSnapshot,omitempty"`
	SBOM         string `json:"sbom,omitempty"`
	SBOMDigest   string `json:"sbomDigest,omitempty"`
	FinishedAt   string `json:"finishedAt"`
	Error        string `json:"error,omitempty"`
}

// RescanSweeper walks every currently-deployed release on an interval.
type RescanSweeper struct {
	Client client.Client

	// Audit is waited on and fail-closed, like every decision path: a
	// re-evaluation the log refuses to record is one this pass does not act
	// on. May be nil.
	Audit *audit.Recorder

	// Activity feeds the dashboard's recent-activity feed. May be nil.
	Activity *activity.Recorder

	// OperatorImage is this operator's own image, which the scanner pod runs
	// two more of its binaries from. It is the same image the quality gate
	// publisher runs and reaches the process through the same flag, because a
	// pod cannot read its own image back. Without it, rescan is configured
	// and never runs.
	OperatorImage string

	// Stores, Attesters and EvidenceReaders are the same three seams the
	// promotion path has, for the same reasons. Nil means the real thing.
	Stores          DecisionStoreFactory
	Attesters       AttesterFactory
	EvidenceReaders EvidenceSetReaderFactory

	// Now is the clock. Nil is time.Now; tests move it to make an interval
	// elapse without waiting for one.
	Now func() time.Time

	// StepInterval overrides rescanStepInterval. Tests alone set it.
	StepInterval time.Duration
}

// NeedLeaderElection makes the sweep a singleton. Two replicas would scan
// every artifact twice, sign both results, and store two decisions per
// interval — an estate that looks twice as busy and evidence that looks like
// it disagrees with itself.
func (s *RescanSweeper) NeedLeaderElection() bool { return true }

func (s *RescanSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Promotions are read here (rollBackForExpiry's fifth guard) under the grant
// the promotion reconciler's own markers already carry, so there is no marker
// for them below: a narrower one would generate a second rule for the same
// resource rather than adding anything.
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=kitchens/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// Start implements manager.Runnable. Like the flow collector and the event
// recorder it never returns an error before the context ends: a scanner that
// will not run, a registry that cannot be reached and a store that is down are
// all conditions the platform reports rather than dies of.
func (s *RescanSweeper) Start(ctx context.Context) error {
	step := s.StepInterval
	if step <= 0 {
		step = rescanStepInterval
	}
	// The configuration is re-read on every step rather than watched: a step
	// is already a read of the singleton, and a pass that noticed a config
	// change only on restart would leave an operator who has just turned the
	// feature on watching nothing happen.
	ticker := time.NewTicker(step)
	defer ticker.Stop()

	for {
		if _, err := s.SweepOnce(ctx); err != nil {
			logf.FromContext(ctx).V(1).Info("the rescan sweep could not complete", "reason", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// RescanSweepReport is what one pass did, which is what the singleton's status
// reports and what a test asserts on.
type RescanSweepReport struct {
	// Running is whether the pass is on and has something to run.
	Running bool
	// Environments is how many deployed pairs were considered, Started how
	// many scans this pass began, Scanning how many were in flight when it
	// finished, and Evaluated how many decisions it recorded.
	Environments int
	Started      int
	Scanning     int
	Evaluated    int
	// Message explains a pass that is not running.
	Message string
}

// SweepOnce advances every deployed (environment, release) pair by one step:
// it starts the scans that are due and the budget allows, collects the ones
// that have finished, and re-evaluates each finished one through the
// environment's own bar.
//
// It is exported because it is the unit of the pass: a test drives it
// directly rather than waiting on a ticker, and so does anything that ever
// wants to ask for a sweep now.
func (s *RescanSweeper) SweepOnce(ctx context.Context) (RescanSweepReport, error) {
	log := logf.FromContext(ctx)

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return RescanSweepReport{}, client.IgnoreNotFound(err)
	}

	report := s.check(kitchen)
	if !report.Running {
		s.publishStatus(ctx, report)
		return report, nil
	}
	spec := kitchen.Spec.Compliance.Rescan
	scanner := *spec.Scanner

	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := s.Client.List(ctx, environments, client.InNamespace(PlatformNamespace)); err != nil {
		return report, err
	}
	// A stable order so that a platform permanently at its concurrency limit
	// still gets round to every environment rather than favouring whichever
	// the API server listed first today.
	items := environments.Items
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	budget := spec.EffectiveConcurrency()
	for i := range items {
		env := &items[i]
		if env.Spec.ReleaseRef.Name == "" || !env.DeletionTimestamp.IsZero() {
			continue
		}
		report.Environments++
		if inFlight(env) {
			budget--
		}
	}

	for i := range items {
		env := &items[i]
		if env.Spec.ReleaseRef.Name == "" || !env.DeletionTimestamp.IsZero() {
			continue
		}
		outcome, err := s.advance(ctx, kitchen, scanner, spec.EffectiveInterval(), env, &budget)
		if err != nil {
			// One environment's failure is not the sweep's: the next pass
			// picks it up where it left off, and the others are still due.
			log.Error(err, "the rescan of an environment could not be advanced",
				"environment", env.Name, "release", env.Spec.ReleaseRef.Name)
			continue
		}
		report.Started += outcome.started
		report.Scanning += outcome.scanning
		report.Evaluated += outcome.evaluated
	}

	s.publishStatus(ctx, report)
	return report, nil
}

// check answers whether the pass has anything to do, and words the reason when
// it does not. The three refusals are all configuration rather than faults,
// which is why each is a message on the singleton and none is an error.
func (s *RescanSweeper) check(kitchen *kitchenv1alpha1.Kitchen) RescanSweepReport {
	compliance := kitchen.Spec.Compliance
	switch {
	case !compliance.Rescan.Enabled:
		return RescanSweepReport{Message: "continuous re-evaluation is off"}
	case compliance.Rescan.Scanner == nil || compliance.Rescan.Scanner.Image == "":
		return RescanSweepReport{Message: "continuous re-evaluation is on and no scanner is configured, " +
			"so nothing is being re-evaluated: set compliance.rescan.scanner.image"}
	case !compliance.Attestation.Enabled:
		// The same argument quality gates make: findings nothing will sign
		// are a blob in a registry no policy can ever trust, produced at the
		// cost of a pod in an application's namespace.
		return RescanSweepReport{Message: "continuous re-evaluation is on and attestation is off, " +
			"so a scan's findings could not be signed and nothing is being re-evaluated"}
	case s.OperatorImage == "":
		return RescanSweepReport{Message: "continuous re-evaluation is on and the operator's own image is " +
			"not known to it, so no scanner pod can be assembled"}
	}
	return RescanSweepReport{Running: true}
}

// rescanOutcome is one environment's contribution to a pass.
type rescanOutcome struct {
	started   int
	scanning  int
	evaluated int
}

// inFlight reports whether an environment currently has a scanner pod out.
func inFlight(env *kitchenv1alpha1.Environment) bool {
	return env.Status.Rescan != nil &&
		env.Status.Rescan.Phase == kitchenv1alpha1.RescanScanning &&
		env.Status.Rescan.Release == env.Spec.ReleaseRef.Name
}

// advance moves one environment by one step.
func (s *RescanSweeper) advance(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	scanner kitchenv1alpha1.VulnerabilityScannerSpec,
	interval time.Duration,
	env *kitchenv1alpha1.Environment,
	budget *int,
) (rescanOutcome, error) {
	state := env.Status.Rescan
	if state != nil && state.Release != env.Spec.ReleaseRef.Name {
		// The release moved. The answer was about the artifact that was
		// running; carrying it forward would be reporting a scan of something
		// that was never scanned.
		state = nil
		env.Status.Rescan = nil
	}

	if state != nil && state.Phase == kitchenv1alpha1.RescanScanning {
		return s.collect(ctx, kitchen, scanner, env)
	}

	if state != nil && state.FinishedAt != nil && s.now().Sub(state.FinishedAt.Time) < interval {
		return rescanOutcome{}, nil
	}
	if *budget <= 0 {
		// Due, and waiting its turn. Nothing is recorded: the pair is due
		// again next step, and a status field saying "queued" would be a
		// state the sweep would then have to keep honest.
		return rescanOutcome{}, nil
	}
	if err := s.start(ctx, scanner, env); err != nil {
		return rescanOutcome{}, err
	}
	*budget--
	return rescanOutcome{started: 1, scanning: 1}, nil
}

// start creates the scanner Job for one environment's deployed release and
// stamps the environment Scanning.
func (s *RescanSweeper) start(
	ctx context.Context,
	scanner kitchenv1alpha1.VulnerabilityScannerSpec,
	env *kitchenv1alpha1.Environment,
) error {
	project := &kitchenv1alpha1.Project{}
	if err := s.Client.Get(ctx, types.NamespacedName{
		Namespace: env.Namespace, Name: env.Spec.ProjectRef.Name,
	}, project); err != nil {
		return err
	}
	release := &kitchenv1alpha1.Release{}
	if err := s.Client.Get(ctx, types.NamespacedName{
		Namespace: env.Namespace, Name: env.Spec.ReleaseRef.Name,
	}, release); err != nil {
		return client.IgnoreNotFound(err)
	}
	repository, digest, byDigest := strings.Cut(release.Spec.Image, "@")
	if !byDigest || digest == "" {
		return s.record(ctx, env, kitchenv1alpha1.EnvironmentRescanStatus{
			Phase:      kitchenv1alpha1.RescanFailed,
			Release:    release.Name,
			FinishedAt: ptr.To(metav1.NewTime(s.now())),
			Message: "the deployed release names no artifact digest, so there is nothing " +
				"a scan could be about",
		})
	}
	artifact := attestation.ArtifactRef(repository, digest)

	name := rescanJobName(env.Name, release.Name)
	appNS := appNamespace(project.Name)
	job := rescanJob(name, appNS, project, env, release, scanner, artifact,
		registrySecretName(project.Spec.Registry.ConnectionRef.Name), s.OperatorImage)

	if err := s.Client.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	logf.FromContext(ctx).Info("rescan started",
		"environment", env.Name, "release", release.Name, "artifact", artifact)
	return s.record(ctx, env, kitchenv1alpha1.EnvironmentRescanStatus{
		Phase:     kitchenv1alpha1.RescanScanning,
		Release:   release.Name,
		Artifact:  artifact,
		JobName:   name,
		StartedAt: ptr.To(metav1.NewTime(s.now())),
		Message:   "matching the artifact's bill of materials against " + scanner.Image,
	})
}

// collect looks at an in-flight scan and, when it has finished, turns it into
// signed evidence and a recorded decision.
func (s *RescanSweeper) collect(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	scanner kitchenv1alpha1.VulnerabilityScannerSpec,
	env *kitchenv1alpha1.Environment,
) (rescanOutcome, error) {
	state := env.Status.Rescan
	project := &kitchenv1alpha1.Project{}
	if err := s.Client.Get(ctx, types.NamespacedName{
		Namespace: env.Namespace, Name: env.Spec.ProjectRef.Name,
	}, project); err != nil {
		return rescanOutcome{}, err
	}
	appNS := appNamespace(project.Name)

	job := &batchv1.Job{}
	switch err := s.Client.Get(ctx, types.NamespacedName{Namespace: appNS, Name: state.JobName}, job); {
	case apierrors.IsNotFound(err):
		// Collected by its TTL before the sweep read it, or deleted by hand.
		// Nothing is known about the artifact, which is what Failed means.
		return rescanOutcome{}, s.record(ctx, env, s.failed(state,
			"the scan job is gone, so its findings were never read back"))
	case err != nil:
		return rescanOutcome{}, err
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case failed:
		if reason := s.podFailure(ctx, appNS, state.JobName); reason != "" {
			message = reason
		}
		return rescanOutcome{}, s.record(ctx, env, s.failed(state, "the scan did not run: "+message))
	case !complete:
		return rescanOutcome{scanning: 1}, nil
	}

	report, found := s.report(ctx, appNS, state.JobName)
	switch {
	case !found:
		return rescanOutcome{}, s.record(ctx, env, s.failed(state,
			"the scan finished and left no report of where its findings went"))
	case report.Error != "":
		return rescanOutcome{}, s.record(ctx, env, s.failed(state,
			"the scan produced no findings: "+report.Error))
	}

	return s.publish(ctx, kitchen, scanner, project, env, report)
}

// publish reads the findings back, signs them onto the artifact, re-runs the
// environment's bar over the enlarged evidence set, and records the decision.
func (s *RescanSweeper) publish(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	scanner kitchenv1alpha1.VulnerabilityScannerSpec,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	report rescanReport,
) (rescanOutcome, error) {
	log := logf.FromContext(ctx)
	state := env.Status.Rescan
	repository, _, _ := strings.Cut(state.Artifact, "@")

	attester, err := s.attester(ctx, project)
	if err != nil {
		return rescanOutcome{}, err
	}
	raw, err := attester.Blob(ctx, repository, report.Blob)
	if err != nil {
		return rescanOutcome{}, s.record(ctx, env, s.failed(state,
			"the scan's findings could not be read back: "+err.Error()))
	}

	findings, fromReport := normalizeFindings(scanner.Format, raw)
	scannedAt := s.now()
	if stamp, err := time.Parse(time.RFC3339, report.FinishedAt); err == nil {
		scannedAt = stamp.UTC()
	}
	snapshot := dataSnapshot(report.DataSnapshot, fromReport, scanner, scannedAt)

	// The findings become evidence here, where the key is — never in the pod.
	//
	// And they are attached *before* the evaluation, which is the ordering the
	// whole pass turns on: the evaluator materializes the artifact's evidence
	// from the registry, so a scan signed after the evaluation would be judged
	// a day late, by tomorrow's sweep, against tomorrow's database.
	manifest, err := s.attest(ctx, kitchen, attester, scanner, state, report,
		findings, raw, snapshot, scannedAt)
	if err != nil {
		// The scan ran; what is missing is the signature, which is a
		// different failure and is recorded as one. Re-evaluating over
		// evidence the platform could not attach would judge the artifact on
		// findings no policy can see.
		return rescanOutcome{}, s.record(ctx, env, s.failed(state,
			"the scan's findings were not attested: "+err.Error()))
	}
	s.indexEvidence(ctx, env, state, manifest)

	release := &kitchenv1alpha1.Release{}
	if err := s.Client.Get(ctx, types.NamespacedName{
		Namespace: env.Namespace, Name: state.Release,
	}, release); err != nil {
		return rescanOutcome{}, client.IgnoreNotFound(err)
	}

	at := s.now()
	evaluation, err := s.evaluator().Evaluate(ctx, EvaluationRequest{
		Kind:         policy.KindRescan,
		At:           at,
		DataSnapshot: snapshot,
		Kitchen:      kitchen,
		Project:      project,
		Environment:  env,
		Release:      release,
	})
	if err != nil {
		return rescanOutcome{}, err
	}
	if evaluation.Refusal != "" {
		return rescanOutcome{}, s.record(ctx, env, s.failed(state,
			"the environment's bar could not be re-evaluated: "+evaluation.Refusal))
	}

	// Record before acting, exactly as a promotion does: the audit record
	// gates the verdict and the store keeps it replayable. A refused record
	// leaves the environment Scanning, so the next pass retries rather than
	// acting on a decision nothing wrote down.
	recorder := &DecisionRecorder{Client: s.Client, Audit: s.Audit, Stores: s.Stores, Attesters: s.Attesters}
	decisionID, err := recorder.Record(ctx, kitchen, env, evaluation.Input, evaluation.Result, evaluation.Bundle)
	if err != nil {
		return rescanOutcome{}, err
	}

	finished := metav1.NewTime(scannedAt)
	next := kitchenv1alpha1.EnvironmentRescanStatus{
		Phase:        kitchenv1alpha1.RescanEvaluated,
		Release:      state.Release,
		Artifact:     state.Artifact,
		JobName:      state.JobName,
		StartedAt:    state.StartedAt,
		FinishedAt:   &finished,
		DataSnapshot: snapshot,
		Findings:     int32(len(findings)), //nolint:gosec // a finding count is not a security boundary
		Verdict:      evaluation.Result.Verdict,
		UnmetRules:   unmetRuleIDs(evaluation.Result),
		DecisionID:   decisionID,
		Message:      evaluation.Message,
	}
	if err := s.record(ctx, env, next); err != nil {
		return rescanOutcome{}, err
	}
	log.Info("rescan evaluated", "environment", env.Name, "release", state.Release,
		"verdict", evaluation.Result.Verdict, "findings", len(findings),
		"dataSnapshot", snapshot, "decision", decisionID)

	if evaluation.Result.Verdict == policy.VerdictBlocked {
		if err := s.rollBackForExpiry(ctx, project, env, release, evaluation.Result, at, decisionID); err != nil {
			return rescanOutcome{evaluated: 1}, err
		}
	}
	return rescanOutcome{evaluated: 1}, nil
}

// dataSnapshot decides what the scan is recorded as having been produced
// against, in the order of how much the platform can stand behind:
//
//  1. what the scanner wrote to KITCHEN_DATA_SNAPSHOT, which is the scanner's
//     own word about its own database;
//  2. what the report itself carries — Grype's `descriptor.db` is the only one
//     of the three understood formats that says;
//  3. the scanner and the day, which is not a database identifier and does not
//     pretend to be. It is prefixed `unpinned:` for exactly that reason: a
//     reader must be able to tell a snapshot that reproduces a scan from one
//     that merely dates it.
func dataSnapshot(
	fromScanner, fromReport string,
	scanner kitchenv1alpha1.VulnerabilityScannerSpec,
	at time.Time,
) string {
	if snapshot := strings.TrimSpace(fromScanner); snapshot != "" {
		return snapshot
	}
	if snapshot := strings.TrimSpace(fromReport); snapshot != "" {
		return snapshot
	}
	identity := scanner.Version
	if identity == "" {
		identity = scanner.Image
	}
	return fmt.Sprintf("unpinned:%s@%s", identity, at.Format("2006-01-02"))
}

// attest signs the vulnerability scan onto the artifact's digest.
//
// The predicate carries both readings of the scan, and the division is the
// point: `findings` is the platform's normalization, which is the shape the
// policy engine judges, and `report` is the scanner's own bytes, unmodified.
// Signing only the first would put the platform's name on a rewriting of
// somebody else's claim; signing only the second would leave every bundle to
// parse three scanners for itself.
//
// There is no pass field, for the same reason a quality gate has none.
func (s *RescanSweeper) attest(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	attester ArtifactAttester,
	scanner kitchenv1alpha1.VulnerabilityScannerSpec,
	state *kitchenv1alpha1.EnvironmentRescanStatus,
	report rescanReport,
	findings []vulnerabilityFinding,
	raw []byte,
	snapshot string,
	scannedAt time.Time,
) (string, error) {
	signer, err := SigningKeyFor(ctx, s.Client, kitchen)
	if err != nil {
		return "", err
	}
	if signer == nil {
		return "", fmt.Errorf("the platform holds no signing key, so the findings were left unsigned")
	}
	repository, digest, _ := strings.Cut(state.Artifact, "@")

	predicate := map[string]any{
		"scanner": map[string]any{
			"name":    scanner.Name,
			"image":   scanner.Image,
			"version": scanner.Version,
		},
		"scannedAt":    scannedAt.Format(time.RFC3339),
		"dataSnapshot": snapshot,
		"findings":     findings,
		"findingCount": len(findings),
		"platform":     map[string]any{"name": "kitchen", "version": version.Version},
	}
	if scanner.Format != "" {
		predicate["format"] = scanner.Format
	}
	if report.SBOM != "" {
		predicate["sbom"] = map[string]any{
			"predicateType": report.SBOM,
			"envelope":      report.SBOMDigest,
		}
	}
	if json.Valid(raw) {
		predicate["report"] = json.RawMessage(raw)
	} else {
		predicate["report"] = string(raw)
	}

	statement, err := attestation.NewStatement(
		repository, digest, attestation.PredicateVulnerabilityScan, predicate)
	if err != nil {
		return "", err
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		return "", err
	}
	manifest, err := attester.Attach(ctx, state.Artifact, envelope, statement.PredicateType)
	if err != nil {
		return "", fmt.Errorf("the findings could not be attached to the artifact: %w", err)
	}
	return manifest, nil
}

// indexEvidence keeps the Build's evidence index naming the newest scan.
//
// The index is exactly that — an index — and the registry is where the
// evidence is. So a rescan replaces the vulnerability-scan entry rather than
// appending one: a daily scan appending forever would turn a pointer into a
// log, and the log already exists in the registry and in the decision store.
// The source is `platform` because the platform ran the scanner, which is what
// that word has always meant here.
//
// Best-effort: the evidence is attached and the decision is about to be
// recorded either way, and a build that has been pruned has no index to keep.
func (s *RescanSweeper) indexEvidence(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	state *kitchenv1alpha1.EnvironmentRescanStatus,
	manifest string,
) {
	release := &kitchenv1alpha1.Release{}
	if err := s.Client.Get(ctx, types.NamespacedName{
		Namespace: env.Namespace, Name: state.Release,
	}, release); err != nil {
		return
	}
	build := &kitchenv1alpha1.Build{}
	if err := s.Client.Get(ctx, types.NamespacedName{
		Namespace: env.Namespace, Name: release.Spec.BuildRef.Name,
	}, build); err != nil {
		return
	}
	artifact := build.Status.Artifact
	if artifact == nil || artifact.Digest == "" ||
		!strings.HasSuffix(state.Artifact, "@"+artifact.Digest) {
		return
	}
	entry := kitchenv1alpha1.ArtifactEvidence{
		PredicateType: attestation.PredicateVulnerabilityScan,
		Manifest:      manifest,
		Source:        sourcePlatform,
	}
	for index, existing := range artifact.Evidence {
		if existing.PredicateType != entry.PredicateType {
			continue
		}
		if existing.Manifest == manifest {
			return
		}
		artifact.Evidence[index] = entry
		if err := s.Client.Status().Update(ctx, build); err != nil {
			logf.FromContext(ctx).V(1).Info("the artifact's evidence index was not updated",
				"build", build.Name, "reason", err.Error())
		}
		return
	}
	artifact.Evidence = append(artifact.Evidence, entry)
	if err := s.Client.Status().Update(ctx, build); err != nil {
		logf.FromContext(ctx).V(1).Info("the artifact's evidence index was not updated",
			"build", build.Name, "reason", err.Error())
	}
}

// rollBackForExpiry acts on spec.autoRollback, and only on it.
//
// The exception reconciler carries the flag and deliberately does not act on
// it: rolling a workload back is a decision to make with the re-evaluation in
// hand, which is here. The conditions are all five of: the verdict is Blocked,
// an exception covering this pair has expired without being resolved, it asked
// for the rollback, it waived a rule that is now firing unwaived, and **this
// release actually went out under it**. A grant that expired without ever
// mattering leaves the workload where it is.
//
// The fifth condition is the one that makes the other four narrow rather than
// merely specific. `Covers` answers about a *pair* and matches any release when
// the grant names none; `EffectivePhase` answers `Expired` forever, because the
// register deliberately retains a grant after it ends. So without it, a 24-hour
// waiver taken out in March to ship a hotfix and never resolved is still an
// armed rollback in August: an unrelated release deployed since, blocked by an
// unrelated CVE that happens to fire the same rule id, retreats production
// citing a five-month-old grant nobody involved has heard of. `status.usedBy`
// is the honest link — the register's own record of which promotions leaned on
// the grant — and a grant this release never leaned on has no business moving
// it. That is the whole difference between acting on the consequence of an
// expiry and having a mechanism that yanks a running workload for an unrelated
// reason.
//
// It fails safe in both awkward directions. A Promotion that has been pruned
// cannot be matched, so the rollback does not happen: nothing new going out is
// the default consequence of an expiry, and a workload left running is the
// recoverable outcome. A release promoted *before* the grant existed was never
// waived by it and is not in `usedBy` either, which is the same answer for the
// same reason.
//
// The audit record comes first and is fail-closed, like every move this
// platform makes on its own initiative — which means an audit log that refuses
// leaves the workload running until the next interval. That is the safe
// direction: the default consequence of an expiry is that nothing new goes
// out, not that something running is yanked.
func (s *RescanSweeper) rollBackForExpiry(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	result policy.Result,
	at time.Time,
	decisionID string,
) error {
	list := &kitchenv1alpha1.ExceptionList{}
	if err := s.Client.List(ctx, list, client.InNamespace(env.Namespace)); err != nil {
		return err
	}
	unmet := unmetRuleIDs(result)

	for i := range list.Items {
		exception := &list.Items[i]
		if !exception.Spec.AutoRollback ||
			!exception.Covers(project.Name, env.Name, release.Name) ||
			exception.EffectivePhase(at) != kitchenv1alpha1.ExceptionExpired {
			continue
		}
		covered := []string{}
		for _, rule := range unmet {
			if exception.WaivesRule(rule) {
				covered = append(covered, rule)
			}
		}
		if len(covered) == 0 {
			continue
		}
		relied, err := s.reliedOnBy(ctx, exception, env, release)
		if err != nil {
			return err
		}
		if !relied {
			logf.FromContext(ctx).V(1).Info(
				"an expired grant asked for a rollback of a release that never went out under it",
				"environment", env.Name, "exception", exception.Name, "release", release.Name)
			continue
		}
		return s.rollBack(ctx, project, env, release, exception, covered, decisionID)
	}
	return nil
}

// reliedOnBy answers whether the release currently deployed here is one that
// went out under this grant — whether one of the promotions in
// `status.usedBy` was a promotion of this release into this environment.
//
// The list holds Promotion names rather than release names because a grant is
// leaned on by a *move*, not by an artifact, so the objects are read back. One
// that is gone answers no: an unmatchable record is not a link, and refusing to
// act on it leaves a workload running, which is the recoverable direction.
func (s *RescanSweeper) reliedOnBy(
	ctx context.Context,
	exception *kitchenv1alpha1.Exception,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
) (bool, error) {
	for _, name := range exception.Status.UsedBy {
		promotion := &kitchenv1alpha1.Promotion{}
		if err := s.Client.Get(ctx, types.NamespacedName{
			Namespace: exception.Namespace, Name: name,
		}, promotion); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return false, err
		}
		if promotion.Spec.EnvironmentRef.Name == env.Name &&
			promotion.Spec.ReleaseRef.Name == release.Name {
			return true, nil
		}
	}
	return false, nil
}

// rollBack retreats the environment to the release it was on before this one.
func (s *RescanSweeper) rollBack(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	exception *kitchenv1alpha1.Exception,
	covered []string,
	decisionID string,
) error {
	log := logf.FromContext(ctx)

	target := ""
	for _, entry := range env.Status.History {
		if entry.Release != "" && entry.Release != release.Name {
			target = entry.Release
			break
		}
	}
	if target == "" {
		log.Info("an expired exception asked for a rollback and there is nothing to roll back to",
			"environment", env.Name, "exception", exception.Name, "release", release.Name)
		return nil
	}
	previous := &kitchenv1alpha1.Release{}
	if err := s.Client.Get(ctx, types.NamespacedName{Namespace: env.Namespace, Name: target}, previous); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("an expired exception asked for a rollback and the previous release is gone",
				"environment", env.Name, "exception", exception.Name, "release", target)
			return nil
		}
		return err
	}

	if err := s.Audit.Record(ctx, audit.Transition{
		Object:      env,
		Kind:        audit.KindEnvironment,
		Controller:  actorRescanSweep,
		Correlation: exception.Name,
		From:        release.Name,
		To:          target,
		Project:     project.Name,
		Reason: fmt.Sprintf(
			"exception %s expired unresolved and asked for a rollback: %s re-evaluated as blocked by %s, "+
				"so %s retreats from release %s to %s",
			exception.Name, env.Name, strings.Join(covered, ", "), env.Name, release.Name, target),
		Details: map[string]any{
			"privileged":   true,
			"exception":    exception.Name,
			"unmetRules":   covered,
			"environment":  env.Name,
			"release":      target,
			"from":         release.Name,
			"decisionID":   decisionID,
			"autoRollback": true,
		},
	}); err != nil {
		return err
	}

	// Re-read before the move. The sweep has just written this object's status
	// twice, and the copy it is holding is a scan's worth of wall-clock old —
	// a spec write off a stale copy is how a rollback would silently lose to
	// somebody deploying while the scanner was out.
	current := &kitchenv1alpha1.Environment{}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(env), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if current.Spec.ReleaseRef.Name != release.Name {
		// Somebody moved it while the scan was out. The re-evaluation was
		// about a release this environment no longer runs, and retreating from
		// it now would undo their deploy.
		log.Info("the environment moved while the scan was out; the rollback is dropped",
			"environment", env.Name, "exception", exception.Name)
		return nil
	}
	current.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: target}
	if err := s.Client.Update(ctx, current); err != nil {
		return err
	}
	if current.RecordReleaseMove(release.Name, kitchenv1alpha1.ReleaseMoveRolledBack,
		audit.ControllerActor(actorRescanSweep)) {
		// The rescan status goes with it: the answer was about the artifact
		// that has just stopped running.
		current.Status.Rescan = nil
		if err := s.Client.Status().Update(ctx, current); err != nil {
			return err
		}
	}
	env.Spec.ReleaseRef = current.Spec.ReleaseRef
	env.Status.Rescan = nil
	s.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventReleaseRolledBack,
		Project:     project.Name,
		Environment: env.Name,
		Release:     target,
		Message: fmt.Sprintf("exception %s expired and %s was rolled back from %s to %s",
			exception.Name, env.Name, release.Name, target),
	})
	log.Info("rescan rolled an environment back", "environment", env.Name,
		"from", release.Name, "to", target, "exception", exception.Name)
	return nil
}

// failed words a scan that could not be made, keeping what is known about it.
func (s *RescanSweeper) failed(
	state *kitchenv1alpha1.EnvironmentRescanStatus, message string,
) kitchenv1alpha1.EnvironmentRescanStatus {
	finished := metav1.NewTime(s.now())
	return kitchenv1alpha1.EnvironmentRescanStatus{
		Phase:      kitchenv1alpha1.RescanFailed,
		Release:    state.Release,
		Artifact:   state.Artifact,
		JobName:    state.JobName,
		StartedAt:  state.StartedAt,
		FinishedAt: &finished,
		Message:    message,
	}
}

// record writes one environment's rescan state, re-reading the object first so
// the sweep's own status write does not clobber a release move somebody made
// while the scanner was out.
func (s *RescanSweeper) record(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	state kitchenv1alpha1.EnvironmentRescanStatus,
) error {
	current := &kitchenv1alpha1.Environment{}
	if err := s.Client.Get(ctx, client.ObjectKeyFromObject(env), current); err != nil {
		return client.IgnoreNotFound(err)
	}
	if current.Spec.ReleaseRef.Name != state.Release {
		// The environment moved while the scan was out. The finding is about
		// an artifact this environment no longer runs, and recording it here
		// would report a scan of something else.
		return nil
	}
	current.Status.Rescan = &state
	if err := s.Client.Status().Update(ctx, current); err != nil {
		return err
	}
	env.Status.Rescan = &state
	return nil
}

// publishStatus keeps the singleton honest about whether the pass is running.
// Best-effort and quiet: the sweep's job is the sweep, and a status write that
// failed is not a reason to skip the next one.
func (s *RescanSweeper) publishStatus(ctx context.Context, report RescanSweepReport) {
	status := &kitchenv1alpha1.RescanStatus{
		Running:      report.Running,
		LastSweep:    ptr.To(metav1.NewTime(s.now())),
		Environments: int32(report.Environments), //nolint:gosec // an estate count is not a security boundary
		Scanning:     int32(report.Scanning),     //nolint:gosec // ditto
		Message:      report.Message,
	}
	current := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, current); err != nil {
		return
	}
	if current.Status.Compliance == nil {
		current.Status.Compliance = &kitchenv1alpha1.ComplianceStatus{}
	}
	current.Status.Compliance.Rescan = status
	if err := s.Client.Status().Update(ctx, current); err != nil {
		logf.FromContext(ctx).V(1).Info("the rescan status was not published", "reason", err.Error())
	}
}

// evaluator builds the shared evaluator from this sweep's own seams — the same
// one the promotion reconciler builds, which is the whole of "the same code
// path as promotion".
func (s *RescanSweeper) evaluator() *PolicyEvaluator {
	return &PolicyEvaluator{Client: s.Client, EvidenceReaders: s.EvidenceReaders}
}

// attester resolves the registry the project's artifacts live in.
func (s *RescanSweeper) attester(
	ctx context.Context, project *kitchenv1alpha1.Project,
) (ArtifactAttester, error) {
	recorder := &DecisionRecorder{Client: s.Client, Audit: s.Audit, Stores: s.Stores, Attesters: s.Attesters}
	return recorder.registryAttester(ctx, project.Name)
}

// report reads what the publisher left on the pod. Init containers are read
// too, because the fetch half's refusal — an artifact carrying no bill of
// materials — is the most common thing that goes wrong here and it is the one
// message a reader needs.
func (s *RescanSweeper) report(ctx context.Context, appNS, jobName string) (rescanReport, bool) {
	pods := &corev1.PodList{}
	if err := s.Client.List(ctx, pods, client.InNamespace(appNS),
		client.MatchingLabels{"job-name": jobName}); err != nil {
		return rescanReport{}, false
	}
	for _, pod := range pods.Items {
		states := append(append([]corev1.ContainerStatus{}, pod.Status.ContainerStatuses...),
			pod.Status.InitContainerStatuses...)
		for _, state := range states {
			if state.State.Terminated == nil || state.State.Terminated.Message == "" {
				continue
			}
			report := rescanReport{}
			if err := json.Unmarshal([]byte(state.State.Terminated.Message), &report); err != nil {
				continue
			}
			if report.Blob != "" || report.Error != "" {
				return report, true
			}
		}
	}
	return rescanReport{}, false
}

// podFailure digs the one useful sentence out of a failed scan: the fetch
// half's refusal, or whatever the scanner left behind.
func (s *RescanSweeper) podFailure(ctx context.Context, appNS, jobName string) string {
	report, found := s.report(ctx, appNS, jobName)
	if found && report.Error != "" {
		return report.Error
	}
	return ""
}

// rescanJob is the pod that rescans one artifact: the bill of materials is
// fetched onto a volume, the scanner reads it and writes findings, and the
// publisher carries those out.
//
// The scanner is given the bill of materials, a credential, and a file to
// write to. It gets no service account token, no cluster access, no artifact
// to pull, and it runs as an unprivileged user with every capability dropped —
// it is an image somebody else wrote, running in an application's namespace,
// and the only thing the platform wants from it is a file.
//
// Nothing from any API request reaches its argv. The scanner's arguments are
// the platform operator's own configuration off the singleton, and everything
// that varies per scan arrives through the environment, where Kubernetes'
// `$(VAR)` expansion — not any templating of Kitchen's — puts it in place.
func rescanJob(
	name, appNS string,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	scanner kitchenv1alpha1.VulnerabilityScannerSpec,
	artifactRef, credsSecret, operatorImage string,
) *batchv1.Job {
	labels := map[string]string{
		labelProject:      project.Name,
		labelEnvironment:  env.Name,
		labelRescan:       release.Name,
		labelManagedByKey: labelManagedByValue,
	}
	environment := []corev1.EnvVar{
		{Name: "KITCHEN_ARTIFACT", Value: artifactRef},
		{Name: "KITCHEN_SBOM", Value: rescanSBOMFile},
		{Name: "KITCHEN_FINDINGS", Value: rescanFindingsFile},
		{Name: "KITCHEN_DATA_SNAPSHOT", Value: rescanSnapshotFile},
		{Name: "KITCHEN_SCANNER", Value: scanner.Name},
		{Name: "KITCHEN_TERMINATION_LOG", Value: terminationLogPath},
		{Name: "KITCHEN_PROJECT", Value: project.Name},
		{Name: "KITCHEN_ENVIRONMENT", Value: env.Name},
		{Name: "KITCHEN_RELEASE", Value: release.Name},
		{Name: "DOCKER_CONFIG", Value: dockerConfigDir},
	}
	mounts := []corev1.VolumeMount{
		dockerConfigMount(),
		{Name: "sbom", MountPath: rescanSBOMDir},
		{Name: "findings", MountPath: rescanFindingsDir},
	}
	unprivileged := &corev1.SecurityContext{
		RunAsUser:                ptr.To(int64(1000)),
		RunAsNonRoot:             ptr.To(true),
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}

	timeout := scanner.TimeoutSeconds
	if timeout <= 0 {
		timeout = defaultRescanTimeoutSeconds
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS, Labels: labels},
		Spec: batchv1.JobSpec{
			// A scan that failed to run is a fact about this artifact today,
			// not something to keep trying: a retry that eventually succeeded
			// would leave an environment whose evidence says the scanner works
			// and whose history says it did not. The next interval is the
			// retry, and it is honest about being a different scan.
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(int64(timeout)),
			TTLSecondsAfterFinished: ptr.To(int32(rescanJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					// It reads a file and writes a file. Nothing it could do
					// with a token is something it should be doing.
					AutomountServiceAccountToken: ptr.To(false),
					InitContainers: []corev1.Container{
						{
							Name:            "sbom",
							Image:           operatorImage,
							Command:         []string{"/rescan", "fetch"},
							Env:             environment,
							VolumeMounts:    mounts,
							SecurityContext: unprivileged,
						},
						{
							Name:            "scan",
							Image:           scanner.Image,
							Args:            scanner.Args,
							Env:             environment,
							VolumeMounts:    mounts,
							SecurityContext: unprivileged,
						},
					},
					Containers: []corev1.Container{{
						Name:            "publish",
						Image:           operatorImage,
						Command:         []string{"/rescan", "publish"},
						Env:             environment,
						VolumeMounts:    mounts,
						SecurityContext: unprivileged,
					}},
					Volumes: []corev1.Volume{
						dockerConfigVolume(credsSecret),
						{Name: "sbom", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "findings", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
		},
	}
}

// rescanJobName names one environment's scan of one release, within the 63
// characters a Kubernetes object name allows.
//
// It is deterministic on the pair rather than on the moment, so a sweep that
// creates a Job it already created finds it already there instead of starting
// a second scanner. The release name gives way rather than the environment's:
// the environment is what makes the name unique across the namespace.
func rescanJobName(envName, releaseName string) string {
	name := envName + "-scan-" + releaseName
	if len(name) <= 63 {
		return name
	}
	room := 63 - len(envName) - len("-scan-")
	if room < 1 {
		return name
	}
	return envName + "-scan-" + releaseName[:room]
}
