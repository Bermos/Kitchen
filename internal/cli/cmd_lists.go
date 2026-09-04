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
	// The one write that hangs off a listing, because it is the same noun and
	// `kitchen projects create` is where somebody looks for it.
	cmd.AddCommand(newProjectCreateCommand(r))
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
build stays in the list saying so.

A failed build gets a WHY column: the container that stopped it and how it
exited. The whole failure, including the last lines that container printed, is
on --json and on "kitchen logs --build".`),
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
				// WHY is populated for a failed build and for a running one
				// that is not moving, and it is the column the list exists
				// for on a bad day: without it every failure reads as the
				// same failure, and a build stuck before its first pod reads
				// as a build that is simply taking a while.
				failures := false
				for _, b := range builds {
					if b.why() != "" {
						failures = true
					}
				}
				for _, b := range builds {
					row := []string{b.Name, s.Phase(b.Phase), short(b.Git.SHA), b.Git.Branch, since(b.CreatedAt)}
					if failures {
						row = append(row, b.why())
					}
					rows = append(rows, row)
				}
				headers := []string{"NAME", "PHASE", "COMMIT", "BRANCH", "STARTED"}
				if failures {
					headers = append(headers, "WHY")
				}
				return s.Table(headers, rows)
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

func newAttestationsCommand(r *Runtime) *cobra.Command {
	var workload string

	cmd := &cobra.Command{
		Use:     "attestations <build>",
		Aliases: []string{"evidence"},
		Short:   "The signed evidence attached to a build's artifact",
		Long: strings.TrimSpace(`
Read the evidence attached to what a build produced.

Everything it prints is read out of the registry, keyed to the artifact's
content digest through OCI referrers — not out of a Kitchen table. The same
envelopes answer "cosign download attestation" and "cosign verify-attestation"
with this platform out of the loop, which is the point of storing them there,
and the reason an installation that stops using Kitchen keeps its evidence.

Each attestation says who made the claim it carries, in the CLAIM column. For
an artifact the platform built: the build record is the reconciler's account
of a build it orchestrated, and provenance and the bill of materials come from
the builder itself and are countersigned. For one it did not build:
"vendor-asserted" is what the publisher attached to the digest, restated and
countersigned, and "platform-observed" is what the platform worked out about
an image it only pulled. The signature on all of them is the platform's, so
the signature cannot tell them apart — which is why the column exists.

An artifact the platform did not build is headed by where it came from, who
admitted it onto this installation, and what became of the vendor's own
signature: verified, unverifiable, or "the vendor publishes no signature",
which is the ordinary state of most published images and not a failure.

"verified" means a signature was accepted by a key this platform holds. A set
read where the platform holds no key reports itself as a listing rather than a
verification: a reader that could not tell the two apart would eventually treat
one as the other.

A commit that builds more than one image produces one artifact per workload,
and each carries its own evidence against its own digest. --workload names
which to read; without it you get the project's own image, which is the only
one a single-workload project has. "kitchen builds" and the build itself list
the workloads a commit produced.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			set, err := client.buildAttestations(ctx, args[0], workload)
			if err != nil {
				return err
			}
			return r.printer().document(set, func(s tui.Styles) string {
				preamble := artifactOrigin(set)
				if len(set.Attestations) == 0 {
					return preamble + "Nothing is attached to this artifact. It is real and what runs from it is " +
						"honest about what it is — what it cannot do is satisfy a policy that requires evidence.\n"
				}
				rows := make([][]string, 0, len(set.Attestations))
				for _, found := range set.Attestations {
					checked := "not checked"
					switch {
					case found.Verified:
						checked = "verified"
					case set.Verified:
						checked = "not signed by a key this platform holds"
					}
					claim := set.Sources[found.PredicateType]
					if claim == "" {
						claim = "attached by something else"
					}
					rows = append(rows, []string{
						found.PredicateType, claim, checked, short(found.Digest),
					})
				}
				return preamble +
					s.Table([]string{"PREDICATE", "CLAIM", "SIGNATURE", "ENVELOPE"}, rows)
			})
		}),
	}
	cmd.Flags().StringVar(&workload, "workload", "",
		"which image of the unit to read, e.g. api — the project's own image by default")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/builds/{name}/attestations"},
		Output: output{Mode: outputDocument, Kind: "evidenceSet"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"What is attached to a build's artifact", "kitchen attestations shop-bld-7 --json"},
			{"What is attached to one workload's image",
				"kitchen attestations shop-bld-7 --workload api --json"},
		},
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
			failures := false
			for _, b := range builds {
				if b.why() != "" {
					failures = true
				}
			}
			for _, b := range builds {
				row := []string{b.Name, s.Phase(b.Phase), short(b.Git.SHA), since(b.CreatedAt)}
				if failures {
					row = append(row, b.why())
				}
				rows = append(rows, row)
			}
			headers := []string{"BUILD", "PHASE", "COMMIT", "STARTED"}
			if failures {
				headers = append(headers, "WHY")
			}
			out.WriteString(s.Table(headers, rows))
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

// artifactOrigin heads the evidence table for an artifact the platform did
// not build: where it came from, who admitted it, and what became of the
// vendor's own signature.
//
// It is a preamble rather than four more columns because it is one fact about
// the artifact rather than a fact about each attestation — and because it is
// empty for every artifact the platform built, which is most of them.
func artifactOrigin(set *evidenceSet) string {
	if set == nil || set.Upstream == nil {
		return ""
	}
	lines := []string{"Upstream: " + set.Upstream.Reference}
	if set.Upstream.AdmittedBy != "" {
		admitted := "Admitted by " + set.Upstream.AdmittedBy
		if set.Upstream.AdmittedAt != "" {
			admitted += " on " + set.Upstream.AdmittedAt
		}
		lines = append(lines, admitted)
	}
	switch set.Upstream.Signature {
	case "verified":
		signature := "Upstream signature: verified"
		if set.Upstream.SignatureIdentity != "" {
			signature += " against " + set.Upstream.SignatureIdentity
		}
		lines = append(lines, signature)
	case "none":
		lines = append(lines, "Upstream signature: the vendor publishes none, which is a fact and not a failure")
	case "":
		lines = append(lines, "Upstream signature: not established")
	default:
		signature := "Upstream signature: unverifiable"
		if set.Upstream.SignatureMessage != "" {
			signature += " — " + set.Upstream.SignatureMessage
		}
		lines = append(lines, signature)
	}
	return strings.Join(lines, "\n") + "\n\n"
}
