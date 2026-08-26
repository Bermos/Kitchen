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
	"path"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// Creating a project from a terminal.
//
// This is the one command that runs *before* there is anything to run it
// against: no project, no link, and — in the ordinary case — a working copy of
// the repository it is about. That is what decides its shape. The repository
// defaults from the checkout's `origin`, the name defaults from the
// repository, and the layout is checked before the project is written rather
// than after its first build has failed.
//
// The preflight is the same POST /connections/{name}/detect the dashboard's
// new-project dialog runs, and it is advice: a repository the platform has no
// framework for is still one a project can be created from, so a bare verdict
// asks rather than refuses — and, since nothing here blocks on a prompt, --yes
// answers it.

// projectCreated is what `kitchen projects create` answers with: the project,
// what the preflight made of the repository, and where the link was written.
type projectCreated struct {
	Project project `json:"project"`
	// Detection is what the platform made of the repository, or absent when
	// the preflight could not be run.
	Detection *detection `json:"detection,omitempty"`
	// Path is the .kitchen/project.json that was written, empty when --no-link
	// was given or there was no working copy to write it in.
	Path string `json:"path,omitempty"`
}

func newProjectCreateCommand(r *Runtime) *cobra.Command {
	var (
		repo       string
		connection string
		registry   string
		branch     string
		previews   bool
		root       string
		dockerfile string
		link       bool
		yes        bool
	)

	cmd := &cobra.Command{
		Use:   "create [NAME]",
		Short: "Create a project from this repository",
		Long: strings.TrimSpace(`
Create a project, check what the platform makes of the repository first, and
link this directory to it.

The name defaults to the repository's own; the repository defaults to this
checkout's origin remote. Both can be given instead, which is what makes the
command runnable from a directory that is no checkout at all.

Before the project is written, the repository is read through the same
preflight the dashboard runs: which framework was recognised, whether there is
a Dockerfile, and which port it will be given. A repository nothing was
recognised in is not refused — it is a question, and --yes answers it.

Creating a project starts a build of its production branch straight away, so
--root-directory and --dockerfile are sent with it rather than set afterwards:
a monorepo corrected by a later change is corrected one failed build too late.

It is the one command a CI key cannot run. A project's creator becomes its
admin and an admin issues keys, so creating one is a person's; a key signed in
with KITCHEN_API_KEY is refused, and says so.`),
		Args: cobra.MaximumNArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			options := createOptions{
				name:       first(args),
				repo:       repo,
				connection: connection,
				registry:   registry,
				branch:     branch,
				root:       root,
				dockerfile: dockerfile,
				link:       link,
				yes:        yes,
			}
			if cmd.Flags().Changed("previews") {
				options.previews = &previews
			}
			return createProject(commandContext(cmd), r, options)
		}),
	}

	cmd.Flags().StringVar(&repo, "repo", "", "the repository, as owner/name (default: this checkout's origin)")
	cmd.Flags().StringVar(&connection, "connection", "", "the git connection to read it through")
	cmd.Flags().StringVar(&registry, "registry", "", "the connection to push images to")
	cmd.Flags().StringVar(&branch, "production-branch", "", "the branch production deploys from")
	cmd.Flags().BoolVar(&previews, "previews", false, "deploy a preview for every pull request")
	cmd.Flags().StringVar(&root, "root-directory", "", "the directory within the repository to build")
	cmd.Flags().StringVar(&dockerfile, "dockerfile", "", "a Dockerfile to build with, relative to the root directory")
	cmd.Flags().BoolVar(&link, "link", true, "write .kitchen/project.json for the new project")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "answer every question this would otherwise ask")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/connections",
			"POST /api/v1/connections/{name}/detect",
			"POST /api/v1/projects",
		},
		Output: output{Mode: outputDocument, Kind: "projectCreated",
			Note: "creating a project starts a build of its production branch"},
		Needs: needs{Auth: true, Git: true},
		Examples: []example{
			{"Create a project from this checkout, no terminal needed",
				"kitchen projects create shop --connection github --registry kitchen --yes --json"},
			{"Create one for a repository this machine has no copy of",
				"kitchen projects create shop --repo acme/shop --connection github --registry kitchen --yes --json"},
			{"Create one for an application inside a monorepo",
				"kitchen projects create shop --repo acme/mono --root-directory apps/shop " +
					"--connection github --registry kitchen --yes --json"},
		},
	})
}

