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
	"net/http"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/provider/volume"
)

// What a volume claim can bind, for the moment somebody is making one.
//
// A bound volume is named, not chosen from a form the platform filled in —
// the whole point is that it existed before the cluster did — and a name
// typed from memory is the failure this route exists to remove. So the API
// answers what is actually there: every PersistentVolume in the cluster,
// what it is, what it offers, who already holds it, and whether a new claim
// could write it.
//
// **It is a list of storage, not a list of anybody's data.** A
// PersistentVolume is cluster-scoped and holds no credential, which is why
// this is not the operator's alone: a developer who cannot see it cannot
// write the claim that mounts it, and the alternative is a support ticket
// for every mount. What it does *not* leak is whose it is — a volume held
// by a project the caller cannot see is listed as held, and the claim
// holding it is named only where the caller could have read that claim
// anyway.
//
// The PersistentVolumeClaims in the answer are the other half of the
// vocabulary: one already sitting in a project's own namespace is bound by
// name, and only that project's may be. The claims themselves the platform
// already reads; the volumes behind them are this route's own read, and it
// is a read alone — nothing here writes, and the operator's grant says so:
//
// +kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch

// bindableVolumesBody is what GET /claim-volumes answers.
type bindableVolumesBody struct {
	// PersistentVolumes is every volume in the cluster, whether or not a
	// claim could bind it — one that cannot is listed with the reason, so
	// that a name somebody was told to use does not simply fail to appear.
	PersistentVolumes []bindableVolumeView `json:"persistentVolumes"`

	// PersistentVolumeClaims are the claims already sitting in the
	// application namespaces of the projects this caller can see, which are
	// the only ones a project may bind by name.
	PersistentVolumeClaims []bindableVolumeClaimView `json:"persistentVolumeClaims"`
}

// bindableVolumeView is one PersistentVolume as a claim would find it.
type bindableVolumeView struct {
	Name        string   `json:"name"`
	Capacity    string   `json:"capacity,omitempty"`
	AccessModes []string `json:"accessModes"`
	// StorageClass is what the volume carries, which a statically written
	// one usually does not.
	StorageClass string `json:"storageClass,omitempty"`
	// Phase is the volume's own: Available, Bound, Released, Failed.
	Phase string `json:"phase,omitempty"`
	// Identity is what the volume points at — the NFS export, the CSI
	// handle — and is what tells two PersistentVolumes over one filesystem
	// apart from two over different ones.
	Identity string `json:"identity,omitempty"`

	// HeldBy are the claims already mounting this same storage, as
	// "project/claim" for the ones this caller may see and "another
	// project" for the rest.
	HeldBy []string `json:"heldBy,omitempty"`

	// Writable says a new claim could mount this volume read-write: nothing
	// else is writing the same storage, and the volume offers a mode that
	// writes. Readable says the same for a read-only mount.
	Writable bool `json:"writable"`
	Readable bool `json:"readable"`

	// Note is why a mount is refused where one is, in the words the claim
	// would answer with.
	Note string `json:"note,omitempty"`
}

// bindableVolumeClaimView is one PersistentVolumeClaim a project may bind by
// name — one of its own.
type bindableVolumeClaimView struct {
	Name        string   `json:"name"`
	Project     string   `json:"project"`
	Capacity    string   `json:"capacity,omitempty"`
	AccessModes []string `json:"accessModes"`
	Phase       string   `json:"phase,omitempty"`
	// PersistentVolume is what it is bound to, empty while it is not.
	PersistentVolume string `json:"persistentVolume,omitempty"`
	// ManagedByKitchen says this claim is one the platform made for a claim
	// of its own. Binding it is not wrong — it is how a project keeps a
	// volume across a claim it deleted — but it is not the ordinary case
	// and the screen says so.
	ManagedByKitchen bool `json:"managedByKitchen"`
}

