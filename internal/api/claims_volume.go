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
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	corev1 "k8s.io/api/core/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/volume"
)

// volumeRequirements is the request's volume block in the provider's
// vocabulary, with the source defaulted the way an object written before
// binding existed means it.
//
// The reconciler makes the same translation over the claim it reads
// (volumeRequirementsOf), and the two are deliberately separate: the door
// holds a request and the reconciler holds an object, and the vocabularies
// are kept apart the way claimRequirements keeps the database's. What they
// share is volume.Requirements.Validate, which is why a shape refused here
// is refused there in the same words.
func volumeRequirements(cfg kitchenv1alpha1.VolumeConfig) volume.Requirements {
	req := volume.Requirements{
		Process:      cfg.Process,
		Source:       volume.Source(cfg.Source),
		Size:         cfg.Size,
		StorageClass: cfg.StorageClass,
		MountPath:    cfg.MountPath,
	}
	if req.Source == "" {
		req.Source = volume.SourceProvision
	}
	if cfg.Bind != nil {
		req.Bind = volume.Binding{
			PersistentVolume:      cfg.Bind.PersistentVolume,
			PersistentVolumeClaim: cfg.Bind.PersistentVolumeClaim,
			AccessMode:            corev1.PersistentVolumeAccessMode(cfg.Bind.AccessMode),
		}
	}
	return req
}

// The volume half of the claim API: a persistent volume mounted into one of
// the project's processes. No Connection — the provider is the cluster's
// StorageClass — and no binding secret: the answer carries the mount, the
// access mode the class was detected to support, and the PVC behind it.

// volumeClaimShaper is the claimShaper for type volume.
type volumeClaimShaper struct{}

func (volumeClaimShaper) fields() []claimField {
	return []claimField{
		{
			name:  "volume",
			set:   func(body *createClaimRequest) bool { return body.Volume != nil },
			lacks: "no mount",
		},
	}
}

// config validates the shape of what the claim asks for — and the one thing
// the API can check that the reconciler would otherwise refuse a reconcile
// later: that the process the claim names is one the project has. The
// StorageClass and what it supports are the cluster's answer, and land on
// the claim.
func (volumeClaimShaper) config(
	w http.ResponseWriter,
	body *createClaimRequest,
	project *kitchenv1alpha1.Project,
) (*runtime.RawExtension, bool) {
	if body.Volume == nil {
		badRequest(w, "volume is required on a volume claim: which process mounts it (\"%s\", or one of the "+
			"project's processes), its size, and the mountPath it appears at", kitchenv1alpha1.WebProcessName)
		return nil, false
	}
	cfg := kitchenv1alpha1.VolumeConfig{
		Process:      strings.TrimSpace(body.Volume.Process),
		Source:       kitchenv1alpha1.VolumeSource(strings.TrimSpace(string(body.Volume.Source))),
		Size:         strings.TrimSpace(body.Volume.Size),
		StorageClass: strings.TrimSpace(body.Volume.StorageClass),
		MountPath:    strings.TrimSpace(body.Volume.MountPath),
	}
	if cfg.Source == "" {
		cfg.Source = kitchenv1alpha1.VolumeProvision
	}
	if bind := body.Volume.Bind; bind != nil {
		cfg.Bind = &kitchenv1alpha1.VolumeBinding{
			PersistentVolume:      strings.TrimSpace(bind.PersistentVolume),
			PersistentVolumeClaim: strings.TrimSpace(bind.PersistentVolumeClaim),
			AccessMode:            strings.TrimSpace(bind.AccessMode),
		}
	}
	req := volumeRequirements(cfg)
	if err := req.Validate(); err != nil {
		badRequest(w, "%s", err.Error())
		return nil, false
	}
	if names := project.ProcessNames(); !slices.Contains(names, cfg.Process) {
		badRequest(w, "volume.process %q is not one of project %s's processes: a volume claim names the one "+
			"process that mounts it, and this project's are %s", cfg.Process, project.Name,
			strings.Join(names, ", "))
		return nil, false
	}
	if req.Bound() {
		// Two refusals that belong at the door rather than on the claim.
		// Both are decidable from the request alone — the access mode a
		// bound volume is mounted with is *declared*, not discovered — so
		// there is no reason to create a claim that is about to fail.
		if body.DeletionPolicy == string(kitchenv1alpha1.ClaimDelete) {
			badRequest(w, "deletionPolicy %s is refused on a bound volume: the volume existed before this "+
				"claim, the platform neither provisioned it nor owns it, and destroying somebody else's "+
				"data is not something this API offers. Deleting the claim unmounts the volume and leaves "+
				"every byte of it where it is", kitchenv1alpha1.ClaimDelete)
			return nil, false
		}
		if body.PreviewMode == string(contract.PreviewShared) &&
			volume.AttachesOnce(corev1.PersistentVolumeAccessMode(cfg.Bind.AccessMode)) {
			badRequest(w, "previewMode %q is refused for a volume mounted %s: it attaches to one pod at a "+
				"time and production has it, so a preview mounting it would take it from production. A "+
				"volume mounted %s or %s is mounted read-only by every preview, which is the default; ask "+
				"for none to give previews nothing", contract.PreviewShared, cfg.Bind.AccessMode,
				corev1.ReadOnlyMany, corev1.ReadWriteMany)
			return nil, false
		}
	}
	raw, err := json.Marshal(claimConfigBody{Volume: &cfg})
	if err != nil {
		badRequest(w, "%s", err.Error())
		return nil, false
	}
	return &runtime.RawExtension{Raw: raw}, true
}

