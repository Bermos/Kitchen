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
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The resource claim write surface: asking a database-capable Connection to
// provision something for a project, asking the platform's own identity
// provider for an OAuth client, and taking either request away again. The
// binding credentials the reconciler writes stay in the cluster — the API
// hands out the claim's status, never the secret's contents.

// createClaimRequest asks for one provisioned resource. deletionPolicy
// defaults to Retain, mirroring the CRD: destroying data is opted into.
//
// The last three fields belong to type oidcClient, and the first two to type
// postgres. Sending a field of the other type's is refused rather than
// ignored: a request that says previewBranching and gets an OAuth client has
// been misunderstood, and the caller should hear about it here rather than
// wonder later why no branches appeared.
type createClaimRequest struct {
	Name             string `json:"name"`
	Project          string `json:"project"`
	Connection       string `json:"connection"`
	Type             string `json:"type"`
	PreviewBranching bool   `json:"previewBranching,omitempty"`
	DeletionPolicy   string `json:"deletionPolicy,omitempty"`

	// Postgres is what the database itself has to be: a major version, the
	// extensions the application will call for, and the volume behind it.
	// Only the shape is checked here — whether an extension can actually be
	// supplied is the provisioner's answer, and it lands on the claim as a
	// failure with the provider's own words, because the API cannot know
	// which images the connection was configured with.
	Postgres *kitchenv1alpha1.PostgresConfig `json:"postgres,omitempty"`

	// DataClass classifies the data the resource will hold: public,
	// internal, confidential or strictlyConfidential. It may not exceed the
	// project's own class — a classification narrows going down, never
	// widens — and classifying a claim in an unclassified project is
	// refused the same way: classify the project first. Absent means
	// unclassified, surfaced as such.
	DataClass string `json:"dataClass,omitempty"`

	// CallbackPaths are appended to every environment URL of the project to
	// build the client's redirect list; empty takes the platform's defaults.
	CallbackPaths []string `json:"callbackPaths,omitempty"`

	// RedirectURIs are registered verbatim as well — the addresses the
	// platform does not own, a developer's localhost above all.
	RedirectURIs []string `json:"redirectURIs,omitempty"`

	// Scopes the client may ask for; empty takes the platform's defaults.
	Scopes []string `json:"scopes,omitempty"`
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
	switch body.Type {
	case kitchenv1alpha1.ClaimTypePostgres, kitchenv1alpha1.ClaimTypeOIDCClient:
	default:
		badRequest(w, "type must be %s or %s (got %q)",
			kitchenv1alpha1.ClaimTypePostgres, kitchenv1alpha1.ClaimTypeOIDCClient, body.Type)
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
	policy := kitchenv1alpha1.ClaimDeletionPolicy(body.DeletionPolicy)
	switch policy {
	case "", kitchenv1alpha1.ClaimRetain, kitchenv1alpha1.ClaimDelete:
	default:
		badRequest(w, "deletionPolicy must be Retain or Delete (got %q)", body.DeletionPolicy)
		return
	}
	dataClass, err := dataClassFromRequest(body.DataClass)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	// Narrowable, never wideable: a claim's class must fit within its
	// project's. That includes a classified claim in an unclassified project
	// — there is no ceiling to fit under until somebody declares one.
	if dataClass.Exceeds(project.Spec.DataClass) {
		if !project.Spec.DataClass.Classified() {
			badRequest(w, "claim %q is classified %s but project %q is unclassified: classify the project first "+
				"(PATCH /projects/%s with dataClass), because a claim narrows its project's class and cannot "+
				"exceed it", body.Name, dataClass, project.Name, project.Name)
			return
		}
		badRequest(w, "claim %q may not be classified %s: project %q is classified %s, and a claim's class "+
			"narrows its project's, never exceeds it", body.Name, dataClass, project.Name, project.Spec.DataClass)
		return
	}

	config, ref, ok := s.claimShape(ctx, w, &body)
	if !ok {
		return
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
			ConnectionRef:  ref,
			Type:           body.Type,
			DeletionPolicy: policy,
			Config:         config,
			DataClass:      dataClass,
		},
	}
	reason := fmt.Sprintf("claim %s created: %s", claim.Name, body.Type)
	if body.Connection != "" {
		reason += " via " + body.Connection
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    claim,
		Kind:      audit.KindResourceClaim,
		Operation: clickhouse.AuditCreate,
		To:        body.Type,
		Project:   body.Project,
		Reason:    reason,
		Details: map[string]any{
			"type":           body.Type,
			"connection":     body.Connection,
			"deletionPolicy": string(policy),
			"dataClass":      string(dataClass),
		},
	}) {
		return
	}
	if err := s.Client.Create(ctx, claim); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("claim created through the api",
		"claim", claim.Name, "project", body.Project, "type", body.Type,
		"connection", body.Connection, "caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:    clickhouse.EventClaimCreated,
		Project: body.Project,
		Claim:   claim.Name,
		Message: reason,
		Actor:   callerName(caller),
	})
	writeJSON(w, http.StatusCreated, newClaimView(claim))
}

