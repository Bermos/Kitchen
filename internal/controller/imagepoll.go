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
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
)

// The digest poll (issue #308): what corresponds to a push, for software this
// platform did not build.
//
// A Build is created by a webhook delivery, by a project's first-build
// seeding, or by somebody asking for a rebuild. All three name a commit, and a
// vendored image has none — so once such a project was deployed there was no
// event in the world that could ever move it. The event that corresponds to a
// push is **a new digest under a watched tag**, and this is the thing that
// notices one.
//
// # Why it costs what it costs
//
// A manifest HEAD, per watched reference, per interval. Never a pull: the
// question is "what does this tag point at", the registry answers it in a
// header, and the resolver already used by the acquisition path asks it that
// way (`attestation.Store.Resolve` → `remote.Head`). A reference pinned to a
// digest is not asked at all — it cannot move, so asking would be a request
// whose answer is known — which is what makes pinning both an opt-out from
// moving and an opt-out from the traffic.
//
// The estate is walked in batches rather than all at once. A step is a minute,
// each step polls at most imagePollBatch projects, and each project is due
// only once per the singleton's interval. That bounds the burst at any moment
// and spreads a large estate out permanently, instead of every project in it
// asking its registry the same question in the same second forever.
//
// # Why a Runnable
//
// The same two reasons the rescan sweep is one. The poll must happen once per
// interval across the platform rather than once per replica, and it has a
// platform-wide budget that a per-object requeue has no vantage point from
// which to count. Start never returns an error before the context ends: a
// registry that will not answer is a condition the platform reports rather
// than dies of.
//
// # What it deliberately is not
//
// A registry webhook. That would answer the same question with no polling at
// all, and it would require every vendor's registry to be able to reach this
// cluster — which the home lab this feature exists for cannot arrange, and a
// private installation should not have to. It stays available as a later
// optimisation, recorded in #306 and in docs/api/builds.md.
const (
	// imagePollStepInterval is how often the pass looks at the estate. It is
	// not the poll interval: a step decides which projects are *due* and
	// polls those. A minute is one cached list.
	imagePollStepInterval = time.Minute

	// imagePollBatch is how many projects one step may poll. It is the
	// bound the issue asks for, and it is deliberately a bound on the burst
	// rather than on the total: a hundred vendored projects are all polled,
	// ten at a time, and the estate spreads itself across the interval
	// instead of arriving at every registry at once.
	imagePollBatch = 10

	// imagePollTimeout bounds one reference's question. A registry that
	// hangs must not hold the pass, and a HEAD that has not answered in this
	// long is not going to.
	imagePollTimeout = 30 * time.Second

	// imagePollAnnotation says why the poll created this Build, on the Build
	// itself, so that a person reading the list can tell an acquisition
	// nobody asked for from one somebody did.
	imagePollAnnotation = "kitchen.bermos.dev/image-poll"
)

// ImagePollSweeper asks, on an interval, whether the tags this platform
// follows still name the digests it acquired.
type ImagePollSweeper struct {
	Client client.Client

	// Audit is waited on and fail-closed, like every decision path: an
	// acquisition the log refuses to record is one this pass does not
	// create. May be nil.
	Audit *audit.Recorder

	// Resolvers is how the pass asks a registry which digest a reference
	// names — the same seam BuildReconciler has, for the same reason: a test
	// reaching the real internet would be a test about the internet. Nil is
	// the real registry.
	Resolvers ImageResolverFactory

	// Now is the clock. Nil is time.Now; tests move it to make an interval
	// elapse without waiting for one.
	Now func() time.Time

	// StepInterval overrides imagePollStepInterval. Tests alone set it.
	StepInterval time.Duration
}

// NeedLeaderElection makes the poll a singleton. Two replicas would ask every
// registry twice and race each other to create the same acquisition.
func (s *ImagePollSweeper) NeedLeaderElection() bool { return true }

