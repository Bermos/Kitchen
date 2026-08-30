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
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen secret` — a project's own credentials: the ones Kitchen did not
// mint. A database the project runs itself, a third-party API key, an SMTP
// password.
//
// It is `env`'s sibling and deliberately looks like it, but the two differ in
// exactly one way that matters here: an environment variable's value is *in*
// the project's configuration, where the audit log records a before and after
// and every member of the project can see it; a secret's value is not in it at
// all. What the project holds is a reference, and the value sits in the
// operator's Secret. So this command exists so that a credential never has to
// be written as `kitchen env set API_KEY=...`.
//
// The API's own bargain applies to every command here: **a value goes in and
// never comes back out**. `secret list` prints the whole list and no values,
// because there is nothing to print — there is no route on the platform that
// answers one.

func newSecretCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "secret",
		Aliases: []string{"secrets"},
		Short:   "Read and change a project's own secrets",
		Long: strings.TrimSpace(`
A project's own secrets: credentials the platform did not create for it — a
database it runs itself, a third-party API key, an SMTP password.

Setting one stores it where the application can read it and nowhere a request
can read it back. An environment variable then points at it, so the credential
is never part of the project's configuration:

  kitchen secret set SMTP_PASSWORD
  kitchen env set SMTP_PASSWORD --from-secret kitchen-project-secrets:SMTP_PASSWORD

"kitchen secret list" prints the reference for each one, so the second line
never has to be typed from memory.

A new or rotated value reaches what is already running: the platform restarts
whatever reads it. An environment variable does not — it lands in the next
release.`),
	}
	cmd.AddCommand(newSecretListCommand(r), newSecretSetCommand(r), newSecretRemoveCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{},
		Examples: []example{{"What the project holds", "kitchen secret list --json"}},
	})
}

func newSecretListCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "What secrets the project holds, by name",
		Long: strings.TrimSpace(`
List the project's own secrets.

Names and the reference each is read by, and nothing else: the platform answers
no values, so there is nothing here that could print one. Who set each and when
is the audit log's answer — "kitchen api GET /audit?kind=ProjectSecret".`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			project, err := r.projectName()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			secrets, err := client.projectSecrets(ctx, project)
			if err != nil {
				return err
			}
			return printSecrets(r, secrets)
		}),
	}
	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/projects/{name}/secrets"},
		Output:   output{Mode: outputDocument, Kind: "secretList"},
		Needs:    needs{Auth: true, Project: true},
		Examples: []example{{"What the project holds", "kitchen secret list --json"}},
	})
}

func newSecretSetCommand(r *Runtime) *cobra.Command {
	var (
		value     string
		valueFile string
		fromStdin bool
	)

	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Set a secret, or replace the value of one that is already there",
		Long: strings.TrimSpace(`
Set one of the project's own secrets.

The same command sets a new secret and rotates an existing one — it is the same
write, and the platform does not make the caller find out which it is doing
first.

The value comes from --value, --value-file, --value-stdin, or a prompt that
does not echo. Only the first of those puts a credential in the shell's
history, which is why it is not the only one:

  kitchen secret set SMTP_PASSWORD
  kitchen secret set SMTP_PASSWORD --value-file ./smtp-password
  pass show smtp | kitchen secret set SMTP_PASSWORD --value-stdin

Nothing is answered back but the name and the reference an environment variable
reads it by. A rotated value reaches what is already running: the platform
restarts whatever reads it.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return setSecret(commandContext(cmd), r, args[0], secretValueSource{
				value: value, file: valueFile, stdin: fromStdin,
			})
		}),
	}

	flags := cmd.Flags()
	flags.StringVar(&value, "value", "", "the value, on the command line — and so in the shell's history")
	flags.StringVar(&valueFile, "value-file", "", "read the value from a file, trailing newline trimmed")
	flags.BoolVar(&fromStdin, "value-stdin", false, "read the value from stdin, trailing newline trimmed")

	return describe(cmd, meta{
		Calls:  []string{"PUT /api/v1/projects/{name}/secrets/{secret}"},
		Output: output{Mode: outputDocument, Kind: "secret", Note: "the name and its reference — never the value"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"Set one from a file", "kitchen secret set SMTP_PASSWORD --value-file ./smtp-password --json"},
			{"Rotate one from a password manager", "pass show smtp | kitchen secret set SMTP_PASSWORD --value-stdin --json"},
		},
	})
}

func newSecretRemoveCommand(r *Runtime) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:     "rm NAME",
		Aliases: []string{"remove", "delete"},
		Short:   "Take a secret off the project",
		Long: strings.TrimSpace(`
Remove one of the project's own secrets.

There is no way to read it back first — the platform never answers a value — so
a secret removed by mistake has to be found again wherever it came from.

A secret an environment variable still reads is refused rather than removed:
the variable would leave the application unable to start, and the refusal names
which variables to point somewhere else first.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return removeSecret(commandContext(cmd), r, args[0], yes)
		}),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask first")

	return describe(cmd, meta{
		Calls: []string{
			"DELETE /api/v1/projects/{name}/secrets/{secret}",
			"GET /api/v1/projects/{name}/secrets",
		},
		Output: output{Mode: outputDocument, Kind: "secretList", Note: "what the project holds afterwards"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"Remove one, without being asked", "kitchen secret rm SMTP_PASSWORD --yes --json"},
		},
	})
}

