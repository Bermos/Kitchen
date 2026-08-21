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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/policy"
	"github.com/Bermos/Kitchen/internal/provider"
	"github.com/Bermos/Kitchen/internal/version"
)

// A Promotion is the one door a release walks through into an environment
// that declares requirements. This reconciler is the doorkeeper: it resolves
// the three references, materializes the policy input from stored evidence,
// evaluates through internal/policy — the same engine and the same
// materializer the eligibility preview uses, so the preview's answer is this
// decision — records the decision (audit fail-closed, store, attestation, all
// via DecisionRecorder), and only then moves the environment.
//
// Three phases are terminal, because the spec is immutable:
//
//   - Applied: the environment runs the release.
//   - Blocked: rules stood in the way, named on status.unmetRules. A retry —
//     new evidence attached, an exception granted — is a *new* Promotion; an
//     old one is a record of what was refused and why, and stays one.
//   - Failed: the request itself was unusable (references that do not line
//     up, objects that are gone). Nothing judged the artifact.

// EvidenceSetReader reads what is attached to an artifact's digest. It is the
// read half of the API's EvidenceReader, redeclared here because the arrow
// between the packages points the other way (api imports controller).
type EvidenceSetReader interface {
	Evidence(ctx context.Context, imageRef string, verifiers ...attestation.Verifier) (attestation.EvidenceSet, error)
}

// EvidenceSetReaderFactory builds the reader for one registry out of the
// docker config that registry is reached with. Nil means the real registry.
type EvidenceSetReaderFactory func(dockerConfig []byte, server string) (EvidenceSetReader, error)

func defaultEvidenceSetReader(dockerConfig []byte, server string) (EvidenceSetReader, error) {
	auth, err := attestation.AuthFromDockerConfig(dockerConfig, server)
	if err != nil {
		return nil, err
	}
	return &attestation.Store{Auth: auth}, nil
}

// PromotionReconciler drives a Promotion from Pending to one of its three
// ends, and applies the allowed ones to their environment.
type PromotionReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Activity feeds the dashboard's recent-activity feed. May be nil.
	Activity *activity.Recorder
	// Audit is waited on and fail-closed: a decision or a move the log
	// refuses to record is one this reconciler does not make. May be nil.
	Audit *audit.Recorder
	// Stores and Attesters are handed to the DecisionRecorder; nil means the
	// real ClickHouse and the real registry. Tests inject.
	Stores    DecisionStoreFactory
	Attesters AttesterFactory
	// EvidenceReaders resolves how the artifact's attached evidence is read
	// back for materialization. Nil talks to the real registry with the
	// project's own credential.
	EvidenceReaders EvidenceSetReaderFactory
	// Exceptions lists the active break-glass grants in scope for a
	// promotion. Nil means the real listing — ActiveExceptionsFor over the
	// Exception objects in the platform namespace. Tests inject.
	Exceptions func(ctx context.Context, promotion *kitchenv1alpha1.Promotion) ([]policy.Exception, error)
}