func (s *ImagePollSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// The Builds this pass creates, the Projects it reads and the status it
// writes are all covered by the build and project reconcilers' own grants,
// which are the reconcilers this pass hands its work to. What it needs beyond
// them is nothing: it reads Connections and Secrets to learn which credential
// a private image is read with, and both are already granted to the build
// controller in the same role.

// Start implements manager.Runnable.
func (s *ImagePollSweeper) Start(ctx context.Context) error {
	step := s.StepInterval
	if step <= 0 {
		step = imagePollStepInterval
	}
	// The configuration is re-read on every step rather than watched, like
	// the rescan sweep: an operator who has just changed the interval should
	// not have to restart the operator for it to take.
	ticker := time.NewTicker(step)
	defer ticker.Stop()

	for {
		if _, err := s.PollOnce(ctx); err != nil {
			logf.FromContext(ctx).V(1).Info("the image poll could not complete", "reason", err.Error())
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// ImagePollReport is what one pass did, which is what a test asserts on.
type ImagePollReport struct {
	// Running is whether the poll is on at all.
	Running bool
	// Watched is how many projects follow a reference that can move,
	// Polled how many of them this pass asked about, Acquired how many
	// acquisitions it created because something moved, and Unreadable how
	// many registries would not answer.
	//
	// The last two are counted apart because they are opposite outcomes that
	// both create a Build: one takes a new digest, the other records that
	// none could be read.
	Watched    int
	Polled     int
	Acquired   int
	Unreadable int
	// Message explains a pass that is not running.
	Message string
}

// PollOnce walks the estate once: it asks the registry about every project
// that is due and the batch allows, and creates an acquisition for every one
// whose watched reference has moved.
//
// It is exported because it is the unit of the pass — a test drives it
// directly rather than waiting on a ticker.
func (s *ImagePollSweeper) PollOnce(ctx context.Context) (ImagePollReport, error) {
	log := logf.FromContext(ctx)

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: KitchenSingletonName}, kitchen); err != nil {
		return ImagePollReport{}, client.IgnoreNotFound(err)
	}
	interval := kitchen.Spec.Builds.EffectiveImagePollInterval()
	if interval <= 0 {
		return ImagePollReport{Message: "the digest poll is off: builds.imagePollInterval is 0"}, nil
	}

	projects := &kitchenv1alpha1.ProjectList{}
	if err := s.Client.List(ctx, projects, client.InNamespace(PlatformNamespace)); err != nil {
		return ImagePollReport{}, err
	}

	report := ImagePollReport{Running: true}
	now := s.now()
	for i := range projects.Items {
		project := &projects.Items[i]
		if len(watchedReferences(project)) == 0 {
			continue
		}
		report.Watched++
		if report.Polled >= imagePollBatch {
			continue
		}
		if last := project.Status.ImagePoll; last != nil && last.LastPolledAt != nil &&
			now.Sub(last.LastPolledAt.Time) < interval {
			continue
		}
		report.Polled++
		outcome, err := s.poll(ctx, project)
		if err != nil {
			// One project's registry, one project's problem: the pass goes
			// on to the next rather than leaving the rest of the estate
			// unasked.
			log.V(1).Info("a project's images could not be polled",
				"project", project.Name, "reason", err.Error())
			continue
		}
		switch {
		case outcome.Unreadable:
			report.Unreadable++
			log.Info("a watched registry would not answer",
				"project", project.Name, "build", outcome.Build)
		case outcome.Build != "":
			report.Acquired++
			log.Info("a watched image moved", "project", project.Name, "build", outcome.Build)
		}
	}
	return report, nil
}

// watchedImage is one reference the poll follows, and which workload of the
// unit declares it.
type watchedImage struct {
	// Workload is the process this image belongs to, empty for the web
	// process — which is `spec.runtime` and has no entry in the process list.
	Workload string
	Image    kitchenv1alpha1.ImageSourceSpec
}

// watchedReferences is every reference of a project that can move: each
// vendored image it declares, minus the ones pinned to a digest.
//
// A pinned reference is a project saying it does not want to be moved, and it
// is answered by being asked nothing at all — no request, no Build, no
// acquisition it did not ask for. Which is the acceptance criterion, and also
// the cheapest possible implementation of it.
//
// A project built from a repository is watched by nothing here. Its vendored
// workloads are re-resolved by every build it runs, and it has pushes, which
// is the event this pass exists to substitute for.
func watchedReferences(project *kitchenv1alpha1.Project) []watchedImage {
	if project.Spec.Source.HasRepository() {
		return nil
	}
	watched := make([]watchedImage, 0, len(project.Spec.Processes)+1)
	if web := project.Spec.Source.ImageSource(); web.Repository != "" && web.Digest == "" {
		watched = append(watched, watchedImage{Image: web})
	}
	for _, process := range project.Spec.Processes {
		if process.Image == nil || process.Image.Digest != "" || process.Image.Repository == "" {
			continue
		}
		watched = append(watched, watchedImage{Workload: process.Name, Image: *process.Image})
	}
	return watched
}

// pollOutcome is what one project's poll came to: the Build it created, if
// any, and whether that Build exists because the registry would not answer
// rather than because something moved.
type pollOutcome struct {
	Build      string
	Unreadable bool
}

// poll asks one project's registries what its watched references name now,
// and creates an acquisition where the answer has changed.
func (s *ImagePollSweeper) poll(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
) (pollOutcome, error) {
	// An acquisition already in flight is the answer to this interval's
	// question, and a second one would race it for the same Release. The
	// project is left un-stamped, so the next step asks again rather than
	// waiting out another interval.
	if pending, err := acquisitionInFlight(ctx, s.Client, project); err != nil || pending {
		return pollOutcome{}, err
	}

	// What the project is running, which is what "has it moved" is asked
	// against. A project that has acquired nothing yet is the seeding's to
	// deploy, not this pass's — and until the seed has succeeded there is
	// nothing here to compare with.
	baseline, err := latestAcquisition(ctx, s.Client, project, "")
	if err != nil || baseline == nil {
		return pollOutcome{}, err
	}

	resolved, failure := s.resolve(ctx, project)
	if failure != "" {
		name, err := s.unreadable(ctx, project, failure)
		return pollOutcome{Build: name, Unreadable: true}, err
	}

	moved := movedReferences(baseline, resolved)
	if len(moved) == 0 {
		return pollOutcome{}, s.stamp(ctx, project, "")
	}
	name, err := s.acquire(ctx, project, baseline, resolved, moved)
	return pollOutcome{Build: name}, err
}

// resolve asks every watched reference of one project what it names now. The
// first refusal ends the pass for this project: a registry that would not
// answer one question is not going to answer the next, and a partial answer
// is not something to create an acquisition from.
func (s *ImagePollSweeper) resolve(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
) (map[string]string, string) {
	resolved := map[string]string{}
	for _, watched := range watchedReferences(project) {
		_, dockerConfig, err := pullCredential(ctx, s.Client, project.Namespace, watched.Image)
		if err != nil {
			return nil, fmt.Sprintf("%s could not be asked about: %v", watched.Image.Reference(), err)
		}
		asked, cancel := context.WithTimeout(ctx, imagePollTimeout)
		image, err := resolveWith(asked, s.Resolvers, dockerConfig, watched.Image)
		cancel()
		if err != nil {
			return nil, fmt.Sprintf("%s could not be asked about: %v", watched.Image.Reference(), err)
		}
		resolved[watched.Workload] = image
	}
	return resolved, ""
}

// movedReferences is every watched workload whose digest is not the one the
// last acquisition froze, in the order a person would read them.
//
// A workload the baseline never resolved counts as moved: a vendored workload
// added to a unit since its last acquisition has no image on the Release, and
// the acquisition is what puts one there.
func movedReferences(baseline *kitchenv1alpha1.Build, resolved map[string]string) []string {
	running := map[string]string{"": baseline.Status.Acquisition.Image}
	for _, row := range baseline.Status.Workloads {
		running[row.Name] = row.Image
	}
	var moved []string
	for workload, image := range resolved {
		if running[workload] != image {
			moved = append(moved, workload)
		}
	}
	sort.Strings(moved)
	return moved
}

// acquire creates the Build a moved tag produces.
//
// It is named after what it resolved rather than after what it followed,
// which is the same property AcquisitionNameFor exists for one level down:
// two passes that find the same digests converge on one Build, and a Build
// for a set of digests already acquired is an AlreadyExists that means
// "already taken" rather than a second run of it.
func (s *ImagePollSweeper) acquire(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	baseline *kitchenv1alpha1.Build,
	resolved map[string]string,
	moved []string,
) (string, error) {
	web := resolved[""]
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			Name:      AcquisitionNameFor(project.Name, fingerprint(resolved)),
			Namespace: project.Namespace,
			Labels:    map[string]string{kitchenv1alpha1.ProjectLabel: project.Name},
			Annotations: map[string]string{
				imagePollAnnotation: fmt.Sprintf(
					"a watched tag moved: %s", strings.Join(readableWorkloads(moved), ", ")),
			},
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			Acquire: &kitchenv1alpha1.AcquisitionSpec{
				Reference: project.Spec.Source.ImageSource().Reference(),
				// The pin: this pass has already asked, so the acquisition
				// takes the digest that made it exist rather than asking
				// again and possibly getting a third answer. Empty where the
				// web process is itself pinned and a workload is what moved,
				// in which case the project's own digest is the pin.
				Digest:  digestOf(web),
				Trigger: kitchenv1alpha1.AcquisitionPolled,
			},
		},
	}
	previous := baseline.Status.Acquisition.Image
	reference := project.Spec.Source.ImageSource().Reference()
	// The entry names both digests when the web process is what moved, and
	// the workloads when it is not — a project whose own image is pinned can
	// still carry a workload that is not. Either way it says what was left
	// and what was taken, which is the whole of what the record is for.
	reason := fmt.Sprintf("%s moved: %s is now %s", reference, previous, web)
	if web == "" {
		reason = fmt.Sprintf("a workload of %s moved: %s",
			reference, strings.Join(readableWorkloads(moved), ", "))
	}
	if err := s.Audit.Record(ctx, audit.Transition{
		Object:     build,
		Kind:       audit.KindBuild,
		Operation:  clickhouse.AuditCreate,
		Controller: actorImagePoll,
		To:         string(kitchenv1alpha1.BuildQueued),
		Project:    project.Name,
		Reason:     reason,
		Details: map[string]any{
			"reference": reference,
			"previous":  previous,
			"image":     web,
			"workloads": moved,
		},
	}); err != nil {
		return "", err
	}
	switch err := s.Client.Create(ctx, build); {
	case apierrors.IsAlreadyExists(err):
		// These digests are already being taken, by an earlier pass or by
		// somebody who asked for exactly them.
		return "", s.stamp(ctx, project, "")
	case err != nil:
		return "", err
	}
	return build.Name, s.stamp(ctx, project, "")
}

