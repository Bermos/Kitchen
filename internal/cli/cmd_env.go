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
	"bufio"
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen env` — a project's environment variables.
//
// Two things about the API shape this whole command, and both are deliberate
// (docs/API.md, "Changing a project's environment variables"):
//
//   - **A value goes in and never comes back out.** Reading a project reports
//     whether a variable has one, not what it is. So `kitchen env list` can
//     print the whole list and reveal nothing, and there is no `env pull` that
//     writes a .env file — there is nothing to pull.
//   - **The write replaces the whole list**, and a variable whose `value` the
//     request leaves out keeps the one it already has. That is what makes a
//     one-variable change possible without reading any values: the CLI sends
//     every variable back by name, and a value only for the ones it is
//     changing.
//
// Which means `env set` and `env rm` are the same request with a different
// list, and neither of them can leak or lose a value it never saw.

func newEnvCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Read and change a project's environment variables",
		Long: strings.TrimSpace(`
A project's environment variables: what is set, and setting or removing one.

Values are write-only on this platform — the API reports whether a variable has
one, never what it is — so this command can list every variable without showing
a secret, and there is nothing to read back into a .env file.

Variables land in the next release's snapshot. What is already running keeps
the configuration it was released with until the next deploy.`),
	}
	cmd.AddCommand(newEnvListCommand(r), newEnvSetCommand(r), newEnvRemoveCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{},
		Examples: []example{{"What is set", "kitchen env list --json"}},
	})
}

func newEnvListCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "What variables the project has, and whether each has a value",
		Long: strings.TrimSpace(`
List the project's environment variables.

Each one says whether it has a value (set), whether it has a different one in
previews (previewSet), or which Secret or resource claim it reads instead. The
values themselves are never answered by the API.`),
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

			found, err := client.project(ctx, name)
			if err != nil {
				return err
			}
			answer := list[envVar]{Items: found.Env}
			if answer.Items == nil {
				answer.Items = []envVar{}
			}
			return r.printer().document(answer, func(s tui.Styles) string { return renderEnv(s, answer.Items) })
		}),
	}
	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/projects/{name}"},
		Output:   output{Mode: outputDocument, Kind: "envVarList"},
		Needs:    needs{Auth: true, Project: true},
		Examples: []example{{"What is set", "kitchen env list --json"}},
	})
}

func newEnvSetCommand(r *Runtime) *cobra.Command {
	var (
		previews   []string
		fromSecret []string
		fromClaim  []string
		fromFile   string
	)

	cmd := &cobra.Command{
		Use:   "set [NAME=VALUE ...]",
		Short: "Set variables, keeping every other one as it is",
		Long: strings.TrimSpace(`
Set one or more environment variables on the project.

Every other variable keeps the value it has: the API's write replaces the whole
list, and this command sends the rest back by name alone, which it can do
because a variable whose value is left out keeps the one it already has. So
nothing here has to read a value in order to change a different variable.

  kitchen env set DATABASE_POOL=10 LOG_LEVEL=debug
  kitchen env set API_URL=https://api.example.com --preview API_URL=https://api.invalid
  kitchen env set API_KEY --from-secret shop-api-key:key
  kitchen env set DATABASE_URL --from-claim shop-db:url
  kitchen env set --from-file .env

A name with no value clears it: "kitchen env set LOG_LEVEL=" leaves the
variable in place with nothing in it.`),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return setEnv(commandContext(cmd), r, envWrites{
				assignments: args, previews: previews,
				secrets: fromSecret, claims: fromClaim, file: fromFile,
			})
		}),
	}

	flags := cmd.Flags()
	flags.StringArrayVar(&previews, "preview", nil,
		"NAME=VALUE for the value previews get instead, repeatable")
	flags.StringArrayVar(&fromSecret, "from-secret", nil,
		"NAME=secret:key — point a variable at a Secret in the project's namespace, repeatable")
	flags.StringArrayVar(&fromClaim, "from-claim", nil,
		"NAME=claim:key — point a variable at a resource claim's binding, repeatable")
	flags.StringVar(&fromFile, "from-file", "",
		"read NAME=VALUE lines from a file, - for stdin. Blank lines and # comments are skipped")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/projects/{name}", "PATCH /api/v1/projects/{name}/env"},
		Output: output{Mode: outputDocument, Kind: "envVarList", Note: "the project's whole variable list after the write"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"Set two variables", "kitchen env set LOG_LEVEL=debug DATABASE_POOL=10 --json"},
			{"Set one, with a different value in previews",
				"kitchen env set API_URL=https://api.example.com --preview API_URL=https://api.invalid --json"},
			{"Read a whole file of them", "kitchen env set --from-file .env --json"},
			{"Point one at a Secret", "kitchen env set API_KEY --from-secret shop-api-key:key --json"},
		},
	})
}

func newEnvRemoveCommand(r *Runtime) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:     "rm NAME [NAME ...]",
		Aliases: []string{"remove", "unset"},
		Short:   "Take variables off the project",
		Long: strings.TrimSpace(`
