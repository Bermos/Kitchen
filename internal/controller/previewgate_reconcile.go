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
	"crypto/rand"
	"encoding/base64"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/idp"
	"github.com/Bermos/Kitchen/internal/previewgate"
)

// reconcilePreviewGate runs the forward-auth gate protected previews are
// served through: its signing key, its OAuth client at the identity provider,
// and the Deployment, Service and route that put it in the request path.
//
// The operator owns all of it rather than the chart, for the same reason it
// owns cloudflared: the gate cannot start before an OAuth client exists, and
// only the operator can register one. A chart that deployed it would be
// waiting on a reconcile it has no way to wait for.
func (r *KitchenReconciler) reconcilePreviewGate(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	setCond func(string, metav1.ConditionStatus, string, string),
) bool {
	gate := previewGate(kitchen)
	if gate == nil {
		// No identity provider, or no gate. Previews are either public by the
		// Project's own choice or not routed at all; the Environment
		// reconciler is the one that decides which, per Project.
		if err := r.removePreviewGate(ctx); err != nil {
			setCond(condPreviewGateReady, metav1.ConditionFalse, "CleanupFailed", err.Error())
			return false
		}
		meta.RemoveStatusCondition(&kitchen.Status.Conditions, condPreviewGateReady)
		return true
	}

	if r.PreviewGateImage == "" {
		setCond(condPreviewGateReady, metav1.ConditionFalse, "ImageUnset",
			"the manager was started without --preview-gate-image, so there is no gate to run")
		return false
	}

	oauth, gateErr := r.ensurePreviewGateClient(ctx, kitchen)
	if gateErr != nil {
		setCond(condPreviewGateReady, metav1.ConditionFalse, gateErr.reason, gateErr.Error())
		return false
	}
	cookieSecret, err := r.ensurePreviewGateSigningKey(ctx)
	if err != nil {
		setCond(condPreviewGateReady, metav1.ConditionFalse, "SigningKeyUnavailable", err.Error())
		return false
	}

	if err := r.applyPreviewGateWorkload(ctx, kitchen, oauth, cookieSecret); err != nil {
		setCond(condPreviewGateReady, metav1.ConditionFalse, "DeployFailed", err.Error())
		return false
	}
	if err := r.applyPreviewGateRoute(ctx, gate, gatewaySection(kitchen)); err != nil {
		setCond(condPreviewGateReady, metav1.ConditionFalse, "RouteFailed", err.Error())
		return false
	}

	if !r.previewGateAvailable(ctx) {
		setCond(condPreviewGateReady, metav1.ConditionFalse, "GatePending",
			"the forward-auth gate is not available yet")
		return false
	}
	setCond(condPreviewGateReady, metav1.ConditionTrue, "GateRunning",
		fmt.Sprintf("protected previews are gated at %s, signing visitors in at %s",
			gate.Host, oauth.Issuer))
	return true
}

// gateClient is the registered OAuth client and how to reach the issuer it
// belongs to.
type gateClient struct {
	Issuer      string
	InternalURL string
	ID          string
	CallbackURL string
}

// gateError carries the condition reason a failure should be reported under.
type gateError struct {
	reason string
	err    error
}

func (e *gateError) Error() string { return e.err.Error() }

