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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// How an Environment places its project's configuration files (#311).
//
// A file arrives here already decided: the Release's snapshot says what is in
// it, where it goes and which workloads read it, and this file is only the
// mechanism. That is deliberate — the snapshot is what makes a rollback
// restore the file the release ran with, and a reconciler that read the
// project instead would restore today's.
//
// Two objects carry the content, and which one is which follows from whether
// the file is a credential:
//
//   - **Plain files** are one ConfigMap per environment, rebuilt from the
//     Release on every pass. Per environment because the content is the
//     *release's*: two environments of one project on two releases hold two
//     different files, and a rollback is exactly the case where that has to
//     be true.
//   - **Secret files** are the project's own Secret, mirrored into the
//     application namespace beside the project's secrets — see
//     projectsecrets.go, which mirrors both. Their content is never in a
//     Release, because a Release is readable by everyone who may read the
//     project and a credential is not.
//
// Both are mounted with `subPath`, so the file lands beside whatever else the
// image has in that directory instead of replacing it. A subPath mount does
// not follow later updates to the object behind it, which costs nothing here
// and is the honest shape anyway: what makes a changed file reach a running
// pod is the digest below, not the kubelet noticing.

const (
	// ConfigFilesVolumeName and secretFilesVolumeName are the two volumes a
	// pod mounts its configuration files from — one per source object,
	// however many files come out of it, with one mount per file.
	ConfigFilesVolumeName = "kitchen-files"
	secretFilesVolumeName = "kitchen-secret-files"

	// ConfigFilesRevisionAnnotation carries a digest of the *plain*
	// configuration files one workload reads. It is on the pod template, so
	// a release whose files differ rolls the workloads that read them and
	// leaves every other workload alone.
	//
	// Without it, two releases differing only in a file's content would
	// produce identical pod templates: the ConfigMap would be rewritten and
	// nothing would restart, so a rollback would restore the file on disk
	// for the next pod to start and not for the ones already running. That
	// is the same defect #288 found in a rotated secret, and it takes the
	// same answer.
	//
	// A secret file needs no entry here: it is mounted from a Secret, and
	// SecretsRevisionAnnotation already digests every Secret a pod reads by
	// any path, a mounted file included.
	ConfigFilesRevisionAnnotation = "kitchen.bermos.dev/config-files-revision"
)

// configFilesName is the ConfigMap one environment's plain files live in.
func configFilesName(envName string) string { return envName + "-files" }

// applyConfigFiles writes the environment's plain configuration files, and
// removes the object when the release has none.
//
// It runs before anything that mounts them — the deploy tasks first of all,
// which are a pod like any other and are the first thing of a release to run.
// A container referencing a key that is not there yet does not start, so the
// object goes in before the pods that name it rather than after.
func (r *EnvironmentReconciler) applyConfigFiles(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	appNS string,
	labels map[string]string,
) error {
	name := configFilesName(env.Name)
	files := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: appNS}}

	content := map[string]string{}
	for _, file := range release.Spec.ConfigSnapshot.Files {
		if !file.Secret {
			content[file.Name] = file.Content
		}
	}
	if len(content) == 0 {
		// Nothing to place. The object goes rather than staying behind
		// empty, so that a file taken off a project takes its ConfigMap with
		// it — and so the next reconcile does not have to reason about an
		// object holding a file nothing mounts.
		if err := r.Delete(ctx, files); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, files, func() error {
		files.Labels = labels
		// Written whole every time, so that a file removed from the release
		// is removed from the object rather than lingering under its key.
		files.Data = content
		return nil
	})
	return err
}

