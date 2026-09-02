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
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/provider/contract"
)

// How an Environment mounts its project's volume claims.
//
// A volume claim is not an environment variable, so it never reaches the
// pod through resolveEnv: nothing in the Release's snapshot names it. It is
// found instead by listing the project's claims of type volume, and each is
// applied to exactly the process it names — the web Deployment, a worker's,
// or a scheduled job's pod — as a PersistentVolumeClaim volume and a mount.
//
// A volume that attaches to one pod at a time does two things to that
// process, and both are done here rather than left to the cluster to
// discover: the workload is deployed by recreation, and it runs one replica
// — the Deployment's count where the reconciler writes it, and the
// autoscaler's ceiling where it does not. A class detected to support
// ReadWriteMany lifts both.

// mountedVolume is one volume claim as this environment mounts it.
type mountedVolume struct {
	// claim is the ResourceClaim's name, and process the process it names.
	claim   string
	process string
	// claimName is the PersistentVolumeClaim — production's, or this
	// preview's own.
	claimName string
	mountPath string
	// attachOnce says the volume can be attached to one pod at a time, so
	// the process mounting it is capped at one replica and recreated.
	attachOnce bool
}

// volumeMountsFor collects the project's volume claims as this environment
// mounts them. waitingOn names a claim that has no volume for this
// environment yet — unbound, refused, or a preview whose own volume is still
// being cut — which is a reason to wait, the way an unbound claim is waited
// for: deploying without the mount would have the application write into
// its container's filesystem, and lose it at the next restart. unmounted
// names the claims that bind nothing in this preview, in their own words.
func (r *EnvironmentReconciler) volumeMountsFor(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
) (mounts []mountedVolume, waitingOn string, unmounted []string, err error) {
	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := r.List(ctx, claims, client.InNamespace(env.Namespace)); err != nil {
		return nil, "", nil, err
	}
	isPreview := env.Spec.Type == kitchenv1alpha1.EnvironmentPreview
	for i := range claims.Items {
		claim := &claims.Items[i]
		if claim.Spec.ProjectRef.Name != env.Spec.ProjectRef.Name ||
			claim.Spec.Type != kitchenv1alpha1.ClaimTypeVolume ||
			!claim.DeletionTimestamp.IsZero() {
			continue
		}
		status := claim.Status.Volume
		if status == nil || claim.Status.Phase != kitchenv1alpha1.ClaimBound {
			return nil, claim.Name, nil, nil
		}
		claimName := status.ClaimName
		if isPreview {
			switch mode := contract.PreviewMode(claim.Status.PreviewMode); {
			case mode.Isolated():
				claimName = ""
				for _, preview := range status.Previews {
					if preview.Environment == env.Name {
						claimName = preview.ClaimName
					}
				}
				if claimName == "" {
					return nil, claim.Name, nil, nil
				}
			case mode == contract.PreviewShared:
				// Production's own volume. The API refuses this choice for
				// a volume, so it is reachable only by an object written
				// another way; it is honoured rather than second-guessed.
			case mode == contract.PreviewNone:
				unmounted = append(unmounted, claim.Name+": "+claim.Status.PreviewReason)
				continue
			default:
				return nil, claim.Name, nil, nil
			}
		}
		mounts = append(mounts, mountedVolume{
			claim:      claim.Name,
			process:    status.Process,
			claimName:  claimName,
			mountPath:  status.MountPath,
			attachOnce: status.AccessMode != string(corev1.ReadWriteMany),
		})
	}
	// A stable order, so the pod spec does not differ between reconciles
	// that changed nothing.
	sort.Slice(mounts, func(a, b int) bool { return mounts[a].claim < mounts[b].claim })
	return mounts, "", unmounted, nil
}

// mountsFor is the subset of the environment's mounts one process gets.
func mountsFor(mounts []mountedVolume, process string) []mountedVolume {
	var out []mountedVolume
	for _, mount := range mounts {
		if mount.process == process {
			out = append(out, mount)
		}
	}
	return out
}

// attachesOnce reports whether any of a process's volumes can be attached to
// one pod at a time — the condition under which the process is capped at
// one replica and recreated on every deploy.
func attachesOnce(mounts []mountedVolume) bool {
	for _, mount := range mounts {
		if mount.attachOnce {
			return true
		}
	}
	return false
}

// podVolumes is the pod spec's half of the mounts: one volume per claim,
// named after it, and the container's mount of each.
func podVolumes(mounts []mountedVolume) ([]corev1.Volume, []corev1.VolumeMount) {
	if len(mounts) == 0 {
		return nil, nil
	}
	volumes := make([]corev1.Volume, 0, len(mounts))
	volumeMounts := make([]corev1.VolumeMount, 0, len(mounts))
	for _, mount := range mounts {
		name := "claim-" + mount.claim
		volumes = append(volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: mount.claimName},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: name, MountPath: mount.mountPath})
	}
	return volumes, volumeMounts
}

// capReplicas is the replica count a process mounting a volume that attaches
// once may run: one, whatever it asked for. Anything else is what it asked
// for.
func capReplicas(replicas int32, mounts []mountedVolume) int32 {
	if attachesOnce(mounts) && replicas > 1 {
		return 1
	}
	return replicas
}