// activeExceptions is the Exceptions seam with its default behind it: the
// one shared listing (ActiveExceptionsFor), scoped to the promotion's own
// triple, judged at `at` — the same clock the engine's input carries, so
// what is listed is exactly what ApplyExceptions will honour.
func (r *PromotionReconciler) activeExceptions(
	ctx context.Context, promotion *kitchenv1alpha1.Promotion, at time.Time,
) ([]policy.Exception, error) {
	if r.Exceptions != nil {
		return r.Exceptions(ctx, promotion)
	}
	active, err := ActiveExceptionsFor(ctx, r.Client, promotion.Namespace,
		promotion.Spec.ProjectRef.Name, promotion.Spec.EnvironmentRef.Name,
		promotion.Spec.ReleaseRef.Name, at)
	if err != nil {
		return nil, err
	}
	return PolicyExceptions(active), nil
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=promotions,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=promotions/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=promotions/finalizers,verbs=update
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=projects;releases;builds;connections;kitchens;resourceclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=exceptions,verbs=get;list;watch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch

// Reconcile drives one Promotion.
func (r *PromotionReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	promotion := &kitchenv1alpha1.Promotion{}
	if err := r.Get(ctx, req.NamespacedName, promotion); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !promotion.DeletionTimestamp.IsZero() {
		// Nothing to clean up: a promotion creates nothing that outlives it.
		return ctrl.Result{}, nil
	}
	switch promotion.Status.Phase {
	case kitchenv1alpha1.PromotionApplied, kitchenv1alpha1.PromotionBlocked, kitchenv1alpha1.PromotionFailed:
		// Terminal. The spec is immutable, so nothing about this object can
		// change the answer; a retry is a new Promotion.
		return ctrl.Result{}, nil
	}

	project, env, release, refusal, err := r.resolve(ctx, promotion)
	if err != nil {
		return ctrl.Result{}, err
	}
	if refusal != "" {
		return r.fail(ctx, promotion, refusal)
	}

	// An allowed promotion that did not finish applying re-enters here and
	// goes straight back to the apply — the decision is already recorded, and
	// recording it again would double the trail.
	if promotion.Status.Phase == kitchenv1alpha1.PromotionAllowed ||
		promotion.Status.Phase == kitchenv1alpha1.PromotionAllowedWithException {
		return r.apply(ctx, promotion, project, env, release)
	}

	if promotion.Status.Phase != kitchenv1alpha1.PromotionEvaluating {
		promotion.Status.Phase = kitchenv1alpha1.PromotionEvaluating
		if err := r.Status().Update(ctx, promotion); err != nil {
			return ctrl.Result{}, err
		}
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		kitchen = nil
	}

	input, result, bundle, message, refusal, err := r.evaluate(ctx, kitchen, promotion, project, env, release)
	if err != nil {
		return ctrl.Result{}, err
	}
	if refusal != "" {
		return r.fail(ctx, promotion, refusal)
	}

	// Record before acting: the audit record gates the verdict, the store
	// keeps it replayable, and — this being a promotion — the artifact gets
	// the promotion-decision attestation. A refused record is a requeue, not
	// a promotion.
	recorder := r.recorder()
	decisionID, err := recorder.Record(ctx, kitchen, promotion, input, result, bundle)
	if err != nil {
		return ctrl.Result{}, err
	}

	// An emergency deployment is loud before it moves: the exceptions it
	// stands on record the reliance (usedBy), the audit log gets a privileged
	// record, and the artifact gets the break-glass attestation — all before
	// the phase says the platform will act. Idempotent per exception, so a
	// requeue does not double the trail.
	if result.Verdict == policy.VerdictAllowedWithException {
		if err := r.recordBreakGlass(ctx, kitchen, promotion, project, env, release, result); err != nil {
			return ctrl.Result{}, err
		}
	}

	promotion.Status.Verdict = result.Verdict
	promotion.Status.DecisionID = decisionID
	promotion.Status.EvaluatedAt = ptr.To(metav1.Now())
	promotion.Status.UnmetRules = unmetRuleIDs(result)
	promotion.Status.Message = message

	switch result.Verdict {
	case policy.VerdictBlocked:
		promotion.Status.Phase = kitchenv1alpha1.PromotionBlocked
		r.setReady(promotion, metav1.ConditionFalse, "Blocked", message)
		log.Info("promotion blocked", "promotion", promotion.Name,
			"environment", env.Name, "release", release.Name, "unmetRules", promotion.Status.UnmetRules)
		return ctrl.Result{}, r.Status().Update(ctx, promotion)
	case policy.VerdictAllowedWithException:
		promotion.Status.Phase = kitchenv1alpha1.PromotionAllowedWithException
	default:
		promotion.Status.Phase = kitchenv1alpha1.PromotionAllowed
	}
	if err := r.Status().Update(ctx, promotion); err != nil {
		return ctrl.Result{}, err
	}
	return r.apply(ctx, promotion, project, env, release)
}

// resolve loads the three references and checks they tell one story. A
// refusal (second-to-last return) is a permanent fault of the request — the
// promotion goes Failed with that sentence; an error is transient.
func (r *PromotionReconciler) resolve(
	ctx context.Context,
	promotion *kitchenv1alpha1.Promotion,
) (*kitchenv1alpha1.Project, *kitchenv1alpha1.Environment, *kitchenv1alpha1.Release, string, error) {
	load := func(name string, into client.Object, what string) (string, error) {
		err := r.Get(ctx, types.NamespacedName{Namespace: promotion.Namespace, Name: name}, into)
		if apierrors.IsNotFound(err) {
			return fmt.Sprintf("%s %q does not exist", what, name), nil
		}
		return "", err
	}

	project := &kitchenv1alpha1.Project{}
	if refusal, err := load(promotion.Spec.ProjectRef.Name, project, "project"); refusal != "" || err != nil {
		return nil, nil, nil, refusal, err
	}
	env := &kitchenv1alpha1.Environment{}
	if refusal, err := load(promotion.Spec.EnvironmentRef.Name, env, "environment"); refusal != "" || err != nil {
		return nil, nil, nil, refusal, err
	}
	release := &kitchenv1alpha1.Release{}
	if refusal, err := load(promotion.Spec.ReleaseRef.Name, release, "release"); refusal != "" || err != nil {
		return nil, nil, nil, refusal, err
	}

	// All three must line up: promoting one project's release into another
	// project's environment would deploy a stranger's image under this
	// project's URL, and a promotion whose own projectRef disagrees with
	// either is a record that misattributes what it did.
	if env.Spec.ProjectRef.Name != promotion.Spec.ProjectRef.Name {
		return nil, nil, nil, fmt.Sprintf("environment %s belongs to project %s, not %s",
			env.Name, env.Spec.ProjectRef.Name, promotion.Spec.ProjectRef.Name), nil
	}
	if release.Spec.ProjectRef.Name != promotion.Spec.ProjectRef.Name {
		return nil, nil, nil, fmt.Sprintf("release %s belongs to project %s, not %s",
			release.Name, release.Spec.ProjectRef.Name, promotion.Spec.ProjectRef.Name), nil
	}
	return project, env, release, "", nil
}

// evaluate materializes the input and asks the engine. Returns, in order: the
// input and result and the bundle they came from, the sentence for status
// .message, a permanent refusal (promotion goes Failed), and a transient
// error (requeue).
func (r *PromotionReconciler) evaluate(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	promotion *kitchenv1alpha1.Promotion,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
) (policy.Input, policy.Result, policy.Bundle, string, string, error) {
	none := policy.Input{}

	build := &kitchenv1alpha1.Build{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: promotion.Namespace, Name: release.Spec.BuildRef.Name,
	}, build); err != nil {
		if !apierrors.IsNotFound(err) {
			return none, policy.Result{}, nil, "", "", err
		}
		// A pruned build leaves the release judged on what the registry still
		// carries — honestly nothing, if the index is gone too.
		build = nil
	}

	claims, err := r.claimFacts(ctx, env)
	if err != nil {
		return none, policy.Result{}, nil, "", "", err
	}

	requirements := env.Spec.Requirements
	if requirements == nil {
		// An environment that declares no bar accepts anything — exactly
		// today's behaviour, stated rather than implied: the decision is
		// still recorded, with an empty bundle and no rules evaluated.
		input := policy.MaterializeInput(policy.KindPromotion, time.Now().UTC(), project, env, release, build, nil, claims)
		message := fmt.Sprintf(
			"environment %s declares no requirements, so no rules were evaluated and the release is allowed",
			env.Name)
		return input, policy.Result{Verdict: policy.VerdictAllowed, Fired: []policy.FiredRule{}},
			policy.Bundle{}, message, "", nil
	}

	resolver := &policy.Resolver{Client: r.Client, Namespace: PlatformNamespace}
	info, err := resolver.Resolve(ctx, requirements.BundleDigest)
	if err != nil {
		// A bar that cannot be read is not a bar that is cleared: the
		// promotion fails, naming the digest, rather than guessing in either
		// direction. (Blocked would claim rules fired; none were evaluated.)
		return none, policy.Result{}, nil, "",
			"the environment's requirements could not be evaluated: " + err.Error(), nil
	}

	evidence, err := r.materializeEvidence(ctx, kitchen, project, build)
	if err != nil {
		return none, policy.Result{}, nil, "", "", err
	}

	at := time.Now().UTC()
	exceptions, err := r.activeExceptions(ctx, promotion, at)
	if err != nil {
		return none, policy.Result{}, nil, "", "", err
	}

	input := policy.MaterializeInput(policy.KindPromotion, at, project, env, release, build, evidence, claims)
	input.Exceptions = exceptions
	result, err := policy.Evaluate(ctx, info.Bundle, input)
	if err != nil {
		// The bundle resolved and would not evaluate: a broken bundle, not a
		// broken platform. Permanent for this promotion.
		return none, policy.Result{}, nil, "",
			"the environment's requirements could not be evaluated: " + err.Error(), nil
	}

	message := ""
	switch result.Verdict {
	case policy.VerdictAllowed:
		message = fmt.Sprintf("the release clears bundle %s", requirements.BundleDigest)
	case policy.VerdictAllowedWithException:
		message = fmt.Sprintf("the release clears bundle %s, with every fired rule waived by an exception",
			requirements.BundleDigest)
	default:
		message = fmt.Sprintf("blocked by bundle %s: %s", requirements.BundleDigest, firedSentence(result))
		// When an expired, unresolved exception would have waived what fired,
		// the refusal says so: the reader's next move is to resolve or renew
		// it, and a message that hid the connection would send them hunting.
		if note := r.expiredExceptionNote(ctx, promotion, result, at); note != "" {
			message += "; " + note
		}
	}
	return input, result, info.Bundle, message, "", nil
}

