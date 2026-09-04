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

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// `POST /environments/{name}/redeploy` — the same commit, today's settings
// (#392).
//
// A corrected setting used to have nowhere to go. The runtime lands with the
// next release; a rebuild of an unchanged commit resolves to the Release that
// commit already has; and a Release is immutable, which is exactly what makes
// a rollback exact and what closed the last door. So a project could hold a
// wrong setting, the right setting, and no path between them — and the
// recovery was two `kubectl delete`s, which is the one thing this platform
// says must never be the answer.
//
// This is that path, and it bends neither rule it is between: the Release
// already deployed is untouched and still describes precisely what it
// deployed, and what this makes is a *new* Release — the same commit, the same
// digests, a fresh snapshot — that a rollback can return to like any other.
//
// It is a write with real blast radius and it is honest about it: nothing
// silent happens. An environment with no release is refused, and so is a
// project whose settings the running release already froze, because a Release
// identical to the one running is not a recovery, it is noise in a history
// that is read under pressure.

// redeployView is what the request is answered with: the Release that was
// made and where it is going. It is a 202 because making the Release is the
// whole of what happened synchronously — the deploy itself is the
// environment reconciler's, and the environment's own screen is where it is
// watched.
type redeployView struct {
	Environment string `json:"environment"`
	Project     string `json:"project"`
	// Release is the Release this call made — or found, when the same commit
	// was already redeployed with exactly these settings once before.
	Release string `json:"release"`
	// PreviousRelease is what the environment was on. It is here because the
	// name is the only handle on the snapshot being left behind, and rolling
	// back to it is how a redeploy is undone.
	PreviousRelease string `json:"previousRelease"`
	Image           string `json:"image"`
	// Promotion is set when the environment declares requirements: the move
	// is not made here, it is asked for, and the policy engine decides. One
	// door into a gated environment, whichever route knocks.
	Promotion string `json:"promotion,omitempty"`
	Message   string `json:"message"`
}

func (s *Server) redeployEnvironment(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	env := &kitchenv1alpha1.Environment{}
	if err := s.get(ctx, req.PathValue("name"), env); err != nil {
		s.writeError(w, err)
		return
	}
	if env.Spec.ReleaseRef.Name == "" {
		badRequest(w, "environment %s is not running a release yet, so there is nothing to redeploy: "+
			"build a commit for it first", env.Name)
		return
	}

	current := &kitchenv1alpha1.Release{}
	if err := s.get(ctx, env.Spec.ReleaseRef.Name, current); err != nil {
		if apierrors.IsNotFound(err) {
			badRequest(w, "environment %s names release %s, which no longer exists, "+
				"so there is no commit to redeploy: build one",
				env.Name, env.Spec.ReleaseRef.Name)
			return
		}
		s.writeError(w, err)
		return
	}

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, env.Spec.ProjectRef.Name, project); err != nil {
		s.writeError(w, err)
		return
	}

	// The build is where the commit's own kitchen.json is kept, and a
	// snapshot taken without it would silently drop everything the repository
	// declares — a worker, a health path, a port. So a build that has been
	// pruned is refused rather than worked around: what this route promises
	// is the commit's configuration with the project's corrections over it,
	// and half of that is not it.
	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, current.Spec.BuildRef.Name, build); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
				"the build that produced %s (%s) is gone, so the settings that commit declared in its "+
					"kitchen.json cannot be read back, and a redeploy without them would drop what the "+
					"repository asked for. Build the commit again instead",
				current.Name, current.Spec.BuildRef.Name)})
			return
		}
		s.writeError(w, err)
		return
	}

	fresh, err := controller.RedeployRelease(project, build, current)
	if err != nil {
		// The one way this fails is a project and a kitchen.json that cannot
		// both be true — the same refusal a build gives, in the same words.
		badRequest(w, "%s", err.Error())
		return
	}
	if equality.Semantic.DeepEqual(fresh.Spec.ConfigSnapshot, current.Spec.ConfigSnapshot) {
		writeJSON(w, http.StatusConflict, errorBody{Error: fmt.Sprintf(
			"%s is already running exactly what %s declares now: release %s froze these settings, "+
				"so there is nothing to redeploy",
			env.Name, project.Name, current.Name)})
		return
	}

	// The same hard check the direct move makes (#137): a classified
	// project's release does not land on an environment rated below it, and a
	// project reclassified since the last deploy is exactly when that becomes
	// true without anybody deploying anything.
	if refusal := controller.DataClassRefusal(project, env); refusal != "" {
		badRequest(w, "%s", refusal)
		return
	}

	if !s.recorded(w, req, audit.Transition{
		Object:    fresh,
		Kind:      audit.KindRelease,
		Operation: clickhouse.AuditCreate,
		To:        fresh.Spec.Image,
		Project:   project.Name,
		Reason: fmt.Sprintf("release %s was redeployed with %s's current settings",
			current.Name, project.Name),
		Details: map[string]any{
			"environment":     env.Name,
			"previousRelease": current.Name,
			"build":           fresh.Spec.BuildRef.Name,
			"image":           fresh.Spec.Image,
		},
	}) {
		return
	}
	// An already-existing Release of this name is this exact snapshot of this
	// exact commit — the name is a fingerprint of the spec — so it is adopted
	// rather than refused: a project that corrects a setting, corrects it
	// back and corrects it again converges on two Releases, not four.
	if err := s.Client.Create(ctx, fresh); err != nil && !apierrors.IsAlreadyExists(err) {
		s.writeError(w, err)
		return
	}

	view := redeployView{
		Environment:     env.Name,
		Project:         project.Name,
		Release:         fresh.Name,
		PreviousRelease: current.Name,
		Image:           fresh.Spec.Image,
	}

	// An environment that declares requirements takes releases only through a
	// Promotion, however the release was made. The 202 says the same thing it
	// says on the PATCH: the move is accepted for evaluation, not made.
	if env.Spec.Requirements != nil {
		caller, _ := CallerFrom(ctx)
		promotion := s.manualPromotion(caller, project.Name, env, fresh,
			fmt.Sprintf("redeploying %s with %s's current settings", current.Name, project.Name))
		if !s.recorded(w, req, promotionTransition(promotion)) {
			return
		}
		if err := s.Client.Create(ctx, promotion); err != nil {
			s.writeError(w, err)
			return
		}
		s.log().Info("redeploy routed through a promotion",
			"environment", env.Name, "release", fresh.Name,
			"promotion", promotion.Name, "caller", callerName(caller))
		view.Promotion = promotion.Name
		view.Message = fmt.Sprintf(
			"%s was cut from the same commit with %s's current settings, and awaits %s's requirements",
			fresh.Name, project.Name, env.Name)
		writeJSON(w, http.StatusAccepted, view)
		return
	}

	// A redeploy supersedes whatever the environment is on, exactly the way a
	// newer commit's release does — including one whose deploy is still in
	// flight, which is the wedge this route exists to end.
	if !s.pointEnvironmentAt(w, req, env, fresh, releaseMove{
		reason: kitchenv1alpha1.ReleaseMoveSuperseded,
		verb:   "redeployed as", event: clickhouse.EventReleaseRedeployed,
	}) {
		return
	}
	view.Message = fmt.Sprintf("%s is deploying %s: the same commit as %s, with %s's current settings",
		env.Name, fresh.Name, current.Name, project.Name)
	writeJSON(w, http.StatusAccepted, view)
}
