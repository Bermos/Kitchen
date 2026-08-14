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
	"fmt"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/previewgate"
)

const (
	// PreviewGateName is what the gate's Deployment, Service and route are
	// called in the platform namespace.
	PreviewGateName = "kitchen-preview-gate"

	// PreviewGateSecretName holds the key the gate signs sessions with. The
	// operator generates it; deleting the Secret rotates it, which signs
	// every preview visitor out.
	PreviewGateSecretName = "kitchen-preview-gate"

	// PreviewGateClientSecretName holds the OAuth client the operator
	// registered for the gate at the identity provider.
	PreviewGateClientSecretName = "kitchen-preview-gate-oidc"

	// PreviewGateHostPrefix is the subdomain the gate finishes logins on:
	// previews.<baseDomain>.
	PreviewGateHostPrefix = "previews"

	// previewGatePort is the Service port protected routes send traffic to,
	// and the container port behind it.
	previewGatePort = 80
	// previewGateContainerPort is where the proxy listens.
	previewGateContainerPort = 8080
	// previewGateHealthPort is a second listener, so that the application's
	// own /healthz stays the application's.
	previewGateHealthPort = 8081

	// defaultPreviewGateReplicas and defaultPreviewGateSessionTTL match the
	// CRD defaults, for Kitchen objects written before the fields existed.
	defaultPreviewGateReplicas   = 2
	defaultPreviewGateSessionTTL = 8 * time.Hour

	// Keys the operator writes into the gate's OAuth client secret.
	gateSecretKeyIssuer       = "issuer"
	gateSecretKeyInternalURL  = "issuerInternalURL"
	gateSecretKeyClientID     = "clientId"
	gateSecretKeyClientSecret = "clientSecret"
	gateSecretKeyCallbackURL  = "callbackUrl"

	// gateSecretKeyCookie is the signing key in the gate's own secret.
	gateSecretKeyCookie = "cookieSecret"
)

// previewGateBackend is a resolved gate: where protected routes send traffic,
// and which host finishes their logins.
type previewGateBackend struct {
	// Service and Port in the platform namespace.
	Service string
	Port    int32
	// Host the gate serves its own endpoints on.
	Host string
}

// previewGate resolves the platform's gate, or nil when there is none to
// route through. Both switches are meaningful: an installation without an
// identity provider has nothing to authenticate against, and one that turns
// the gate off has asked for previews to be served some other way.
func previewGate(kitchen *kitchenv1alpha1.Kitchen) *previewGateBackend {
	if !kitchen.Spec.Auth.Enabled || !kitchen.Spec.Auth.PreviewGate.Enabled {
		return nil
	}
	host := previewGateHost(kitchen)
	if host == "" {
		return nil
	}
	return &previewGateBackend{Service: PreviewGateName, Port: previewGatePort, Host: host}
}

// previewGateHost is the hostname the gate finishes logins on.
func previewGateHost(kitchen *kitchenv1alpha1.Kitchen) string {
	if host := kitchen.Spec.Auth.PreviewGate.Host; host != "" {
		return host
	}
	if kitchen.Spec.BaseDomain == "" {
		return ""
	}
	return fmt.Sprintf("%s.%s", PreviewGateHostPrefix, kitchen.Spec.BaseDomain)
}

// previewGateCallbackURL is the single redirect URI the gate's OAuth client
// is registered with. Previews come and go behind it without the client ever
// changing — which is the whole reason the login finishes on a host of the
// platform's own rather than on the preview's.
func previewGateCallbackURL(kitchen *kitchenv1alpha1.Kitchen) string {
	host := previewGateHost(kitchen)
	if host == "" {
		return ""
	}
	return fmt.Sprintf("%s://%s%s", platformScheme(kitchen), host, previewgate.CallbackPath)
}

// previewGateSessionTTL is how long a visitor stays signed in to a preview.
func previewGateSessionTTL(kitchen *kitchenv1alpha1.Kitchen) time.Duration {
	if ttl := kitchen.Spec.Auth.PreviewGate.SessionTTL; ttl != nil && ttl.Duration > 0 {
		return ttl.Duration
	}
	return defaultPreviewGateSessionTTL
}

// previewGateReplicas is how many gates run.
func previewGateReplicas(kitchen *kitchenv1alpha1.Kitchen) int32 {
	if replicas := kitchen.Spec.Auth.PreviewGate.Replicas; replicas > 0 {
		return replicas
	}
	return defaultPreviewGateReplicas
}

// upstreamAddress is the application a protected route forwards to, in the
// form the gate expects: an in-cluster Service address it can check.
func upstreamAddress(appNS, name string, port int32) string {
	return fmt.Sprintf("%s.%s.svc.cluster.local:%d", name, appNS, port)
}

// platformScheme is how generated URLs are reached from outside, which
// follows the platform's TLS mode.
func platformScheme(kitchen *kitchenv1alpha1.Kitchen) string {
	return kitchen.Spec.TLS.Mode.Scheme()
}
