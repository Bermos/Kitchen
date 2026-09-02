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
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/detect"
	"github.com/Bermos/Kitchen/internal/framework"
	"github.com/Bermos/Kitchen/internal/gitprovider"
	"github.com/Bermos/Kitchen/internal/provider"
)

// detectTimeout bounds one preflight — the default branch when the caller
// named no ref, and the two the detection itself makes. It is shorter than
// the repository listing's because that is a walk and this is not, and
// somebody is waiting on a form they have half filled in.
const detectTimeout = 15 * time.Second

// detectRequest asks what a repository is before there is a project to ask
// about. Every field is the value the form currently holds, which is what
// makes the answer worth showing: changing the root directory and asking
// again is the whole of "or fix the build context".
type detectRequest struct {
	// Repo is the repository, owner/name.
	Repo string `json:"repo"`

	// Ref is the branch, tag or commit to look at. Empty means the
	// repository's default branch — which the caller usually knows, since
	// the repository picker hands it over, but need not.
	Ref string `json:"ref,omitempty"`

	// RootDirectory and DockerfilePath are the build context as the form
	// currently has it.
	RootDirectory  string `json:"rootDirectory,omitempty"`
	DockerfilePath string `json:"dockerfilePath,omitempty"`
}

// detectionView is what the repository looks like to the platform.
//
// Detected is false for a repository the platform read and did not recognise,
// which is not an error: it is the answer, and it is the answer the form
// exists to deliver early. Message says why in words a form can show, whether
// or not anything was detected.
type detectionView struct {
	Detected  bool   `json:"detected"`
	Framework string `json:"framework,omitempty"`
	Strategy  string `json:"strategy,omitempty"`
	Port      int32  `json:"port,omitempty"`

	// Ref is what was actually read, so a form that sent none can show which
	// branch the answer is about.
	Ref string `json:"ref,omitempty"`

	// RootDirectory is the directory the answer is about, normalised the way
	// a build would normalise it.
	RootDirectory string `json:"rootDirectory,omitempty"`

	// Dockerfile says the project's Dockerfile is where the request said it
	// would be — the one thing a person can be wrong about in a way that
	// silently changes the strategy.
	Dockerfile bool `json:"dockerfile"`

	// Stages are the named stages that Dockerfile declares, in file order,
	// so the stage to ship can be chosen from what the file has rather than
	// typed and found out about later. Absent for a repository with no
	// Dockerfile where the project says one is, and for one whose stages are
	// all unnamed — which is the ordinary single-stage file, and is why an
	// empty list is not an answer about a target being wrong.
	Stages []string `json:"stages,omitempty"`

	// Files are the names at the build root, so somebody who disagrees with
	// the verdict can see what it was reached from.
	Files []string `json:"files,omitempty"`

	// Unreadable is the repository itself not having been read: it is not
	// there, or the connection's credential cannot see it. It is the one
	// "detected": false that correcting the build context will not change,
	// and a form that headed it "no framework detected" would be sending
	// somebody to edit a field that is already right.
	Unreadable bool `json:"unreadable,omitempty"`

	Message string `json:"message,omitempty"`
}

