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

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// The ceiling on live preview environments per project (#294).
//
// A pull request is a running environment, so a project with `c` claims and
// `e` open requests materializes `c × e` deployments of backing
// infrastructure — and `e` is the one number in the platform nobody chose.
// Build appetite has been bounded since #278 and an application's workloads
// are bounded by what its project declares; this bounds the third.
//
// It is a ceiling on *previews*, never on environments. Production is not
// counted and neither is anything promoted: the ephemeral half is where the
// multiplication happens, and refusing to deploy production because six pull
// requests are open would be an outage caused by a cost control.
//
// What a refusal is not is a queue. A refused pull request gets its preview on
// its next push, once a slot has come free; nothing retries on a timer. A
// queue would have the platform deploy a commit nobody asked it to deploy,
// minutes or days after the push, and the platform would then owe an answer
// for what it deployed while nobody was looking.

// previewCapacity is one project measured against its ceiling: how many
// previews are live, and what the ceiling in force is — the project's own
// `spec.previews.max` where it has one, the platform's
// `spec.previews.maxPerProject` otherwise. A max of 0 is no ceiling at all.
type previewCapacity struct {
	Live int32
	Max  int32
}

// Reached reports whether one more preview would exceed the ceiling. An
// unbounded ceiling is never reached.
func (c previewCapacity) Reached() bool { return c.Max > 0 && c.Live >= c.Max }

// previewCapacityOf measures a project against its ceiling.
//
// `live` counts the preview Environments that exist and are not being torn
// down. An environment with a deletion timestamp is a slot on its way back:
// counting it would keep a project at its ceiling for as long as a finalizer
// took, which for a claim with a database branch to tear down is not
// instantaneous.
func previewCapacityOf(
	ctx context.Context,
	reader client.Reader,
	project *kitchenv1alpha1.Project,
	platform int32,
) (previewCapacity, error) {
	capacity := previewCapacity{Max: project.Spec.Previews.MaxOrPlatform(platform)}
	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := reader.List(ctx, environments, client.InNamespace(project.Namespace)); err != nil {
		return previewCapacity{}, err
	}
	for i := range environments.Items {
		env := &environments.Items[i]
		if env.Spec.ProjectRef.Name != project.Name || env.Spec.Type != kitchenv1alpha1.EnvironmentPreview {
			continue
		}
		if !env.DeletionTimestamp.IsZero() {
			continue
		}
		capacity.Live++
	}
	return capacity, nil
}

// platformPreviewMax is the platform-wide default, read off the singleton.
// A platform the operator cannot read gives the compiled-in default rather
// than an unbounded one: a ceiling that disappears when an API call fails is
// not a ceiling.
func platformPreviewMax(ctx context.Context, reader client.Reader) int32 {
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := reader.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return kitchenv1alpha1.DefaultPreviewsPerProject
	}
	return kitchen.Spec.Previews.EffectiveMaxPerProject()
}

// previewRefusalMessage is the one sentence the refusal is reported with —
// on the commit status, in the pull request comment and on the Project. It
// names the count, the ceiling and the two settings that move it, because a
// refusal that does not say where to change it is a refusal somebody has to
// go and read the source for.
func previewRefusalMessage(project *kitchenv1alpha1.Project, capacity previewCapacity) string {
	setting := "spec.previews.maxPerProject on the platform"
	if project.Spec.Previews.Max != nil {
		setting = fmt.Sprintf("spec.previews.max on project %s", project.Name)
	}
	return fmt.Sprintf(
		"%s already has %d of %d preview environments live, so this pull request gets none. "+
			"Close a pull request to free a slot and push again, or raise %s",
		project.Name, capacity.Live, capacity.Max, setting)
}