func (volumeClaimShaper) view(claim *kitchenv1alpha1.ResourceClaim, view *claimView) {
	view.Volume = volumeOf(claim)
}

func (volumeClaimShaper) deletionOutcome(claim *kitchenv1alpha1.ResourceClaim) string {
	if claim.Volume().Bound() {
		return "the volume is unmounted and nothing on it is touched: it was never the platform's to delete"
	}
	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete {
		return "the volume and the data on it are deleted"
	}
	return "the volume is kept, and a claim of the same name binds to it again"
}

// claimVolumeView is the claim's volume as it answered it: what was asked
// for, and — once the reconciler has looked — what the cluster made of it.
type claimVolumeView struct {
	Process string `json:"process"`
	// Source is provision or bind — which of the two shapes below this is,
	// answered rather than left to be inferred from which fields are set.
	Source string `json:"source"`
	Size   string `json:"size,omitempty"`
	// Bind is what a bound claim asked for, and Bound what it resolved to.
	Bind         *claimVolumeBindView  `json:"bind,omitempty"`
	Bound        *claimBoundVolumeView `json:"bound,omitempty"`
	StorageClass string                `json:"storageClass,omitempty"`
	MountPath    string                `json:"mountPath"`
	// AccessMode is what the StorageClass was detected to support —
	// ReadWriteOnce, which caps the process at one replica and forces a
	// recreate, or ReadWriteMany, which lifts both — and AccessModeReason
	// what decided it. Absent until the claim has been reconciled.
	AccessMode       string `json:"accessMode,omitempty"`
	AccessModeReason string `json:"accessModeReason,omitempty"`
	// ClaimName is the PersistentVolumeClaim in the application namespace,
	// and PersistentVolume the volume it is bound to once it is.
	ClaimName        string `json:"claimName,omitempty"`
	PersistentVolume string `json:"persistentVolume,omitempty"`
}

// claimVolumeBindView is what a bound claim named, echoed back.
type claimVolumeBindView struct {
	PersistentVolume      string `json:"persistentVolume,omitempty"`
	PersistentVolumeClaim string `json:"persistentVolumeClaim,omitempty"`
	AccessMode            string `json:"accessMode"`
}

// claimBoundVolumeView is the volume a bound claim resolved to: what it is,
// how much of it there is, whether this project may write it, and who else
// is holding the same data.
//
// SharedWith is on the answer for the reason it is on the status: a
// filesystem two projects hold is a fact that must not have to be
// discovered, and the one screen where somebody is looking at this claim is
// where it belongs.
type claimBoundVolumeView struct {
	PersistentVolume string   `json:"persistentVolume"`
	Capacity         string   `json:"capacity,omitempty"`
	Named            string   `json:"named,omitempty"`
	Identity         string   `json:"identity,omitempty"`
	Writable         bool     `json:"writable"`
	SharedWith       []string `json:"sharedWith,omitempty"`
}

// volumeOf is the claim's volume, and nothing at all for a claim of another
// type.
func volumeOf(claim *kitchenv1alpha1.ResourceClaim) *claimVolumeView {
	cfg := claim.Volume()
	if cfg == (kitchenv1alpha1.VolumeConfig{}) {
		return nil
	}
	source := cfg.Source
	if source == "" {
		source = kitchenv1alpha1.VolumeProvision
	}
	view := &claimVolumeView{
		Process:      cfg.Process,
		Source:       string(source),
		Size:         cfg.Size,
		StorageClass: cfg.StorageClass,
		MountPath:    cfg.MountPath,
	}
	if cfg.Bind != nil {
		view.Bind = &claimVolumeBindView{
			PersistentVolume:      cfg.Bind.PersistentVolume,
			PersistentVolumeClaim: cfg.Bind.PersistentVolumeClaim,
			AccessMode:            cfg.Bind.AccessMode,
		}
	}
	if status := claim.Status.Volume; status != nil {
		view.StorageClass = status.StorageClass
		view.AccessMode = status.AccessMode
		view.AccessModeReason = status.AccessModeReason
		view.ClaimName = status.ClaimName
		view.PersistentVolume = status.PersistentVolume
		if bound := status.Bound; bound != nil {
			view.Bound = &claimBoundVolumeView{
				PersistentVolume: bound.PersistentVolume,
				Capacity:         bound.Capacity,
				Named:            bound.Named,
				Identity:         bound.Identity,
				Writable:         bound.Writable,
				SharedWith:       bound.SharedWith,
			}
			view.PersistentVolume = bound.PersistentVolume
		}
	}
	return view
}