// secretValueSource is the four ways a value arrives, in the order they are
// consulted.
type secretValueSource struct {
	value string
	file  string
	stdin bool
}

// read resolves the value. Every question has a flag that answers it, so a
// terminal is never required: without one of the three flags and without a
// terminal to prompt at, this fails naming them rather than waiting.
func (s secretValueSource) read(r *Runtime, name string) (string, error) {
	switch {
	case s.file != "":
		body, err := os.ReadFile(s.file)
		if err != nil {
			return "", fail(codeUsage, "reading "+s.file+": "+err.Error())
		}
		return strings.TrimRight(string(body), "\r\n"), nil
	case s.stdin:
		body, err := io.ReadAll(io.LimitReader(r.Stdin, 1<<20))
		if err != nil {
			return "", fail(codeUsage, "reading the value from stdin: "+err.Error())
		}
		return strings.TrimRight(string(body), "\r\n"), nil
	case s.value != "":
		return s.value, nil
	}

	if r.noInput || !r.StdinTerminal {
		return "", failf(codeUsage, "no value for %s", name).
			withHint("pass --value-file, --value-stdin or --value — this command is not asking, " +
				"since stdin is not a terminal or --no-input was given")
	}
	_, _ = fmt.Fprintf(r.Stderr, "Value for %s: ", name)
	typed, err := readSecret(r.Stdin)
	_, _ = fmt.Fprintln(r.Stderr)
	if err != nil {
		return "", fail(codeUsage, "reading the value: "+err.Error())
	}
	return typed, nil
}

func setSecret(parent context.Context, r *Runtime, name string, source secretValueSource) error {
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
		return fail(codeUsage, "a secret with no name")
	}
	value, err := source.read(r, name)
	if err != nil {
		return err
	}
	if value == "" {
		return failf(codeUsage, "the value for %s is empty", name).
			withHint("removing a secret is `kitchen secret rm " + name + "`")
	}

	ctx, cancel := r.context(parent)
	defer cancel()

	written, err := client.setProjectSecret(ctx, project, name, value)
	if err != nil {
		return err
	}
	return r.printer().document(written, func(s tui.Styles) string {
		return fmt.Sprintf("%s is set. Read it with %s\n",
			s.Accent.Render(written.Name),
			s.Accent.Render(fmt.Sprintf("kitchen env set %s --from-secret %s:%s",
				written.Name, written.Reference.Name, written.Reference.Key)))
	})
}

func removeSecret(parent context.Context, r *Runtime, name string, yes bool) error {
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

	if err := confirm(r, fmt.Sprintf("Remove %s from %s? It cannot be read back first.", name, project),
		yes); err != nil {
		return err
	}
	if err := client.deleteProjectSecret(ctx, project, name); err != nil {
		return err
	}
	// The delete answers 204, so what is left is read back rather than
	// guessed at — the same thing `env rm` prints, for the same reason.
	remaining, err := client.projectSecrets(ctx, project)
	if err != nil {
		return err
	}
	return printSecrets(r, remaining)
}

func printSecrets(r *Runtime, secrets []projectSecret) error {
	answer := list[projectSecret]{Items: secrets}
	if answer.Items == nil {
		answer.Items = []projectSecret{}
	}
	return r.printer().document(answer, func(s tui.Styles) string { return renderSecrets(s, answer.Items) })
}

// renderSecrets draws what there is: the names, and the reference each is read
// by. Never a value — there is none to draw.
func renderSecrets(s tui.Styles, secrets []projectSecret) string {
	if len(secrets) == 0 {
		return "No secrets.\n"
	}
	rows := make([][]string, 0, len(secrets))
	for _, secret := range secrets {
		rows = append(rows, []string{
			secret.Name,
			s.Subtle.Render(secret.Reference.Name + ":" + secret.Reference.Key),
		})
	}
	return s.Table([]string{"NAME", "REFERENCE"}, rows)
}
