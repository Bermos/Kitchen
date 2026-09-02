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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/volume"
)

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
		Size:         strings.TrimSpace(body.Volume.Size),
		StorageClass: strings.TrimSpace(body.Volume.StorageClass),
		MountPath:    strings.TrimSpace(body.Volume.MountPath),
	}
	req := volume.Requirements{
		Process:      cfg.Process,
		Size:         cfg.Size,
		StorageClass: cfg.StorageClass,
		MountPath:    cfg.MountPath,
	}
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
	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete {
		return "the volume and the data on it are deleted"
	}
	return "the volume is kept, and a claim of the same name binds to it again"
}

// claimVolumeView is the claim's volume as it answered it: what was asked
// for, and — once the reconciler has looked — what the cluster made of it.
type claimVolumeView struct {
	Process      string `json:"process"`
	Size         string `json:"size"`
	StorageClass string `json:"storageClass,omitempty"`
	MountPath    string `json:"mountPath"`
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

// volumeOf is the claim's volume, and nothing at all for a claim of another
// type.
func volumeOf(claim *kitchenv1alpha1.ResourceClaim) *claimVolumeView {
	cfg := claim.Volume()
	if cfg == (kitchenv1alpha1.VolumeConfig{}) {
		return nil
	}
	view := &claimVolumeView{
		Process:      cfg.Process,
		Size:         cfg.Size,
		StorageClass: cfg.StorageClass,
		MountPath:    cfg.MountPath,
	}
	if status := claim.Status.Volume; status != nil {
		view.StorageClass = status.StorageClass
		view.AccessMode = status.AccessMode
		view.AccessModeReason = status.AccessModeReason
		view.ClaimName = status.ClaimName
		view.PersistentVolume = status.PersistentVolume
	}
	return view
}
