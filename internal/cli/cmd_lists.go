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

// The reads: what exists, so that the commands that act on something have a
// way of finding out what to act on.
//
// Every one of them is a filtered list rather than a whole one — the API
// answers a cross-project read about the projects the caller can see, and a
// project somebody holds no role on is answered exactly as if it were not
// there. So an empty list means "nothing of yours", which is worth knowing
// when a name that plainly exists cannot be found.

func newProjectsCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "projects",
		Aliases: []string{"project"},
		Short:   "The projects this account can see",
		Long: strings.TrimSpace(`
List the projects this credential holds a role on.

Every project carries the calling account's role on it, which is what decides
what the other commands will be allowed to do: viewer reads, developer deploys
and rolls back, admin changes the project's settings and its people.

An API key is a member of exactly one project, so a key sees one.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			projects, err := client.projects(ctx)
			if err != nil {
				return err
			}
			answer := list[project]{Items: projects}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(projects) == 0 {
					return "No projects.\n"
				}
				rows := make([][]string, 0, len(projects))
				for _, p := range projects {
					rows = append(rows, []string{p.Name, p.Role, p.Repo, p.ProductionBranch, since(p.CreatedAt)})
				}
				return s.Table([]string{"NAME", "ROLE", "REPO", "BRANCH", "CREATED"}, rows)
			})
		}),
	}
	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/projects"},
		Output:   output{Mode: outputDocument, Kind: "projectList"},
		Needs:    needs{Auth: true},
		Examples: []example{{"Every project this credential can see", "kitchen projects --json"}},
	})
}

func newBuildsCommand(r *Runtime) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:     "builds",
		Aliases: []string{"build"},
		Short:   "The project's builds, newest first",
		Long: strings.TrimSpace(`
List the linked project's builds, newest first.

A build is the history of who asked for what: it is never mutated, so a rebuild
of the same commit is another build rather than a changed one, and a cancelled
build stays in the list saying so.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			name, err := r.projectName()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			builds, err := client.projectBuilds(ctx, name)
			if err != nil {
				return err
			}
			if limit > 0 && len(builds) > limit {
				builds = builds[:limit]
			}
			answer := list[build]{Items: builds}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(builds) == 0 {
					return "No builds yet.\n"
				}
				rows := make([][]string, 0, len(builds))
				for _, b := range builds {
					rows = append(rows, []string{
						b.Name, s.Phase(b.Phase), short(b.Git.SHA), b.Git.Branch, since(b.CreatedAt),
					})
				}
				return s.Table([]string{"NAME", "PHASE", "COMMIT", "BRANCH", "STARTED"}, rows)
			})
		}),
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "how many to show, newest first. 0 shows all of them")

	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/projects/{name}/builds"},
		Output:   output{Mode: outputDocument, Kind: "buildList"},
		Needs:    needs{Auth: true, Project: true},
		Examples: []example{{"The last five builds", "kitchen builds --limit 5 --json"}},
	})
}

func newReleasesCommand(r *Runtime) *cobra.Command {
	var limit int

	cmd := &cobra.Command{
		Use:     "releases",
		Aliases: []string{"release"},
		Short:   "The project's releases, newest first",
		Long: strings.TrimSpace(`
List the linked project's releases, newest first.

A release is an immutable snapshot of an image digest and the configuration it
runs with. It is what "kitchen rollback" moves an environment to, and what
makes doing so put back exactly what was running.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			name, err := r.projectName()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			releases, err := client.projectReleases(ctx, name)
			if err != nil {
				return err
			}
			if limit > 0 && len(releases) > limit {
				releases = releases[:limit]
			}
			answer := list[release]{Items: releases}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(releases) == 0 {
					return "No releases yet.\n"
				}
				rows := make([][]string, 0, len(releases))
				for _, rel := range releases {
					rows = append(rows, []string{
						rel.Name, rel.Build, strings.Join(rel.Environments, ","), since(rel.CreatedAt),
					})
				}
				return s.Table([]string{"NAME", "BUILD", "ON", "CREATED"}, rows)
			})
		}),
	}
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "how many to show, newest first. 0 shows all of them")

	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/projects/{name}/releases"},
		Output:   output{Mode: outputDocument, Kind: "releaseList"},
		Needs:    needs{Auth: true, Project: true},
		Examples: []example{{"What there is to roll back to", "kitchen releases --json"}},
	})
}

func newEnvironmentsCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "environments",
		Aliases: []string{"environment", "envs"},
		Short:   "The project's environments and where they answer",
		Long: strings.TrimSpace(`
List the linked project's environments: production, and a preview for each open
pull request the platform built.

Each one carries the release it is meant to be on, the release it is observed
to be on, its phase and its URL.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			name, err := r.projectName()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			environments, err := client.projectEnvironments(ctx, name)
			if err != nil {
				return err
			}
			answer := list[environment]{Items: environments}
			return r.printer().document(answer, func(s tui.Styles) string {
				return renderEnvironments(s, environments)
			})
		}),
	}
	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/projects/{name}/environments"},
		Output:   output{Mode: outputDocument, Kind: "environmentList"},
		Needs:    needs{Auth: true, Project: true},
		Examples: []example{{"Where everything is running", "kitchen environments --json"}},
	})
}

