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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

const (
	// RegistryRouteName is the HTTPRoute publishing the bundled registry on
	// the shared Gateway. The chart owns the registry itself; the route is
	// the operator's, because the Gateway is.
	RegistryRouteName = "kitchen-registry"

	// RegistryConnectionName is the dockerRegistry Connection the operator
	// seeds so a fresh installation's registry picker has a working default
	// rather than an empty list.
	RegistryConnectionName = "kitchen-registry"

	// RegistryCredentialsSecretName holds that Connection's credential. The
	// prefix is the one the REST API uses for the credentials it writes, so
	// deleting the seeded connection from the connections page removes its
	// secret with it, exactly as for one someone created by hand.
	RegistryCredentialsSecretName = "kitchen-connection-" + RegistryConnectionName

	// registryProviderName is the Connection provider the seeded connection
	// uses. It is an ordinary dockerRegistry connection — the platform's own
	// registry is not a special case anywhere downstream of this.
	registryProviderName = "dockerRegistry"

	// RegistryHostPrefix is the subdomain the registry is published on:
	// registry.<baseDomain>.
	RegistryHostPrefix = "registry"

	// defaultRegistryService, defaultRegistryPort and defaultRegistrySecret
	// match what the chart writes under the conventional release name.
	//
	// They matter most on an upgrade: the Kitchen singleton is not re-applied
	// by default, because it is also editable from the UI, so an installation
	// that predates the registry reads back with the CRD's defaults and none
	// of the names the chart would have supplied. Assuming the conventional
	// release name there is what makes the registry appear on upgrade rather
	// than waiting for someone to fill three fields in — and an installation
	// under another release name gets a "secret not found" that names exactly
	// what it is looking for.
	defaultRegistryService = "kitchen-registry"
	defaultRegistryPort    = int32(5000)
	defaultRegistrySecret  = "kitchen-registry"

	// Keys the chart writes the registry's own credential under.
	registrySecretKeyUsername = "username"
	registrySecretKeyPassword = "password"

	condRegistryReady = "RegistryReady"
)

// platformRegistry is a resolved bundled registry: where it is published and
// what the route points at.
type platformRegistry struct {
	// Host images are pushed under and pulled from.
	Host string
	// Service and Port in the platform namespace.
	Service string
	Port    int32
	// SecretName holds the registry's own username and password.
	SecretName string
}

// registryURL is the prefix builds push under: <host>/<project>:<sha>. It
// carries no path segment, so a project's images are a repository of their own
// in the registry.
func (r platformRegistry) registryURL() string { return r.Host }

// resolveRegistry describes the bundled registry, or nil when the platform
// runs none. Both halves of "none" are real: an installation can turn it off
// because it has a registry of its own, and one in TLS mode none cannot have
// it at all — the node's container runtime pulls the image, and it will not
// pull over plain HTTP or an untrusted certificate without node configuration
// Kitchen is in no position to ask for.
func resolveRegistry(kitchen *kitchenv1alpha1.Kitchen) *platformRegistry {
	spec := kitchen.Spec.Registry
	if !spec.Enabled {
		return nil
	}
	if kitchen.Spec.TLS.Mode == kitchenv1alpha1.TLSModeNone {
		return nil
	}
	host := registryHost(kitchen)
	if host == "" {
		return nil
	}
	registry := &platformRegistry{
		Host:    host,
		Service: spec.Service,
		Port:    spec.Port,
	}
	if spec.SecretRef != nil {
		registry.SecretName = spec.SecretRef.Name
	}
	if registry.Service == "" {
		registry.Service = defaultRegistryService
	}
	if registry.Port == 0 {
		registry.Port = defaultRegistryPort
	}
	if registry.SecretName == "" {
		registry.SecretName = defaultRegistrySecret
	}
	return registry
}

// registryHost is the name the registry is published under.
func registryHost(kitchen *kitchenv1alpha1.Kitchen) string {
	if host := kitchen.Spec.Registry.Host; host != "" {
		return host
	}
	if kitchen.Spec.BaseDomain == "" {
		return ""
	}
	return fmt.Sprintf("%s.%s", RegistryHostPrefix, kitchen.Spec.BaseDomain)
}

// registryUnavailableReason explains an enabled registry the platform still
// cannot publish, for the condition. Empty when there is nothing to explain.
func registryUnavailableReason(kitchen *kitchenv1alpha1.Kitchen) (reason, message string) {
	switch {
	case kitchen.Spec.TLS.Mode == kitchenv1alpha1.TLSModeNone:
		return "TLSModeNone", "the bundled registry is published on the shared Gateway and pulled from by the node's " +
			"container runtime, which needs a certificate it already trusts; tls.mode none serves plain HTTP, so there " +
			"is none. Set tls.mode to acme or cloudflared, or point projects at a registry of your own."
	case registryHost(kitchen) == "":
		return "HostUnknown", "the registry has nowhere to be published: set spec.baseDomain or spec.registry.host."
	}
	return "", ""
}