// detectRepository is the preflight the new project form runs: read the
// repository as the build would read it, and say what the platform thinks it
// is while the build context is still editable.
//
// It exists because the alternative is finding out from a failed build, which
// is several minutes later and reads like the platform is broken rather than
// like a root directory being one level off. It answers 200 for every case
// the caller can act on — a repository nobody recognises included — and
// reserves failures for the platform genuinely being unable to look.
//
// It reads no credential back: the token is used to ask the provider a
// question and never leaves the operator, and nothing here writes anything.
func (s *Server) detectRepository(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	body := detectRequest{}
	if err := decodeBody(req, &body); err != nil {
		badRequest(w, "%s", err.Error())
		return
	}
	body.Repo = strings.TrimSpace(body.Repo)
	if body.Repo == "" {
		badRequest(w, "repo is required: name the repository as owner/name")
		return
	}

	connection := &kitchenv1alpha1.Connection{}
	if err := s.get(ctx, req.PathValue("name"), connection); err != nil {
		s.writeError(w, err)
		return
	}

	git, unsupported, err := s.gitProviderFor(ctx, connection)
	if unsupported != "" {
		writeJSON(w, http.StatusOK, detectionView{Message: unsupported})
		return
	}
	if err != nil {
		s.writeProviderError(w, connection, err)
		return
	}
	reader, ok := gitprovider.Source(git)
	if !ok {
		writeJSON(w, http.StatusOK, detectionView{Message: fmt.Sprintf(
			"the platform's %s support cannot read a repository, so the layout is worked out at build time",
			connection.Spec.Provider)})
		return
	}

	detectCtx, cancel := context.WithTimeout(ctx, detectTimeout)
	defer cancel()

	ref := strings.TrimSpace(body.Ref)
	if ref == "" {
		// The default branch is the one thing the caller may not know, and
		// the provider does. It is resolved as a branch name rather than as a
		// commit because that is what is echoed back, and a caller that sent
		// no ref has to be able to show which branch the answer is about.
		resolver, ok := gitprovider.DefaultBranches(git)
		if !ok {
			badRequest(w, "ref is required: the platform's %s support cannot work out a "+
				"repository's default branch, so name the branch to look at", connection.Spec.Provider)
			return
		}
		branch, err := resolver.DefaultBranch(detectCtx, body.Repo)
		switch {
		case errors.Is(err, gitprovider.ErrFileNotFound):
			// The repository endpoint answered 404, which is the one place
			// that answer is unambiguous: it is the repository that cannot be
			// read, not a path inside one. The caller's to fix — a name they
			// mistyped, or a token that was never granted it — so it is an
			// answer rather than a failure.
			writeJSON(w, http.StatusOK, detectionView{
				Unreadable: true,
				Message:    detect.UnreadableRepositoryMessage(connection.Name, body.Repo),
			})
			return
		case err != nil:
			writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
				"connection %q could not read %s: %s", connection.Name, body.Repo, err.Error())})
			return
		case branch == "":
			badRequest(w, "ref is required: %s has no default branch to fall back on, "+
				"so name the branch to look at", body.Repo)
			return
		}
		ref = branch
	}

	target := detect.Target{
		Repo:               body.Repo,
		Ref:                ref,
		RootDirectory:      normalizeRootDirectory(body.RootDirectory),
		DockerfilePath:     normalizeDockerfilePath(body.DockerfilePath),
		ConsiderDockerfile: true,
	}

	signals, err := detect.Signals(detectCtx, reader, target)
	if errors.Is(err, detect.ErrRepositoryUnreadable) {
		// Not a verdict about a directory at all: the repository is not
		// there, or this connection may not see it. Which of the two is not
		// knowable — a provider that said so would be a way to enumerate
		// private repositories — and both are the caller's to act on, so it
		// is an answer rather than a failure, and it names the connection
		// because an installation may have several.
		writeJSON(w, http.StatusOK, detectionView{
			Ref: ref, RootDirectory: target.RootDirectory, Unreadable: true,
			Message: detect.UnreadableRepositoryMessage(connection.Name, body.Repo),
		})
		return
	}
	if errors.Is(err, detect.ErrNotRecognised) {
		// The root directory is not there. That is the commonest thing this
		// preflight exists to catch, and it is the caller's to fix.
		writeJSON(w, http.StatusOK, detectionView{
			Ref: ref, RootDirectory: target.RootDirectory, Message: err.Error(),
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
			"connection %q could not read %s: %s", connection.Name, body.Repo, err.Error())})
		return
	}

	view := detectionView{
		Ref:           ref,
		RootDirectory: target.RootDirectory,
		Dockerfile:    signals.Dockerfile,
		Files:         signals.Files,
	}
	if signals.Dockerfile {
		// One more read, and only when there is a file to read: what stages
		// it declares is the difference between choosing the one to ship and
		// guessing at it. A file that cannot be read now is not an error —
		// the rest of the answer stands, and the field is simply absent.
		if stages, err := detect.DockerfileStages(detectCtx, reader, target); err == nil {
			view.Stages = stages
		}
	}
	if found, ok := framework.Detect(signals); ok {
		view.Detected = true
		view.Framework = found.Name
		view.Strategy = string(found.Strategy)
		view.Port = found.Port
	} else {
		view.Message = "the platform did not recognise this directory: " +
			"add a Dockerfile, correct the root directory, or set the project's build strategy yourself"
	}
	writeJSON(w, http.StatusOK, view)
}

// normalizeRootDirectory spells the build root the way a build spells it, so
// that the preflight is answering about the directory the build would read
// rather than about a near miss. One implementation, in internal/detect,
// because the build spelling it differently is the whole failure this exists
// to avoid.
func normalizeRootDirectory(root string) string {
	return detect.NormalizeRoot(root)
}

