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
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/platformhost"
)

// Declaring an environment before anything has deployed into it (#491).
//
// Until this route existed an Environment was created by exactly one thing —
// the first build for its target, in the build controller's ensureEnvironment
// — so the bar its owners set could only ever be raised *after* a release had
// already landed there. The one moment a requirement matters most was the one
// moment it could not be expressed.
//
// So an environment can now be declared ahead of its first deployment, and
// nothing about the lazy path changes: ensureEnvironment still creates the
// environments nobody declared, and a declared one is simply already there
// when the first build arrives, with its bar already set. What that build then
// does is the promotion path exactly as it stands — an environment declaring
// requirements gets a Promotion rather than a flip, which is what
// promoteOrFlip has always done for an environment that has one.
//
// **The body is two halves, and they are guarded differently.** The name and
// the type say which environment this is, and belong to whoever deploys the
// project — the same developer role that deletes one. The owners, the bar,
// the classification and the continuity tolerances are the environment
// owners' declaration, guarded here the way PATCH .../requirements guards
// them: its owners or a platform operator, and an environment that does not
// exist yet has no owners, so at creation that is operators alone. A
// developer who could name themselves an owner on the way in would be
// granting themselves the say over what the environment demands, which is the
// whole of what spec.owners exists to keep apart.

// createEnvironmentRequest declares an environment of a project. Only `name`
// is required; everything else is either derived (the type) or a declaration
// the caller has to be an operator to make.
type createEnvironmentRequest struct {
	// Name is the environment's own name, and it becomes a hostname: a DNS
	// label, unique across the platform because environments live in one
	// namespace.
	Name string `json:"name"`
	// Type is `production` or `stage`, and it is checked rather than taken.
	// Which environment is which is derived from the project's promotion
	// pipeline (EnvironmentTypeFor) and re-derived by the environment
	// reconciler on every pass, so a type sent here that disagrees with the
	// pipeline would be overwritten within seconds — it is refused instead,
	// saying what the pipeline makes of the name. Absent means "derive it",
	// which is what the dashboard and the CLI send.
	Type string `json:"type,omitempty"`

	// The owners' declaration, from here down. Each is exactly the field the
	// requirements endpoint writes, and setting any of them at creation asks
	// the same of the caller.
	Owners       []string                                 `json:"owners,omitempty"`
	Requirements *kitchenv1alpha1.EnvironmentRequirements `json:"requirements,omitempty"`
	DataClass    *string                                  `json:"dataClass,omitempty"`
	Residency    *string                                  `json:"residency,omitempty"`
	// Serves is who this environment will answer: the classes of another
	// project's environments that may bind to an offering served from here
	// (#494). Absent serves nobody, which is the state an environment
	// declared without it is in and the state one nobody declared is in.
	Serves      *[]string `json:"serves,omitempty"`
	Criticality *string   `json:"criticality,omitempty"`
	RTO         *string   `json:"rto,omitempty"`
	RPO         *string   `json:"rpo,omitempty"`
}

// declares reports whether the body carries any of the owners' half. It is
// what decides whether the operator gate applies at all: a developer
// declaring an environment and nothing about it is the ordinary case.
func (r createEnvironmentRequest) declares() bool {
	return len(r.Owners) > 0 || r.Requirements != nil || r.DataClass != nil ||
		r.Residency != nil || r.Serves != nil || r.Criticality != nil || r.RTO != nil || r.RPO != nil
}

// declarationRefusal is the 403 for a caller who may declare an environment
// and may not declare what it demands. It says both halves of the way
// forward, because "ask an operator" on its own is a dead end.
const declarationRefusal = "an environment that does not exist yet names no owners, so only a platform " +
	"operator may set its owners, requirements, dataClass, residency, serves, criticality, rto or rpo at " +
	"creation. Create it with a name and a type, and have an operator declare the rest through " +
	"PATCH /api/v1/environments/{name}/requirements — which is also where an owner they name may " +
	"change it afterwards"

