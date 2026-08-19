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

package previewgate

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/access"
)

// Directory is where the gate looks a visitor up: the two places membership
// is written down, spec.access on a Project and spec.access.operators on the
// Kitchen singleton.
//
// It is an interface with one production implementation because what matters
// about it is what it is *not*: it is not the REST API. The gate resolves
// membership against its own cache, so a preview does not close while the API
// is restarting, and so the membership rule has exactly one implementation —
// internal/access, which both halves of the platform call.
type Directory interface {
	// Kitchen is the platform singleton: the operator list, and the base
	// domain generated hostnames are built from.
	Kitchen(ctx context.Context) (*kitchenv1alpha1.Kitchen, error)
	// Project is one Project by name. A name nothing answers to must come
	// back as an apierrors NotFound, which is a route the gate refuses
	// rather than a platform it cannot read.
	Project(ctx context.Context, name string) (*kitchenv1alpha1.Project, error)
}

// CachedDirectory reads the platform's objects through a controller-runtime
// cache — an informer, not a call per request. The gate is in the request
// path of every protected preview, so an admission decision has to cost a map
// lookup.
type CachedDirectory struct {
	// Reader is a cached client. It must already be synced: a reader that
	// answers "not found" because its informer has not caught up would admit
	// nobody and blame the project.
	Reader client.Reader
	// Namespace is where Kitchen's custom resources live. Every one of them
	// is in the platform namespace, so this is the only namespace the gate
	// watches.
	Namespace string
}

// Kitchen returns the platform singleton.
//
// It is found by listing rather than by name, so the gate does not carry a
// second spelling of a constant the operator owns. Kitchen owns the cluster
// it is installed into and there is exactly one; if an installation somehow
// has more, the first by name is used, so the answer is at least stable.
func (d CachedDirectory) Kitchen(ctx context.Context) (*kitchenv1alpha1.Kitchen, error) {
	list := &kitchenv1alpha1.KitchenList{}
	if err := d.Reader.List(ctx, list); err != nil {
		return nil, err
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("there is no Kitchen object, so the platform's operators are unknown")
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return &list.Items[0], nil
}

// Project returns one Project out of the platform namespace.
func (d CachedDirectory) Project(ctx context.Context, name string) (*kitchenv1alpha1.Project, error) {
	project := &kitchenv1alpha1.Project{}
	key := client.ObjectKey{Namespace: d.Namespace, Name: name}
	if err := d.Reader.Get(ctx, key, project); err != nil {
		return nil, err
	}
	return project, nil
}

// AppNamespace is the namespace a Project's workloads run in.
//
// It lives here, next to the headers, rather than only in the reconciler that
// creates it, because the derivation is what makes the project header
// checkable: an upstream address the gate is told to forward to names the
// namespace it is in, and only one project's namespace is named after the
// project the header claims. The Environment reconciler calls this rather
// than spelling the prefix a second time, so the two cannot drift.
func AppNamespace(project string) string {
	return "kitchen-" + project
}

// projectName is what a Project may be called: a Kubernetes object name. The
// header is checked against it before anything is looked up, so a header full
// of nonsense is a refusal rather than a malformed request to the API server
// — which would come back as an error and be read as "the platform is
// unreadable", closing every preview on the platform.
var projectName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// refusal is a page the gate answers with instead of the application. Each
// one says what is wrong and what would fix it, which is the rule the chart's
// guards already follow.
type refusal struct {
	status  int
	title   string
	message string
}

// cannotCheck is the fail-closed answer: the gate could not read the platform,
// so it does not know who is on this project and will not guess.
//
// Guessing in the other direction is the failure mode this exists to avoid. A
// gate that admitted everyone whenever its cache was unavailable would publish
// every unreleased preview on the platform at exactly the moment nobody is
// watching.
var cannotCheck = &refusal{
	status: http.StatusServiceUnavailable,
	title:  "Access cannot be checked",
	message: "The platform cannot check who is on this project at the moment, " +
		"so this preview stays closed. Try again shortly.",
}

// notRouted is what a request whose project header the gate cannot believe
// gets. It is deliberately the platform's problem rather than the visitor's:
// a route built without the header, a request that did not come through the
// Gateway, and a forged header are the same page from the outside, and the
// gate should not tell them apart out loud.
var notRouted = &refusal{
	status: http.StatusBadGateway,
	title:  "Not available",
	message: "This preview's route does not say which project it belongs to, " +
		"so the platform cannot tell who may open it.",
}

// admit decides whether a signed-in visitor may reach the application, and
// returns the page to answer with instead when they may not.
//
// The rule is docs/AUTH.md's: any role on the project is enough — viewer
// included, since that is the role the preview link gets pasted to — and an
// operator holds admin on every project, present and future. Both come out of
// access.ProjectRoleFor, which is the only implementation of either.
func (s *Server) admit(r *http.Request, upstream *url.URL, visitor claims) *refusal {
	if s.directory == nil {
		// The gate was built without a way to read the platform. Nothing
		// downstream can recover from that, and the safe direction is closed.
		s.log.Info("the gate has no directory, so it can admit nobody", "host", r.Host)
		return cannotCheck
	}
	ctx := r.Context()

	name := strings.TrimSpace(r.Header.Get(ProjectHeader))
	if !projectName.MatchString(name) {
		s.log.Info("the route names no project", "host", r.Host, "project", name)
		return notRouted
	}

	kitchen, err := s.directory.Kitchen(ctx)
	if err != nil {
		s.log.Error(err, "could not read the platform", "host", r.Host)
		return cannotCheck
	}
	project, err := s.directory.Project(ctx, name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// A route naming a project the platform does not have. Either it
			// outlived its project, or the header did not come from a route.
			s.log.Error(err, "the route names no project of this platform",
				"host", r.Host, "project", name)
			return notRouted
		}
		s.log.Error(err, "could not read the project", "host", r.Host, "project", name)
		return cannotCheck
	}

	if !routedFor(name, upstream, r.Host, kitchen) {
		s.log.Info("the route does not belong to the project it names",
			"host", r.Host, "project", name, "upstream", upstream.Host)
		return notRouted
	}

	caller := access.Caller{
		Subject:       visitor.Subject,
		Email:         visitor.Email,
		EmailVerified: visitor.EmailVerified,
	}
	if access.ProjectRoleFor(caller, kitchen, project).AtLeast(access.ProjectViewer) {
		return nil
	}

	s.log.Info("a signed-in visitor is not on this project",
		"host", r.Host, "project", name, "subject", visitor.Subject)
	return notAMember(name, visitor.Email)
}