// ensurePreviewGateClient makes sure the gate has an OAuth client at the
// platform's identity provider, and that its credentials are in the Secret
// the gate reads.
//
// Registration goes through dynamic client registration — the integration
// contract in docs/AUTH.md — so this works against any issuer that supports
// it, not just the better-auth service the chart ships. Credentials are
// handed out once and never again, which is why the Secret, not the issuer,
// is the source of truth: a client is only registered again when the Secret
// is missing, incomplete, or was written for a different issuer or callback.
func (r *KitchenReconciler) ensurePreviewGateClient(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
) (*gateClient, *gateError) {
	ref := kitchen.Spec.Auth.SecretRef
	if ref == nil {
		return nil, &gateError{"AuthSecretMissing",
			fmt.Errorf("spec.auth.secretRef is required to register the gate's OAuth client")}
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: PlatformNamespace, Name: ref.Name}, secret); err != nil {
		return nil, &gateError{"AuthSecretMissing", err}
	}
	cfg, err := idp.ConfigFromSecret(secret)
	if err != nil {
		return nil, &gateError{"AuthSecretInvalid", err}
	}

	callback := previewGateCallbackURL(kitchen)
	resolved := &gateClient{
		Issuer:      cfg.Issuer,
		InternalURL: internalURLOf(cfg),
		CallbackURL: callback,
	}

	key := types.NamespacedName{Namespace: PlatformNamespace, Name: PreviewGateClientSecretName}
	current := &corev1.Secret{}
	err = r.Get(ctx, key, current)
	switch {
	case err == nil && gateClientMatches(current, cfg, callback):
		// Registered already, and still for this issuer and this callback:
		// nothing to do, and nothing to write either — rewriting the Secret
		// on every reconcile would roll the gate for no reason.
		resolved.ID = string(current.Data[gateSecretKeyClientID])
		return resolved, nil
	case err != nil && !apierrors.IsNotFound(err):
		return nil, &gateError{"ClientSecretUnreadable", err}
	}

	registered, err := idp.New(cfg).Register(ctx, idp.ClientRegistration{
		Name:         "Kitchen preview gate",
		RedirectURIs: []string{callback},
		// The gate exchanges a code once and mints its own session from the
		// result; it never refreshes a token, so it asks for no grant that
		// would let it.
		GrantTypes: []string{"authorization_code"},
		Scopes:     []string{"openid", "profile", "email"},
	})
	if err != nil {
		return nil, &gateError{"ClientNotRegistered", err}
	}
	if err := r.writeGateClientSecret(ctx, key, cfg, callback, registered.ID, registered.Secret); err != nil {
		// The client exists at the issuer but its secret never reached the
		// cluster: it is unusable, and the next reconcile registers another.
		// Say so, because the abandoned one has to be cleaned up by hand.
		logf.FromContext(ctx).Error(err, "registered an OAuth client the cluster could not keep",
			"clientId", registered.ID, "issuer", cfg.Issuer)
		return nil, &gateError{"ClientSecretNotWritten", err}
	}
	resolved.ID = registered.ID
	return resolved, nil
}

// gateClientMatches reports whether a stored client is still the right one:
// complete, and registered for this issuer, reached this way, returning to
// this callback. Anything else means registering again.
func gateClientMatches(secret *corev1.Secret, cfg idp.Config, callback string) bool {
	return string(secret.Data[gateSecretKeyClientID]) != "" &&
		string(secret.Data[gateSecretKeyClientSecret]) != "" &&
		string(secret.Data[gateSecretKeyIssuer]) == cfg.Issuer &&
		string(secret.Data[gateSecretKeyInternalURL]) == internalURLOf(cfg) &&
		string(secret.Data[gateSecretKeyCallbackURL]) == callback
}

// internalURLOf is the cluster-internal address of the issuer, empty when the
// operator reaches it at its public URL like everyone else.
func internalURLOf(cfg idp.Config) string {
	if cfg.BaseURL == cfg.Issuer {
		return ""
	}
	return cfg.BaseURL
}

// writeGateClientSecret stores the gate's client where its Deployment can
// read it. The internal URL travels with it so the gate reaches the issuer
// the same way the operator does.
func (r *KitchenReconciler) writeGateClientSecret(
	ctx context.Context,
	key types.NamespacedName,
	cfg idp.Config,
	callback, clientID, clientSecret string,
) error {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = previewGateLabels()
		secret.Type = corev1.SecretTypeOpaque
		secret.StringData = map[string]string{
			gateSecretKeyIssuer:       cfg.Issuer,
			gateSecretKeyInternalURL:  internalURLOf(cfg),
			gateSecretKeyClientID:     clientID,
			gateSecretKeyClientSecret: clientSecret,
			gateSecretKeyCallbackURL:  callback,
		}
		return nil
	})
	return err
}