// expiredExceptionNote words the blocked-by-expired case: an exception that
// covers this pair, names a rule that fired unwaived, and has run out without
// being resolved. Best-effort — a listing failure loses the note, never the
// verdict.
func (r *PromotionReconciler) expiredExceptionNote(
	ctx context.Context, promotion *kitchenv1alpha1.Promotion, result policy.Result, at time.Time,
) string {
	list := &kitchenv1alpha1.ExceptionList{}
	if err := r.List(ctx, list, client.InNamespace(promotion.Namespace)); err != nil {
		return ""
	}
	notes := []string{}
	for i := range list.Items {
		exception := &list.Items[i]
		if !exception.Covers(promotion.Spec.ProjectRef.Name,
			promotion.Spec.EnvironmentRef.Name, promotion.Spec.ReleaseRef.Name) {
			continue
		}
		if exception.EffectivePhase(at) != kitchenv1alpha1.ExceptionExpired {
			continue
		}
		covered := []string{}
		for _, rule := range result.Fired {
			if !rule.Waived && exception.WaivesRule(rule.Rule) {
				covered = append(covered, rule.Rule)
			}
		}
		if len(covered) == 0 {
			continue
		}
		notes = append(notes, fmt.Sprintf(
			"exception %s expired %s and no longer waives %s — resolve it or grant a new one",
			exception.Name, exception.Spec.ExpiresAt.UTC().Format(time.RFC3339),
			strings.Join(covered, ", ")))
	}
	return strings.Join(notes, "; ")
}

