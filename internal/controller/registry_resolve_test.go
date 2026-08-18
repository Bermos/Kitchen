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
	"testing"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// registryKitchen is a singleton as the chart writes it, with the registry on.
func registryKitchen(mode kitchenv1alpha1.TLSMode) *kitchenv1alpha1.Kitchen {
	return &kitchenv1alpha1.Kitchen{
		Spec: kitchenv1alpha1.KitchenSpec{
			BaseDomain: "apps.example.com",
			TLS:        kitchenv1alpha1.TLSSpec{Mode: mode},
			Registry: kitchenv1alpha1.ImageRegistrySpec{
				Enabled:   true,
				SecretRef: &kitchenv1alpha1.LocalObjectReference{Name: "kitchen-registry"},
			},
		},
	}
}

func TestResolveRegistry(t *testing.T) {
	t.Run("defaults the host, service and port the chart usually supplies", func(t *testing.T) {
		// A Kitchen written before the registry existed carries none of them,
		// and an upgrade must not leave the route pointing at nothing.
		registry := resolveRegistry(registryKitchen(kitchenv1alpha1.TLSModeACME))
		if registry == nil {
			t.Fatal("want a registry, got none")
		}
		if registry.Host != "registry.apps.example.com" {
			t.Errorf("host = %q, want registry.apps.example.com", registry.Host)
		}
		if registry.Service != defaultRegistryService || registry.Port != defaultRegistryPort {
			t.Errorf("backend = %s:%d, want %s:%d",
				registry.Service, registry.Port, defaultRegistryService, defaultRegistryPort)
		}
		if registry.registryURL() != "registry.apps.example.com" {
			t.Errorf("push prefix = %q, want the bare host: a project's images are a repository of their own",
				registry.registryURL())
		}
	})

	t.Run("is nothing in TLS mode none", func(t *testing.T) {
		// The node's container runtime pulls the image, and it will not pull
		// over plain HTTP without node configuration Kitchen cannot ask for.
		if registry := resolveRegistry(registryKitchen(kitchenv1alpha1.TLSModeNone)); registry != nil {
			t.Fatalf("want no registry in mode none, got %+v", registry)
		}
		reason, message := registryUnavailableReason(registryKitchen(kitchenv1alpha1.TLSModeNone))
		if reason != "TLSModeNone" || message == "" {
			t.Errorf("reason = %q, message = %q, want TLSModeNone with an explanation", reason, message)
		}
	})

	t.Run("assumes the conventional credential secret when the singleton names none", func(t *testing.T) {
		// The upgrade path: the Kitchen object is not re-applied by default,
		// so an installation that predates the registry names none of this.
		kitchen := registryKitchen(kitchenv1alpha1.TLSModeACME)
		kitchen.Spec.Registry.SecretRef = nil
		registry := resolveRegistry(kitchen)
		if registry == nil {
			t.Fatal("want a registry on an upgraded singleton, got none")
		}
		if registry.SecretName != defaultRegistrySecret {
			t.Errorf("secret = %q, want %q", registry.SecretName, defaultRegistrySecret)
		}
	})

	t.Run("is nothing with nowhere to be published", func(t *testing.T) {
		kitchen := registryKitchen(kitchenv1alpha1.TLSModeACME)
		kitchen.Spec.BaseDomain = ""
		if registry := resolveRegistry(kitchen); registry != nil {
			t.Fatalf("want no registry without a host, got %+v", registry)
		}
		if reason, _ := registryUnavailableReason(kitchen); reason != "HostUnknown" {
			t.Errorf("reason = %q, want HostUnknown", reason)
		}
	})

	t.Run("takes an explicit host over the base domain", func(t *testing.T) {
		kitchen := registryKitchen(kitchenv1alpha1.TLSModeACME)
		kitchen.Spec.Registry.Host = "images.example.com"
		if got := resolveRegistry(kitchen).Host; got != "images.example.com" {
			t.Errorf("host = %q, want images.example.com", got)
		}
	})

	t.Run("is nothing when it is switched off", func(t *testing.T) {
		kitchen := registryKitchen(kitchenv1alpha1.TLSModeACME)
		kitchen.Spec.Registry.Enabled = false
		if registry := resolveRegistry(kitchen); registry != nil {
			t.Fatalf("want no registry when disabled, got %+v", registry)
		}
	})
}