// projectStatus is `kitchen status`: the one screen a person wants when they
// arrive — what the project is, what is running, and what has been built.
type projectStatus struct {
	Project      project       `json:"project"`
	Environments []environment `json:"environments"`
	Builds       []build       `json:"builds"`
}

func newStatusCommand(r *Runtime) *cobra.Command {
	var builds int

	cmd := &cobra.Command{
		Use:   "status",
		Short: "The linked project: what is running, and what has been built",
		Long: strings.TrimSpace(`
One answer for "where is this project": its settings, its environments with
their phases and URLs, and its most recent builds.

It is three reads joined, so a caller that wants one of the three can ask for
it directly with "kitchen environments" or "kitchen builds".`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			return status(commandContext(cmd), r, builds)
		}),
	}
	cmd.Flags().IntVar(&builds, "builds", 5, "how many recent builds to include")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/projects/{name}",
			"GET /api/v1/projects/{name}/environments",
			"GET /api/v1/projects/{name}/builds",
		},
		Output:   output{Mode: outputDocument, Kind: "projectStatus"},
		Needs:    needs{Auth: true, Project: true},
		Examples: []example{{"Where the linked project is", "kitchen status --json"}},
	})
}

func status(parent context.Context, r *Runtime, recentBuilds int) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	name, err := r.projectName()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	found, err := client.project(ctx, name)
	if err != nil {
		return err
	}
	environments, err := client.projectEnvironments(ctx, name)
	if err != nil {
		return err
	}
	builds, err := client.projectBuilds(ctx, name)
	if err != nil {
		return err
	}
	if recentBuilds > 0 && len(builds) > recentBuilds {
		builds = builds[:recentBuilds]
	}

	answer := projectStatus{Project: *found, Environments: environments, Builds: builds}
	return r.printer().document(answer, func(s tui.Styles) string {
		out := &strings.Builder{}
		fmt.Fprintf(out, "%s %s\n%s\n\n",
			s.Title.Render(found.Name), s.Subtle.Render("("+found.Role+")"),
			s.Subtle.Render(found.Repo+" · "+found.ProductionBranch))
		out.WriteString(renderEnvironments(s, environments))
		if len(builds) > 0 {
			out.WriteString("\n")
			rows := make([][]string, 0, len(builds))
			for _, b := range builds {
				rows = append(rows, []string{b.Name, s.Phase(b.Phase), short(b.Git.SHA), since(b.CreatedAt)})
			}
			out.WriteString(s.Table([]string{"BUILD", "PHASE", "COMMIT", "STARTED"}, rows))
		}
		return out.String()
	})
}

func renderEnvironments(s tui.Styles, environments []environment) string {
	if len(environments) == 0 {
		return "No environments yet.\n"
	}
	rows := make([][]string, 0, len(environments))
	for _, e := range environments {
		rows = append(rows, []string{e.Name, e.Type, s.Phase(e.Phase), e.Release, s.Accent.Render(e.URL)})
	}
	return s.Table([]string{"NAME", "TYPE", "PHASE", "RELEASE", "URL"}, rows)
}