// claimFacts materializes the project's resource claims for the target
// environment — the data facts the dataclass and provenance rules judge.
// Listing them here keeps the promotion's input the same input the
// eligibility preview assembles: same claims, same facts, same answer.
func (r *PromotionReconciler) claimFacts(
	ctx context.Context, env *kitchenv1alpha1.Environment,
) ([]policy.Claim, error) {
	list := &kitchenv1alpha1.ResourceClaimList{}
	if err := r.List(ctx, list, client.InNamespace(env.Namespace)); err != nil {
		return nil, err
	}
	return policy.ClaimFacts(env, list.Items), nil
}

// materializeEvidence reads what the release's artifact carries, verified
// against the platform's key — the registry is the source of truth, and a
// registry that cannot be asked is a requeue rather than a judgement over a
// guess. A release whose build or artifact is gone is judged on nothing.
func (r *PromotionReconciler) materializeEvidence(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	project *kitchenv1alpha1.Project,
	build *kitchenv1alpha1.Build,
) ([]policy.Evidence, error) {
	if build == nil || build.Status.Artifact == nil || build.Status.Artifact.Digest == "" {
		return policy.IndexedEvidence(build), nil
	}
	artifact := build.Status.Artifact

	reader, err := r.evidenceReader(ctx, project)
	if err != nil {
		return nil, err
	}
	verifiers := []attestation.Verifier{}
	if key, err := SigningKeyFor(ctx, r.Client, kitchen); err == nil && key != nil {
		verifiers = append(verifiers, key)
	}
	set, err := reader.Evidence(ctx, artifact.Repository+"@"+artifact.Digest, verifiers...)
	if err != nil {
		return nil, fmt.Errorf("the artifact's evidence could not be read from the registry: %w", err)
	}
	return policy.EvidenceFrom(set, policy.EvidenceSources(build)), nil
}

