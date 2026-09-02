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
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Removing a platform dependency, which is the operation with the largest
// blast radius the platform has.
//
// Deleting an Addon is three different things depending on what is there, and
// conflating them is how somebody loses a database:
//
//   - An entry something depends on does not go at all. A Connection
//     provisioning through it, or a claim bound through one, means the delete
//     is refused and the Addon stays — with the dependents named, so the
//     answer is a list of things to remove and not a shrug.
//   - An entry the platform did not install loses its record and nothing
//     else. The release is somebody else's; the platform never wrote to it
//     and does not get to remove it either.
//   - An entry the platform installed is uninstalled, by a job under the same
//     account that installed it, in the reverse of the order it went in.
//
// What is *not* here is a confirmation prompt. That belongs to the API and
// the dashboard, which state the blast radius before they send the delete —
// the same shape as deleting a project.

const (
	// addonUninstallComponentSuffix names an uninstall job apart from the
	// install job of the same entry, so the log collector and the ownership
	// evidence never confuse the two: an uninstall that completed is not
	// evidence that anything is installed.
	addonUninstallComponentSuffix = "-uninstall"

	// addonUninstallTimeout is what each helm uninstall is given. They
	// --wait, so it is time for the workloads to actually go.
	addonUninstallTimeout = 10 * time.Minute
)

// finalize answers a deleted Addon.
func (r *AddonReconciler) finalize(
	ctx context.Context,
	addon *kitchenv1alpha1.Addon,
	entry addonEntry,
	known bool,
) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(addon, addonFinalizer) {
		return ctrl.Result{}, nil
	}

	// An Addon naming no catalogue entry installed nothing and so has
	// nothing to remove. It is let go rather than held on a refusal nobody
	// can satisfy.
	if !known {
		return r.release(ctx, addon)
	}

	dependents, err := r.addonDependents(ctx, entry)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(dependents) > 0 {
		setAddonCondition(addon, metav1.ConditionFalse, "UninstallRefused", fmt.Sprintf(
			"%s cannot be uninstalled while %s. Remove those first; this Addon stays until they are gone",
			entry.Title, strings.Join(dependents, ", ")))
		if err := r.Status().Update(ctx, addon); err != nil {
			return ctrl.Result{}, err
		}
		log.Info("refused addon uninstall", "addon", addon.Name, "dependents", len(dependents))
		return ctrl.Result{RequeueAfter: addonRequeueDelay}, nil
	}

	// A release the platform did not create is not the platform's to remove.
	if !addon.Status.Managed {
		return r.release(ctx, addon)
	}

	done, err := r.runUninstall(ctx, addon, entry)
	if err != nil || !done {
		return ctrl.Result{RequeueAfter: addonRequeueDelay}, err
	}
	return r.release(ctx, addon)
}

// release drops the finalizer and lets the object go.
func (r *AddonReconciler) release(ctx context.Context, addon *kitchenv1alpha1.Addon) (ctrl.Result, error) {
	controllerutil.RemoveFinalizer(addon, addonFinalizer)
	return ctrl.Result{}, client.IgnoreNotFound(r.Update(ctx, addon))
}

// addonDependents is what would break if the entry went, named the way
// somebody could go and find them.
//
// It is Connections and claims and nothing else, deliberately: those are the
// two objects whose whole function is provisioning through the dependency, so
// their existence is a statement that something is using it. What the entry
// costs beyond them is entry.BlastRadius, which is stated rather than
// refused — an environment that stops idling is a regression somebody chose,
// not data nobody can get back.
func (r *AddonReconciler) addonDependents(ctx context.Context, entry addonEntry) ([]string, error) {
	if len(entry.Providers) == 0 {
		return nil, nil
	}
	providers := map[string]bool{}
	for _, provider := range entry.Providers {
		providers[provider] = true
	}

	connections := &kitchenv1alpha1.ConnectionList{}
	if err := r.List(ctx, connections, client.InNamespace(PlatformNamespace)); err != nil {
		return nil, err
	}
	using := map[string]bool{}
	dependents := make([]string, 0, len(connections.Items))
	for i := range connections.Items {
		conn := &connections.Items[i]
		if !providers[conn.Spec.Provider] {
			continue
		}
		using[conn.Name] = true
		dependents = append(dependents, fmt.Sprintf("connection %q provisions through it", conn.Name))
	}

	claims := &kitchenv1alpha1.ResourceClaimList{}
	if err := r.List(ctx, claims); err != nil {
		return nil, err
	}
	for i := range claims.Items {
		claim := &claims.Items[i]
		if !using[claim.Connection()] {
			continue
		}
		dependents = append(dependents, fmt.Sprintf("claim %q in project %q is bound through it",
			claim.Name, claim.Spec.ProjectRef.Name))
	}

	sort.Strings(dependents)
	return dependents, nil
}

