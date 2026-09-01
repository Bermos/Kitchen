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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// Defaults matching the CRD's, for Kitchen objects written before the
	// fields existed. They describe the HTTP add-on installed the way the
	// chart README describes: its own Helm release in its own namespace. The
	// add-on names its proxy Service after its own chart rather than after the
	// release, so that name is a constant and not a rendered one.
	defaultInterceptorService   = "keda-add-ons-http-interceptor-proxy"
	defaultInterceptorNamespace = "keda"
	defaultInterceptorPort      = int32(8080)

	// interceptorComponentName labels the ReferenceGrants that let application
	// namespaces route to the interceptor, and names them.
	interceptorComponentName = "kitchen-interceptor"

	// ConditionScaleToZero is where an Environment says whether it parks at
	// zero when it is quiet, and why not when it does not. It is exported
	// because the API decides from it whether the scaled object is one of the
	// objects that environment is made of.
	ConditionScaleToZero = condScaleToZero
)

// HTTPScaledObjectGVK is the KEDA HTTP add-on's routing-and-scaling record for
// one application. Like cert-manager's kinds it is addressed as an
// unstructured object: the operator writes one small spec, and importing the
// add-on's Go types would tie Kitchen's build to its release cadence. It is
// exported because the API's inspect surface reads the same object back.
func HTTPScaledObjectGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: "http.keda.sh", Version: "v1alpha1", Kind: "HTTPScaledObject"}
}

// idleBackend is the interceptor an idling environment's traffic goes to. It
// holds the first request that arrives at a parked application, asks KEDA to
// scale the workload back up, and forwards the request once a pod answers —
// which is why routing has to name it rather than the application whenever the
// application is allowed to have no pods.
type idleBackend struct {
	Service   string
	Namespace string
	Port      int32
}

// Address is the in-cluster address the preview gate forwards to. A protected
// preview reaches its application through the gate, so it is the gate, not the
// Gateway, that has to be pointed at the interceptor — and it forwards the
// visitor's Host header unchanged, which is what the interceptor routes on.
func (b *idleBackend) Address() string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", b.Service, b.Namespace, b.Port)
}

// interceptorBackend resolves the platform's interceptor, or nil when the
// platform idles nothing. Scale-to-zero needs KEDA and its HTTP add-on
// running, which is an install-time choice, so this is the one switch that
// overrides every Project's policy.
func interceptorBackend(kitchen *kitchenv1alpha1.Kitchen) *idleBackend {
	if !kitchen.Spec.ScaleToZero.Enabled {
		return nil
	}
	spec := kitchen.Spec.ScaleToZero.Interceptor
	backend := &idleBackend{
		Service:   spec.Service,
		Namespace: spec.Namespace,
		Port:      spec.Port,
	}
	if backend.Service == "" {
		backend.Service = defaultInterceptorService
	}
	if backend.Namespace == "" {
		backend.Namespace = defaultInterceptorNamespace
	}
	if backend.Port == 0 {
		backend.Port = defaultInterceptorPort
	}
	return backend
}