// listBindableVolumes answers what a volume claim could bind.
func (s *Server) listBindableVolumes(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	scope := scopeFrom(ctx)

	volumes := &corev1.PersistentVolumeList{}
	if err := s.reader().List(ctx, volumes); err != nil {
		s.writeError(w, err)
		return
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := s.reader().List(ctx, pvcs); err != nil {
		s.writeError(w, err)
		return
	}
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := s.Client.List(ctx, claims, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}

	// Who already holds each identity, and whether any of them writes it.
	held := map[string][]string{}
	written := map[string]bool{}
	for i := range claims.Items {
		claim := &claims.Items[i]
		bound := claim.Status.Volume
		if claim.Spec.Type != kitchenv1alpha1.ClaimTypeVolume || bound == nil || bound.Bound == nil {
			continue
		}
		identity := bound.Bound.Identity
		holder := "another project"
		if scope.allows(claim.Spec.ProjectRef.Name) {
			holder = claim.Spec.ProjectRef.Name + "/" + claim.Name
		}
		held[identity] = append(held[identity], holder)
		written[identity] = written[identity] || bound.Bound.Writable
	}

	body := bindableVolumesBody{
		PersistentVolumes:      make([]bindableVolumeView, 0, len(volumes.Items)),
		PersistentVolumeClaims: make([]bindableVolumeClaimView, 0),
	}
	for i := range volumes.Items {
		body.PersistentVolumes = append(body.PersistentVolumes,
			bindableVolume(&volumes.Items[i], held, written))
	}
	sort.Slice(body.PersistentVolumes, func(a, b int) bool {
		return body.PersistentVolumes[a].Name < body.PersistentVolumes[b].Name
	})

	// A project's own namespace is the only one whose claims it may bind,
	// so the listing is the visible projects' namespaces and nothing else.
	namespaces := map[string]string{}
	projects := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, projects, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	for i := range projects.Items {
		if scope.allows(projects.Items[i].Name) {
			namespaces[controller.AppNamespace(projects.Items[i].Name)] = projects.Items[i].Name
		}
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		project, ours := namespaces[pvc.Namespace]
		if !ours {
			continue
		}
		body.PersistentVolumeClaims = append(body.PersistentVolumeClaims, bindableVolumeClaimView{
			Name:             pvc.Name,
			Project:          project,
			Capacity:         quantityOf(pvc.Status.Capacity),
			AccessModes:      accessModeNames(pvc.Spec.AccessModes),
			Phase:            string(pvc.Status.Phase),
			PersistentVolume: pvc.Spec.VolumeName,
			ManagedByKitchen: pvc.Labels[managedByLabelKey] == managedByLabelValue,
		})
	}
	sort.Slice(body.PersistentVolumeClaims, func(a, b int) bool {
		if body.PersistentVolumeClaims[a].Project != body.PersistentVolumeClaims[b].Project {
			return body.PersistentVolumeClaims[a].Project < body.PersistentVolumeClaims[b].Project
		}
		return body.PersistentVolumeClaims[a].Name < body.PersistentVolumeClaims[b].Name
	})
	writeJSON(w, http.StatusOK, body)
}

// bindableVolume is one PersistentVolume with the two questions answered
// that decide whether a claim can be written against it: may this be read,
// and may this be written.
func bindableVolume(pv *corev1.PersistentVolume, held map[string][]string, written map[string]bool) bindableVolumeView {
	identity := volume.VolumeIdentity(pv)
	view := bindableVolumeView{
		Name:         pv.Name,
		Capacity:     quantityOf(pv.Spec.Capacity),
		AccessModes:  accessModeNames(pv.Spec.AccessModes),
		StorageClass: pv.Spec.StorageClassName,
		Phase:        string(pv.Status.Phase),
		Identity:     identity,
		HeldBy:       held[identity],
	}
	offers := func(mode corev1.PersistentVolumeAccessMode) bool {
		for _, offered := range pv.Spec.AccessModes {
			if offered == mode || (offered == corev1.ReadWriteMany && mode == corev1.ReadOnlyMany) {
				return true
			}
		}
		return false
	}
	view.Readable = offers(corev1.ReadOnlyMany)
	view.Writable = (offers(corev1.ReadWriteOnce) || offers(corev1.ReadWriteMany)) && !written[identity]

	claimed := pv.Spec.ClaimRef != nil
	switch {
	case written[identity]:
		view.Note = "another project already writes this storage, and one filesystem has one writer. Mount it " +
			"read-only, or ask for a volume of its own"
	case claimed && len(view.HeldBy) == 0:
		// Bound to something that is not a Kitchen claim: a workload
		// outside the platform, or a claim left behind.
		view.Readable, view.Writable = false, false
		view.Note = "this volume is already bound to a PersistentVolumeClaim outside the platform, and a " +
			"PersistentVolume binds to one claim at a time. Two consumers of one export are two " +
			"PersistentVolumes over it"
	case !view.Readable && !view.Writable:
		view.Note = "this volume offers no access mode a claim can ask for"
	}
	return view
}

func accessModeNames(modes []corev1.PersistentVolumeAccessMode) []string {
	names := make([]string, 0, len(modes))
	for _, mode := range modes {
		names = append(names, string(mode))
	}
	return names
}

func quantityOf(list corev1.ResourceList) string {
	if quantity, ok := list[corev1.ResourceStorage]; ok {
		return quantity.String()
	}
	return ""
}