// configFileMounts is the pod spec's half of one workload's files: at most
// two volumes — the environment's ConfigMap and the project's Secret — and
// one mount per file, each naming the file itself.
//
// `envName` is the environment whose ConfigMap the plain files come from, and
// the files are the Release's, already narrowed to this workload.
func configFileMounts(envName string, files []kitchenv1alpha1.ConfigFile) ([]corev1.Volume, []corev1.VolumeMount) {
	if len(files) == 0 {
		return nil, nil
	}
	var plain, secret []corev1.KeyToPath
	mounts := make([]corev1.VolumeMount, 0, len(files))
	for _, file := range files {
		volume := ConfigFilesVolumeName
		if file.Secret {
			volume = secretFilesVolumeName
			secret = append(secret, corev1.KeyToPath{Key: file.Name, Path: file.Name})
		} else {
			plain = append(plain, corev1.KeyToPath{Key: file.Name, Path: file.Name})
		}
		mounts = append(mounts, corev1.VolumeMount{
			Name:      volume,
			MountPath: file.Path,
			SubPath:   file.Name,
			// A configuration file is the platform's to place and the
			// application's to read. An application that writes its own
			// config file back wants a volume, not this.
			ReadOnly: true,
		})
	}

	var volumes []corev1.Volume
	// Items rather than the whole object, so that a file another workload
	// reads is not projected into this one — the digest below is taken over
	// the same narrowed list, and the two have to describe one thing.
	if len(plain) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: ConfigFilesVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: configFilesName(envName)},
					Items:                plain,
				},
			},
		})
	}
	if len(secret) > 0 {
		volumes = append(volumes, corev1.Volume{
			Name: secretFilesVolumeName,
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{SecretName: ProjectFilesName, Items: secret},
			},
		})
	}
	return volumes, mounts
}

// configFilesRevision digests the plain files one workload mounts, and
// answers "" for a workload that mounts none.
//
// The content is in hand — it is the Release's — so this needs no read: a
// rollback changes the snapshot, the digest moves with it, and the workload
// rolls. The path goes into the digest with the content, because moving a
// file from one path to another is a change even where the bytes are the
// same.
func configFilesRevision(files []kitchenv1alpha1.ConfigFile) string {
	digest := sha256.New()
	counted := 0
	for _, file := range files {
		if file.Secret {
			continue
		}
		counted++
		for _, part := range []string{file.Name, file.Path, file.Content} {
			digest.Write([]byte(part))
			digest.Write([]byte{0})
		}
	}
	if counted == 0 {
		return ""
	}
	return hex.EncodeToString(digest.Sum(nil))[:16]
}

// configFilesOnPod puts one workload's files on a pod spec: the volumes, and
// the mounts on its application container.
//
// It takes the spec the caller has finished building and edits it, which is
// what keeps the three pod shapes — the web Deployment, a worker's, and a
// scheduled or deploy-time run's — from each growing their own version of
// this. The digest that rolls a workload when its files change is stamped
// separately, by whoever owns the template: a Job is created once per attempt
// and has nothing to roll.
func configFilesOnPod(spec *corev1.PodSpec, envName string, files []kitchenv1alpha1.ConfigFile) {
	volumes, mounts := configFileMounts(envName, files)
	if len(volumes) == 0 {
		return
	}
	spec.Volumes = append(spec.Volumes, volumes...)
	for i := range spec.Containers {
		if spec.Containers[i].Name != AppContainerName {
			continue
		}
		spec.Containers[i].VolumeMounts = append(spec.Containers[i].VolumeMounts, mounts...)
	}
}

// applyConfigFilesRevision stamps a pod template with the digest, or takes
// the stamp off a template that no longer reads a plain file.
func applyConfigFilesRevision(template *metav1.ObjectMeta, revision string) {
	if revision == "" {
		delete(template.Annotations, ConfigFilesRevisionAnnotation)
		return
	}
	if template.Annotations == nil {
		template.Annotations = map[string]string{}
	}
	template.Annotations[ConfigFilesRevisionAnnotation] = revision
}

// deleteConfigFiles removes an environment's ConfigMap, for the finalizer.
func (r *EnvironmentReconciler) deleteConfigFiles(ctx context.Context, appNS, envName string) error {
	files := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: configFilesName(envName), Namespace: appNS}}
	if err := r.Delete(ctx, files); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// configFilesOf is the files one workload of a Release mounts.
func configFilesOf(release *kitchenv1alpha1.Release, workload string) []kitchenv1alpha1.ConfigFile {
	return kitchenv1alpha1.ConfigFilesFor(release.Spec.ConfigSnapshot.Files, workload)
}