// evidenceReader resolves the registry the project's artifacts live in — the
// same resolution the DecisionRecorder's attester makes, for the read.
func (r *PromotionReconciler) evidenceReader(
	ctx context.Context, project *kitchenv1alpha1.Project,
) (EvidenceSetReader, error) {
	connection := &kitchenv1alpha1.Connection{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: PlatformNamespace, Name: project.Spec.Registry.ConnectionRef.Name,
	}, connection); err != nil {
		return nil, err
	}
	registry, err := provider.Registry(connection)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: connection.Spec.CredentialsSecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("the registry credential could not be read: %w", err)
	}
	factory := r.EvidenceReaders
	if factory == nil {
		factory = defaultEvidenceSetReader
	}
	return factory(secret.Data[corev1.DockerConfigJsonKey], registry.Server)
}

// apply moves the environment onto the release — the same move the build
// controller's fast path makes, attributed to the promotion — then mints the
// deployment attestation and, when the project's next stage auto-promotes,
// creates the next Promotion. Only after all of that does the phase read
// Applied, so a requeue anywhere along the way re-enters an idempotent path.
func (r *PromotionReconciler) apply(
	ctx context.Context,
	promotion *kitchenv1alpha1.Promotion,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if env.Spec.ReleaseRef.Name != release.Name {
		outgoing := env.Spec.ReleaseRef.Name
		if err := r.Audit.Record(ctx, audit.Transition{
			Object:      env,
			Kind:        audit.KindEnvironment,
			Controller:  actorPromotionController,
			Correlation: promotion.Name,
			From:        outgoing,
			To:          release.Name,
			Project:     project.Name,
			Reason: fmt.Sprintf("promotion %s moved %s onto release %s (%s)",
				promotion.Name, env.Name, release.Name, promotion.Status.Verdict),
			Details: map[string]any{
				"release":         release.Name,
				"previousRelease": outgoing,
				"promotion":       promotion.Name,
				"verdict":         promotion.Status.Verdict,
				"decisionID":      promotion.Status.DecisionID,
			},
		}); err != nil {
			return ctrl.Result{}, err
		}
		env.Spec.ReleaseRef = kitchenv1alpha1.LocalObjectReference{Name: release.Name}
		if err := r.Update(ctx, env); err != nil {
			return ctrl.Result{}, err
		}
		r.Activity.Record(ctx, clickhouse.Event{
			Type:        clickhouse.EventReleasePromoted,
			Project:     project.Name,
			Environment: env.Name,
			Release:     release.Name,
			Message:     fmt.Sprintf("promotion %s moved %s onto %s", promotion.Name, env.Name, release.Name),
		})
		if env.RecordReleaseMove(outgoing, kitchenv1alpha1.ReleaseMovePromoted, promotion.Name) {
			if err := r.Status().Update(ctx, env); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	// The deployment attestation: the release went live on this environment.
	// Best-effort and idempotent, like every attach — the registry being
	// briefly unreachable must not park an environment between release and
	// record, and the decision itself is already recorded either way.
	if err := r.attestDeployment(ctx, promotion, project, env, release); err != nil {
		log.Error(err, "the deployment could not be attested on the artifact",
			"promotion", promotion.Name, "artifact", release.Spec.Image)
	}

	// The next rung: a stage marked autoPromote is entered by the platform as
	// soon as the stage before it applies. Whether the release then clears it
	// is that environment's own requirements — evidence-gating an automatic
	// promotion is a rule on the environment, not a mechanism here. This
	// happens before the phase flips so a refused record retries.
	if next := project.Spec.Promotion.NextStage(env.Name); next != nil && next.AutoPromote {
		if err := createAutomaticPromotion(ctx, r.Client, r.Audit, actorPromotionController,
			promotion.Name, promotion.Namespace, project, next.Environment, release.Name,
			audit.ControllerActor(actorPromotionController),
			fmt.Sprintf("stage %s applied release %s; stage %s auto-promotes", env.Name, release.Name, next.Name),
		); err != nil {
			return ctrl.Result{}, err
		}
	}

	promotion.Status.Phase = kitchenv1alpha1.PromotionApplied
	promotion.Status.AppliedAt = ptr.To(metav1.Now())
	// The message keeps explaining the verdict; the applied fact lives on the
	// phase, the timestamp and the condition.
	applied := fmt.Sprintf("environment %s runs release %s", env.Name, release.Name)
	if promotion.Status.Message == "" {
		promotion.Status.Message = applied
	}
	r.setReady(promotion, metav1.ConditionTrue, "Applied", applied)
	log.Info("promotion applied", "promotion", promotion.Name,
		"environment", env.Name, "release", release.Name)
	return ctrl.Result{}, r.Status().Update(ctx, promotion)
}

// attestDeployment mints the reserved deployment/v1 attestation on the
// artifact: this release went live on this environment, through this
// promotion, under this platform version.
func (r *PromotionReconciler) attestDeployment(
	ctx context.Context,
	promotion *kitchenv1alpha1.Promotion,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
) error {
	repository, digest, byDigest := strings.Cut(release.Spec.Image, "@")
	if !byDigest || digest == "" {
		// A tag-only image has nothing to attach to; the artifact identity
		// story already reported that on the build.
		return nil
	}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		kitchen = nil
	}
	signer, err := SigningKeyFor(ctx, r.Client, kitchen)
	if err != nil {
		return err
	}
	if signer == nil {
		// Not signing is a state, not a failure; the platform status says so.
		return nil
	}
	attester, err := r.recorder().registryAttester(ctx, project.Name)
	if err != nil {
		return err
	}
	statement, err := attestation.NewStatement(
		repository, digest, attestation.PredicateDeployment, map[string]any{
			"project":     project.Name,
			"environment": env.Name,
			"release":     release.Name,
			"promotion":   promotion.Name,
			"appliedAt":   time.Now().UTC().Format(time.RFC3339),
			"platform":    map[string]any{"name": "kitchen", "version": version.Version},
		})
	if err != nil {
		return err
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		return err
	}
	_, err = attester.Attach(ctx, repository+"@"+digest, envelope, statement.PredicateType)
	return err
}

// recordBreakGlass makes an allowed-with-exception promotion loud: for every
// exception the verdict stands on — each named on a waived fired rule — it
// appends a privileged audit record, adds the promotion to the exception's
// usedBy, and mints the break-glass attestation on the artifact. The audit
// record is fail-closed and comes first; the attestation is best-effort like
// every attach, because the registry being briefly unreachable must not park
// an emergency deployment behind the record of itself.
//
// Idempotence rides on usedBy: an exception that already names this
// promotion was recorded on an earlier pass and is skipped whole.
func (r *PromotionReconciler) recordBreakGlass(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	promotion *kitchenv1alpha1.Promotion,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	result policy.Result,
) error {
	log := logf.FromContext(ctx)

	waivedBy := map[string][]string{}
	for _, rule := range result.Fired {
		if rule.Waived && rule.Exception != "" {
			waivedBy[rule.Exception] = append(waivedBy[rule.Exception], rule.Rule)
		}
	}
	names := make([]string, 0, len(waivedBy))
	for name := range waivedBy {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		exception := &kitchenv1alpha1.Exception{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: promotion.Namespace, Name: name}, exception); err != nil {
			if apierrors.IsNotFound(err) {
				// A test-injected exception with no object behind it, or one
				// deleted mid-flight: the decision and its waiving are already
				// recorded; there is no register row to mark.
				log.Info("a waiving exception has no object to record use on", "exception", name)
				continue
			}
			return err
		}
		already := false
		for _, user := range exception.Status.UsedBy {
			if user == promotion.Name {
				already = true
				break
			}
		}
		if already {
			continue
		}
		if err := r.Audit.Record(ctx, breakGlassTransition(promotion, exception, waivedBy[name])); err != nil {
			return err
		}
		if err := appendUsedBy(ctx, r.Client, exception, promotion.Name); err != nil {
			return err
		}
		if err := r.attestBreakGlass(ctx, kitchen, promotion, project, env, release, exception, waivedBy[name]); err != nil {
			log.Error(err, "the break-glass use could not be attested on the artifact",
				"exception", name, "promotion", promotion.Name, "artifact", release.Spec.Image)
		}
	}
	return nil
}

