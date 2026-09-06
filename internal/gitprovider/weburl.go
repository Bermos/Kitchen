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

package gitprovider

import (
	"fmt"
	"net/url"
	"strings"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// WebLinks is where a Connection's repositories live for a *person*, as
// against the API URL, which is where they live for a program. It answers the
// four addresses that are stable across every provider the platform speaks
// to: a repository, a branch, a commit, and a pull request.
//
// It is here rather than in a client because the host is a fact about the
// Connection and not a constant: two of the three providers are routinely
// self-hosted, so `https://github.com/{repo}/commit/{sha}` is right for one
// provider and wrong for the others. It is the same question `configuredAPIURL`
// answers, asked for the browser instead — which is why it is derived from the
// same field.
//
// Every method answers "" for something it has not been given, so a caller
// with a half-empty revision renders no link rather than a broken one.
type WebLinks interface {
	// Repository is the repository's own page, `owner/name`.
	Repository(repo string) string
	// Commit is one commit's page in a repository.
	Commit(repo, sha string) string
	// Branch is the repository browsed at the tip of one branch.
	Branch(repo, branch string) string
	// PullRequest is one pull request — a merge request on GitLab, which is
	// the same object under a different word and a different path.
	PullRequest(repo string, number int32) string
}

// Web resolves a Connection's web links. It takes no credential: where a
// provider publishes its repositories is not a secret, and asking for a token
// to compose a URL would make a link a privileged read.
//
// A Connection whose provider has no implementation is ErrUnsupportedProvider,
// which callers read as "no link" rather than as a failure.
func Web(conn *kitchenv1alpha1.Connection) (WebLinks, error) {
	switch conn.Spec.Provider {
	case ProviderGitHub:
		apiURL, err := configuredAPIURL(conn, "https://api.github.com")
		if err != nil {
			return nil, err
		}
		base, err := webBase(apiURL, "/api/v3", true)
		if err != nil {
			return nil, err
		}
		return webLinks{base: base, commit: "commit", branch: "tree", pull: "pull"}, nil
	case ProviderGitLab:
		apiURL, err := configuredAPIURL(conn, "https://gitlab.com/api/v4")
		if err != nil {
			return nil, err
		}
		base, err := webBase(apiURL, "/api/v4", false)
		if err != nil {
			return nil, err
		}
		// GitLab puts everything that is not a repository path under `/-/`,
		// so that a branch called `tree` cannot collide with the route that
		// browses one, and calls a pull request a merge request.
		return webLinks{base: base, commit: "-/commit", branch: "-/tree", pull: "-/merge_requests"}, nil
	case ProviderGitea:
		apiURL, err := configuredAPIURL(conn, "https://gitea.com/api/v1")
		if err != nil {
			return nil, err
		}
		base, err := webBase(apiURL, "/api/v1", false)
		if err != nil {
			return nil, err
		}
		// Gitea browses a ref through `src/branch/{name}` and pluralizes its
		// pull requests; the commit path is GitHub's.
		return webLinks{base: base, commit: "commit", branch: "src/branch", pull: "pulls"}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedProvider, conn.Spec.Provider)
	}
}

// webLinks is every provider's web routing, which differs only in three path
// infixes. Writing three implementations of four one-line methods would say
// the same thing three times and hide the part that actually varies.
//
// Every value it is handed is somebody else's text — a branch name is written
// by whoever pushed it — so each goes through escapePath, which escapes one
// path segment at a time and so keeps `release/1.2` two segments, which is
// what every provider's browse route expects.
type webLinks struct {
	base   string
	commit string
	branch string
	pull   string
}

func (w webLinks) Repository(repo string) string {
	if repo == "" {
		return ""
	}
	return w.base + "/" + escapePath(repo)
}

func (w webLinks) Commit(repo, sha string) string {
	if repo == "" || sha == "" {
		return ""
	}
	return w.Repository(repo) + "/" + w.commit + "/" + escapePath(sha)
}

func (w webLinks) Branch(repo, branch string) string {
	if repo == "" || branch == "" {
		return ""
	}
	return w.Repository(repo) + "/" + w.branch + "/" + escapePath(branch)
}

func (w webLinks) PullRequest(repo string, number int32) string {
	if repo == "" || number <= 0 {
		return ""
	}
	return fmt.Sprintf("%s/%s/%d", w.Repository(repo), w.pull, number)
}

// webBase turns an API URL into the address a browser goes to.
//
// The two shapes it has to undo are the two providers use: a path suffix
// (`https://git.example.com/api/v4`, every self-hosted install of all three)
// and a host prefix (`https://api.github.com`, and GitHub Enterprise Cloud's
// `https://api.acme.ghe.com`). The prefix is GitHub's alone, which is why it
// is asked for rather than assumed — a self-hosted GitLab reached at
// `api.example.com` is reached at `api.example.com` in a browser too.
func webBase(apiURL, apiPath string, hostPrefixed bool) (string, error) {
	parsed, err := url.Parse(apiURL)
	if err != nil {
		return "", fmt.Errorf("invalid api url %q: %w", apiURL, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid api url %q: it names no scheme and host", apiURL)
	}
	parsed.RawQuery, parsed.Fragment = "", ""
	parsed.Path = strings.TrimSuffix(strings.TrimSuffix(parsed.Path, "/"), apiPath)
	if hostPrefixed && parsed.Path == "" {
		parsed.Host = strings.TrimPrefix(parsed.Host, "api.")
	}
	return strings.TrimSuffix(parsed.String(), "/"), nil
}