// ensurePreviewGateSigningKey generates the key the gate signs sessions with,
// once. Deleting the Secret rotates it, which signs every preview visitor
// out and is the supported way to do that.
func (r *KitchenReconciler) ensurePreviewGateSigningKey(ctx context.Context) (string, error) {
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: PreviewGateSecretName}
	secret := &corev1.Secret{}
	err := r.Get(ctx, key, secret)
	if err == nil {
		if value := string(secret.Data[gateSecretKeyCookie]); value != "" {
			return value, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", err
	}

	generated, err := randomSecret()
	if err != nil {
		return "", err
	}
	secret = &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, secret, func() error {
		secret.Labels = previewGateLabels()
		secret.Type = corev1.SecretTypeOpaque
		if secret.StringData == nil {
			secret.StringData = map[string]string{}
		}
		secret.StringData[gateSecretKeyCookie] = generated
		return nil
	})
	if err != nil {
		return "", err
	}
	return generated, nil
}

// randomSecret returns a signing key with 48 bytes of entropy behind it.
func randomSecret() (string, error) {
	buf := make([]byte, 48)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// applyPreviewGateWorkload deploys the gate and the Service protected routes
// send traffic to.
func (r *KitchenReconciler) applyPreviewGateWorkload(
	ctx context.Context,
	kitchen *kitchenv1alpha1.Kitchen,
	oauth *gateClient,
	cookieSecret string,
) error {
	labels := previewGateLabels()
	fromClientSecret := func(key string) *corev1.EnvVarSource {
		return &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: PreviewGateClientSecretName},
			Key:                  key,
		}}
	}

	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: PreviewGateName, Namespace: PlatformNamespace,
	}}
	if _, err := controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec.Replicas = ptr.To(previewGateReplicas(kitchen))
		deploy.Spec.Selector = &metav1.LabelSelector{MatchLabels: map[string]string{labelComponentKey: PreviewGateName}}
		deploy.Spec.Template.Labels = labels
		// Re-registering the client changes what the pods must run with, and
		// environment variables from a Secret do not change under a running
		// pod. The client id is not secret, so it can say so here.
		deploy.Spec.Template.Annotations = map[string]string{
			"kitchen.bermos.dev/oauth-client": oauth.ID,
		}
		deploy.Spec.Template.Spec.Containers = []corev1.Container{{
			Name:    "gate",
			Image:   r.PreviewGateImage,
			Command: []string{"/gate"},
			Env: []corev1.EnvVar{
				{Name: "KITCHEN_GATE_ADDR", Value: fmt.Sprintf(":%d", previewGateContainerPort)},
				{Name: "KITCHEN_GATE_HEALTH_ADDR", Value: fmt.Sprintf(":%d", previewGateHealthPort)},
				{Name: "KITCHEN_GATE_ISSUER", ValueFrom: fromClientSecret(gateSecretKeyIssuer)},
				{Name: "KITCHEN_GATE_ISSUER_INTERNAL_URL", ValueFrom: fromClientSecret(gateSecretKeyInternalURL)},
				{Name: "KITCHEN_GATE_CLIENT_ID", ValueFrom: fromClientSecret(gateSecretKeyClientID)},
				{Name: "KITCHEN_GATE_CLIENT_SECRET", ValueFrom: fromClientSecret(gateSecretKeyClientSecret)},
				{Name: "KITCHEN_GATE_CALLBACK_URL", ValueFrom: fromClientSecret(gateSecretKeyCallbackURL)},
				{Name: "KITCHEN_GATE_COOKIE_SECRET", Value: cookieSecret},
				// A Secure cookie would never come back over plain HTTP, and
				// an installation in TLS mode "none" has chosen plain HTTP.
				{Name: "KITCHEN_GATE_COOKIE_SECURE",
					Value: fmt.Sprintf("%t", platformScheme(kitchen) == "https")},
				{Name: "KITCHEN_GATE_SESSION_TTL", Value: previewGateSessionTTL(kitchen).String()},
			},
			Ports: []corev1.ContainerPort{
				{Name: "http", ContainerPort: previewGateContainerPort},
				{Name: "health", ContainerPort: previewGateHealthPort},
			},
			LivenessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
					Path: "/healthz", Port: intstr.FromString("health"),
				}},
				PeriodSeconds: 20,
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
					Path: "/readyz", Port: intstr.FromString("health"),
				}},
				PeriodSeconds: 10,
			},
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("64Mi"),
				},
				Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")},
			},
			SecurityContext: &corev1.SecurityContext{
				AllowPrivilegeEscalation: ptr.To(false),
				ReadOnlyRootFilesystem:   ptr.To(true),
				RunAsNonRoot:             ptr.To(true),
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			},
		}}
		return nil
	}); err != nil {
		return err
	}

	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: PreviewGateName, Namespace: PlatformNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, svc, func() error {
		svc.Labels = labels
		svc.Spec.Selector = map[string]string{labelComponentKey: PreviewGateName}
		svc.Spec.Ports = []corev1.ServicePort{{
			Name:       "http",
			Port:       previewGatePort,
			TargetPort: intstr.FromString("http"),
			Protocol:   corev1.ProtocolTCP,
		}}
		return nil
	})
	return err
}