// breakGlassTransition is the privileged audit record a break-glass use
// appends before anything moves — built apart from the recording so a test
// can hold it up to the light without a store.
func breakGlassTransition(
	promotion *kitchenv1alpha1.Promotion,
	exception *kitchenv1alpha1.Exception,
	waivedRules []string,
) audit.Transition {
	details := map[string]any{
		"privileged":  true,
		"exception":   exception.Name,
		"waivedRules": waivedRules,
		"environment": promotion.Spec.EnvironmentRef.Name,
		"release":     promotion.Spec.ReleaseRef.Name,
		"requestedBy": exception.Spec.RequestedBy,
		"approvedBy":  exception.Spec.ApprovedBy,
		"reason":      exception.Spec.Reason,
		"expiresAt":   exception.Spec.ExpiresAt.UTC().Format(time.RFC3339),
	}
	if exception.Spec.IncidentRef != "" {
		details["incidentRef"] = exception.Spec.IncidentRef
	}
	return audit.Transition{
		Object:      promotion,
		Kind:        audit.KindPromotion,
		Controller:  actorPromotionController,
		Correlation: exception.Name,
		To:          string(kitchenv1alpha1.PromotionAllowedWithException),
		Project:     promotion.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf(
			"break-glass: promotion %s proceeds with %s waived by exception %s, approved by %s, expiring %s",
			promotion.Name, strings.Join(waivedRules, ", "), exception.Name,
			exception.Spec.ApprovedBy, exception.Spec.ExpiresAt.UTC().Format(time.RFC3339)),
		Details: details,
	}
}

