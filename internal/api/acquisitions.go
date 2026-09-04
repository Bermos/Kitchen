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
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// Asking for an acquisition (#308).
//
// The digest poll is what makes a vendored image move on its own, and this is
// the same thing on demand: "check now", for somebody who has just published
// upstream and would rather not wait out an interval, and "take this exact
// digest", which is what a vendor's own pipeline would call at the end of its
// publish.
//
// It is `POST /projects/{name}/acquisitions` and not a second spelling of
// `POST /projects/{name}/builds` because the two ask different questions of
// different projects: a build names a commit, an acquisition names an image,
// and a project has one of those or the other and never both. A single route
// that decided by which key the body carried would be exactly the shape the
// authorization model refuses for writes.
//
// **Admin, where a rebuild is a developer's.** A rebuild runs the commit the
// project already has through the same builder; an acquisition takes a new
// artifact from a third party's registry onto this platform, which is nearer
// to changing where the project's software comes from than to re-running the
// build of it. Naming a digest outright is nearer still.

// acquisitionDigest is the one form a pinned digest is accepted in. The
// registry vocabulary has exactly one spelling of a manifest digest, and
// accepting a bare hex string "helpfully" would be the API guessing which
// algorithm a caller meant.
var acquisitionDigest = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// createAcquisitionRequest is the whole of the body, and it is optional: an
// empty body means "whatever the reference names now", which is the check-now
// case and the one a button presses.
type createAcquisitionRequest struct {
	// Digest pins what to take, `sha256:…`. It is refused where it is not
	// that, rather than passed through to fail at admission.
	Digest string `json:"digest,omitempty"`
}

func (s *Server) createAcquisition(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, req.PathValue("name"), project); err != nil {
		s.writeError(w, err)
		return
	}
	if project.Spec.Source.HasRepository() {
		badRequest(w,
			"project %q is built from %s, so there is nothing to acquire: what moves it is a commit. "+
				"Push one, or ask for a build of one with POST /api/v1/projects/%s/builds",
			project.Name, project.Spec.Source.GitSource().Repo, project.Name)
		return
	}

	body := createAcquisitionRequest{}
	if req.ContentLength != 0 {
		if err := decodeBody(req, &body); err != nil {
			badRequest(w, "%s", err.Error())
			return
		}
	}
	digest := strings.TrimSpace(body.Digest)
	if digest != "" && !acquisitionDigest.MatchString(digest) {
		badRequest(w, "%q is not an image digest: give the whole of one, as `sha256:` and sixty-four hex digits", digest)
		return
	}

	// What this acquisition follows. Naming a digest is following that
	// digest — the record says what was actually taken, not what the project
	// happens to declare — and naming nothing follows the project's own
	// reference.
	image := project.Spec.Source.ImageSource()
	reference := image.Reference()
	if digest != "" {
		reference = image.Repository + "@" + digest
	}

	caller, _ := CallerFrom(ctx)
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			// Generated, like a rebuild's and for the same reason: who asked
			// for what, and when, is the point of the record. The poll's
			// deterministic names are left free for the acquisitions it
			// deduplicates.
			GenerateName: project.Name + "-acq-",
			Namespace:    s.Namespace,
			Labels:       map[string]string{"kitchen.bermos.dev/project": project.Name},
			Annotations:  map[string]string{"kitchen.bermos.dev/requested-by": callerName(caller)},
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			Acquire: &kitchenv1alpha1.AcquisitionSpec{
				Reference: reference,
				Digest:    digest,
				Trigger:   kitchenv1alpha1.AcquisitionRequested,
			},
		},
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    build,
		Kind:      audit.KindBuild,
		Operation: clickhouse.AuditCreate,
		To:        string(kitchenv1alpha1.BuildQueued),
		Project:   project.Name,
		Reason:    fmt.Sprintf("an acquisition of %s was requested", reference),
		Details:   map[string]any{"reference": reference, "digest": digest},
	}) {
		return
	}
	if err := s.Client.Create(ctx, build); err != nil {
		s.writeError(w, err)
		return
	}
	s.log().Info("acquisition requested through the api",
		"project", project.Name, "build", build.Name, "reference", reference, "caller", callerName(caller))
	// 202: the platform has taken the request, and what answers it is the
	// operator resolving the digest and producing a Release. The Build is
	// where that answer lands, which is why it is the body.
	writeJSON(w, http.StatusAccepted, newBuildView(build))
}