// unreadable is a registry that would not answer.
//
// It is a Build that fails for a stated reason rather than a silence, which
// is the acceptance criterion — an image nobody can resolve should read like
// a build that could not run, not like a platform that stopped looking. And
// it is exactly *one* Build: the message on the project is the record that it
// has already been said, so a registry that stays down for a week produces
// one failed Build and not a thousand. What it does not do is touch what is
// running: an environment serving the digest it acquired last month goes on
// serving it.
func (s *ImagePollSweeper) unreadable(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	failure string,
) (string, error) {
	if last := project.Status.ImagePoll; last != nil && last.Message != "" {
		return "", s.stamp(ctx, project, failure)
	}
	build := &kitchenv1alpha1.Build{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: project.Name + "-acq-",
			Namespace:    project.Namespace,
			Labels:       map[string]string{kitchenv1alpha1.ProjectLabel: project.Name},
			Annotations:  map[string]string{imagePollAnnotation: failure},
		},
		Spec: kitchenv1alpha1.BuildSpec{
			ProjectRef: kitchenv1alpha1.LocalObjectReference{Name: project.Name},
			// No pin: this pass has nothing to pin, and the Build asking the
			// registry itself is what records why the answer could not be
			// had — through the same failure path an acquisition somebody
			// asked for would take.
			Acquire: &kitchenv1alpha1.AcquisitionSpec{
				Reference: project.Spec.Source.ImageSource().Reference(),
				Trigger:   kitchenv1alpha1.AcquisitionPolled,
			},
		},
	}
	if err := s.Audit.Record(ctx, audit.Transition{
		Object:     build,
		Kind:       audit.KindBuild,
		Operation:  clickhouse.AuditCreate,
		Controller: actorImagePoll,
		To:         string(kitchenv1alpha1.BuildQueued),
		Project:    project.Name,
		Reason:     failure,
		Details:    map[string]any{"reference": project.Spec.Source.ImageSource().Reference()},
	}); err != nil {
		return "", err
	}
	if err := s.Client.Create(ctx, build); err != nil {
		return "", err
	}
	return build.Name, s.stamp(ctx, project, failure)
}

