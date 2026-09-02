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
	"regexp"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// What the platform singleton still says about its dependencies, now that
// installing them is an Addon's job.
//
// The installs moved out — one engine, one object per dependency, its own
// conditions and its own failure, instead of a seventh status block on a
// 976-line singleton. What stayed is the roll-up: two conditions on the
// Kitchen object answering the questions everything downstream already asks,
// "can this cluster idle an environment" and "can it provision a database".
// Those are facts about the *cluster* rather than about an install, so they
// belong where somebody reads the platform's health, and moving them would
// have been a second breaking change for no gain.
//
// Each is now a mirror of one Addon's Ready condition, with the two states an
// Addon cannot have — no Addon at all — answered here.

const (
	// condScaleToZeroReady is where the platform says whether it can idle an
	// environment to zero at all. Each Environment's own ScaleToZero
	// condition answers the narrower question of whether it is idling.
	condScaleToZeroReady = "ScaleToZeroReady"

	// condDatabasesReady is where the platform says whether it can provision
	// a database into this cluster at all. A claim's own conditions say
	// whether *it* bound.
	condDatabasesReady = "DatabasesReady"
)

// reconcileKeda rolls the keda Addon up onto the singleton.
//
// spec.scaleToZero.enabled stays here and is not the Addon's business: it is
// whether environments idle at all, which is a platform decision that remains
// meaningful in a cluster somebody else installed KEDA into. Whether the
// platform installs KEDA is spec.install on the Addon.
func (r *KitchenReconciler) reconcileKeda(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	if !kitchen.Spec.ScaleToZero.Enabled {
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condScaleToZeroReady)
		return true
	}
	return r.rollUpAddon(ctx, AddonKeda, condScaleToZeroReady, setCond,
		"no environment idles. Create the addon and set spec.install to have the platform install KEDA and "+
			"its HTTP add-on itself, or install the two Helm releases yourself (see the chart README)")
}

// reconcileDatabases rolls the cloudnative-pg Addon up onto the singleton.
//
// Unlike scale-to-zero there is no feature switch in front of it: a cnpg
// Connection is the demand signal, and it reports the absence of the operator
// on itself. So an installation that has neither the Addon nor the operator
// carries no condition at all rather than a permanent False about something
// nobody asked for.
func (r *KitchenReconciler) reconcileDatabases(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	addon := &kitchenv1alpha1.Addon{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: AddonCNPG}
	err := r.Get(ctx, key, addon)
	if apierrors.IsNotFound(err) {
		// Not asked for and not installed: the guidance is not lost by
		// staying quiet, because a cnpg Connection in a cluster without the
		// operator says exactly this, on the connection, where somebody is
		// looking.
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condDatabasesReady)
		return true
	}
	return r.rollUpAddon(ctx, AddonCNPG, condDatabasesReady, setCond, "")
}

// rollUpAddon copies one Addon's Ready condition onto the singleton, so that
// the platform's own health screen answers without following a reference.
//
// The reason and message are the Addon's own: they were written to be read by
// whoever can fix them, and rewording them here would leave two texts to keep
// in step. absent is what to say where there is no Addon at all — the one
// state the Addon cannot report on itself.
func (r *KitchenReconciler) rollUpAddon(
	ctx context.Context,
	id, condition string,
	setCond func(string, metav1.ConditionStatus, string, string),
	absent string,
) bool {
	log := logf.FromContext(ctx)

	addon := &kitchenv1alpha1.Addon{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: id}
	switch err := r.Get(ctx, key, addon); {
	case apierrors.IsNotFound(err):
		setCond(condition, metav1.ConditionFalse, "AddonMissing",
			fmt.Sprintf("there is no addon named %q in %s, so %s", id, PlatformNamespace, absent))
		return true
	case err != nil:
		setCond(condition, metav1.ConditionFalse, "AddonUnreadable",
			fmt.Sprintf("could not read the addon named %q: %s", id, err))
		return false
	}

	ready := meta.FindStatusCondition(addon.Status.Conditions, kitchenv1alpha1.AddonReady)
	if ready == nil {
		setCond(condition, metav1.ConditionUnknown, "AddonNotAssessed",
			fmt.Sprintf("the addon named %q has not been reconciled yet", id))
		return false
	}
	setCond(condition, ready.Status, ready.Reason, ready.Message)
	log.V(1).Info("rolled up addon", "addon", id, "status", ready.Status, "reason", ready.Reason)

	// A refusal is settled: nothing changes without a spec edit or a chart
	// upgrade, and both reconcile the objects on their own. Anything else
	// that is not True is still moving.
	return ready.Status == metav1.ConditionTrue ||
		ready.Reason == kitchenv1alpha1.AddonRefused ||
		ready.Reason == "NotInstalled"
}

// dnsLabel is what a namespace name has to look like. The name reaches helm
// as its own argv element rather than through a shell, so this buys a legible
// failure rather than safety: a job bound to cluster-admin that dies on an
// unparseable --namespace has already been created, and refusing before that
// is cheaper.
var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// nonAlphanumeric is what a version is sanitised through to become part of a
// job name.
var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// apiServed reports whether the cluster serves a kind at all. A CRD that is
// not installed is a RESTMapper no-match rather than an error worth
// reporting, which is the same signal reconcileScaleToZero falls back on when
// it cannot write an HTTPScaledObject.
//
// It reads through an APIReader rather than a cache on purpose: caching every
// object of a kind in the cluster to answer "does this kind exist" would cost
// far more than it saves, and starting an informer for a kind that may not
// exist is how a probe becomes a wait.
func apiServed(ctx context.Context, reader client.Reader, gvk schema.GroupVersionKind) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group: gvk.Group, Version: gvk.Version, Kind: gvk.Kind + "List",
	})
	err := reader.List(ctx, list, client.Limit(1))
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err), apierrors.IsNotFound(err):
		return false, nil
	default:
		return false, err
	}
}