// createProjectEnvironment declares an environment of a project before
// anything deploys into it.
func (s *Server) createProjectEnvironment(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := createEnvironmentRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Name = strings.TrimSpace(body.Name)

	if body.declares() && !platformRoleFrom(ctx).AtLeast(access.PlatformOperator) {
		forbidden(w, declarationRefusal)
		return
	}

	envType, problem := s.environmentNameAndType(ctx, project, body)
	if problem != "" {
		badRequest(w, "%s", problem)
		return
	}

	// The environments of every project share one namespace, so a name taken
	// anywhere is taken here — including by a project this caller cannot see,
	// which is why the conflict names the environment and not its project.
	existing := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, body.Name, existing); err == nil {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"environment %q already exists", body.Name)})
		return
	} else if !apierrors.IsNotFound(err) {
		s.writeError(w, err)
		return
	}

	continuity, err := continuityFromRequest(body.Criticality, body.RTO, body.RPO)
	if err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	dataClass := project.Spec.DataClass
	if body.DataClass != nil {
		class, err := dataClassFromRequest(*body.DataClass)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		dataClass = class
	}
	if body.Requirements != nil {
		if !bundleDigestPattern.MatchString(strings.TrimSpace(body.Requirements.BundleDigest)) {
			badRequest(w, "requirements.bundleDigest %q is not a bundle digest: it has the form sha256:<64 hex characters>",
				body.Requirements.BundleDigest)
			return
		}
	}
	for _, owner := range body.Owners {
		if strings.TrimSpace(owner) == "" {
			badRequest(w, "owners must name accounts: an issuer subject, or an email address containing %q", "@")
			return
		}
	}

	caller, _ := CallerFrom(ctx)
	env := &kitchenv1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      body.Name,
			Namespace: s.Namespace,
			// The same two labels the build controller writes on an
			// environment it creates, so a declared one is found by
			// everything that looks for an environment by label rather than
			// by spec.
			Labels: map[string]string{
				controller.LabelProject: project.Name,
				managedByLabelKey:       managedByLabelValue,
			},
			Annotations: map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.EnvironmentSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			Type:       envType,
			// No release: that is what declaring an environment means, and
			// the environment reports AwaitingDeployment until a build puts
			// one here.
			Owners:       body.Owners,
			Requirements: body.Requirements,
			// Issue #137's inheritance, kept the same here as in the build
			// controller: an environment the platform is asked for takes its
			// project's class unless the caller rated it otherwise, so a
			// classified project's own environments can hold its data by
			// construction rather than by somebody remembering.
			DataClass: dataClass,
		},
	}
	if body.Residency != nil {
		env.Spec.Residency = strings.TrimSpace(*body.Residency)
	}
	if body.Serves != nil {
		serves, err := servesFromRequest(env, *body.Serves)
		if err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
		env.Spec.Serves = &kitchenv1alpha1.EnvironmentServes{Consumers: serves.next}
	}
	continuity.apply(&env.Spec.Criticality, &env.Spec.RTO, &env.Spec.RPO)

	details := map[string]any{"type": string(envType), "declared": true}
	if len(env.Spec.Owners) > 0 {
		details["owners"] = env.Spec.Owners
	}
	if env.Spec.Requirements != nil {
		details["bundleDigest"] = env.Spec.Requirements.BundleDigest
	}
	if env.Spec.DataClass != "" {
		details["dataClass"] = string(env.Spec.DataClass)
	}
	if env.Spec.Serves != nil {
		served := make([]string, 0, len(env.Spec.Serves.Consumers))
		for _, class := range env.ServedConsumers() {
			served = append(served, string(class))
		}
		details["serves"] = served
	}
	continuity.recordInto(details, kitchenv1alpha1.Continuity{})
	transition := audit.Transition{
		Object:    env,
		Kind:      audit.KindEnvironment,
		Operation: clickhouse.AuditCreate,
		Project:   project.Name,
		Reason: fmt.Sprintf("environment %s was declared for %s before anything deployed into it",
			env.Name, project.Name),
		Details: details,
	}
	// A declaration that sets the bar is the same privileged act the
	// requirements endpoint records, and is marked as one: the log alone has
	// to say what an environment demanded from the moment it existed.
	if body.declares() {
		transition.Privileged = audit.PrivilegeRequirements
	}
	if !s.recorded(w, req, transition) {
		return
	}
	if err := s.Client.Create(ctx, env); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("environment declared through the api",
		"environment", env.Name, "project", project.Name, "type", string(envType),
		"caller", callerName(caller))
	s.Activity.Record(ctx, clickhouse.Event{
		Type:        clickhouse.EventEnvironmentDeclared,
		Project:     project.Name,
		Environment: env.Name,
		Message: fmt.Sprintf("environment %s declared — nothing deployed into it yet",
			env.Name),
		Actor: callerName(caller),
	})
	writeJSON(w, http.StatusCreated,
		newEnvironmentView(env, s.sourceLinker().forProjectNamed(ctx, project.Name),
			project.Spec.Exposure))
}

