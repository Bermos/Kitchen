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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/naming"
)

// How a claim's certificate authority reaches the pods that read the claim
// (#456).
//
// A binding carries the certificate itself, because an application pod cannot
// mount a Secret in the platform's own namespace and no image the platform
// did not build carries a root the platform generated. That made verification
// possible and left every application to plumb it: both Postgres drivers read
// `sslrootcert` as a *file*, `AWS_CA_BUNDLE` names a file, and a certificate
// delivered as a variable is one the application has to write to disk itself
// before any of that works. So an application on a claim either passed
// `ssl: { ca: process.env.DATABASE_CA }` by hand or verified nothing — and
// the second is what a `pg` application did on a binding that had just gained
// `sslmode=require`, since node-postgres reads `require` as `verify-full`.
//
// The platform knows all of it, so the platform does it. Every workload the
// claim's variables reach — the web process, its workers, its services, its
// scheduled runs and its deploy-time tasks — mounts the authority of every
// claim it reads, at [contract.CADir], and the binding names that path back:
// `sslmode=verify-full&sslrootcert=…` in a database URL, `caCertFile` beside
// `caCert` in a bucket's. An application that nobody has touched verifies.
//
// Two rules keep the two halves from ever disagreeing:
//
//   - **The mount is unconditional.** Every binding Secret with a non-empty
//     authority key is mounted, whether or not anything reads the key, so a
//     binding can never name a path that is not there. It is the cheap half
//     of the pair: a projected Secret key and no container change.
//   - **A binding with no authority is untouched.** An external provider
//     whose certificate the host's public roots already vouch for keeps the
//     URL it had, mounts nothing, and carries no path — because a path is
//     only ever written next to a certificate to mount at it.

// claimCAVolumePrefix names the volumes this file adds, inside the
// `kitchen-` reservation every platform-owned object in an application
// namespace sits in (#426).
const claimCAVolumePrefix = "kitchen-claim-ca-"

// dnsLabelBudget is what a volume name has to fit, since one is a DNS label.
// A claim name is a DNS label too, so the prefix is what can push it over.
const dnsLabelBudget = 63

// claimCA is one claim's certificate authority as a workload mounts it.
type claimCA struct {
	// claim is what the mount directory is named after — the claim, never
	// the resource behind it, so that a preview reading its own branch finds
	// its certificate exactly where production's binding says it is.
	claim string
	// secret is the binding Secret it is projected from: the claim's own, or
	// a preview's branch binding.
	secret string
	// key is which key of that Secret holds it — `ca` for a database, or
	// `caCert` for an object store.
	key string
}

// volumeName is the pod volume this authority is projected through. It is
// derived from the claim so that two claims of one project cannot collide,
// and truncated with a digest because a 63-character claim name plus the
// prefix does not fit a DNS label.
func (c claimCA) volumeName() string {
	return naming.Truncate(claimCAVolumePrefix+c.claim, dnsLabelBudget)
}

// claimCAFor answers what a workload reading this claim's binding mounts, and
// nil where the binding hands over no authority.
//
// A binding Secret that is not there yet answers nil rather than an error: it
// is written by the claim's own reconciler, the reconcile that writes it
// wakes this one, and an environment referencing a claim whose Secret has
// gone missing already fails on the variable rather than on the mount.
func (r *EnvironmentReconciler) claimCAFor(
	ctx context.Context,
	appNS, claimName, secretName string,
) (*claimCA, error) {
	binding := &corev1.Secret{}
	key := types.NamespacedName{Namespace: appNS, Name: secretName}
	if err := r.Get(ctx, key, binding); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	caKey := contract.BindingCAKey(binding.Data)
	if caKey == "" {
		return nil, nil
	}
	return &claimCA{claim: claimName, secret: secretName, key: caKey}, nil
}

// claimCAsOnPod puts the authorities on a pod spec: one volume per claim, and
// one mount on the application container.
//
// It edits the spec the caller has finished building, which is what keeps the
// two pod shapes — the web process's Deployment, and the one every other
// workload runs — from each growing their own version of this, exactly as
// configFilesOnPod does.
//
// Nothing rolls a workload from here: a certificate that changes changes the
// Secret, and stampSecretsRevision already digests every Secret the finished
// template reads, a mounted one included.
func claimCAsOnPod(spec *corev1.PodSpec, cas []claimCA) {
	for _, ca := range cas {
		spec.Volumes = append(spec.Volumes, corev1.Volume{
			Name: ca.volumeName(),
			VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
				SecretName: ca.secret,
				// The one key, under the one name a binding can name it by.
				// Projecting the whole Secret would put the password on disk
				// beside the certificate, which is a credential in a file
				// nothing asked for.
				Items: []corev1.KeyToPath{{Key: ca.key, Path: contract.CAFileName}},
			}},
		})
		mount := corev1.VolumeMount{
			Name:      ca.volumeName(),
			MountPath: contract.CADir(ca.claim),
			// The platform's to place and the application's to read. A
			// directory of its own, so the mount shadows nothing the image
			// shipped.
			ReadOnly: true,
		}
		for i := range spec.Containers {
			if spec.Containers[i].Name != AppContainerName {
				continue
			}
			spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts, mount)
		}
	}
}
