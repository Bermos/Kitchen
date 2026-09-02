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
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
)

// The addon surface: what the platform can install into its own cluster, what
// it has, and the two writes that change either.
//
// The list is the *catalogue* rather than the objects, and that is the whole
// shape of the screen behind it: an operator asking "can this cluster give me
// a database" wants every entry this operator knows how to install, annotated
// with what is actually there. The platform seeds an Addon for each, so the
// two lists usually agree — but an Addon somebody deleted stays deleted, and
// listing objects would then drop exactly the row they came to the page to
// click.
//
// Nothing here can name a chart, a repository or a version. The catalogue is
// compiled into the operator, a request carries an entry ID and a namespace,
// and the install job's account can apply CRDs and ClusterRoles — which is
// why those two are the only things a request gets to say.

// addonView is one catalogue entry and what the cluster says about it.
type addonView struct {
	// The entry, as the operator compiled it in.
	ID               string                           `json:"id"`
	Title            string                           `json:"title"`
	Summary          string                           `json:"summary"`
	Charts           []controller.AddonCatalogueChart `json:"charts"`
	DefaultNamespace string                           `json:"defaultNamespace"`
	DependsOn        []string                         `json:"dependsOn,omitempty"`
	BlastRadius      string                           `json:"blastRadius"`

	// Permitted is whether this installation granted an account to install
	// it with, and ChartValue is the value that would. An entry that is not
	// permitted is still listed: knowing what the platform *could* run, and
	// the one line that would let it, is the answer to the question the page
	// is open for.
	Permitted    bool   `json:"permitted"`
	ChartValue   string `json:"chartValue"`
	ClusterAdmin bool   `json:"clusterAdmin"`
	GrantBecause string `json:"grantBecause"`

	// Requested is whether an Addon exists and asks for the install; absent
	// where there is no Addon at all.
	Requested *bool `json:"requested,omitempty"`

	// What the cluster has. Serving is whether its API answers, whoever
	// installed it; Managed is whether that was the platform.
	Serving    bool                               `json:"serving"`
	Managed    bool                               `json:"managed"`
	Namespace  string                             `json:"namespace,omitempty"`
	Installed  []kitchenv1alpha1.AddonChartStatus `json:"installed,omitempty"`
	Conditions []conditionView                    `json:"conditions,omitempty"`
}

// addonWriteRequest is a create or a change. It carries an entry and a
// namespace and nothing else, because there is nothing else it may say.
type addonWriteRequest struct {
	// ID names the catalogue entry. Ignored on a change, where the path
	// says which.
	ID string `json:"id,omitempty"`
	// Install is the request. Nil on a change leaves it as it is.
	Install *bool `json:"install,omitempty"`
	// Namespace is where the entry is installed. Empty is the entry's own
	// default.
	Namespace string `json:"namespace,omitempty"`
}