// environmentNameAndType checks the half of the body a project member sends
// and answers with the type the environment will have, or with what is wrong
// with the name.
//
// The type is *derived* and only checked against what was sent, because
// nothing in this API writes an environment's type: EnvironmentTypeFor reads
// it off the project's promotion pipeline, and the environment reconciler
// re-derives it on every pass (#490). A type accepted here that disagreed
// with the pipeline would be corrected within seconds, so the disagreement is
// answered now, in words, rather than silently later.
func (s *Server) environmentNameAndType(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	body createEnvironmentRequest,
) (kitchenv1alpha1.EnvironmentType, string) {
	if body.Name == "" {
		return "", "name is required: the environment's own name, which becomes its hostname"
	}
	if errs := validation.IsDNS1123Label(body.Name); len(errs) > 0 {
		return "", fmt.Sprintf("name must work as a DNS label — lowercase letters, digits and '-', "+
			"starting and ending alphanumeric (got %q)", body.Name)
	}

	derived := controller.EnvironmentTypeFor(project, body.Name)
	if sent := kitchenv1alpha1.EnvironmentType(strings.TrimSpace(body.Type)); sent != "" && sent != derived {
		if sent == kitchenv1alpha1.EnvironmentPreview {
			return "", "a preview is created from a pull request and deleted with it, never declared: " +
				"declare a production or stage environment, or open a pull request"
		}
		if sent != kitchenv1alpha1.EnvironmentProduction && sent != kitchenv1alpha1.EnvironmentStage {
			return "", fmt.Sprintf("type must be production or stage, or absent to derive it from the "+
				"project's promotion pipeline (got %q)", body.Type)
		}
		return "", fmt.Sprintf("environment %s of project %s is a %s environment, not a %s one: which "+
			"environment production lands on is the last rung of spec.promotion.stages, and every other "+
			"durable environment of the project is a stage. Send type %q, or leave it out",
			body.Name, project.Name, derived, sent, derived)
	}

	// The hostname this environment would publish at, refused for the two
	// collisions a generated address can have: a label the platform serves
	// itself, and the shape of another project's preview (#423). A project
	// name is checked the same way at creation; an environment name is the
	// other half of the same address.
	label := controller.EnvironmentHostLabel(project.Name, body.Name, derived)
	baseDomain := s.platformBaseDomain(ctx)
	if platformhost.IsReserved(label) {
		return "", fmt.Sprintf("environment %q would publish at %s, which the platform serves itself: "+
			"choose another name", body.Name, hostOrLabel(label, baseDomain))
	}
	if platformhost.IsPreviewShaped(label) {
		return "", fmt.Sprintf("environment %q would publish at %s, which is where a pull request "+
			"preview is published: choose a name that does not end in -pr-<number>",
			body.Name, hostOrLabel(label, baseDomain))
	}
	return derived, ""
}

// hostOrLabel spells the address out where the installation has a base domain
// to spell it with, and names the label where it does not — the same fallback
// platformhost's own refusals use.
func hostOrLabel(label, baseDomain string) string {
	if host := platformhost.Host(label, baseDomain); host != "" {
		return host
	}
	return label + ".<the platform's base domain>"
}
