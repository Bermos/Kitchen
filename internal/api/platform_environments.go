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
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/controller"
)

// +kubebuilder:rbac:groups=kitchen.bermos.dev,resources=platformenvironments,verbs=get;list;watch;create;update;patch

type platformEnvironmentView struct {
	Name         string            `json:"name"`
	Owners       []string          `json:"owners,omitempty"`
	Requirements *requirementsView `json:"requirements,omitempty"`
	Serves       []string          `json:"serves"`
	DataClass    string            `json:"dataClass,omitempty"`
	Residency    string            `json:"residency,omitempty"`
	Criticality  string            `json:"criticality,omitempty"`
	RTO          string            `json:"rto,omitempty"`
	RPO          string            `json:"rpo,omitempty"`
	CreatedAt    metav1.Time       `json:"createdAt"`
}

func newPlatformEnvironmentView(env *kitchenv1alpha1.PlatformEnvironment) platformEnvironmentView {
	serves := []string{}
	for _, class := range env.ServesConsumers() {
		serves = append(serves, string(class))
	}
	return platformEnvironmentView{
		Name:         env.Name,
		Owners:       append([]string{}, env.Spec.Owners...),
		Requirements: newRequirementsView(env.Spec.Requirements),
		Serves:       serves,
		DataClass:    string(env.Spec.DataClass),
		Residency:    env.Spec.Residency,
		Criticality:  string(env.Spec.Criticality),
		RTO:          string(env.Spec.RTO),
		RPO:          string(env.Spec.RPO),
		CreatedAt:    env.CreationTimestamp,
	}
}

type createPlatformEnvironmentRequest struct {
	Name         string                                   `json:"name"`
	Owners       []string                                 `json:"owners,omitempty"`
	Requirements *kitchenv1alpha1.EnvironmentRequirements `json:"requirements,omitempty"`
	Serves       []string                                 `json:"serves,omitempty"`
	DataClass    string                                   `json:"dataClass,omitempty"`
	Residency    string                                   `json:"residency,omitempty"`
	Criticality  string                                   `json:"criticality,omitempty"`
	RTO          string                                   `json:"rto,omitempty"`
	RPO          string                                   `json:"rpo,omitempty"`
}

func normalizeServes(values []string) ([]kitchenv1alpha1.EnvironmentType, error) {
	known := kitchenv1alpha1.EnvironmentTypes()
	out := []kitchenv1alpha1.EnvironmentType{}
	for _, value := range values {
		class := kitchenv1alpha1.EnvironmentType(strings.TrimSpace(value))
		if !slices.Contains(known, class) {
			return nil, badRequestf("serves names %q, which is not a class of environment: the classes are %s",
				value, joinEnvironmentTypes(known))
		}
		if !slices.Contains(out, class) {
			out = append(out, class)
		}
	}
	ordered := []kitchenv1alpha1.EnvironmentType{}
	for _, class := range known {
		if slices.Contains(out, class) {
			ordered = append(ordered, class)
		}
	}
	return ordered, nil
}

type badRequestError struct{ message string }

func (e badRequestError) Error() string { return e.message }
func badRequestf(format string, args ...any) error {
	return badRequestError{message: fmt.Sprintf(format, args...)}
}

func (s *Server) listPlatformEnvironments(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	list := &kitchenv1alpha1.PlatformEnvironmentList{}
	if err := s.Client.List(ctx, list, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	items := make([]platformEnvironmentView, 0, len(list.Items))
	for i := range list.Items {
		items = append(items, newPlatformEnvironmentView(&list.Items[i]))
	}
	writeJSON(w, http.StatusOK, listBody[platformEnvironmentView]{Items: items})
}

func (s *Server) getPlatformEnvironment(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	p := &kitchenv1alpha1.PlatformEnvironment{}
	if err := s.get(ctx, req.PathValue("name"), p); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newPlatformEnvironmentView(p))
}

