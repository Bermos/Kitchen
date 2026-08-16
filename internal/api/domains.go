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
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The custom-domain write surface. Creating a Domain does nothing to traffic
// by itself: the reconciler answers with the DNS record to create, and only a
// verified domain reaches the Gateway. The response therefore always carries
// the verification instructions, so the caller knows what to do next.

// createDomainRequest attaches a hostname to an environment. The name is
// derived from the hostname when absent; tls empty inherits the platform mode.
type createDomainRequest struct {
	Name        string `json:"name,omitempty"`
	Hostname    string `json:"hostname"`
	Environment string `json:"environment"`
	TLS         string `json:"tls,omitempty"`
}

// domainNameFromHostname turns "shop.example.com" into "shop-example-com":
// the object name a caller does not have to invent.
func domainNameFromHostname(hostname string) string {
	return strings.ReplaceAll(hostname, ".", "-")
}

// validateHostname checks what becomes a Gateway listener hostname and a
// route entry. A bare label is refused: a single-label "domain" is not a name
// anyone can point DNS at the platform with.
func validateHostname(hostname string) error {
	if hostname == "" {
		return fmt.Errorf("hostname is required, e.g. {\"hostname\": \"shop.example.com\"}")
	}
	if errs := validation.IsDNS1123Subdomain(hostname); len(errs) > 0 {
		return fmt.Errorf("hostname must be a DNS name — lowercase labels separated by dots (got %q)", hostname)
	}
	if !strings.Contains(hostname, ".") {
		return fmt.Errorf("hostname must be fully qualified, e.g. shop.example.com (got %q)", hostname)
	}
	return nil
}

func (s *Server) createDomain(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := createDomainRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(body.Hostname)), ".")
	body.Environment = strings.TrimSpace(body.Environment)

	if err := validateHostname(body.Hostname); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	tls := kitchenv1alpha1.TLSMode(strings.TrimSpace(body.TLS))
	switch tls {
	case "", kitchenv1alpha1.TLSModeACME, kitchenv1alpha1.TLSModeCloudflared, kitchenv1alpha1.TLSModeNone:
	default:
		badRequest(w, "tls must be acme, cloudflared or none — or absent to inherit the platform's mode (got %q)", body.TLS)
		return
	}
	if body.Name == "" {
		body.Name = domainNameFromHostname(body.Hostname)
	}
	if errs := validation.IsDNS1123Label(body.Name); len(errs) > 0 {
		badRequest(w, "name must work as a DNS label — lowercase letters, digits and '-', starting and ending alphanumeric (got %q)", body.Name)
		return
	}

	// A hostname under the base domain is already served: every
	// <slug>.<baseDomain> is generated routing and the wildcard certificate.
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err == nil {
		base := strings.ToLower(kitchen.Spec.BaseDomain)
		if base != "" && (body.Hostname == base || strings.HasSuffix(body.Hostname, "."+base)) {
			badRequest(w, "%s is under the platform's base domain %s: names there are generated and routed already, custom domains are for zones outside it",
				body.Hostname, base)
			return
		}
	}

	if body.Environment == "" {
		badRequest(w, "environment is required: the name of the environment this hostname routes to")
		return
	}
	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, body.Environment, env); err != nil {
		if apierrors.IsNotFound(err) {
			badRequest(w, "environment %q does not exist", body.Environment)
		} else {
			s.writeError(w, err)
		}
		return
	}

	// One hostname, one Domain: a second one would fight the first over the
	// Gateway listener and the route.
	existing := &kitchenv1alpha1.DomainList{}
	if err := s.Client.List(ctx, existing, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	for i := range existing.Items {
		if existing.Items[i].Spec.Hostname == body.Hostname {
			writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
				"hostname %s is already attached as domain %q", body.Hostname, existing.Items[i].Name)})
			return
		}
	}

	caller, _ := CallerFrom(ctx)
	domain := &kitchenv1alpha1.Domain{
		ObjectMeta: metav1.ObjectMeta{
			Name:        body.Name,
			Namespace:   s.Namespace,
			Annotations: map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.DomainSpec{
			Hostname:       body.Hostname,
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: env.Name},
			TLS:            tls,
		},
	}
	if err := s.Client.Create(ctx, domain); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("domain attached through the api",
		"domain", domain.Name, "hostname", body.Hostname, "environment", env.Name, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventDomainAttached,
		Project:     env.Spec.ProjectRef.Name,
		Environment: env.Name,
		Message:     fmt.Sprintf("domain %s attached to %s", body.Hostname, env.Name),
		Actor:       callerName(caller),
	})
	writeJSON(w, http.StatusCreated, newDomainView(domain))
}

func (s *Server) deleteDomain(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	domain := &kitchenv1alpha1.Domain{}
	if err := s.get(ctx, req.PathValue("name"), domain); err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.Client.Delete(ctx, domain); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("domain deleted through the api",
		"domain", domain.Name, "hostname", domain.Spec.Hostname, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventDomainRemoved,
		Environment: domain.Spec.EnvironmentRef.Name,
		Message:     fmt.Sprintf("domain %s removed", domain.Spec.Hostname),
		Actor:       callerName(caller),
	})
	// 202, not 200: the operator's finalizer still has the certificate and
	// its secret to remove when this response goes out.
	writeJSON(w, http.StatusAccepted, newDomainView(domain))
}