// createOptions is what the flags and the argument said, before anything has
// been worked out from the checkout or asked of the platform.
type createOptions struct {
	name       string
	repo       string
	connection string
	registry   string
	branch     string
	previews   *bool
	root       string
	dockerfile string
	link       bool
	yes        bool
}

func createProject(parent context.Context, r *Runtime, options createOptions) error {
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

	if options.repo == "" {
		options.repo = originRepo(ctx, r.WorkingDir)
	}
	if options.repo == "" {
		return fail(codeUsage, "no repository given").
			withHint("pass --repo owner/name — this directory has no git origin to take one from")
	}
	if options.name == "" {
		options.name = path.Base(options.repo)
	}

	if err := chooseConnections(ctx, r, client, &options); err != nil {
		return err
	}

	// The preflight, and the reason the command exists rather than being a
	// `kitchen api POST /projects`. A failure to run it is reported and
	// carried on from: it is advice about the repository, and the platform
	// being unable to give any is not a reason to refuse to create a project.
	verdict, err := client.detect(ctx, options.connection, detectTarget{
		Repo:           options.repo,
		Ref:            options.branch,
		RootDirectory:  options.root,
		DockerfilePath: options.dockerfile,
	})
	if err != nil {
		verdict = nil
		r.printer().warn("could not read the repository first: %v", err)
	} else if err := confirmLayout(r, verdict, options); err != nil {
		return err
	}

	// The branch the preflight settled on is the branch the project deploys
	// from. Without this a create that named none takes the platform's own
	// default of "main", which for a repository whose trunk is called
	// anything else is a first build looking for a branch that is not there.
	if options.branch == "" && verdict != nil && verdict.Ref != "" {
		options.branch = verdict.Ref
	}

	created, err := client.createProject(ctx, newProject{
		Name:             options.name,
		Repo:             options.repo,
		Connection:       options.connection,
		Registry:         options.registry,
		ProductionBranch: options.branch,
		Previews:         options.previews,
		RootDirectory:    options.root,
		DockerfilePath:   options.dockerfile,
	})
	if err != nil {
		return err
	}

	answer := projectCreated{Project: *created, Detection: verdict}
	// The link is written last and its failure is not the command's: the
	// project exists by now, and reporting the create as a failure would send
	// somebody looking for a project that is already there.
	if options.link {
		if written, err := writeLink(repositoryRoot(r.WorkingDir), &link{Project: created.Name, API: base}); err != nil {
			r.printer().warn("created the project but could not link this directory: %v", err)
		} else {
			answer.Path = written
		}
	}

	return r.printer().document(answer, func(s tui.Styles) string {
		out := &strings.Builder{}
		fmt.Fprintf(out, "%s %s\n", s.OK.Render("Created"), s.Title.Render(created.Name))
		fmt.Fprintf(out, "%s\n", s.Subtle.Render(created.Repo+"  "+created.ProductionBranch))
		if verdict != nil {
			fmt.Fprintf(out, "%s\n", s.Subtle.Render(describeDetection(verdict)))
		}
		if answer.Path != "" {
			fmt.Fprintf(out, "%s\n", s.Subtle.Render("wrote "+answer.Path))
		}
		fmt.Fprintf(out, "%s\n", s.Accent.Render("building "+created.ProductionBranch+
			" — kitchen builds watch"))
		return out.String()
	})
}

