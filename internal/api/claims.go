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
	"regexp"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/provider/contract"
	"github.com/Bermos/Kitchen/internal/provider/declarations"
)

// The resource claim write surface: asking a database-capable Connection to
// provision something for a project, asking the platform's own identity
// provider for an OAuth client, asking an Inngest account for the keys a
// worker connects with, and taking any of those requests away again. The
// binding credentials the reconciler writes stay in the cluster — the API
// hands out the claim's status, never the secret's contents.

// createClaimRequest asks for one provisioned resource. deletionPolicy
// defaults to Retain, mirroring the CRD: destroying data is opted into.
//
// The fields after dataClass each belong to one type — postgres to
// postgres, objectStore to objectStore, the last three to oidcClient — and
// each type's shaper (claims_postgres.go, claims_objectstore.go,
// claims_oidc.go) says which. Sending a field of another type's is refused
// rather than ignored.
// postgres, the three lists to oidcClient, inngest to inngest — and each
// type's shaper (claims_postgres.go, claims_oidc.go, claims_inngest.go)
// says which. Sending a field of another type's is refused rather than
// ignored.
type createClaimRequest struct {
	Name       string `json:"name"`
	Project    string `json:"project"`
	Connection string `json:"connection"`
	Type       string `json:"type"`

	// PreviewMode is what the project's preview environments bind to: the
	// mode the connection's provider declares (GET /claim-types says which),
	// "shared" for production's own resource — which has to be asked for by
	// name, because a preview reading production data is never a default —
	// or "none". Empty takes the provider's declaration.
	PreviewMode string `json:"previewMode,omitempty"`

	DeletionPolicy string `json:"deletionPolicy,omitempty"`

	// Postgres is what the database itself has to be: a major version, the
	// extensions the application will call for, and the volume behind it.
	// Only the shape is checked here — whether an extension can actually be
	// supplied is the provisioner's answer, and it lands on the claim as a
	// failure with the provider's own words, because the API cannot know
	// which images the connection was configured with.
	Postgres *kitchenv1alpha1.PostgresConfig `json:"postgres,omitempty"`

	// ObjectStore is what the bucket has to be: versioned, publicly
	// readable, or held to a size. Shape alone is checked here; whether the
	// store can honour each is the provisioner's answer, landing on the
	// claim as a refusal naming what it could not supply.
	ObjectStore *kitchenv1alpha1.ObjectStoreConfig `json:"objectStore,omitempty"`
	// Volume is what a volume claim asks for: the process that mounts it,
	// the size, the StorageClass and the mount path. The process is checked
	// against the project here; the class and what it supports are the
	// cluster's answer, and land on the claim.
	Volume *kitchenv1alpha1.VolumeConfig `json:"volume,omitempty"`

	// Redis is what the instance has to be: a cache or a queue, how much
	// memory it may use, and which Valkey. `usage` is the one that matters —
	// see RedisConfig for why getting it backwards loses work silently.
	Redis *kitchenv1alpha1.RedisConfig `json:"redis,omitempty"`

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

	// Inngest is what an inngest claim binds: the app ID the worker
	// connects as (empty takes the claim's name), the Inngest environment
	// production reads (empty means production), and the mode — connect,
	// the only one provisioned. Only the shape is checked here; whether the
	// environment has an event key to read is the provider's answer, and it
	// lands on the claim as a failure saying where to create one.
	Inngest *kitchenv1alpha1.InngestConfig `json:"inngest,omitempty"`
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
	claimType, shaper, ok := claimShaperFor(body.Type)
	if !ok {
		badRequest(w, "type must be one of %s (got %q)", strings.Join(kitchenv1alpha1.ClaimTypeNames(), ", "), body.Type)
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
	if policy != "" && !claimType.HoldsData {
		badRequest(w, "%s claim takes no deletionPolicy: the policy decides what happens to provisioned "+
			"data, and %s holds none — it is always removed with the claim",
			withArticle(claimType.Name), withArticle(claimType.Resource))
		return
	}
	if policy == kitchenv1alpha1.ClaimDelete &&
		!s.mayDestroyData(ctx, w, project, project.Name, "asking for a claim that destroys its "+claimType.Resource) {
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

	config, ref, ok := s.claimShape(ctx, w, claimType, shaper, project, &body)
	if !ok {
		return
	}
	config, ok = s.withPreviewMode(ctx, w, claimType, ref, &body, config)
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

// claimShaper is the API's half of one claim type: the fields of a create
// request that belong to the type, how they become the config the reconciler
// reads, and the type's own slice of the claim's view. The reconciler's half
// is the contract in internal/controller; the two are registered against the
// same table, kitchenv1alpha1.ClaimTypes, and the tests on each refuse a row
// without a registration.
type claimShaper interface {
	// fields is every type-specific field of createClaimRequest this type
	// reads. A request that sets a field of another type's is refused
	// rather than quietly ignored: a request that says previewBranching and
	// gets an OAuth client has been misunderstood, and the caller should
	// hear about it here rather than wonder later why no branches appeared.
	fields() []claimField

	// config validates the type's fields and answers spec.config as the
	// reconciler will read it, nil when nothing was asked. project is the
	// one the claim is for, for a type whose request names something of
	// the project's. ok=false means a refusal has already been written.
	config(
		w http.ResponseWriter,
		body *createClaimRequest,
		project *kitchenv1alpha1.Project,
	) (config *runtime.RawExtension, ok bool)

	// view fills the type's own fields of the claim's view.
	view(claim *kitchenv1alpha1.ResourceClaim, view *claimView)

	// deletionOutcome says, in a sentence, what deleting the claim does to
	// what it provisioned.
	deletionOutcome(claim *kitchenv1alpha1.ResourceClaim) string
}

// claimField is one type-specific field of createClaimRequest: how to tell
// it was sent, and what a claim of another type lacks that makes it
// meaningless there.
type claimField struct {
	name string
	set  func(body *createClaimRequest) bool
	// lacks completes "a <resource> has ..." in the refusal — "no redirect
	// list", "no version, no extensions and no volume".
	lacks string
}

// claimShapers is the registry: one shaper per row of
// kitchenv1alpha1.ClaimTypes.
var claimShapers = map[string]claimShaper{
	kitchenv1alpha1.ClaimTypePostgres:    postgresClaimShaper{},
	kitchenv1alpha1.ClaimTypeOIDCClient:  oidcClaimShaper{},
	kitchenv1alpha1.ClaimTypeObjectStore: objectStoreClaimShaper{},
	kitchenv1alpha1.ClaimTypeVolume:      volumeClaimShaper{},
	kitchenv1alpha1.ClaimTypeInngest:     inngestClaimShaper{},
	kitchenv1alpha1.ClaimTypeRedis:       redisClaimShaper{},
}

// claimShaperFor resolves a request's type to the table's row and the API's
// shaper for it; ok is false for a type either does not know.
func claimShaperFor(typeName string) (kitchenv1alpha1.ClaimType, claimShaper, bool) {
	claimType, ok := kitchenv1alpha1.LookupClaimType(typeName)
	if !ok {
		return kitchenv1alpha1.ClaimType{}, nil, false
	}
	shaper, ok := claimShapers[claimType.Name]
	if !ok {
		return claimType, nil, false
	}
	return claimType, shaper, true
}

// claimShape validates the half of the request that belongs to the claim's
// type, and answers with the two things that differ between types: the
// Connection the claim provisions through, and the config the reconciler
// reads. ok=false means a refusal has already been written.
//
// A type is refused the fields of every other type rather than quietly
// ignoring them, and the CRD refuses the same shapes at admission — this is
// the layer that can say why in a sentence.
func (s *Server) claimShape(
	ctx context.Context,
	w http.ResponseWriter,
	claimType kitchenv1alpha1.ClaimType,
	shaper claimShaper,
	project *kitchenv1alpha1.Project,
	body *createClaimRequest,
) (*runtime.RawExtension, *kitchenv1alpha1.LocalObjectReference, bool) {
	mine := map[string]bool{}
	for _, field := range shaper.fields() {
		mine[field.name] = true
	}
	for _, other := range kitchenv1alpha1.ClaimTypes {
		if other.Name == claimType.Name {
			continue
		}
		for _, field := range claimShapers[other.Name].fields() {
			if mine[field.name] || !field.set(body) {
				continue
			}
			badRequest(w, "%s belongs to a claim of type %s: %s claim provisions %s, which has %s",
				field.name, other.Name, withArticle(claimType.Name), withArticle(claimType.Resource), field.lacks)
			return nil, nil, false
		}
	}

	var ref *kitchenv1alpha1.LocalObjectReference
	if claimType.TakesConnection() {
		if !s.requireConnection(ctx, w, "connection", body.Connection, claimType.Capability) {
			return nil, nil, false
		}
		ref = &kitchenv1alpha1.LocalObjectReference{Name: body.Connection}
	} else if body.Connection != "" {
		badRequest(w, "%s claim takes no connection: the platform provisions %s itself, and there is no "+
			"Connection in front of it", withArticle(claimType.Name), withArticle(claimType.Resource))
		return nil, nil, false
	}

	config, ok := shaper.config(w, body, project)
	if !ok {
		return nil, nil, false
	}
	return config, ref, true
}

// withPreviewMode validates the claim's choice of what its previews bind to
// against what the connection's provider declares, and writes it into
// spec.config beside the type's own block.
//
// The choice is checked here, at the door, so that a claim asking a
// provider for a mode it cannot give is refused with the provider's own
// declaration rather than created and left binding nothing in previews.
// The reconciler makes the same decision again from the claim's status —
// this layer is the one that can say it in a sentence before anything
// exists.
func (s *Server) withPreviewMode(
	ctx context.Context,
	w http.ResponseWriter,
	claimType kitchenv1alpha1.ClaimType,
	ref *kitchenv1alpha1.LocalObjectReference,
	body *createClaimRequest,
	config *runtime.RawExtension,
) (*runtime.RawExtension, bool) {
	choice := contract.PreviewMode(strings.TrimSpace(body.PreviewMode))
	if choice == "" {
		return config, true
	}
	if !choice.Known() {
		badRequest(w, "previewMode must be one of %s (got %q): what a preview environment binds to",
			joinModes(contract.PreviewModes), body.PreviewMode)
		return nil, false
	}
	// The provider is the connection's; for a type the platform provisions
	// itself it is the one provider the declarations list for the type.
	provider := ""
	if ref != nil {
		conn := &kitchenv1alpha1.Connection{}
		if err := s.get(ctx, ref.Name, conn); err != nil {
			s.writeError(w, err)
			return nil, false
		}
		provider = conn.Spec.Provider
	} else {
		for _, d := range declarations.All() {
			if d.Type == claimType.Name {
				provider = d.Provider
				break
			}
		}
	}
	declaration, declared := declarations.Lookup(claimType.Name, provider)
	if choice == contract.PreviewShared && declared && declaration.ForcesRecreate {
		// A resource that attaches to one pod at a time cannot be shared
		// between production and a preview without taking it from
		// production: the reconciler would give previews nothing, and this
		// is the layer that can say so before the claim exists.
		badRequest(w, "previewMode \"shared\" is refused for a %s claim: %s declares that its %s attaches to "+
			"one pod at a time, so a preview mounting production's would take it from production. Ask for "+
			"%s — %s — or for none", claimType.Name, provider, claimType.Resource, declaration.Preview,
			declaration.PreviewNote)
		return nil, false
	}
	if choice.Isolated() && (!declared || declaration.Preview != choice) {
		gives := "nothing"
		if declared {
			gives = fmt.Sprintf("%s — %s", declaration.Preview, declaration.PreviewNote)
		}
		badRequest(w, "previewMode %q is not something %s gives a preview: it gives %s. Ask for that, for "+
			"shared (production's own %s — previews read and write it), or for none",
			choice, provider, gives, claimType.Resource)
		return nil, false
	}

	// spec.config is one object: the type's block, plus the platform's own
	// previewMode beside it.
	merged := map[string]any{}
	if config != nil {
		if err := json.Unmarshal(config.Raw, &merged); err != nil {
			s.writeError(w, err)
			return nil, false
		}
	}
	merged["previewMode"] = string(choice)
	raw, err := json.Marshal(merged)
	if err != nil {
		s.writeError(w, err)
		return nil, false
	}
	return &runtime.RawExtension{Raw: raw}, true
}

func joinModes(modes []contract.PreviewMode) string {
	names := make([]string, 0, len(modes))
	for _, mode := range modes {
		names = append(names, string(mode))
	}
	return strings.Join(names, ", ")
}

// withArticle is the noun with its indefinite article — "a database", "an
// OAuth client", "an oidcClient" — for the sentences the refusals are made
// of. Good enough for the nouns the table holds; it is not a linguist.
func withArticle(noun string) string {
	if noun == "" {
		return noun
	}
	switch noun[0] {
	case 'a', 'e', 'i', 'o', 'u', 'A', 'E', 'I', 'O', 'U':
		return "an " + noun
	default:
		return "a " + noun
	}
}

// extensionNamePattern is what may be written into the bootstrap SQL the
// provisioner builds. It is checked here as well as there because a refusal
// at the door explains itself better than one three layers in — and because
// nothing should be able to reach a CREATE EXTENSION statement unchecked.
var extensionNamePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]*$`)

// mayDestroyData is the one escalation on the claims surface: `deletionPolicy:
// Delete` is the admin's, everything else about a claim is the developer's.
//
// It lives in the handlers rather than in the route table because the route
// table's unit is a whole route, and this condition is a field of the request
// body on the way in and a field of the stored claim on the way out. So the
// row in internal/api/policy.go is the **floor** — a developer claims
// resources and takes them away — and this is the ceiling on the one case that
// destroys data: the CNPG Cluster and its PVCs, every object version in the
// bucket, the Valkey StatefulSet and its volume. The compliance regime expects
// destroying data to be segregated from the day job, and admin is already the
// role that may delete the project all of it belongs to.
//
// The refusal names the field and the role it wants, which is the rule every
// refusal on this API follows. project may be nil, for a claim whose project
// is no longer there: an operator holds admin on it anyway, and nobody else
// holds anything.
func (s *Server) mayDestroyData(
	ctx context.Context,
	w http.ResponseWriter,
	project *kitchenv1alpha1.Project,
	projectName string,
	doing string,
) bool {
	role := s.roleOn(ctx, project)
	if role.AtLeast(access.ProjectAdmin) {
		return true
	}
	held := role.String()
	if held == "" {
		held = "no role"
	}
	forbidden(w, fmt.Sprintf("you have %s on %s; %s needs admin: deletionPolicy Delete destroys the "+
		"provisioned resource and the data on it, and there is no undo", held, projectName, doing))
	return false
}

func (s *Server) deleteClaim(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	claim := &kitchenv1alpha1.ResourceClaim{}
	if err := s.get(ctx, req.PathValue("name"), claim); err != nil {
		s.writeError(w, err)
		return
	}
	caller, _ := CallerFrom(ctx)
	outcome := "the provisioned resource is left where it is"
	resource := "resource"
	if claimType, shaper, ok := claimShaperFor(claim.Spec.Type); ok {
		outcome = shaper.deletionOutcome(claim)
		resource = claimType.Resource
	}
	// The floor is developer, in the table; taking a claim away is the day
	// job. Taking away a claim whose policy is Delete is not — it destroys
	// the resource and the data on it — so this one is the admin's, and the
	// claim's own spec is what says which of the two this is.
	if claim.Spec.DeletionPolicy == kitchenv1alpha1.ClaimDelete {
		project := &kitchenv1alpha1.Project{}
		if err := s.get(ctx, claim.Spec.ProjectRef.Name, project); err != nil {
			if !apierrors.IsNotFound(err) {
				s.writeError(w, err)
				return
			}
			// A dangling project reference. Nobody holds a role on a project
			// that is not there, and only an operator — admin on every
			// project, present, future and missing — gets past the check
			// below to clean the claim up.
			project = nil
		}
		if !s.mayDestroyData(ctx, w, project, claim.Spec.ProjectRef.Name,
			"deleting a claim that destroys its "+resource) {
			return
		}
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
