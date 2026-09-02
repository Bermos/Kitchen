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
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Where an Addon comes from.
//
// **The chart must not render these objects.** templates/kitchen.yaml is a
// post-install hook precisely because a chart cannot apply a custom resource
// whose CRD arrives in the same release, and a chart-rendered Addon would
// inherit that trap whole — which is the exact bug the old KEDA installer's
// comment complained about, where a value flipped on upgrade created the
// ServiceAccount and never reached the object.
//
// So the operator seeds them, the way it seeds the registry Connection:
// created once, the fact recorded on the singleton, and never recreated. An
// Addon somebody deletes stays deleted, because an installation that would
// rather run its own KEDA — a shared one, a pinned one, one its GitOps owns —
// has to be able to end up with no object at all.
//
// Only the entries this installation permitted are seeded. Granting the
// install account is an explicit act — a chart value somebody set, creating a
// ServiceAccount bound to cluster-admin — and it is not an act anyone
// performs without wanting the dependency, so the seeded Addon asks for the
// install. Turning it off afterwards is one field, and it stays off.

// seedAddons creates an Addon for every catalogue entry the chart permitted
// and the platform has not seeded yet.
//
// It returns whether the singleton's status changed, so the caller writes the
// record in its own status update rather than this taking a second one.
func (r *KitchenReconciler) seedAddons(ctx context.Context, kitchen *kitchenv1alpha1.Kitchen) (bool, error) {
	log := logf.FromContext(ctx)

	seeded := []string{}
	if kitchen.Status.Addons != nil {
		seeded = kitchen.Status.Addons.Seeded
	}

	changed := false
	for _, entry := range addonEntries() {
		if !r.Addons.permits(entry.ID) || slices.Contains(seeded, entry.ID) {
			continue
		}

		addon := &kitchenv1alpha1.Addon{
			ObjectMeta: metav1.ObjectMeta{
				Name:      entry.ID,
				Namespace: PlatformNamespace,
				Labels:    platformLabels(entry.ID, entry.ID),
			},
			Spec: kitchenv1alpha1.AddonSpec{
				Install:   true,
				Namespace: r.seedNamespace(kitchen, entry),
			},
		}
		err := r.Create(ctx, addon)
		switch {
		case err == nil:
			log.Info("seeded addon", "addon", entry.ID)
		case apierrors.IsAlreadyExists(err):
			// Someone created it by hand before the platform got to it. The
			// record is still written, so the platform does not keep trying
			// and does not recreate it if they later delete it.
		default:
			return changed, err
		}

		seeded = append(seeded, entry.ID)
		changed = true
	}

	if changed {
		kitchen.Status.Addons = &kitchenv1alpha1.AddonsStatus{Seeded: seeded}
	}
	return changed, nil
}

// seedNamespace is where a seeded Addon installs its entry.
//
// It is the entry's own default — upstream's — with one exception, and the
// exception is the reason this is not a constant: KEDA's HTTP add-on is
// reached as well as installed, and where the platform looks for its
// interceptor is spec.scaleToZero.interceptor on the singleton. Seeding the
// Addon anywhere else would install the add-on in one namespace and look for
// it in another.
func (r *KitchenReconciler) seedNamespace(kitchen *kitchenv1alpha1.Kitchen, entry addonEntry) string {
	if entry.ID != AddonKeda {
		return ""
	}
	if namespace := interceptorBackend(kitchen).Namespace; namespace != entry.DefaultNamespace {
		return namespace
	}
	return ""
}