// claimShape validates the half of the request that belongs to the claim's
// type, and answers with the two things that differ between types: the
// Connection the claim provisions through, and the config the reconciler
// reads. ok=false means a refusal has already been written.
//
// The two types are refused each other's fields rather than quietly ignoring
// them, and the CRD refuses the same shapes at admission — this is the layer
// that can say why in a sentence.
func (s *Server) claimShape(
	ctx context.Context,
	w http.ResponseWriter,
	body *createClaimRequest,
) (*runtime.RawExtension, *kitchenv1alpha1.LocalObjectReference, bool) {
	if body.Type == kitchenv1alpha1.ClaimTypePostgres {
		if len(body.CallbackPaths) > 0 || len(body.RedirectURIs) > 0 || len(body.Scopes) > 0 {
			badRequest(w, "callbackPaths, redirectURIs and scopes belong to a claim of type %s: "+
				"a %s claim provisions a database, which has no redirect list",
				kitchenv1alpha1.ClaimTypeOIDCClient, kitchenv1alpha1.ClaimTypePostgres)
			return nil, nil, false
		}
		if !s.requireConnection(ctx, w, "connection", body.Connection, kitchenv1alpha1.CapabilityDatabase) {
			return nil, nil, false
		}
		postgres, ok := validPostgresConfig(w, body.Postgres)
		if !ok {
			return nil, nil, false
		}
		var config *runtime.RawExtension
		if body.PreviewBranching || postgres != nil {
			raw, err := json.Marshal(claimConfigBody{
				PreviewBranching: body.PreviewBranching,
				Postgres:         postgres,
			})
			if err != nil {
				s.writeError(w, err)
				return nil, nil, false
			}
			config = &runtime.RawExtension{Raw: raw}
		}
		return config, &kitchenv1alpha1.LocalObjectReference{Name: body.Connection}, true
	}

	return s.oidcClaimShape(w, body)
}

// oidcClaimShape is the other half of claimShape: type oidcClient, which has
// no Connection to name because the client is registered at the identity
// provider the platform is already configured with, by the operator's own
// credential.
func (s *Server) oidcClaimShape(
	w http.ResponseWriter,
	body *createClaimRequest,
) (*runtime.RawExtension, *kitchenv1alpha1.LocalObjectReference, bool) {
	if body.Connection != "" {
		badRequest(w, "an %s claim takes no connection: the client is registered at the platform's own "+
			"identity provider, and there is no Connection in front of it",
			kitchenv1alpha1.ClaimTypeOIDCClient)
		return nil, nil, false
	}
	if body.PreviewBranching {
		badRequest(w, "previewBranching belongs to a claim of type %s: an OAuth client is not branched per "+
			"preview, its redirect list grows one entry per preview instead",
			kitchenv1alpha1.ClaimTypePostgres)
		return nil, nil, false
	}
	if body.Postgres != nil {
		badRequest(w, "postgres belongs to a claim of type %s: an OAuth client has no version, no extensions "+
			"and no volume", kitchenv1alpha1.ClaimTypePostgres)
		return nil, nil, false
	}
	if body.DeletionPolicy != "" {
		badRequest(w, "an %s claim takes no deletionPolicy: the policy decides what happens to provisioned "+
			"data, and an OAuth client holds none — it is always deregistered with the claim",
			kitchenv1alpha1.ClaimTypeOIDCClient)
		return nil, nil, false
	}

	cfg := kitchenv1alpha1.OIDCClientConfig{}
	for _, path := range body.CallbackPaths {
		path = strings.TrimSpace(path)
		if !strings.HasPrefix(path, "/") {
			badRequest(w, "callbackPaths are paths, not URLs, and start with '/' (got %q): they are appended "+
				"to every URL the project's environments are reachable at, which is what keeps previews "+
				"working without anyone writing their URLs down", path)
			return nil, nil, false
		}
		cfg.CallbackPaths = append(cfg.CallbackPaths, path)
	}
	for _, uri := range body.RedirectURIs {
		uri = strings.TrimSpace(uri)
		parsed, err := url.Parse(uri)
		if err != nil || !parsed.IsAbs() || parsed.Host == "" ||
			(parsed.Scheme != "http" && parsed.Scheme != "https") {
			badRequest(w, "redirectURIs are absolute http(s) URLs (got %q): they are registered verbatim, "+
				"for the addresses the platform does not own", uri)
			return nil, nil, false
		}
		cfg.RedirectURIs = append(cfg.RedirectURIs, uri)
	}
	for _, scope := range body.Scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" || strings.ContainsAny(scope, " \t") {
			badRequest(w, "scopes are single words (got %q)", scope)
			return nil, nil, false
		}
		cfg.Scopes = append(cfg.Scopes, scope)
	}
	if len(cfg.Scopes) > 0 && !slices.Contains(cfg.Scopes, "openid") {
		badRequest(w, "scopes must include openid: without it the issuer answers with an OAuth token and no "+
			"identity, which is not what a sign-in needs")
		return nil, nil, false
	}
	if len(cfg.CallbackPaths) == 0 && len(cfg.RedirectURIs) == 0 && len(cfg.Scopes) == 0 {
		return nil, nil, true
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		s.writeError(w, err)
		return nil, nil, false
	}
	return &runtime.RawExtension{Raw: raw}, nil, true
}

