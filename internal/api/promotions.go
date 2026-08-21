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
	"sort"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The promotion surface: asking for a release to land on an environment, and
// reading what became of the asking.
//
// A Promotion is a request, not a move. The API only ever creates the object
// — phase Pending, 201 — and the promotion reconciler evaluates the
// environment's requirements, records the decision, and applies or refuses
// it. That split is the point: the answer to "may this artifact go there" is
// the policy engine's, recorded and replayable, never an API handler's.
//
// Rollback is the same request at an older release: the Release is an
// immutable snapshot, so re-promoting it puts back exactly what ran, without
// a rebuild.

// createPromotionRequest asks for one release to land on one environment.
type createPromotionRequest struct {
	Environment string `json:"environment"`
	Release     string `json:"release"`
	// Reason is the requester's own words for why, carried into the audit
	// record. Optional for routine moves.
	Reason string `json:"reason,omitempty"`
}

// promotionTransition is the audit record a manual promotion appends before
// the object exists. Built apart from the recording so a test can hold it up
// to the light without a store.
func promotionTransition(promotion *kitchenv1alpha1.Promotion) audit.Transition {
	details := map[string]any{
		"environment": promotion.Spec.EnvironmentRef.Name,
		"release":     promotion.Spec.ReleaseRef.Name,
		"trigger":     string(promotion.Spec.Trigger),
	}
	if promotion.Spec.Reason != "" {
		details["reason"] = promotion.Spec.Reason
	}
	return audit.Transition{
		Object:    promotion,
		Kind:      audit.KindPromotion,
		Operation: clickhouse.AuditCreate,
		To:        promotion.Spec.ReleaseRef.Name,
		Project:   promotion.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf("promotion of release %s to %s requested",
			promotion.Spec.ReleaseRef.Name, promotion.Spec.EnvironmentRef.Name),
		Details: details,
	}
}

// manualPromotion builds the Promotion object a caller's request becomes —
// one construction, shared with the environment PATCH that routes through a
// promotion when the target declares requirements.
func (s *Server) manualPromotion(
	caller Caller,
	project string,
	env *kitchenv1alpha1.Environment,
	release *kitchenv1alpha1.Release,
	reason string,
) *kitchenv1alpha1.Promotion {
	return &kitchenv1alpha1.Promotion{
		ObjectMeta: metav1.ObjectMeta{
			// A retry of a blocked promotion is a new object — the spec is
			// immutable, and the history of what was asked and refused is the
			// point — so names are generated rather than derived.
			GenerateName: project + "-promo-",
			Namespace:    s.Namespace,
			Labels:       map[string]string{"kitchen.bermos.dev/project": project},
			Annotations:  map[string]string{requestedByAnnotation: callerName(caller)},
		},
		Spec: kitchenv1alpha1.PromotionSpec{
			ProjectRef:     kitchenv1alpha1.LocalObjectReference{Name: project},
			EnvironmentRef: kitchenv1alpha1.LocalObjectReference{Name: env.Name},
			ReleaseRef:     kitchenv1alpha1.LocalObjectReference{Name: release.Name},
			RequestedBy:    callerName(caller),
			Trigger:        kitchenv1alpha1.PromotionManual,
			Reason:         reason,
		},
	}
}

// createPromotion handles POST /api/v1/projects/{name}/promotions.
func (s *Server) createPromotion(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	body := createPromotionRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Environment = strings.TrimSpace(body.Environment)
	body.Release = strings.TrimSpace(body.Release)
	if body.Environment == "" || body.Release == "" {
		badRequest(w, "environment and release are both required: "+
			`{"environment": "<environment name>", "release": "<release name>"}`)
		return
	}

	// Both must exist and belong to this project. A body naming something
	// that does not is a 400, not a 404 — the endpoint exists, the body names
	// something that doesn't.
	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, body.Environment, env); err != nil {
		badRequest(w, "environment %q does not exist", body.Environment)
		return
	}
	if env.Spec.ProjectRef.Name != project.Name {
		badRequest(w, "environment %q belongs to project %q, not %q",
			env.Name, env.Spec.ProjectRef.Name, project.Name)
		return
	}
	release := &kitchenv1alpha1.Release{}
	if err := s.get(ctx, body.Release, release); err != nil {
		badRequest(w, "release %q does not exist", body.Release)
		return
	}
	if release.Spec.ProjectRef.Name != project.Name {
		badRequest(w, "release %q belongs to project %q, not %q",
			release.Name, release.Spec.ProjectRef.Name, project.Name)
		return
	}

	caller, _ := CallerFrom(ctx)
	promotion := s.manualPromotion(caller, project.Name, env, release, strings.TrimSpace(body.Reason))
	if !s.recorded(w, req, promotionTransition(promotion)) {
		return
	}
	if err := s.Client.Create(ctx, promotion); err != nil {
		s.writeError(w, err)
		return
	}

	s.log().Info("promotion requested through the api", "promotion", promotion.Name,
		"project", project.Name, "environment", env.Name, "release", release.Name,
		"caller", callerName(caller))
	writeJSON(w, http.StatusCreated, newPromotionView(promotion))
}

// listProjectPromotions handles GET /api/v1/projects/{name}/promotions,
// newest first, narrowed by ?environment=, ?release= and ?phase=.
func (s *Server) listProjectPromotions(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}

	promotions := &kitchenv1alpha1.PromotionList{}
	if err := s.Client.List(ctx, promotions, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}

	environment := strings.TrimSpace(req.URL.Query().Get("environment"))
	release := strings.TrimSpace(req.URL.Query().Get("release"))
	phase := strings.TrimSpace(req.URL.Query().Get("phase"))

	views := []promotionView{}
	for i := range promotions.Items {
		promotion := &promotions.Items[i]
		if promotion.Spec.ProjectRef.Name != project.Name {
			continue
		}
		if environment != "" && promotion.Spec.EnvironmentRef.Name != environment {
			continue
		}
		if release != "" && promotion.Spec.ReleaseRef.Name != release {
			continue
		}
		view := newPromotionView(promotion)
		if phase != "" && !strings.EqualFold(view.Phase, phase) {
			continue
		}
		views = append(views, view)
	}
	// Newest first; names break the tie because creation timestamps are
	// second-granular.
	sort.Slice(views, func(i, j int) bool {
		if !views[i].CreatedAt.Equal(views[j].CreatedAt) {
			return views[i].CreatedAt.After(views[j].CreatedAt)
		}
		return views[i].Name > views[j].Name
	})
	writeList(w, views)
}

// getPromotion handles GET /api/v1/promotions/{name}.
func (s *Server) getPromotion(w http.ResponseWriter, req *http.Request) {
	promotion := &kitchenv1alpha1.Promotion{}
	if err := s.get(req.Context(), req.PathValue("name"), promotion); err != nil {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, newPromotionView(promotion))
}