// attestBreakGlass mints the break-glass/v1 attestation on the artifact: the
// fact that an exception carried this artifact travels with it, while the
// authoritative record stays on the Exception bound to the pair.
func (r *PromotionReconciler) attestBreakGlass(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	promotion *kitchenv1alpha1.Promotion,
	project *kitchenv1alpha1.Project,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	exception *kitchenv1alpha1.Exception,
	waivedRules []string,
) error {
	repository, digest, byDigest := strings.Cut(release.Spec.Image, "@")
	if !byDigest || digest == "" {
		return nil
	}
	signer, err := SigningKeyFor(ctx, r.Client, kitchen)
	if err != nil {
		return err
	}
	if signer == nil {
		return nil
	}
	attester, err := r.recorder().registryAttester(ctx, project.Name)
	if err != nil {
		return err
	}
	predicate := map[string]any{
		"exception":   exception.Name,
		"ruleIDs":     waivedRules,
		"reason":      exception.Spec.Reason,
		"requestedBy": exception.Spec.RequestedBy,
		"approvedBy":  exception.Spec.ApprovedBy,
		"expiresAt":   exception.Spec.ExpiresAt.UTC().Format(time.RFC3339),
		"environment": env.Name,
		"promotion":   promotion.Name,
	}
	if exception.Spec.IncidentRef != "" {
		predicate["incidentRef"] = exception.Spec.IncidentRef
	}
	statement, err := attestation.NewStatement(repository, digest, attestation.PredicateBreakGlass, predicate)
	if err != nil {
		return err
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		return err
	}
	_, err = attester.Attach(ctx, repository+"@"+digest, envelope, statement.PredicateType)
	return err
}

// fail marks a promotion Failed, with the audit record first.
func (r *PromotionReconciler) fail(
	ctx context.Context, promotion *kitchenv1alpha1.Promotion, message string,
) (ctrl.Result, error) {
	if err := r.Audit.Record(ctx, audit.Transition{
		Object:     promotion,
		Kind:       audit.KindPromotion,
		Controller: actorPromotionController,
		From:       string(promotion.Status.Phase),
		To:         string(kitchenv1alpha1.PromotionFailed),
		Project:    promotion.Spec.ProjectRef.Name,
		Reason:     fmt.Sprintf("promotion %s failed: %s", promotion.Name, message),
		Details: map[string]any{
			"environment": promotion.Spec.EnvironmentRef.Name,
			"release":     promotion.Spec.ReleaseRef.Name,
		},
	}); err != nil {
		return ctrl.Result{}, err
	}
	promotion.Status.Phase = kitchenv1alpha1.PromotionFailed
	promotion.Status.Message = message
	r.setReady(promotion, metav1.ConditionFalse, "Failed", message)
	return ctrl.Result{}, r.Status().Update(ctx, promotion)
}

