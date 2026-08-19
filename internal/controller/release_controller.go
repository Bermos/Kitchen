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
	"slices"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/activity"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// ReleaseReconciler answers "where is this release live?", and bounds how many
// releases a project keeps.
//
// A Release is inert — nothing materializes from it directly; an Environment
// runs it. So the reconciler owns the two things that are nobody else's:
//
//   - status.environments, the back-reference. The relationship is only
//     declared in the other direction (Environment.spec.releaseRef), and
//     without this every caller wanting to know which release is serving
//     production would have to list environments and match refs itself.
//     It is informational and eventually consistent by design.
//   - retention. Every successful build leaves a Release behind, and nothing
//     else ever removes one short of the project being deleted.
//
// The spec needs no defending here: it is immutable at admission, through a
// CEL transition rule on the CRD (see ReleaseSpec). There is no webhook, and
// no reconciler check would be as good as one — an edit is refused before it
// is ever written.
type ReleaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Activity records pruned releases into the platform's feed. May be nil.
	Activity *activity.Recorder
	// Audit appends this reconciler's state transitions to the tamper-evident
	// log. Unlike Activity it is waited on: a transition it refuses is a
	// transition this reconciler does not make. May be nil.
	Audit *audit.Recorder
}

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=releases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=releases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=releases/finalizers,verbs=update
// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=environments,verbs=get;list;watch

// Reconcile records where the Release is serving, then prunes the releases its
// Project has outgrown.
func (r *ReleaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	release := &kitchenv1alpha1.Release{}
	if err := r.Get(ctx, req.NamespacedName, release); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !release.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := r.List(ctx, environments, client.InNamespace(release.Namespace)); err != nil {
		return ctrl.Result{}, err
	}

	serving := servingEnvironments(release.Name, environments.Items)
	if !slices.Equal(serving, release.Status.Environments) {
		release.Status.Environments = serving
		if err := r.Status().Update(ctx, release); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, r.prune(ctx, release, environments.Items)
}

// servingEnvironments names the Environments pointing at the Release, sorted
// so that an unchanged set never looks like a change.
func servingEnvironments(releaseName string, environments []kitchenv1alpha1.Environment) []string {
	var serving []string
	for i := range environments {
		if environments[i].Spec.ReleaseRef.Name == releaseName {
			serving = append(serving, environments[i].Name)
		}
	}
	sort.Strings(serving)
	return serving
}

// prune deletes the Releases of this Release's Project beyond the retention
// count, newest first — except any an Environment is still pointing at, which
// are kept on top of the count however old they are. That exception is what
// keeps a rollback exact: an environment parked on release 3 while 40 more
// were built still has release 3 to run.
//
// Every Release of the project runs this, and they all compute the same
// answer, so a delete that lost the race is not an error.
//
// The image the pruned Release points at is left in the registry. Reclaiming
// that needs a per-provider delete API and a count of who else references the
// digest; this bounds the platform's own objects, which is what makes the
// count meaningful in the first place.
func (r *ReleaseReconciler) prune(
	ctx context.Context,
	release *kitchenv1alpha1.Release,
	environments []kitchenv1alpha1.Environment,
) error {
	keep := r.retention(ctx)
	if keep <= 0 {
		return nil
	}

	project := release.Spec.ProjectRef.Name
	list := &kitchenv1alpha1.ReleaseList{}
	if err := r.List(ctx, list, client.InNamespace(release.Namespace)); err != nil {
		return err
	}
	siblings := make([]kitchenv1alpha1.Release, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.ProjectRef.Name == project {
			siblings = append(siblings, list.Items[i])
		}
	}
	if len(siblings) <= int(keep) {
		return nil
	}

	// Newest first. Releases minted within the same second — a repository
	// whose pushes arrive together — tie-break on name, which carries the
	// commit, so the order is at least stable across reconciles.
	sort.Slice(siblings, func(i, j int) bool {
		if siblings[i].CreationTimestamp.Equal(&siblings[j].CreationTimestamp) {
			return siblings[i].Name > siblings[j].Name
		}
		return siblings[j].CreationTimestamp.Before(&siblings[i].CreationTimestamp)
	})

	referenced := make(map[string]struct{}, len(environments))
	for i := range environments {
		referenced[environments[i].Spec.ReleaseRef.Name] = struct{}{}
	}

	log := logf.FromContext(ctx)
	for i := int(keep); i < len(siblings); i++ {
		stale := &siblings[i]
		if _, live := referenced[stale.Name]; live {
			continue
		}
		if err := r.Audit.Record(ctx, audit.Transition{
			Object:     stale,
			Kind:       audit.KindRelease,
			Operation:  clickhouse.AuditDelete,
			Controller: actorReleaseController,
			From:       stale.Spec.Image,
			Project:    project,
			Reason: fmt.Sprintf("pruned: %s keeps the newest %d releases and nothing is serving this one",
				project, keep),
			Details: map[string]any{"image": stale.Spec.Image, "retention": keep},
		}); err != nil {
			return err
		}
		if err := r.Delete(ctx, stale); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		log.Info("pruned a release past the retention count",
			"release", stale.Name, "project", project, "retention", keep)
		r.Activity.Record(ctx, clickhouse.Event{
			Type:    clickhouse.EventReleasePruned,
			Project: project,
			Release: stale.Name,
			Message: fmt.Sprintf("release %s pruned: %s keeps %d", stale.Name, project, keep),
		})
	}
	return nil
}

// retention is how many Releases a Project keeps, from the Kitchen singleton.
//
// A singleton that cannot be read answers 0, which prunes nothing: a platform
// the operator cannot see its configuration for is the last one that should be
// deleting things on a guess.
func (r *ReleaseReconciler) retention(ctx context.Context) int32 {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := r.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return 0
	}
	return kitchen.Spec.Builds.ReleaseRetention
}

