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
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen files` — the configuration files a project places into its
// workloads (#311).
//
// **This is a command rather than `kitchen api`**, and the decision is
// recorded in docs/CLI.md's table. The content of a config file is a *file*,
// which is the one kind of value a terminal is better at than a form: it is
// already on disk, it is too long to type, and `--content-file` is the whole
// interaction. Leaving it to `kitchen api` would mean hand-assembling a JSON
// document with a file's bytes escaped into it, which nobody does twice.
//
// Three things about the API shape it, and all three are the same ones
// `kitchen env` is built on:
//
//   - **The write replaces the whole list**, and a file whose `content` the
//     request leaves out keeps the content it has. So `files set` reads the
//     project, changes one entry and sends every other back by name — with no
//     content at all, which is what lets it edit a list containing a secret
//     file it was never shown.
//   - **A secret file's content never comes back out.** `files list` prints
//     the whole list and no content for a secret file: a digest and a size,
//     which is everything the platform will answer.
//   - **A secret file's content has its own route.** Declaring the file and
//     writing its content are two requests, made in that order, because the
//     content route refuses a file that has not been declared.

func newFilesCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "files",
		Aliases: []string{"file"},
		Short:   "Read and change the configuration files a project places",
		Long: strings.TrimSpace(`
The configuration files this project's workloads are handed.

Software written for this platform is configured by environment variables.
Software somebody else wrote is often configured by a file at a fixed path —
Home Assistant's configuration.yaml, Gitea's app.ini — and this is where one
goes:

  kitchen files set configuration --path /config/configuration.yaml \
    --content-file ./configuration.yaml

A file may hold a credential, and then its content goes in and never comes back
out — not to this command, not to the dashboard, not to anyone:

  kitchen files set app-ini --path /data/gitea/conf/app.ini --secret \
    --content-file ./app.ini

Files are frozen into every release, so a rollback restores the file that
release ran with. Changing one lands in the next release, like a variable —
except a secret file's content, which reaches what is already running, because
the platform restarts whatever reads it.`),
	}
	cmd.AddCommand(newFilesListCommand(r), newFilesSetCommand(r), newFilesRemoveCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{},
		Examples: []example{{"What the project places", "kitchen files list --json"}},
	})
}

func newFilesListCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "What files the project places, and where each is mounted",
		Long: strings.TrimSpace(`
List the project's configuration files.

Each row is a file: its name, the path it is mounted at, the workloads that
read it — empty meaning all of them — and what the platform holds. A plain
file's content is answered in full by --json; a secret file's is answered as a
digest and a size and never as itself.`),
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

			answer, err := client.project(ctx, name)
			if err != nil {
				return err
			}
			return printFiles(r, answer.Files)
		}),
	}
	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/projects/{name}"},
		Output:   output{Mode: outputDocument, Kind: "fileList"},
		Needs:    needs{Auth: true, Project: true},
		Examples: []example{{"What the project places", "kitchen files list --json"}},
	})
}

func newFilesSetCommand(r *Runtime) *cobra.Command {
	var (
		path      string
		workloads []string
		secret    bool
		source    contentSource
	)

	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Add a configuration file, or change one that is already there",
		Long: strings.TrimSpace(`
Declare one configuration file, and give it its content.

The same command adds a file and changes one, because it is the same write. Any
part left out keeps what it had: a file that only needs new content keeps its
path and its workloads, and one that only needs to move keeps its content.

  kitchen files set configuration --path /config/configuration.yaml \
    --content-file ./configuration.yaml
  kitchen files set configuration --content-file ./configuration.yaml
  kitchen files set configuration --workloads web,worker

--secret says the content is a credential. It is then written through a route
of its own and no request ever answers it again — which also means there is
nothing to read back before replacing it, and nothing this command can print.

--workloads names the workloads that mount the file, "web" being the web
process. Left out, every workload of the project gets it.

A plain file's content lands in the next release, like an environment
variable. A secret file's reaches what is already running: the platform
restarts whatever reads it.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return setFile(commandContext(cmd), r, args[0], fileChange{
				path:         path,
				workloads:    workloads,
				secret:       secret,
				secretGiven:  cmd.Flags().Changed("secret"),
				workloadsSet: cmd.Flags().Changed("workloads"),
				content:      source,
				contentGiven: source.given(),
			})
		}),
	}

	flags := cmd.Flags()
	flags.StringVar(&path, "path", "", "where the file is mounted in the container, like /config/app.yaml")
	flags.StringSliceVar(&workloads, "workloads", nil,
		"the workloads that read it — \"web\" and the project's own; empty is all of them")
	flags.BoolVar(&secret, "secret", false, "the content is a credential: it goes in and never comes back out")
	flags.StringVar(&source.value, "content", "", "the content, on the command line — and so in the shell's history")
	flags.StringVar(&source.file, "content-file", "", "read the content from a file")
	flags.BoolVar(&source.stdin, "content-stdin", false, "read the content from stdin")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/projects/{name}",
			"PATCH /api/v1/projects/{name}",
			"PUT /api/v1/projects/{name}/files/{file}",
		},
		Output: output{Mode: outputDocument, Kind: "fileList", Note: "what the project places afterwards"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{
				"Place a file every workload reads",
				"kitchen files set configuration --path /config/configuration.yaml " +
					"--content-file ./configuration.yaml --json",
			},
			{
				"Place one that holds a credential",
				"kitchen files set app-ini --path /data/conf/app.ini --secret --content-file ./app.ini --json",
			},
		},
	})
}