// normalizeDockerfilePath spells a project's Dockerfile the way a build
// spells it — cleaned, and relative to the build root.
//
// An unset field stays unset rather than becoming "Dockerfile": the CRD's own
// default says that, and writing it out here would turn "the project has not
// said" into a setting somebody has to notice they can clear.
func normalizeDockerfilePath(dockerfile string) string {
	if strings.TrimSpace(dockerfile) == "" {
		return ""
	}
	return detect.NormalizeDockerfile(dockerfile)
}

// checkBuildPath refuses a path that reaches outside the directory it is
// relative to — an absolute one, or one that climbs out with "..".
//
// The root directory is the build root, and everything a project declares is
// relative to it: BuildKit is handed that directory as its whole context and
// the buildpacks lifecycle is pointed at it, so there is nothing above it for
// a path to be resolved against. A repository's own kitchen.json has always
// been refused one — this is the same rule, at the other place the same paths
// are written, and a refusal on the form beats a build that cannot say why it
// failed.
func checkBuildPath(field, value, within string) error {
	if detect.LeavesRoot(value) {
		return fmt.Errorf("%s must stay inside %s (got %q)", field, within, value)
	}
	return nil
}

// normalizeDockerfileTarget spells a project's Dockerfile target the way a
// build spells it. Case is left alone: the frontend matches a stage name
// without regard to it, and lowercasing here would report back something
// other than what was written.
func normalizeDockerfileTarget(target string) string {
	return detect.NormalizeTarget(target)
}

// checkDockerfileTarget refuses a target that no Dockerfile stage could be
// called.
//
// Which stages a file actually has is not knowable here — the repository is
// not read on a write, and the file changes with every commit — so what is
// checked is the shape of the name. A name the dockerfile frontend cannot
// hold is one that could never match a stage, and refusing it on the form
// beats a build that fails several minutes later with a builder's sentence
// about an option nobody typed. The preflight is where the actual stages come
// from, and it lists them.
func checkDockerfileTarget(target string) error {
	if detect.ValidTarget(target) {
		return nil
	}
	return fmt.Errorf("dockerfileTarget must name a stage of the Dockerfile — %s (got %q)",
		detect.StageNameRule, target)
}

// The two things a project's build paths are relative to, named once so the
// refusal reads the same wherever it is written.
const (
	withinRepository = "the repository"
	withinBuildRoot  = "the project's root directory, which is the whole of what a build sees"
)

// gitProviderFor resolves the git provider behind a connection, using the
// credential the operator holds and never handing it back.
//
// The middle return is the reason there is nothing to ask, in words a form
// can show: a provider the platform cannot speak is not a failure, it is a
// field that has to be typed into instead. An error is the platform being
// unable to look.
func (s *Server) gitProviderFor(
	ctx context.Context,
	connection *kitchenv1alpha1.Connection,
) (gitprovider.Provider, string, error) {
	providerName := connection.Spec.Provider

	// Whether there is anything to ask is a fact about the provider, so it is
	// settled before the credential is read: matched on the capability the
	// platform would use the connection for, never on the provider's name —
	// which is also what keeps a registry connection from being asked for a
	// token it does not have.
	if !slices.Contains(provider.Capabilities(providerName), kitchenv1alpha1.CapabilityGitSource) {
		return nil, fmt.Sprintf(
			"connection %q is a %s connection, which is not a source of repositories",
			connection.Name, providerName), nil
	}

	creds := &corev1.Secret{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: connection.Spec.CredentialsSecretRef.Name}
	if err := s.Client.Get(ctx, key, creds); err != nil {
		return nil, "", err
	}
	if len(creds.Data[gitTokenKey]) == 0 {
		return nil, "", fmt.Errorf(
			"the credential stored for connection %q has no %q key, so the provider cannot be asked anything",
			connection.Name, gitTokenKey)
	}

	factory := s.GitProviders
	if factory == nil {
		factory = gitprovider.Default
	}
	git, err := factory(connection, string(creds.Data[gitTokenKey]))
	if errors.Is(err, gitprovider.ErrUnsupportedProvider) {
		return nil, fmt.Sprintf("the platform has no %s implementation yet", providerName), nil
	}
	if err != nil {
		return nil, "", err
	}
	return git, "", nil
}

// writeProviderError renders a failure to reach the provider. It is a bad
// gateway rather than a 500 for the same reason the repository listing's is:
// the platform is working, and the thing it asked did not answer.
func (s *Server) writeProviderError(w http.ResponseWriter, connection *kitchenv1alpha1.Connection, err error) {
	if apierrors.IsNotFound(err) {
		s.writeError(w, err)
		return
	}
	writeJSON(w, http.StatusBadGateway, errorBody{Error: fmt.Sprintf(
		"connection %q could not be used: %s", connection.Name, err.Error())})
}
