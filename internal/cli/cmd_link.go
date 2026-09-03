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

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// linked is what `kitchen link` answers with: what this directory is now
// about, and where the fact was written.
type linked struct {
	Project string `json:"project"`
	API     string `json:"api"`
	// Role is the calling account's role on it, so that a link is not
	// reported as a success for a project this credential cannot deploy.
	Role string `json:"role,omitempty"`
	Repo string `json:"repo,omitempty"`
	// Path is the file that was written.
	Path string `json:"path"`
}

func newLinkCommand(r *Runtime) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "link",
		Short: "Associate this working directory with a project",
		Long: strings.TrimSpace(`
Record which project this working directory deploys to, so the other commands
need no flags.

The name goes in .kitchen/project.json at the root of the working copy,
alongside the installation's URL. It holds no credential — everybody working on
the repository deploys the same project, so committing it is a reasonable thing
to do.

With no --project and a terminal to draw on, it offers the projects this
account can see. Without one, --project is required: nothing here will block
waiting for an answer it was not given.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			return linkDirectory(commandContext(cmd), r, yes)
		}),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "overwrite an existing link without asking")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/projects", "GET /api/v1/projects/{name}"},
		Output: output{Mode: outputDocument, Kind: "linked"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"Link this directory to a project, no terminal needed",
				"kitchen link --project shop --json"},
			{"Link a checkout on another installation",
				"kitchen link --api https://kitchen.example.com --project shop --json"},
		},
	})
}

func linkDirectory(parent context.Context, r *Runtime, yes bool) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	base, err := r.apiURL()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	name, err := chooseProject(ctx, r, client)
	if err != nil {
		return err
	}

	// Read the project before writing anything. A link to a name that is not
	// there — or that this account holds no role on, which the API answers
	// identically and on purpose — is a link every later command fails on,
	// and the failure would arrive somewhere far less obvious than here.
	found, err := client.project(ctx, name)
	if err != nil {
		return err
	}

	root, _, err := linkTarget(r, found.Name, yes)
	if err != nil {
		return err
	}

	path, err := writeLink(root, &link{Project: found.Name, API: base})
	if err != nil {
		return err
	}

	answer := linked{Project: found.Name, API: base, Role: found.Role, Repo: found.Repo, Path: path}
	return r.printer().document(answer, func(s tui.Styles) string {
		return fmt.Sprintf("%s %s (%s) on %s\n%s\n",
			s.OK.Render("Linked to"), s.Title.Render(found.Name), found.Role, s.Accent.Render(base),
			s.Subtle.Render("wrote "+path))
	})
}

// linkTarget decides where a link naming this project belongs, and asks first
// when the directory already deploys a different one.
//
// It is shared by `kitchen link` and `kitchen projects create`, because a link
// written without a word is the same surprise whichever command wrote it: the
// next `kitchen builds` is quietly about another project, which reads as the
// platform having lost this one. The question is the same sentence in both,
// and --yes is the answer to it in both.
//
// It answers the directory to write in and the project that was linked there
// before — empty for a directory that was not linked, and for one already
// linked to this same project, since neither replaces anything.
func linkTarget(r *Runtime, name string, yes bool) (root, replaced string, err error) {
	existing, dir, err := findLink(r.WorkingDir)
	if err != nil {
		return "", "", err
	}
	if existing == nil || existing.Project == name {
		return repositoryRoot(r.WorkingDir), "", nil
	}
	if err := confirm(r, fmt.Sprintf("%s is linked to %s. Link it to %s instead?",
		dir, existing.Project, name), yes); err != nil {
		return "", "", err
	}
	// The link that is being replaced is rewritten where it already is, which
	// for a link found in a parent directory is not this one.
	return dir, existing.Project, nil
}

// chooseProject resolves which project to link to: the flag or the
// environment, and otherwise the picker — which only runs when there is a
// terminal to run it in.
func chooseProject(ctx context.Context, r *Runtime, c *client) (string, error) {
	if name := strings.TrimSpace(r.projectFlag); name != "" {
		return name, nil
	}
	if name := r.env("KITCHEN_PROJECT"); name != "" {
		return name, nil
	}

	projects, err := c.projects(ctx)
	if err != nil {
		return "", err
	}
	if len(projects) == 0 {
		return "", fail(codeNotFound, "this account can see no projects").
			withHint("create one in the dashboard, or ask an admin of the project for a role on it")
	}

	if !r.interactive() {
		names := make([]string, 0, len(projects))
		for _, p := range projects {
			names = append(names, p.Name)
		}
		return "", fail(codeUsage, "no project given").
			withHint("pass --project, one of: " + strings.Join(names, ", "))
	}

	choices := make([]tui.Choice, 0, len(projects))
	for _, p := range projects {
		choices = append(choices, tui.Choice{
			Name:   p.Name,
			Detail: strings.TrimSpace(p.Repo + "  " + p.Role),
		})
	}
	chosen, err := tui.Pick(r.Stdin, r.Stderr, "Which project does this directory deploy to?", choices)
	if err != nil {
		return "", err
	}
	if chosen == "" {
		return "", fail(codeFailed, "cancelled")
	}
	return chosen, nil
}