// recordPreviewRefusal puts the refusal on the Project, where the dashboard,
// the CLI and the next person to ask "why has this request no preview" can
// read it. It writes only when something moved: a build reconciled twice for
// the same commit refuses twice and writes once.
func (r *BuildReconciler) recordPreviewRefusal(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	capacity previewCapacity,
	pullRequest int32,
	commit string,
) error {
	fresh := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: project.Name}, fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	status := fresh.Status.Previews
	if status == nil {
		status = &kitchenv1alpha1.PreviewCapacityStatus{}
	}
	changed := status.Live != capacity.Live || status.Max != capacity.Max
	status.Live, status.Max = capacity.Live, capacity.Max
	if status.RecordRefusedPreview(pullRequest, commit, metav1.Now()) {
		changed = true
	}
	fresh.Status.Previews = status
	if !changed {
		return nil
	}
	return r.Status().Update(ctx, fresh)
}

// clearPreviewRefusal takes a pull request off the refusal list once it has
// its preview after all — the push after a slot came free.
func (r *BuildReconciler) clearPreviewRefusal(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	pullRequest int32,
) error {
	fresh := &kitchenv1alpha1.Project{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: project.Namespace, Name: project.Name}, fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	if fresh.Status.Previews == nil || !fresh.Status.Previews.ClearRefusedPreview(pullRequest) {
		return nil
	}
	return r.Status().Update(ctx, fresh)
}

// measurePreviewCapacity refreshes what the Project says about its ceiling:
// how many previews are live, what the ceiling is, and whether a pull request
// opened now would get one.
//
// The build controller records a refusal; this keeps the count honest as
// previews come and go. Both are needed: the build controller only hears
// about a project when something is built, and the ceiling stops being
// reached because a *different* pull request closed.
//
// It writes to the status struct in place — the caller's own status update is
// what persists it, so a project reconcile is still one write.
func (r *ProjectReconciler) measurePreviewCapacity(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	setCond func(string, metav1.ConditionStatus, string, string),
) error {
	capacity, err := previewCapacityOf(ctx, r.Client, project, platformPreviewMax(ctx, r.Client))
	if err != nil {
		return err
	}
	if project.Status.Previews == nil {
		if !previewsEnabled(project) || capacity.Max <= 0 {
			// Nothing to say and nothing said. A project that gets no
			// previews at all, and one on a platform with no ceiling, carry
			// no block — a status field that reads the same on every object
			// is one nobody reads.
			removePreviewCapacityCondition(project)
			return nil
		}
		project.Status.Previews = &kitchenv1alpha1.PreviewCapacityStatus{}
	}
	project.Status.Previews.Live = capacity.Live
	project.Status.Previews.Max = capacity.Max
	setPreviewCapacityCondition(project, capacity, setCond, func(string) {
		removePreviewCapacityCondition(project)
	})
	return nil
}

func removePreviewCapacityCondition(project *kitchenv1alpha1.Project) {
	meta.RemoveStatusCondition(&project.Status.Conditions, condPreviewCapacity)
}

// setPreviewCapacityCondition says on the Project whether a pull request
// opened now would get a preview.
//
// It is removed on a project with no ceiling and on one that gets no previews
// at all: a condition that reads the same on every object teaches everybody to
// skip the conditions list, which is where the ones that matter are.
func setPreviewCapacityCondition(
	project *kitchenv1alpha1.Project,
	capacity previewCapacity,
	setCond func(string, metav1.ConditionStatus, string, string),
	removeCond func(string),
) {
	if capacity.Max <= 0 || !previewsEnabled(project) {
		removeCond(condPreviewCapacity)
		return
	}
	if !capacity.Reached() {
		setCond(condPreviewCapacity, metav1.ConditionTrue, "Available", fmt.Sprintf(
			"%d of %d preview environments are live; the next pull request gets one",
			capacity.Live, capacity.Max))
		return
	}
	waiting := ""
	if refused := project.Status.Previews; refused != nil && len(refused.Refused) > 0 {
		waiting = fmt.Sprintf(" %d pull request(s) are waiting on a slot and get a preview on their next push.",
			len(refused.Refused))
	}
	setCond(condPreviewCapacity, metav1.ConditionFalse, "CapReached", fmt.Sprintf(
		"%d of %d preview environments are live, so a new pull request gets none.%s "+
			"Close a pull request to free a slot, or raise spec.previews.max on this project "+
			"(or spec.previews.maxPerProject on the platform)",
		capacity.Live, capacity.Max, waiting))
}