// listAddons answers the catalogue, annotated with what is installed.
func (s *Server) listAddons(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	addons := &kitchenv1alpha1.AddonList{}
	if err := s.Client.List(ctx, addons, client.InNamespace(s.Namespace)); err != nil {
		s.writeError(w, err)
		return
	}
	byID := map[string]*kitchenv1alpha1.Addon{}
	for i := range addons.Items {
		byID[addons.Items[i].Name] = &addons.Items[i]
	}

	views := make([]addonView, 0, len(controller.AddonCatalogue()))
	for _, entry := range controller.AddonCatalogue() {
		views = append(views, s.addonViewOf(entry, byID[entry.ID]))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": views})
}

// getAddon answers one entry.
func (s *Server) getAddon(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	name := req.PathValue("name")

	entry, known := controller.LookupAddonCatalogue(name)
	if !known {
		writeJSON(w, http.StatusNotFound, errorBody{Error: fmt.Sprintf(
			"there is no addon named %q: this platform installs %s", name, addonNames())})
		return
	}

	addon := &kitchenv1alpha1.Addon{}
	if err := s.get(ctx, name, addon); err != nil && !apierrors.IsNotFound(err) {
		s.writeError(w, err)
		return
	} else if err != nil {
		addon = nil
	}
	writeJSON(w, http.StatusOK, s.addonViewOf(entry, addon))
}

// createAddon asks the platform for a catalogue entry it has no Addon for —
// one deleted earlier, or one this installation permitted after the seed.
func (s *Server) createAddon(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := addonWriteRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.ID = strings.TrimSpace(body.ID)

	entry, known := controller.LookupAddonCatalogue(body.ID)
	if !known {
		badRequest(w, "there is no addon named %q: this platform installs %s. The catalogue is compiled into "+
			"the operator — an addon cannot name a chart to install, because its install job can apply CRDs "+
			"and ClusterRoles", body.ID, addonNames())
		return
	}
	if err := validAddonNamespace(body.Namespace); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	addon := &kitchenv1alpha1.Addon{
		ObjectMeta: metav1.ObjectMeta{
			Name:      entry.ID,
			Namespace: s.Namespace,
			// The same labels the operator's own seed writes, so an addon
			// somebody re-created here is indistinguishable from one that was
			// never deleted.
			Labels: map[string]string{
				managedByLabelKey:             managedByLabelValue,
				"app.kubernetes.io/component": entry.ID,
			},
		},
		Spec: kitchenv1alpha1.AddonSpec{
			Install:   body.Install == nil || *body.Install,
			Namespace: body.Namespace,
		},
	}
	if !s.recorded(w, req, audit.Transition{
		Object:    addon,
		Kind:      audit.KindAddon,
		Operation: clickhouse.AuditCreate,
		To:        addonRequestState(addon.Spec.Install),
		Reason: fmt.Sprintf("addon %s was created asking the platform to %s it", entry.ID,
			addonVerb(addon.Spec.Install)),
		Details: map[string]any{"install": addon.Spec.Install, "namespace": addon.Spec.Namespace},
	}) {
		return
	}
	if err := s.Client.Create(ctx, addon); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("addon created through the api", "addon", entry.ID, "caller", callerName(caller))
	writeJSON(w, http.StatusCreated, s.addonViewOf(entry, addon))
}

// patchAddon turns an entry's install on or off, or moves where it goes.
func (s *Server) patchAddon(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	name := req.PathValue("name")

	entry, known := controller.LookupAddonCatalogue(name)
	if !known {
		writeJSON(w, http.StatusNotFound, errorBody{Error: fmt.Sprintf(
			"there is no addon named %q: this platform installs %s", name, addonNames())})
		return
	}

	body := addonWriteRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	if err := validAddonNamespace(body.Namespace); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}

	addon := &kitchenv1alpha1.Addon{}
	if err := s.get(ctx, name, addon); err != nil {
		s.writeError(w, err)
		return
	}
	was := addon.Spec.Install

	// Moving an entry that is already installed does not move the release:
	// the platform installed it where it is, and helm would put a second one
	// beside it rather than migrate the first. Saying so beats a namespace
	// field that silently means nothing.
	if body.Namespace != "" && body.Namespace != addon.Spec.Namespace && addon.Status.Managed {
		badRequest(w, "%s is already installed in %s. Uninstall it before moving it: changing this field "+
			"would install a second copy beside the first rather than move it",
			entry.Title, addon.Status.Namespace)
		return
	}

	if body.Install != nil {
		addon.Spec.Install = *body.Install
	}
	if body.Namespace != "" {
		addon.Spec.Namespace = body.Namespace
	}

	if !s.recorded(w, req, audit.Transition{
		Object:    addon,
		Kind:      audit.KindAddon,
		Operation: clickhouse.AuditUpdate,
		From:      addonRequestState(was),
		To:        addonRequestState(addon.Spec.Install),
		Reason: fmt.Sprintf("addon %s now asks the platform to %s it", entry.ID,
			addonVerb(addon.Spec.Install)),
		Details: map[string]any{"install": addon.Spec.Install, "namespace": addon.Spec.Namespace},
	}) {
		return
	}
	if err := s.Client.Update(ctx, addon); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("addon changed through the api", "addon", entry.ID,
		"install", addon.Spec.Install, "caller", callerName(caller))
	writeJSON(w, http.StatusOK, s.addonViewOf(entry, addon))
}