// mapEnvironmentToReleases enqueues every Release of the Environment's
// project, not just the one it now points at: a move leaves the release it
// came off holding a stale back-reference, and the object no longer says which
// that was.
func (r *ReleaseReconciler) mapEnvironmentToReleases(ctx context.Context, obj client.Object) []ctrl.Request {
	env, ok := obj.(*kitchenv1alpha1.Environment)
	if !ok {
		return nil
	}
	return r.releaseRequests(ctx, env.Namespace, env.Spec.ProjectRef.Name)
}

// mapPlatformToReleases enqueues every Release when the platform's
// configuration changes, so a new retention count takes effect without waiting
// for the next build.
func (r *ReleaseReconciler) mapPlatformToReleases(ctx context.Context, _ client.Object) []ctrl.Request {
	return r.releaseRequests(ctx, "", "")
}

// releaseRequests enqueues the Releases in a namespace, optionally narrowed to
// one project. An empty namespace is every namespace.
func (r *ReleaseReconciler) releaseRequests(ctx context.Context, namespace, project string) []ctrl.Request {
	list := &kitchenv1alpha1.ReleaseList{}
	var opts []client.ListOption
	if namespace != "" {
		opts = append(opts, client.InNamespace(namespace))
	}
	if err := r.List(ctx, list, opts...); err != nil {
		logf.FromContext(ctx).Error(err, "could not list releases to requeue")
		return nil
	}
	requests := make([]ctrl.Request, 0, len(list.Items))
	for i := range list.Items {
		if project != "" && list.Items[i].Spec.ProjectRef.Name != project {
			continue
		}
		requests = append(requests, ctrl.Request{NamespacedName: types.NamespacedName{
			Namespace: list.Items[i].Namespace, Name: list.Items[i].Name,
		}})
	}
	return requests
}

// SetupWithManager sets up the controller with the Manager.
func (r *ReleaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kitchenv1alpha1.Release{}).
		Watches(&kitchenv1alpha1.Environment{}, handler.EnqueueRequestsFromMapFunc(r.mapEnvironmentToReleases)).
		// Only spec changes: the singleton's status moves far more often than
		// the retention count does, and every Release is requeued here.
		Watches(&kitchenv1alpha1.Kitchen{}, handler.EnqueueRequestsFromMapFunc(r.mapPlatformToReleases),
			builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Named("release").
		Complete(r)
}