// notAMember is the page a signed-in visitor who is not on the project sees.
//
// It is a page and not a redirect on purpose. Sending them back to the
// identity provider would sign them in again — they already are — and land
// them on this same wall, which from the outside is a browser that hangs in a
// loop for reasons nobody can see. So the gate says what happened and what
// would change it.
func notAMember(project, email string) *refusal {
	signedIn := "You are signed in"
	if email != "" {
		signedIn += " as " + email
	}
	return &refusal{
		status: http.StatusForbidden,
		title:  "Not on this project",
		message: fmt.Sprintf(
			"%s, but you are not on the project %q, so this preview is not yours to open. "+
				"Ask an admin of %s to add you — any role is enough, viewer included. "+
				"If it is another account of yours that is on it, sign out at %s and open this URL again.",
			signedIn, project, project, SignOutPath),
	}
}

// routedFor reports whether the request in front of the gate really is the
// named project's.
//
// The Gateway sets both headers with a RequestHeaderModifier filter, which
// overwrites whatever the client sent, so neither can be forged from outside
// the cluster. They are checked anyway, for the same reason parseUpstream
// checks the upstream is an in-cluster Service: a header that decides who may
// see unreleased work should not be believed merely because it arrived.
//
// Two things the platform derives are enough to check it against, and a
// request has to satisfy at least one:
//
//   - The address it will be forwarded to. An Environment's Service lives in
//     its project's application namespace, whose name is derived from the
//     project's — so an upstream in kitchen-shop belongs to shop and to
//     nothing else. This is the exact check, and it is the one that holds for
//     an ordinary protected preview.
//   - The hostname it arrived on. The platform generates <project>-pr-<n>
//     under the base domain, so a preview's own URL names its project too.
//     This is what covers an *idling* environment, where the upstream is the
//     KEDA interceptor rather than the application (see CLAUDE.md, "Routing
//     an idling environment") and therefore names no project at all.
//
// Its limits, stated rather than hidden. It cannot vouch for an idling
// environment reached through a *custom* domain: the upstream is the shared
// interceptor and the hostname is one somebody chose, so neither derivation
// applies and the gate refuses. Refusing is the safe direction — the
// alternative is believing an unverifiable header — but it is a refusal of a
// legitimate request, and closing it needs the gate to be able to read
// Domains, which is more than the read-only identity it is given today. And
// none of this defends against something already inside the cluster, which is
// the platform's stated trust boundary: cluster access is operator access.
func routedFor(project string, upstream *url.URL, host string, kitchen *kitchenv1alpha1.Kitchen) bool {
	if upstreamNamespace(upstream) == AppNamespace(project) {
		return true
	}
	return isGeneratedHost(project, host, kitchen.Spec.BaseDomain)
}

// upstreamNamespace is the namespace of the Service an upstream address
// names. parseUpstream has already established the shape.
func upstreamNamespace(upstream *url.URL) string {
	labels := strings.Split(upstream.Hostname(), ".")
	if len(labels) < 3 {
		return ""
	}
	return labels[1]
}

// isGeneratedHost reports whether host is one the platform generates for this
// project: <project>.<baseDomain> for production, <project>-pr-<n> for a
// preview. It mirrors the reconciler's hostname().
func isGeneratedHost(project, host, baseDomain string) bool {
	if baseDomain == "" {
		return false
	}
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	slug, found := strings.CutSuffix(strings.ToLower(host), "."+strings.ToLower(baseDomain))
	if !found {
		return false
	}
	if slug == project {
		return true
	}
	number, found := strings.CutPrefix(slug, project+"-pr-")
	if !found || number == "" {
		return false
	}
	return strings.IndexFunc(number, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