// chooseConnections resolves the two connections a project names. Each is the
// flag, or the only one the platform offers that can do the job, or a picker
// — and, with nothing to pick with, a failure naming the flag and the choices.
func chooseConnections(ctx context.Context, r *Runtime, c *client, options *createOptions) error {
	if options.connection != "" && options.registry != "" {
		return nil
	}
	available, err := c.connections(ctx)
	if err != nil {
		return err
	}
	if options.connection == "" {
		options.connection, err = pickConnection(r, available, capabilityGitSource,
			"--connection", "Which connection is the repository on?")
		if err != nil {
			return err
		}
	}
	if options.registry == "" {
		options.registry, err = pickConnection(r, available, capabilityImageStore,
			"--registry", "Where should the images go?")
		if err != nil {
			return err
		}
	}
	return nil
}

// pickConnection narrows the connections to the ones that can do a thing and
// resolves which. One candidate is not a question — a platform with a single
// registry has nothing to ask about — and none is a failure that says so
// rather than one about a missing flag.
func pickConnection(r *Runtime, available []connection, capability, flag, question string) (string, error) {
	candidates := make([]connection, 0, len(available))
	for _, c := range available {
		if c.can(capability) {
			candidates = append(candidates, c)
		}
	}
	switch {
	case len(candidates) == 0:
		return "", fail(codeNotFound, "no connection offers "+capability).
			withHint("create one in the dashboard, under Settings — a project needs a " +
				"repository to build from and a registry to push to")
	case len(candidates) == 1:
		return candidates[0].Name, nil
	}

	names := make([]string, 0, len(candidates))
	for _, c := range candidates {
		names = append(names, c.Name)
	}
	if !r.interactive() {
		return "", fail(codeUsage, "no "+strings.TrimPrefix(flag, "--")+" given").
			withHint("pass " + flag + ", one of: " + strings.Join(names, ", "))
	}

	choices := make([]tui.Choice, 0, len(candidates))
	for _, c := range candidates {
		detail := c.Provider
		if !c.Ready {
			detail = strings.TrimSpace(detail + " (not ready)")
		}
		choices = append(choices, tui.Choice{Name: c.Name, Detail: detail})
	}
	chosen, err := tui.Pick(r.Stdin, r.Stderr, question, choices)
	if err != nil {
		return "", err
	}
	if chosen == "" {
		return "", fail(codeFailed, "cancelled")
	}
	return chosen, nil
}

// confirmLayout asks about a repository the platform recognised nothing in.
//
// A Dockerfile is an answer in itself — the build has something to run — so
// only a repository with neither a framework nor a Dockerfile is a question,
// and it is a question rather than a refusal because the detector reads a
// commit and the person is reading the whole repository.
func confirmLayout(r *Runtime, verdict *detection, options createOptions) error {
	if verdict.Detected || verdict.Dockerfile {
		return nil
	}
	where := options.repo
	if options.root != "" {
		where += "/" + options.root
	}
	message := "nothing recognisable in " + where
	if verdict.Message != "" {
		message = verdict.Message
	}
	return confirm(r, message+". Create the project anyway?", options.yes)
}

// describeDetection is the verdict as one line.
//
// The port is left out when the platform implies none, which is every
// Dockerfile build: an image decides its own port, and "on port 0" reads as a
// port rather than as the absence of one.
func describeDetection(verdict *detection) string {
	switch {
	case verdict.Framework != "" && verdict.Port > 0:
		return fmt.Sprintf("detected %s, built with %s on port %d",
			verdict.Framework, verdict.Strategy, verdict.Port)
	case verdict.Framework != "":
		return fmt.Sprintf("detected %s, built with %s", verdict.Framework, verdict.Strategy)
	case verdict.Dockerfile:
		return "no framework recognised, building the Dockerfile"
	default:
		return "no framework recognised"
	}
}

// first is the argument, when there is one.
func first(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return strings.TrimSpace(args[0])
}
