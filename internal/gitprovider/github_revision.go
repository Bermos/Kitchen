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
	"context"
	"fmt"
	"net/http"
	"net/url"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// githubCommit is the part of the commits API's answer a Build needs.
type githubCommit struct {
	SHA    string `json:"sha"`
	Commit struct {
		Message string `json:"message"`
		Author  struct {
			Name string `json:"name"`
		} `json:"author"`
	} `json:"commit"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
}

// HeadRevision implements RevisionResolver against the commits API, which
// resolves a branch name as readily as a sha.
func (g *GitHub) HeadRevision(ctx context.Context, repo, ref string) (Revision, error) {
	commit := githubCommit{}
	path := "/repos/" + repo + "/commits/" + url.PathEscape(ref)
	if err := g.do(ctx, http.MethodGet, path, nil, &commit); err != nil {
		if isNotFound(err) {
			// An empty repository answers 404 here too, which is the same
			// answer for the caller's purposes: there is no commit to build.
			return Revision{}, fmt.Errorf("%w: %s at %s", ErrFileNotFound, ref, repo)
		}
		return Revision{}, err
	}
	if commit.SHA == "" {
		return Revision{}, fmt.Errorf("%w: %s at %s", ErrFileNotFound, ref, repo)
	}

	// The account is the attribution when the provider has one; the commit's
	// own author name is what is left for a commit written by somebody with
	// no account on the provider.
	author := commit.Commit.Author.Name
	if commit.Author != nil && commit.Author.Login != "" {
		author = commit.Author.Login
	}
	return Revision{
		SHA:     commit.SHA,
		Branch:  ref,
		Message: kitchenv1alpha1.CommitSubject(commit.Commit.Message),
		Author:  author,
	}, nil
}