Remove environment variables from the project.

Removing one drops whatever it held, and there is no way to read it back first
— the API never answers a value. A name the project does not have is a failure
rather than a silent success, so a typo cannot read as a removal.`),
		Args: cobra.MinimumNArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return removeEnv(commandContext(cmd), r, args, yes)
		}),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask first")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/projects/{name}", "PATCH /api/v1/projects/{name}/env"},
		Output: output{Mode: outputDocument, Kind: "envVarList"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"Remove one, without being asked", "kitchen env rm LOG_LEVEL --yes --json"},
		},
	})
}

// envWrites is what `env set` was asked to change.
type envWrites struct {
	assignments []string
	previews    []string
	secrets     []string
	claims      []string
	file        string
}

func setEnv(parent context.Context, r *Runtime, writes envWrites) error {
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

	// changes is what this command is changing, by variable name. Everything
	// else on the project is sent back by name alone.
	changes := map[string]*envVarWrite{}
	touch := func(variable string) (*envVarWrite, error) {
		if variable == "" {
			return nil, fail(codeUsage, "a variable with no name")
		}
		if existing, ok := changes[variable]; ok {
			return existing, nil
		}
		created := &envVarWrite{Name: variable}
		changes[variable] = created
		return created, nil
	}

	if writes.file != "" {
		fromFile, err := readAssignments(r, writes.file)
		if err != nil {
			return err
		}
		writes.assignments = append(fromFile, writes.assignments...)
	}

	for _, assignment := range writes.assignments {
		variable, value, hasValue := strings.Cut(assignment, "=")
		variable = strings.TrimSpace(variable)
		target, err := touch(variable)
		if err != nil {
			return err
		}
		if hasValue {
			target.Value = &value
		}
	}
	for _, assignment := range writes.previews {
		variable, value, hasValue := strings.Cut(assignment, "=")
		if !hasValue {
			return failf(codeUsage, "--preview %q is not NAME=VALUE", assignment)
		}
		target, err := touch(strings.TrimSpace(variable))
		if err != nil {
			return err
		}
		target.PreviewValue = &value
	}
	if err := referenceEnv(writes.secrets, "--from-secret", touch, func(w *envVarWrite, ref *keyRef) {
		w.FromSecret = ref
	}); err != nil {
		return err
	}
	if err := referenceEnv(writes.claims, "--from-claim", touch, func(w *envVarWrite, ref *keyRef) {
		w.FromClaim = ref
	}); err != nil {
		return err
	}

	if len(changes) == 0 {
		return fail(codeUsage, "nothing to set").
			withHint("pass NAME=VALUE, --from-secret, --from-claim or --from-file")
	}

	found, err := client.project(ctx, name)
	if err != nil {
		return err
	}
	updated, err := client.setEnv(ctx, name, mergeEnv(found.Env, changes))
	if err != nil {
		return err
	}
	return printEnv(r, updated)
}

// referenceEnv reads the NAME=object:key form both reference flags take.
func referenceEnv(
	values []string,
	flag string,
	touch func(string) (*envVarWrite, error),
	set func(*envVarWrite, *keyRef),
) error {
	for _, value := range values {
		variable, reference, hasReference := strings.Cut(value, "=")
		if !hasReference {
			return failf(codeUsage, "%s %q is not NAME=object:key", flag, value)
		}
		object, key, hasKey := strings.Cut(reference, ":")
		if !hasKey || object == "" || key == "" {
			return failf(codeUsage, "%s %q does not name a key: write NAME=object:key", flag, value)
		}
		target, err := touch(strings.TrimSpace(variable))
		if err != nil {
			return err
		}
		set(target, &keyRef{Name: object, Key: key})
	}
	return nil
}

func removeEnv(parent context.Context, r *Runtime, names []string, yes bool) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	project, err := r.projectName()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	found, err := client.project(ctx, project)
	if err != nil {
		return err
	}

	removing := map[string]struct{}{}
	for _, name := range names {
		removing[name] = struct{}{}
	}
	kept := make([]envVarWrite, 0, len(found.Env))
	for _, variable := range found.Env {
		if _, drop := removing[variable.Name]; drop {
			delete(removing, variable.Name)
			continue
		}
		kept = append(kept, envVarWrite{Name: variable.Name})
	}
	if len(removing) > 0 {
		missing := make([]string, 0, len(removing))
		for name := range removing {
			missing = append(missing, name)
		}
		slices.Sort(missing)
		return failf(codeNotFound, "%s has no variable %s", project, strings.Join(missing, ", ")).
			withHint("`kitchen env list` says what there is")
	}

	if err := confirm(r, fmt.Sprintf("Remove %s from %s?", strings.Join(names, ", "), project), yes); err != nil {
		return err
	}

	updated, err := client.setEnv(ctx, project, kept)
	if err != nil {
		return err
	}
	return printEnv(r, updated)
}

// mergeEnv is the whole list to send: every variable the project has, by name
// alone unless this command is changing it.
//
// Sending a name with no value is what keeps the value the platform holds —
// which is the only reason a partial change is possible at all against a route
// that replaces the list.
func mergeEnv(existing []envVar, changes map[string]*envVarWrite) []envVarWrite {
	out := make([]envVarWrite, 0, len(existing)+len(changes))
	seen := map[string]struct{}{}

	for _, variable := range existing {
		seen[variable.Name] = struct{}{}
		if changed, ok := changes[variable.Name]; ok {
			out = append(out, *changed)
			continue
		}
		// Untouched: name alone keeps the literal value, and the reference has
		// to be restated because it is not a value the platform is holding on
		// this variable's behalf — it is what the variable *is*.
		keep := envVarWrite{Name: variable.Name}
		keep.FromSecret, keep.FromClaim = variable.FromSecret, variable.FromClaim
		out = append(out, keep)
	}

	added := make([]string, 0, len(changes))
	for name := range changes {
		if _, ok := seen[name]; !ok {
			added = append(added, name)
		}
	}
	slices.Sort(added)
	for _, name := range added {
		out = append(out, *changes[name])
	}
	return out
}

// readAssignments reads NAME=VALUE lines out of a file, or out of stdin for
// "-". It is deliberately not a shell parser: `export`, quoting rules and
// substitution are how a "simple" .env reader ends up disagreeing with the
// shell about what a value is. Surrounding quotes are stripped because every
// tool that writes these files adds them, and nothing else is interpreted.
func readAssignments(r *Runtime, path string) ([]string, error) {
	var reader *bufio.Scanner
	if path == "-" {
		reader = bufio.NewScanner(r.Stdin)
	} else {
		file, err := os.Open(path)
		if err != nil {
			return nil, fail(codeUsage, "reading "+path+": "+err.Error())
		}
		defer func() { _ = file.Close() }()
		reader = bufio.NewScanner(file)
	}

	assignments := []string{}
	for line := 1; reader.Scan(); line++ {
		text := strings.TrimSpace(reader.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		name, value, ok := strings.Cut(text, "=")
		if !ok {
			return nil, failf(codeUsage, "%s line %d is not NAME=VALUE: %q", path, line, text)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		assignments = append(assignments, strings.TrimSpace(name)+"="+value)
	}
	if err := reader.Err(); err != nil {
		return nil, fail(codeUsage, "reading "+path+": "+err.Error())
	}
	return assignments, nil
}

func printEnv(r *Runtime, updated *project) error {
	answer := list[envVar]{Items: updated.Env}
	if answer.Items == nil {
		answer.Items = []envVar{}
	}
	return r.printer().document(answer, func(s tui.Styles) string { return renderEnv(s, answer.Items) })
}

// renderEnv draws the list a person reads: what is there, and where each one's
// value comes from. Never a value — there is none to draw.
func renderEnv(s tui.Styles, variables []envVar) string {
	if len(variables) == 0 {
		return "No environment variables.\n"
	}
	rows := make([][]string, 0, len(variables))
	for _, variable := range variables {
		rows = append(rows, []string{variable.Name, envSource(s, variable), envPreview(s, variable)})
	}
	return s.Table([]string{"NAME", "VALUE", "PREVIEW"}, rows)
}

func envSource(s tui.Styles, variable envVar) string {
	switch {
	case variable.FromSecret != nil:
		return s.Accent.Render("secret " + variable.FromSecret.Name + ":" + variable.FromSecret.Key)
	case variable.FromClaim != nil:
		return s.Accent.Render("claim " + variable.FromClaim.Name + ":" + variable.FromClaim.Key)
	case variable.Set:
		return s.OK.Render("set")
	default:
		return s.Subtle.Render("empty")
	}
}

func envPreview(s tui.Styles, variable envVar) string {
	if variable.PreviewSet {
		return s.OK.Render("set")
	}
	return s.Subtle.Render(noValue)
}
