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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The resource claim write surface: asking a database-capable Connection to
// provision something for a project, and taking that request away again. The
// binding credentials the reconciler writes stay in the cluster — the API
// hands out the claim's status, never the secret's contents.

// createClaimRequest asks for one provisioned resource. deletionPolicy
// defaults to Retain, mirroring the CRD: destroying data is opted into.
type createClaimRequest struct {
	Name             string `json:"name"`
	Project          string `json:"project"`
	Connection       string `json:"connection"`
	Type             string `json:"type"`
	PreviewBranching bool   `json:"previewBranching,omitempty"`
	DeletionPolicy   string `json:"deletionPolicy,omitempty"`
}

func (s *Server) createClaim(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := createClaimRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.Project = strings.TrimSpace(body.Project)
	body.Connection = strings.TrimSpace(body.Connection)
	body.Type = strings.TrimSpace(body.Type)
	body.DeletionPolicy = strings.TrimSpace(body.DeletionPolicy)

	if body.Name == "" {
		badRequest(w, "name is required")
		return
	}
	if errs := validation.IsDNS1123Label(body.Name); len(errs) > 0 {
		badRequest(w, "name must work as a DNS label — lowercase letters, digits and '-', starting and ending alphanumeric (got %q)", body.Name)
		return
	}
	if body.Type != "postgres" {
		badRequest(w, "type must be postgres (got %q) — it is the one resource type the platform provisions today", body.Type)
		return
	}
	if body.Project == "" {
		badRequest(w, "project is required: the name of the Project the resource is provisioned for")
		return
	}
	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, body.Project, project); err != nil {
		if apierrors.IsNotFound(err) {
			badRequest(w, "project %q does not exist: create the project first", body.Project)
		} else {
			s.writeError(w, err)
		}
		return
	}
	if !s.requireConnection(ctx, w, "connection", body.Connection, kitchenv1alpha1.CapabilityDatabase) {
		return
	}
	policy := kitchenv1alpha1.ClaimDeletionPolicy(body.DeletionPolicy)
	switch policy {
	case "", kitchenv1alpha1.ClaimRetain, kitchenv1alpha1.ClaimDelete:
	default:
		badRequest(w, "deletionPolicy must be Retain or Delete (got %q)", body.DeletionPolicy)
		return
	}

	var config *runtime.RawExtension
	if body.PreviewBranching {
		raw, err := json.Marshal(map[string]any{"previewBranching": true})
		if err != nil {
			s.writeError(w, err)
			return
		}
		config = &runtime.RawExtension{Raw: raw}
	}

	caller, _ := CallerFrom(ctx)
	claim := &kitchenv1alpha1.ResourceClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        body.Name,
			Namespace:   s.Namespace,
			Annotations: map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.ResourceClaimSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: body.Project},
			ConnectionRef:  kitchenv1alpha1.LocalObjectReference{Name: body.Connection},
			Type:           body.Type,
			DeletionPolicy: policy,
			Config:         config,
		},
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    claim,
		Kind:      audit.KindResourceClaim,
		Operation: clickhouse.AuditCreate,
		To:        body.Type,
		Project:   body.Project,
		Reason:    fmt.Sprintf("claim %s created: %s via %s", claim.Name, body.Type, body.Connection),
		Details: map[string]any{
			"type":           body.Type,
			"connection":     body.Connection,
			"deletionPolicy": string(policy),
		},
	}) {
		return
	}
	if err := s.Client.Create(ctx, claim); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("claim created through the api",
		"claim", claim.Name, "project", body.Project, "connection", body.Connection, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventClaimCreated,
		Project: body.Project,
		Claim:   claim.Name,
		Message: fmt.Sprintf("claim %s created: %s via %s", claim.Name, body.Type, body.Connection),
		Actor:   callerName(caller),
	})
	writeJSON(w, http.StatusCreated, newClaimView(claim))
}

func (s *Server) deleteClaim(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(ctx, req.PathValue("name"), claim); err != nil {
		s.writeError(w, err)
		return
	}
	caller, _ := CallerFrom(ctx)
	outcome := "the database is kept at the provider"
	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete {
		outcome = "the database is deprovisioned"
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    claim,
		Kind:      audit.KindResourceClaim,
		Operation: clickhouse.AuditDelete,
		From:      string(claim.Status.Phase),
		Project:   claim.Spec.ProjectRef.Name,
		Reason:    fmt.Sprintf("claim %s deleted: %s", claim.Name, outcome),
		Details: map[string]any{
			"type":           claim.Spec.Type,
			"deletionPolicy": string(claim.Spec.DeletionPolicy),
		},
	}) {
		return
	}
	if err := s.Client.Delete(ctx, claim); err != nil {
		s.writeError(w, err)
		return
	}
	s.log().Info("claim deleted through the api",
		"claim", claim.Name, "project", claim.Spec.ProjectRef.Name,
		"deletionPolicy", claim.Spec.DeletionPolicy, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventClaimDeleted,
		Project: claim.Spec.ProjectRef.Name,
		Claim:   claim.Name,
		Message: fmt.Sprintf("claim %s deleted: %s", claim.Name, outcome),
		Actor:   callerName(caller),
	})
	// 202, not 200: the operator's finalizer still has branches, binding
	// secrets and — under deletionPolicy Delete — the instance itself to
	// remove when this response goes out.
	writeJSON(w, http.StatusAccepted, newClaimView(claim))
}