// stamp records that this project has been asked, and what stopped the
// asking where something did.
func (s *ImagePollSweeper) stamp(
	ctx context.Context,
	project *kitchenv1alpha1.Project,
	message string,
) error {
	project.Status.ImagePoll = &kitchenv1alpha1.ImagePollStatus{
		LastPolledAt: ptr.To(metav1.NewTime(s.now())),
		Message:      message,
	}
	return s.Client.Status().Update(ctx, project)
}

// acquisitionInFlight reports whether this project already has an acquisition
// that has not finished.
func acquisitionInFlight(
	ctx context.Context,
	c client.Client,
	project *kitchenv1alpha1.Project,
) (bool, error) {
	builds := &kitchenv1alpha1.BuildList{}
	if err := c.List(ctx, builds, client.InNamespace(project.Namespace),
		client.MatchingLabels{kitchenv1alpha1.ProjectLabel: project.Name}); err != nil {
		return false, err
	}
	for i := range builds.Items {
		build := &builds.Items[i]
		if build.Spec.ProjectRef.Name != project.Name || build.FromRepository() {
			continue
		}
		switch build.Status.Phase {
		case kitchenv1alpha1.BuildSucceeded, kitchenv1alpha1.BuildFailed, kitchenv1alpha1.BuildCancelled:
		default:
			return true, nil
		}
	}
	return false, nil
}

// fingerprint is the whole of what one pass resolved, as one string, so that
// a set of digests names one Build. It is sorted for the obvious reason: a
// map has no order and a Build's name may not depend on one.
func fingerprint(resolved map[string]string) string {
	pairs := make([]string, 0, len(resolved))
	for workload, image := range resolved {
		pairs = append(pairs, workload+"="+image)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, " ")
}

// digestOf is the digest half of a resolved reference, empty for anything
// that is not one.
func digestOf(image string) string {
	_, digest, found := strings.Cut(image, "@")
	if !found {
		return ""
	}
	return digest
}

// readableWorkloads names the workloads that moved the way a person would,
// with the web process called what the screens call it rather than left as
// the empty string it is in the spec.
func readableWorkloads(moved []string) []string {
	named := make([]string, 0, len(moved))
	for _, workload := range moved {
		if workload == "" {
			workload = kitchenv1alpha1.WebProcessName
		}
		named = append(named, workload)
	}
	return named
}