// claimConfigBody is spec.config as this API writes it. It is the platform's
// own slice of that object — the plugin's half is what the provisioner reads —
// and it is spelled here rather than reused from the CRD package because the
// CRD's copy is unexported on purpose: what is written into a RawExtension is
// the API's contract with its callers, and it should have to change on
// purpose.
type claimConfigBody struct {
	PreviewBranching bool                            `json:"previewBranching,omitempty"`
	Postgres         *kitchenv1alpha1.PostgresConfig `json:"postgres,omitempty"`
}

// validPostgresConfig checks the shape of what a postgres claim asks of its
// database, and normalizes it: an empty block is nothing rather than an empty
// object on the spec.
//
// Only shape. Whether the version exists and whether an extension can be
// supplied is the provisioner's to answer against the images its Connection
// was configured with, and the answer lands on the claim's status as a
// failure naming what could not be supplied. The division matters: this layer
// refuses what is not a version, that layer refuses what is not available.
func validPostgresConfig(
	w http.ResponseWriter,
	cfg *kitchenv1alpha1.PostgresConfig,
) (*kitchenv1alpha1.PostgresConfig, bool) {
	if cfg == nil {
		return nil, true
	}
	out := kitchenv1alpha1.PostgresConfig{
		Version: strings.TrimSpace(cfg.Version),
		Storage: kitchenv1alpha1.PostgresStorage{
			Size:         strings.TrimSpace(cfg.Storage.Size),
			StorageClass: strings.TrimSpace(cfg.Storage.StorageClass),
		},
	}
	if out.Version != "" {
		major, err := strconv.Atoi(out.Version)
		if err != nil || major < 9 || major > 99 {
			badRequest(w, "postgres.version is a major version and nothing else — \"17\", not %q. Which majors "+
				"this connection can actually serve is the connection's answer, and a version it cannot serve "+
				"fails the claim with the list", cfg.Version)
			return nil, false
		}
	}
	for _, extension := range cfg.Extensions {
		extension = strings.TrimSpace(extension)
		if extension == "" {
			continue
		}
		if !extensionNamePattern.MatchString(extension) {
			badRequest(w, "postgres.extensions are the identifiers CREATE EXTENSION takes — letters, digits "+
				"and underscores (got %q)", extension)
			return nil, false
		}
		out.Extensions = append(out.Extensions, extension)
	}
	if out.Storage.Size != "" {
		quantity, err := resource.ParseQuantity(out.Storage.Size)
		if err != nil {
			badRequest(w, "postgres.storage.size is a Kubernetes quantity — \"10Gi\" (got %q): %s",
				cfg.Storage.Size, err.Error())
			return nil, false
		}
		if quantity.Sign() <= 0 {
			badRequest(w, "postgres.storage.size must be more than nothing (got %q)", cfg.Storage.Size)
			return nil, false
		}
	}
	if out.Storage.StorageClass != "" {
		if errs := validation.IsDNS1123Subdomain(out.Storage.StorageClass); len(errs) > 0 {
			badRequest(w, "postgres.storage.storageClass must be a StorageClass name (got %q): %s",
				cfg.Storage.StorageClass, strings.Join(errs, "; "))
			return nil, false
		}
	}
	if out.Version == "" && len(out.Extensions) == 0 && out.Storage.Size == "" && out.Storage.StorageClass == "" {
		return nil, true
	}
	return &out, true
}

// extensionNamePattern is what may be written into the bootstrap SQL the
// provisioner builds. It is checked here as well as there because a refusal
// at the door explains itself better than one three layers in — and because
// nothing should be able to reach a CREATE EXTENSION statement unchecked.
var extensionNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

func (s *Server) deleteClaim(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(ctx, req.PathValue("name"), claim); err != nil {
		s.writeError(w, err)
		return
	}
	caller, _ := CallerFrom(ctx)
	outcome := "the database is kept at the provider"
	switch {
	case claim.Spec.Type == kitchenv1alpha1.ClaimTypeOIDCClient:
		outcome = "the OAuth client is deregistered"
	case claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete:
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