// applyPreviewGateRoute publishes the gate's own hostname. It is where every
// protected preview's login comes back to, and the only redirect URI the
// OAuth client has.
func (r *KitchenReconciler) applyPreviewGateRoute(
	ctx context.Context,
	gate *previewGateBackend,
	// section is the Gateway listener to attach to; with edge TLS on, port 80
	// carries only the redirect.
	section *gatewayv1.SectionName,
) error {
	route := &gatewayv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: PreviewGateName, Namespace: PlatformNamespace,
	}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, route, func() error {
		route.Labels = previewGateLabels()
		route.Spec.CommonRouteSpec = gatewayv1.CommonRouteSpec{
			ParentRefs: []gatewayv1.ParentReference{{
				Name:        SharedGatewayName,
				Namespace:   ptr.To(gatewayv1.Namespace(PlatformNamespace)),
				SectionName: section,
			}},
		}
		route.Spec.Hostnames = []gatewayv1.Hostname{gatewayv1.Hostname(gate.Host)}
		route.Spec.Rules = []gatewayv1.HTTPRouteRule{{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path: &gatewayv1.HTTPPathMatch{
					Type:  ptr.To(gatewayv1.PathMatchPathPrefix),
					Value: ptr.To(previewgate.PathPrefix),
				},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{{
				BackendRef: gatewayv1.BackendRef{
					BackendObjectReference: gatewayv1.BackendObjectReference{
						Name: gatewayv1.ObjectName(gate.Service),
						Port: ptr.To(gatewayv1.PortNumber(gate.Port)),
					},
				},
			}},
		}}
		return nil
	})
	return err
}

// previewGateAvailable reports whether the gate can take traffic.
func (r *KitchenReconciler) previewGateAvailable(ctx context.Context) bool {
	deploy := &appsv1.Deployment{}
	key := types.NamespacedName{Namespace: PlatformNamespace, Name: PreviewGateName}
	if err := r.Get(ctx, key, deploy); err != nil {
		return false
	}
	for _, condition := range deploy.Status.Conditions {
		if condition.Type == appsv1.DeploymentAvailable && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// removePreviewGate tears the gate down when the platform stops wanting one.
// The signing key and the registered client are left behind on purpose: they
// are what a gate turned back on would need, and dropping them would sign
// everyone out and leave an orphaned client at the issuer.
func (r *KitchenReconciler) removePreviewGate(ctx context.Context) error {
	objectMeta := metav1.ObjectMeta{Name: PreviewGateName, Namespace: PlatformNamespace}
	for _, obj := range []client.Object{
		&appsv1.Deployment{ObjectMeta: objectMeta},
		&corev1.Service{ObjectMeta: objectMeta},
		&gatewayv1.HTTPRoute{ObjectMeta: objectMeta},
	} {
		if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func previewGateLabels() map[string]string {
	// The selector matches on PreviewGateName and cannot be changed on an
	// existing Deployment, so that stays exactly as it was.
	return platformLabels(PreviewGateName, "preview-gate")
}