func newFilesRemoveCommand(r *Runtime) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:     "rm NAME",
		Aliases: []string{"remove", "delete"},
		Short:   "Take a configuration file off the project",
		Long: strings.TrimSpace(`
Remove one of the project's configuration files.

The declaration goes and the content goes with it. For a secret file that is
final: there is no way to read one back first, so a file removed by mistake has
to be found again wherever it came from.

What is already running keeps the file its release was built with. The removal
lands in the next release, which is when the workloads that read it stop being
handed it.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return removeFile(commandContext(cmd), r, args[0], yes)
		}),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask first")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/projects/{name}",
			"PATCH /api/v1/projects/{name}",
		},
		Output: output{Mode: outputDocument, Kind: "fileList", Note: "what the project places afterwards"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"Remove one, without being asked", "kitchen files rm configuration --yes --json"},
		},
	})
}

// fileChange is what one `files set` was asked to change. Each half carries
// whether it was given at all, because "left out" and "cleared" are different
// requests: `--workloads ""` takes a file back to every workload, and leaving
// the flag off keeps the list it had.
type fileChange struct {
	path         string
	workloads    []string
	workloadsSet bool
	secret       bool
	secretGiven  bool
	content      contentSource
	contentGiven bool
}

// contentSource is the three ways a file's content arrives.
//
// It is deliberately not the secret value's reader next door, and the
// difference is one character: that one trims the trailing newline every
// editor adds, because a credential does not have one. A configuration file
// does — the platform places it byte for byte, and a YAML document silently
// losing its last newline is a file the platform changed on its way in.
//
// There is no prompt either. A config file is not something anybody types at
// one, so a missing flag is a failure naming the three that answer it rather
// than a wait.
type contentSource struct {
	value string
	file  string
	stdin bool
}

// given reports whether any of the three was used.
func (s contentSource) given() bool { return s.value != "" || s.file != "" || s.stdin }

// read resolves the content, exactly as written.
func (s contentSource) read(r *Runtime, name string) (string, error) {
	switch {
	case s.file != "":
		body, err := os.ReadFile(s.file)
		if err != nil {
			return "", fail(codeUsage, "reading "+s.file+": "+err.Error())
		}
		return string(body), nil
	case s.stdin:
		body, err := io.ReadAll(io.LimitReader(r.Stdin, kitchenv1alpha1.ConfigFileContentLimit+1))
		if err != nil {
			return "", fail(codeUsage, "reading the content of "+name+" from stdin: "+err.Error())
		}
		return string(body), nil
	default:
		return s.value, nil
	}
}

// setFile declares one file and, where content was given, writes it.
//
// The project is read, the one entry is added or edited, and the whole list
// goes back with every other file exactly as it came — by name, with no
// content, which is what lets a list holding a secret file be edited by a
// client that has never seen it.
func setFile(parent context.Context, r *Runtime, name string, change fileChange) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	project, err := r.projectName()
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return fail(codeUsage, "a file with no name")
	}

	ctx, cancel := r.context(parent)
	defer cancel()

	current, err := client.project(ctx, project)
	if err != nil {
		return err
	}
	existing := fileNamed(current.Files, name)

	entry := fileWrite{Name: name}
	switch {
	case change.path != "":
		entry.Path = strings.TrimSpace(change.path)
	case existing != nil:
		entry.Path = existing.Path
	default:
		return failf(codeUsage, "the file %s is new, so it needs a path", name).
			withHint("pass --path, naming where in the container the file is mounted — like /config/app.yaml")
	}
	switch {
	case change.workloadsSet:
		entry.Workloads = change.workloads
	case existing != nil:
		entry.Workloads = existing.Workloads
	}
	switch {
	case change.secretGiven:
		entry.Secret = change.secret
	case existing != nil:
		entry.Secret = existing.Secret
	}

	// The content, read before anything is written: a --content-file that is
	// not there should not leave the declaration half changed.
	content := ""
	if change.contentGiven {
		if content, err = change.content.read(r, name); err != nil {
			return err
		}
	}
	switch {
	case entry.Secret:
		// It travels on its own route, after the declaration lands. Nothing
		// about it goes into this list.
	case change.contentGiven:
		entry.Content = &content
	case existing == nil:
		return failf(codeUsage, "the file %s is new, so it needs content", name).
			withHint("pass --content-file, --content-stdin or --content — or --secret, " +
				"which stores the content where nothing reads it back")
	}
	if entry.Secret && !change.contentGiven && (existing == nil || existing.ContentHash == "") {
		return failf(codeUsage, "the secret file %s has no content", name).
			withHint("pass --content-file, --content-stdin or --content — a workload mounting a file the " +
				"platform holds nothing for does not start")
	}

	files := make([]fileWrite, 0, len(current.Files)+1)
	replaced := false
	for _, file := range current.Files {
		if file.Name == name {
			files, replaced = append(files, entry), true
			continue
		}
		// Every other file by name, with no content: the API keeps what it
		// holds for a file whose content a request leaves out, which is the
		// whole of how this edits a list it cannot read.
		files = append(files, fileWrite{
			Name:      file.Name,
			Path:      file.Path,
			Secret:    file.Secret,
			Workloads: file.Workloads,
		})
	}
	if !replaced {
		files = append(files, entry)
	}

	written, err := client.setFiles(ctx, project, files)
	if err != nil {
		return err
	}
	if entry.Secret && change.contentGiven {
		if _, err := client.setProjectFile(ctx, project, name, content); err != nil {
			return err
		}
		// Read back so the printed list carries the digest of what was just
		// written rather than of what was there before it.
		if written, err = client.project(ctx, project); err != nil {
			return err
		}
	}
	return printFiles(r, written.Files)
}

func removeFile(parent context.Context, r *Runtime, name string, yes bool) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	project, err := r.projectName()
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)

	ctx, cancel := r.context(parent)
	defer cancel()

	current, err := client.project(ctx, project)
	if err != nil {
		return err
	}
	existing := fileNamed(current.Files, name)
	if existing == nil {
		return failf(codeNotFound, "%s places no file %s", project, name)
	}

	question := fmt.Sprintf("Remove %s from %s?", name, project)
	if existing.Secret {
		question += " Its content cannot be read back first."
	}
	if err := confirm(r, question, yes); err != nil {
		return err
	}

	files := make([]fileWrite, 0, len(current.Files))
	for _, file := range current.Files {
		if file.Name == name {
			continue
		}
		files = append(files, fileWrite{
			Name:      file.Name,
			Path:      file.Path,
			Secret:    file.Secret,
			Workloads: file.Workloads,
		})
	}
	written, err := client.setFiles(ctx, project, files)
	if err != nil {
		return err
	}
	return printFiles(r, written.Files)
}

// fileNamed is one of a project's files, or nil.
func fileNamed(files []configFile, name string) *configFile {
	for i := range files {
		if files[i].Name == name {
			return &files[i]
		}
	}
	return nil
}

func printFiles(r *Runtime, files []configFile) error {
	answer := list[configFile]{Items: files}
	if answer.Items == nil {
		answer.Items = []configFile{}
	}
	return r.printer().document(answer, func(s tui.Styles) string { return renderFiles(s, answer.Items) })
}

// renderFiles draws what there is. A plain file's content is a byte count
// rather than the file — a terminal is not where a YAML document is read —
// and a secret file's is the digest, which is all there is.
func renderFiles(s tui.Styles, files []configFile) string {
	if len(files) == 0 {
		return "No files. Software configured by one at a fixed path goes here.\n"
	}
	rows := make([][]string, 0, len(files))
	for _, file := range files {
		workloads := "all"
		if len(file.Workloads) > 0 {
			workloads = strings.Join(file.Workloads, ", ")
		}
		rows = append(rows, []string{
			file.Name,
			file.Path,
			s.Subtle.Render(workloads),
			fileContentSummary(s, file),
		})
	}
	return s.Table([]string{"NAME", "PATH", "WORKLOADS", "CONTENT"}, rows)
}

// fileContentSummary is what can be said about one file's content.
func fileContentSummary(s tui.Styles, file configFile) string {
	switch {
	case !file.Secret && file.Content != nil:
		return s.Subtle.Render(strconv.Itoa(len(*file.Content)) + " bytes")
	case !file.Secret:
		return s.Subtle.Render("—")
	case file.ContentHash == "":
		// The state that stops a workload starting, so it is named rather
		// than left as a blank cell.
		return s.Warn.Render("secret, not set")
	default:
		return s.Subtle.Render("secret, " + file.ContentHash)
	}
}