// runUninstall creates the uninstall job, or reads the outcome of the one it
// created earlier. It answers whether the entry is gone.
func (r *AddonReconciler) runUninstall(
	ctx context.Context,
	addon *kitchenv1alpha1.Addon,
	entry addonEntry,
) (bool, error) {
	name := addonUninstallJobName(entry)
	namespace := addon.Status.Namespace
	if namespace == "" {
		namespace = entry.DefaultNamespace
	}
	cfg := r.Installs.forEntry(entry)

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: name}, job)
	switch {
	case apierrors.IsNotFound(err):
		// The account that installed it is the account that removes it, and
		// an installation that has since revoked the grant cannot do either.
		// The Addon stays, saying so, rather than disappearing while the
		// release it stood for keeps running.
		if !r.Installs.permits(entry.ID) {
			setAddonCondition(addon, metav1.ConditionFalse, "UninstallNotPermitted", fmt.Sprintf(
				"the platform installed %s and can no longer remove it: this installation has revoked the "+
					"install account. Set `--set %s=true` to let it finish, or uninstall the release yourself "+
					"and delete this Addon again", entry.Title, entry.ChartValue))
			return false, r.Status().Update(ctx, addon)
		}
		if err := r.Create(ctx, addonUninstallJob(name, namespace, entry, cfg)); err != nil &&
			!apierrors.IsAlreadyExists(err) {
			return false, err
		}
		setAddonCondition(addon, metav1.ConditionFalse, "Uninstalling", fmt.Sprintf(
			"removing %s from %s. %s", entry.Title, namespace, entry.BlastRadius))
		return false, r.Status().Update(ctx, addon)
	case err != nil:
		return false, err
	}

	complete, failed, message := jobOutcome(job)
	switch {
	case complete:
		return true, nil
	case failed:
		setAddonCondition(addon, metav1.ConditionFalse, "UninstallFailed", fmt.Sprintf(
			"removing %s failed: %s. The job's logs are in the platform's own, under the component %q; it is "+
				"retried once the finished job is reaped", entry.Title, message, entry.Component+
				addonUninstallComponentSuffix))
		return false, r.Status().Update(ctx, addon)
	default:
		setAddonCondition(addon, metav1.ConditionFalse, "Uninstalling", fmt.Sprintf(
			"removing %s from %s. %s", entry.Title, namespace, entry.BlastRadius))
		return false, r.Status().Update(ctx, addon)
	}
}

// addonUninstallJobName names the uninstall after the entry. It carries no
// version: there is only ever one release of an entry to remove, and a job
// named after a pin would be re-created by a pin that moved mid-uninstall.
func addonUninstallJobName(entry addonEntry) string {
	name := "kitchen-" + entry.ID + "-uninstall"
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	return name
}

// addonUninstallJob removes the entry's releases, in the reverse of the order
// they went in — the add-on before the KEDA it was installed against, so that
// nothing is left reconciling against a CRD that has gone.
//
// Its argv takes nothing from a request either. The release names are the
// catalogue's and the namespace is the one the install recorded, checked
// against a DNS label before it reaches helm as its own argument.
func addonUninstallJob(name, namespace string, entry addonEntry, cfg AddonInstallConfig) *batchv1.Job {
	labels := installLabels(map[string]string{
		labelManagedByKey:  labelManagedByValue,
		labelComponentKind: entry.Component + addonUninstallComponentSuffix,
	}, namespace, nil)

	env := []corev1.EnvVar{
		{Name: "HELM_CACHE_HOME", Value: "/tmp/helm/cache"},
		{Name: "HELM_CONFIG_HOME", Value: "/tmp/helm/config"},
		{Name: "HELM_DATA_HOME", Value: "/tmp/helm/data"},
	}
	mounts := []corev1.VolumeMount{{Name: "helm-home", MountPath: "/tmp"}}

	container := func(chart addonChart) corev1.Container {
		return corev1.Container{
			Name:    chart.Chart,
			Image:   cfg.HelmImage,
			Command: []string{"helm"},
			Args: []string{
				"uninstall", chart.Release,
				"--namespace", namespace,
				// A release that is already gone is the outcome asked for,
				// not a failure to report.
				"--ignore-not-found",
				"--wait",
				"--timeout", addonUninstallTimeout.String(),
			},
			Env:          env,
			VolumeMounts: mounts,
		}
	}

	reversed := make([]addonChart, 0, len(entry.Charts))
	for i := len(entry.Charts) - 1; i >= 0; i-- {
		reversed = append(reversed, entry.Charts[i])
	}
	last := len(reversed) - 1
	initContainers := make([]corev1.Container, 0, last)
	for _, chart := range reversed[:last] {
		initContainers = append(initContainers, container(chart))
	}

	total := time.Duration(len(reversed)) * addonUninstallTimeout
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: PlatformNamespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(0)),
			ActiveDeadlineSeconds:   ptr.To(int64((total + addonInstallDeadlineBuffer).Seconds())),
			TTLSecondsAfterFinished: ptr.To(int32(addonInstallJobTTLSeconds)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: cfg.ServiceAccount,
					InitContainers:     initContainers,
					Containers:         []corev1.Container{container(reversed[last])},
					Volumes: []corev1.Volume{{
						Name:         "helm-home",
						VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
					}},
				},
			},
		},
	}
}
