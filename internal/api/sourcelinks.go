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

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/gitprovider"
)

// sourceLinks is where one project's code lives on the web, ready to be
// asked for a commit, a branch, the repository or a pull request (#435).
//
// The API composes these rather than the clients because the host is a fact
// about the project's Connection — a GitLab or Gitea connection can name a
// forge anybody self-hosted — and three clients deriving a URL scheme per
// provider is three chances to derive it differently. `internal/gitprovider`
// holds the derivation, beside the API URL it is the same question as.
//
// The zero value answers "" to everything, which is what a project with no
// repository, a connection that has been deleted and a provider the platform
// cannot address all get: no link, rendered as the plain text it always was.
type sourceLinks struct {
	repo  string
	links gitprovider.WebLinks
}

func (l sourceLinks) repositoryURL() string {
	if l.links == nil {
		return ""
	}
	return l.links.Repository(l.repo)
}

// commitURL is one commit's page. A pull request from a fork has its head
// commit in the fork and nowhere else (#422), so that is the repository the
// commit and its branch are addressed in — linking a fork's SHA in the base
// repository is a 404 with a plausible-looking URL.
func (l sourceLinks) commitURL(revision kitchenv1alpha1.GitRevision) string {
	if l.links == nil {
		return ""
	}
	return l.links.Commit(l.commitRepo(revision), revision.SHA)
}

func (l sourceLinks) branchURL(revision kitchenv1alpha1.GitRevision) string {
	if l.links == nil {
		return ""
	}
	return l.links.Branch(l.commitRepo(revision), revision.Branch)
}

// pullRequestURL is the request itself, which lives in the project's own
// repository however far the branch came from.
func (l sourceLinks) pullRequestURL(number *int32) string {
	if l.links == nil || number == nil {
		return ""
	}
	return l.links.PullRequest(l.repo, *number)
}

// commitRepo is where this revision's commit actually is: the fork it came
// from, or the project's own. A fork the platform was told about without a
// name is the project's own repository as far as a link is concerned — there
// is nothing else to address it with, and no link is better than a wrong one.
func (l sourceLinks) commitRepo(revision kitchenv1alpha1.GitRevision) string {
	if revision.ForkRepo != "" && revision.ForkRepo != kitchenv1alpha1.UnknownForkRepo {
		return revision.ForkRepo
	}
	return l.repo
}

// sourceLinker resolves sourceLinks for the objects one response carries, and
// remembers what it read.
//
// A listing of builds is a listing of one or a handful of projects, and every
// row of it wants the same repository and the same connection: without the
// memory this would be two reads a row. Nothing here fails — a project or a
// connection that cannot be read is a response without links, not a response
// with an error, because a link is a convenience on an answer that was
// already correct without it.
type sourceLinker struct {
	server   *Server
	projects map[string]sourceLinks
}

func (s *Server) sourceLinker() *sourceLinker {
	return &sourceLinker{server: s, projects: map[string]sourceLinks{}}
}

// forProject is the links for a project already in hand.
func (l *sourceLinker) forProject(ctx context.Context, project *kitchenv1alpha1.Project) sourceLinks {
	if cached, ok := l.projects[project.Name]; ok {
		return cached
	}
	resolved := l.resolve(ctx, project)
	l.projects[project.Name] = resolved
	return resolved
}

// forProjectNamed is the links for a project named by something that
// references it — a Build, a Release, an Environment.
func (l *sourceLinker) forProjectNamed(ctx context.Context, name string) sourceLinks {
	if cached, ok := l.projects[name]; ok {
		return cached
	}
	project := &kitchenv1alpha1.Project{}
	if err := l.server.get(ctx, name, project); err != nil {
		l.projects[name] = sourceLinks{}
		return sourceLinks{}
	}
	return l.forProject(ctx, project)
}

func (l *sourceLinker) resolve(ctx context.Context, project *kitchenv1alpha1.Project) sourceLinks {
	source := project.Spec.Source.GitSource()
	if source.Repo == "" || source.ConnectionRef.Name == "" {
		return sourceLinks{}
	}
	connection := &kitchenv1alpha1.Connection{}
	if err := l.server.get(ctx, source.ConnectionRef.Name, connection); err != nil {
		return sourceLinks{repo: source.Repo}
	}
	links, err := gitprovider.Web(connection)
	if err != nil {
		// A provider with no web routing of its own is a repository name and
		// no link, which is what every screen showed before this existed.
		return sourceLinks{repo: source.Repo}
	}
	return sourceLinks{repo: source.Repo, links: links}
}