// reconcileScaleToZero brings the environment's HTTPScaledObject in line with
// the Project's idle policy, and reports what routing should do about it: the
// interceptor to send traffic through, or nil for an environment that keeps
// its pods.
//
// The condition it returns is the Environment's ScaleToZero condition, or nil
// to say the condition does not apply and should be dropped — a platform that
// idles nothing would otherwise carry the same "off" line on every environment
// it has.
//
// Nothing here is allowed to take an application off the air. Whenever the
// scaled object cannot be put in place, the environment falls back to plain
// Deployment routing with its own replicas: routing through an interceptor
// that no operator is watching would park the workload behind a component with
// nothing to cold-start it.
func (r *EnvironmentReconciler) reconcileScaleToZero(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	project *kitchenv1alpha1.Project,
	kitchen *kitchenv1alpha1.Kitchen,
	appNS, host string,
	labels map[string]string,
	// port is the Service port the interceptor forwards to.
	port int32,
	// running is how many pods the environment has when it is not idling. The
	// policy's ceiling is never allowed below it, so turning idling on cannot
	// shrink an environment.
	running int32,
	// routed is false for an environment the platform is withholding a URL
	// from. Nothing could deliver the request that cold-starts it, so it stays
	// on its pods.
	routed bool,
	// pinnedBy names the claims this environment reads whose provider
	// declares the binding holds the workload up — a worker holding an
	// outbound connection never goes quiet, so there is nothing for the
	// interceptor to park.
	pinnedBy []string,
) (*idleBackend, *metav1.Condition, error) {
	interceptor := interceptorBackend(kitchen)
	policy := project.Spec.ScaleToZero

	switch {
	case interceptor == nil, !routed:
		return nil, nil, r.deleteHTTPScaledObject(ctx, appNS, env.Name)
	// The Project's live declaration, not the Release's frozen copy of it —
	// the same reading the policy itself gets, and for the same reason: an
	// application that turns out to do work nobody asked for must not have
	// to wait for a build to stop being parked, and a rollback must not
	// quietly start parking it again.
	//
	// It is checked before the policy so that an environment which would
	// not have idled anyway still says which of the two reasons applies.
	// "This workload is not request-driven" is the more useful sentence, and
	// it stays true when somebody later widens the mode.
	case project.Spec.Runtime.NotRequestDriven:
		if err := r.deleteHTTPScaledObject(ctx, appNS, env.Name); err != nil {
			return nil, nil, err
		}
		return nil, &metav1.Condition{
			Status: metav1.ConditionFalse,
			Reason: "NotRequestDriven",
			Message: "spec.runtime.notRequestDriven is set on this Project: the workload does work nobody " +
				"asked for, and an idle environment stops doing everything rather than only serving. " +
				"Every environment of this project keeps its pods",
		}, nil
	// A claim's provider can say the same thing about the binding it hands
	// over, and it is checked next for the same reason: the sentence naming
	// the claim stays true whatever the mode says.
	case len(pinnedBy) > 0:
		if err := r.deleteHTTPScaledObject(ctx, appNS, env.Name); err != nil {
			return nil, nil, err
		}
		return nil, &metav1.Condition{
			Status: metav1.ConditionFalse,
			Reason: "ClaimKeepsPodsRunning",
			Message: fmt.Sprintf("the provider behind claim %s declares that its binding holds the workload up "+
				"(status.keepsPodsRunning on the claim), so every environment reading it keeps its pods. "+
				"Release the claim to idle this environment", strings.Join(pinnedBy, ", ")),
		}, nil
	case !policy.Covers(env.Spec.Type):
		if err := r.deleteHTTPScaledObject(ctx, appNS, env.Name); err != nil {
			return nil, nil, err
		}
		return nil, &metav1.Condition{
			Status: metav1.ConditionFalse,
			Reason: "AlwaysOn",
			Message: fmt.Sprintf(
				"spec.scaleToZero.mode is %q on this Project, so a %s environment keeps its pods",
				policy.EffectiveMode(), env.Spec.Type),
		}, nil
	}

	ceiling := policy.MaxReplicasOrDefault()
	if ceiling < running {
		ceiling = running
	}
	idleAfter := policy.IdleAfterOrDefault()

	if err := r.applyHTTPScaledObject(ctx, env, appNS, host, labels, port, ceiling, int32(idleAfter.Seconds())); err != nil {
		if !meta.IsNoMatchError(err) {
			return nil, nil, err
		}
		// The HTTP add-on's API is not served: either the platform switch was
		// turned on without installing it, or it is still starting up. Either
		// way this environment keeps its pods and its own routing until the
		// API answers.
		return nil, &metav1.Condition{
			Status: metav1.ConditionFalse,
			Reason: "HTTPAddOnUnavailable",
			Message: "spec.scaleToZero is enabled on the platform but the HTTPScaledObject API is not served: " +
				"install KEDA and its HTTP add-on (each its own Helm release, see the chart README), or " +
				"turn the platform switch off. The environment runs its own replicas until then",
		}, nil
	}

	return interceptor, &metav1.Condition{
		Status: metav1.ConditionTrue,
		Reason: "IdlesToZero",
		Message: fmt.Sprintf(
			"parks at zero pods after %s without a request; the next request cold-starts it through %s",
			idleAfter, interceptor.Address()),
	}, nil
}

// recordScaleToZero puts the reconcile's verdict on the Environment. A nil
// condition removes it: the platform idles nothing, or this environment is not
// routed at all, and neither is worth a line on every object.
func recordScaleToZero(env *kitchenv1alpha1.Environment, condition *metav1.Condition) {
	if condition == nil {
		meta.RemoveStatusCondition(&env.Status.Conditions, condScaleToZero)
		return
	}
	condition.Type = condScaleToZero
	condition.ObservedGeneration = env.Generation
	meta.SetStatusCondition(&env.Status.Conditions, *condition)
}

// applyHTTPScaledObject writes the record the HTTP add-on scales and routes
// this environment by. The hostname is the same one the Gateway matches on:
// the interceptor picks the application out of the Host header, which is what
// lets one interceptor front every idling environment on the platform.
func (r *EnvironmentReconciler) applyHTTPScaledObject(
	ctx context.Context,
	env *kitchenv1alpha1.Environment,
	appNS, host string,
	labels map[string]string,
	port, maxReplicas, scaledownSeconds int32,
) error {
	scaled := &unstructured.Unstructured{}
	scaled.SetGroupVersionKind(HTTPScaledObjectGVK())
	scaled.SetName(env.Name)
	scaled.SetNamespace(appNS)

	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, scaled, func() error {
		scaled.SetLabels(labels)
		return unstructured.SetNestedMap(scaled.Object, map[string]any{
			"hosts": []any{host},
			"scaleTargetRef": map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"name":       env.Name,
				// The Service the interceptor forwards to, and the port on it —
				// not the container's. Both are the ones applyService writes.
				"service": env.Name,
				"port":    int64(port),
			},
			"replicas": map[string]any{
				"min": int64(0),
				"max": int64(maxReplicas),
			},
			"scaledownPeriod": int64(scaledownSeconds),
		}, "spec")
	})
	return err
}

// deleteHTTPScaledObject takes an environment back off the interceptor. An API
// that is not served has nothing to delete: a platform that never installed
// the add-on, or one it has just been uninstalled from, both mean the object
// is already gone.
func (r *EnvironmentReconciler) deleteHTTPScaledObject(ctx context.Context, appNS, name string) error {
	scaled := &unstructured.Unstructured{}
	scaled.SetGroupVersionKind(HTTPScaledObjectGVK())
	scaled.SetName(name)
	scaled.SetNamespace(appNS)
	if err := r.Delete(ctx, scaled); err != nil &&
		!apierrors.IsNotFound(err) && !meta.IsNoMatchError(err) {
		return err
	}
	return nil
}