func (s *Server) createPlatformEnvironment(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	body := createPlatformEnvironmentRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" {
		badRequest(w, "name is required")
		return
	}
	existing := &kitchenv1alpha1.PlatformEnvironment{}
	if err := s.get(ctx, body.Name, existing); err == nil {
		writeJSON(w, http.StatusConflict, errorBody{Error: "platform environment already exists"})
		return
	} else if !apierrors.IsNotFound(err) {
		s.writeError(w, err)
		return
	}
	continuity, err := continuityFromRequest(&body.Criticality, &body.RTO, &body.RPO)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	class, err := dataClassFromRequest(body.DataClass)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.Requirements != nil &&
		(body.Requirements.BundleDigest == "" || !bundleDigestPattern.MatchString(strings.TrimSpace(body.Requirements.BundleDigest))) {
		badRequest(w, "bundleDigest %q is not a bundle digest: it has the form sha256:<64 hex characters>",
			body.Requirements.BundleDigest)
		return
	}
	consumers, err := normalizeServes(body.Serves)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	p := &kitchenv1alpha1.PlatformEnvironment{
		ObjectMeta: metav1.ObjectMeta{Name: body.Name, Namespace: s.Namespace},
		Spec: kitchenv1alpha1.PlatformEnvironmentSpec{
			Owners:       append([]string{}, body.Owners...),
			Requirements: body.Requirements,
			DataClass:    class,
			Residency:    strings.TrimSpace(body.Residency),
		},
	}
	if len(consumers) > 0 {
		p.Spec.Serves = &kitchenv1alpha1.EnvironmentServes{Consumers: consumers}
	}
	continuity.apply(&p.Spec.Criticality, &p.Spec.RTO, &p.Spec.RPO)
	if err := s.Client.Create(ctx, p); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, newPlatformEnvironmentView(p))
}

func (s *Server) patchPlatformEnvironment(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	p := &kitchenv1alpha1.PlatformEnvironment{}
	if err := s.get(ctx, req.PathValue("name"), p); err != nil {
		s.writeError(w, err)
		return
	}
	body := patchEnvironmentRequirementsRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	continuity, err := continuityFromRequest(body.Criticality, body.RTO, body.RPO)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if body.BundleDigest == nil && body.Parameters == nil && body.Owners == nil &&
		body.DataClass == nil && body.Residency == nil && body.Serves == nil && !continuity.touched() {
		badRequest(w, "nothing to change: send bundleDigest, parameters, owners, dataClass, residency, serves, criticality, rto or rpo")
		return
	}
	if body.Owners != nil {
		p.Spec.Owners = append([]string{}, *body.Owners...)
	}
	if body.DataClass != nil {
		class, err := dataClassFromRequest(*body.DataClass)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		p.Spec.DataClass = class
	}
	if body.Residency != nil {
		p.Spec.Residency = strings.TrimSpace(*body.Residency)
	}
	if body.Serves != nil {
		consumers, err := normalizeServes(*body.Serves)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		p.Spec.Serves = &kitchenv1alpha1.EnvironmentServes{Consumers: consumers}
	}
	if body.BundleDigest != nil {
		next := strings.TrimSpace(*body.BundleDigest)
		if next == "" {
			p.Spec.Requirements = nil
		} else {
			if !bundleDigestPattern.MatchString(next) {
				badRequest(w, "bundleDigest %q is not a bundle digest: it has the form sha256:<64 hex characters>", next)
				return
			}
			params := map[string]string(nil)
			if p.Spec.Requirements != nil {
				params = p.Spec.Requirements.Parameters
			}
			if body.Parameters != nil {
				params = body.Parameters
			}
			p.Spec.Requirements = &kitchenv1alpha1.EnvironmentRequirements{
				BundleDigest: next,
				Parameters:   params,
			}
		}
	} else if body.Parameters != nil {
		if p.Spec.Requirements == nil {
			badRequest(w, "environment %q declares no requirements, so there are no parameters to change: set bundleDigest first", p.Name)
			return
		}
		p.Spec.Requirements.Parameters = body.Parameters
	}
	continuity.apply(&p.Spec.Criticality, &p.Spec.RTO, &p.Spec.RPO)
	if err := s.Client.Update(ctx, p); err != nil {
		s.writeError(w, err)
		return
	}
	if err := s.syncPolicyEnvironmentBindings(ctx, p); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newPlatformEnvironmentView(p))
}

func (s *Server) syncPolicyEnvironmentBindings(ctx context.Context, p *kitchenv1alpha1.PlatformEnvironment) error {
	environments := &kitchenv1alpha1.EnvironmentList{}
	if err := s.Client.List(ctx, environments, client.InNamespace(s.Namespace)); err != nil {
		return err
	}
	for i := range environments.Items {
		env := &environments.Items[i]
		if env.Spec.PolicyEnvironmentRef == nil || env.Spec.PolicyEnvironmentRef.Name != p.Name {
			continue
		}
		patch := client.MergeFrom(env.DeepCopy())
		if !controller.ApplyPolicyEnvironment(env, p) {
			continue
		}
		if err := s.Client.Patch(ctx, env, patch); err != nil {
			return err
		}
	}
	return nil
}