// deleteAddon removes an entry: the record for one the platform did not
// install, and the release itself for one it did.
//
// It answers 202, because the operator finishes it — a release that has to be
// uninstalled and waited for, or a refusal because something still provisions
// through it. Which of those happened is the Addon's own condition, and the
// object stays until it is settled, so the answer is never a delete that
// looks done and is not.
func (s *Server) deleteAddon(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	name := req.PathValue("name")

	addon := &kitchenv1alpha1.Addon{}
	if err := s.get(ctx, name, addon); err != nil {
		s.writeError(w, err)
		return
	}
	entry, known := controller.LookupAddonCatalogue(name)

	blastRadius := "the platform's record of it goes; the release itself is somebody else's and stays"
	if known && addon.Status.Managed {
		blastRadius = entry.BlastRadius
	}

	if !s.recorded(w, req, audit.Transition{
		Object:    addon,
		Kind:      audit.KindAddon,
		Operation: clickhouse.AuditDelete,
		From:      addonRequestState(addon.Spec.Install),
		Reason:    fmt.Sprintf("addon %s was removed: %s", name, blastRadius),
		Details:   map[string]any{"managed": addon.Status.Managed, "namespace": addon.Status.Namespace},
	}) {
		return
	}
	if err := s.Client.Delete(ctx, addon); err != nil {
		s.writeError(w, err)
		return
	}

	caller, _ := CallerFrom(ctx)
	s.log().Info("addon deletion requested through the api", "addon", name, "caller", callerName(caller))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"name":        name,
		"managed":     addon.Status.Managed,
		"blastRadius": blastRadius,
		"message": "the operator is removing it; it refuses while a connection or a claim still provisions " +
			"through it, and says which on the addon",
	})
}

// addonViewOf joins one catalogue entry to the Addon that asked for it, if
// any.
func (s *Server) addonViewOf(entry controller.AddonCatalogueEntry, addon *kitchenv1alpha1.Addon) addonView {
	view := addonView{
		ID:               entry.ID,
		Title:            entry.Title,
		Summary:          entry.Summary,
		Charts:           entry.Charts,
		DefaultNamespace: entry.DefaultNamespace,
		DependsOn:        entry.DependsOn,
		BlastRadius:      entry.BlastRadius,
		Permitted:        s.AddonsPermitted[entry.ID],
		ChartValue:       entry.ChartValue,
		ClusterAdmin:     entry.ClusterAdmin,
		GrantBecause:     entry.GrantBecause,
	}
	if addon == nil {
		return view
	}
	view.Requested = &addon.Spec.Install
	view.Serving = addon.Status.Serving
	view.Managed = addon.Status.Managed
	view.Namespace = addon.Status.Namespace
	view.Installed = addon.Status.Charts
	view.Conditions = conditionViews(addon.Status.Conditions)
	return view
}

// addonNames lists the catalogue for a message.
func addonNames() string {
	names := make([]string, 0, len(controller.AddonCatalogue()))
	for _, entry := range controller.AddonCatalogue() {
		names = append(names, entry.ID)
	}
	return strings.Join(names, ", ")
}

// validAddonNamespace checks the one value a request contributes to a
// cluster-admin job's argv.
func validAddonNamespace(namespace string) error {
	if namespace == "" {
		return nil
	}
	if errs := validation.IsDNS1123Label(namespace); len(errs) > 0 {
		return fmt.Errorf("namespace must work as a DNS label — lowercase letters, digits and '-', starting "+
			"and ending alphanumeric (got %q)", namespace)
	}
	return nil
}

// addonRequestState is what an audit record calls the request, so that
// turning an install on and off reads as a transition rather than a boolean.
func addonRequestState(install bool) string {
	if install {
		return "install"
	}
	return "leave"
}

// addonVerb is the same thing in a sentence.
func addonVerb(install bool) string {
	if install {
		return "install"
	}
	return "leave alone"
}