func (r *PromotionReconciler) setReady(
	promotion *kitchenv1alpha1.Promotion, status metav1.ConditionStatus, reason, message string,
) {
	meta.SetStatusCondition(&promotion.Status.Conditions, metav1.Condition{
		Type: condReady, Status: status, Reason: reason, Message: message,
		ObservedGeneration: promotion.Generation,
	})
}

// recorder is the DecisionRecorder this reconciler records through, built
// from its own seams so tests inject once.
func (r *PromotionReconciler) recorder() *DecisionRecorder {
	return &DecisionRecorder{Client: r.Client, Audit: r.Audit, Stores: r.Stores, Attesters: r.Attesters}
}

// unmetRuleIDs lists the fired, unwaived rules by their stable ids — what a
// blocked promotion names on its status.
func unmetRuleIDs(result policy.Result) []string {
	unmet := []string{}
	for _, rule := range result.Fired {
		if !rule.Waived {
			unmet = append(unmet, rule.Rule)
		}
	}
	return unmet
}

// firedSentence words what fired, ids and messages both, for status.message.
func firedSentence(result policy.Result) string {
	parts := make([]string, 0, len(result.Fired))
	for _, rule := range result.Fired {
		if rule.Waived {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", rule.Rule, rule.Message))
	}
	return strings.Join(parts, "; ")
}

// automaticPromotionName is the deterministic name an automatic promotion
// gets, so the flows that create one — a finished build, an applied stage —
// can retry without creating twins. A person retrying a blocked promotion
// goes through the API, which generates a fresh name.
func automaticPromotionName(project, releaseName, environment string) string {
	sum := sha256.Sum256([]byte(releaseName + "\x00" + environment))
	prefix := project
	if len(prefix) > 46 {
		prefix = prefix[:46]
	}
	return fmt.Sprintf("%s-promo-%s", prefix, hex.EncodeToString(sum[:])[:10])
}

// createAutomaticPromotion creates the Promotion an automatic flow asks for,
// audit record first. An already-existing promotion of the same name is
// success: both callers retry, and the name is deterministic exactly so a
// retry finds its own work done.
func createAutomaticPromotion(
	ctx context.Context,
	c client.Client,
	auditor *audit.Recorder,
	controllerActor string,
	correlation string,
	namespace string,
	project *kitchenv1alpha1.Project,
	envName, releaseName, requestedBy, reason string,
) error {
	promotion := &kitchenv1alpha1.Promotion{
		ObjectMeta: metav1.ObjectMeta{
			Name:      automaticPromotionName(project.Name, releaseName, envName),
			Namespace: namespace,
			Labels:    map[string]string{labelProject: project.Name, labelManagedByKey: labelManagedByValue},
		},
		Spec: kitchenv1alpha1.PromotionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: envName},
			ReleaseRef:     kitchenv1alpha1.LocalObjectReference{Name: releaseName},
			RequestedBy:    requestedBy,
			Trigger:        kitchenv1alpha1.PromotionAutomatic,
			Reason:         reason,
		},
	}
	if err := auditor.Record(ctx, audit.Transition{
		Object:      promotion,
		Kind:        audit.KindPromotion,
		Operation:   clickhouse.AuditCreate,
		Controller:  controllerActor,
		Correlation: correlation,
		To:          releaseName,
		Project:     project.Name,
		Reason:      reason,
		Details: map[string]any{
			"environment": envName,
			"release":     releaseName,
			"trigger":     string(kitchenv1alpha1.PromotionAutomatic),
		},
	}); err != nil {
		return err
	}
	if err := c.Create(ctx, promotion); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PromotionReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Promotion{}).
		Named("promotion").
		Complete(r)
}
