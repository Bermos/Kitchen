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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
	"github.com/Bermos/Kitchen/internal/repoconfig"
)

// `kitchen config` — the file a project's settings are committed in.
//
// It asks the platform nothing, which is the point: the answer has to be
// available in a pre-commit hook and in a pull request's checks, where there
// is no credential and often no network. The parser it runs is the operator's
// own — the same package, compiled into the same release — so a file this
// accepts is a file that build will accept, and a file it refuses would have
// failed a build several minutes into one.
func newConfigCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "The kitchen.json a project's settings are committed in",
		Long: strings.TrimSpace(`
Work with kitchen.json: the file a repository declares its own build and
runtime settings in, read from the project's root directory at every commit
the platform builds.`),
	}
	cmd.AddCommand(newConfigCheckCommand(r), newConfigSchemaCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{},
		Examples: []example{{"Check the file in this directory", "kitchen config check --json"}},
	})
}

// configSchemaURL is what `kitchen config schema` answers with.
type configSchemaURL struct {
	// Schema is the published document, for the file's own `$schema` key.
	Schema string `json:"schema"`
}

// configCheck is what `kitchen config check` answers with.
type configCheck struct {
	// Path is the file that was read, as it was given or as it was found.
	Path string `json:"path"`
	// Declares is every setting the file names, in the dotted form the API
	// and the dashboard use — the answer to "what will this change".
	Declares []string `json:"declares"`
}

func newConfigCheckCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "check [PATH]",
		Short: "Check a kitchen.json before it is committed",
		Long: strings.TrimSpace(`
Read a kitchen.json, refuse it the way a build would, and list what it
declares.

With no PATH it reads kitchen.json in the working directory. The check needs
no credential and reaches no network, so it belongs in a pre-commit hook or a
pull request's own checks: this is the same parser the operator runs, so
anything it accepts is accepted by the build, and anything it refuses would
have failed one several minutes in.

What it cannot tell you is whether the file conflicts with the project it will
be built for — a variable the project takes from a provisioned database
cannot be given a literal value here, and that is refused against the project,
not against the file. The build says so; this does not.

It exits 0 for a file that is valid, 1 for one that is not, and 5 when there
is no file at PATH at all.`),
		Args: cobra.MaximumNArgs(1),
		RunE: run(func(_ *cobra.Command, args []string) error {
			path := repoconfig.FileName
			if len(args) == 1 {
				path = args[0]
			}
			return checkConfigFile(r, path)
		}),
	}

	return describe(cmd, meta{
		Output: output{Mode: outputDocument, Kind: "configCheck"},
		Needs:  needs{},
		Examples: []example{
			{"Check the file in this directory", "kitchen config check"},
			{"Check one in a monorepo", "kitchen config check apps/web/kitchen.json"},
			{"List what it changes, for a script", "kitchen config check --json | jq -r '.declares[]'"},
		},
	})
}

func checkConfigFile(r *Runtime, path string) error {
	// Resolved against the command's working directory rather than the
	// process's, like every other path this CLI takes.
	raw, err := os.ReadFile(filepath.Clean(absolute(r, path)))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return failf(codeNotFound, "there is no %s here", path).
			withHint("a project with no " + repoconfig.FileName + " is configured entirely in the dashboard, " +
				"which is not an error — pass a path if the file is somewhere else")
	case err != nil:
		return fail(codeFailed, "reading "+path+": "+err.Error())
	}

	config, err := repoconfig.Parse(raw)
	if err != nil {
		return fail(codeFailed, path+": "+strings.TrimPrefix(err.Error(), repoconfig.ErrInvalid.Error()+": ")).
			withHint("the schema is published at " + repoconfig.SchemaURL)
	}

	answer := configCheck{Path: path, Declares: config.Declares()}
	if answer.Declares == nil {
		answer.Declares = []string{}
	}
	return r.printer().document(answer, func(s tui.Styles) string {
		var out strings.Builder
		if len(answer.Declares) == 0 {
			fmt.Fprintf(&out, "%s is valid, and declares nothing — every setting stays the project's\n",
				s.Accent.Render(path))
			return out.String()
		}
		fmt.Fprintf(&out, "%s is valid, and takes over %s from the project:\n",
			s.Accent.Render(path), plural(len(answer.Declares), "setting", "settings"))
		for _, field := range answer.Declares {
			fmt.Fprintf(&out, "  %s\n", field)
		}
		return out.String()
	})
}

// plural is the count and its noun, for the one sentence above.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

func newConfigSchemaCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Where the JSON Schema for kitchen.json is published",
		Long: strings.TrimSpace(`
Print the URL of the JSON Schema for kitchen.json.

Putting it in the file's "$schema" key is what makes an editor complete the
fields and flag a typo before it is committed. The platform reads that key
only to ignore it: what a file may say is decided by the release that builds
it, not by a document fetched at build time, so a schema from a newer release
than the installation describes fields it will refuse.

This is not "kitchen schema", which publishes the CLI's own surface.`),
		Args: cobra.NoArgs,
		RunE: run(func(_ *cobra.Command, _ []string) error {
			answer := configSchemaURL{Schema: repoconfig.SchemaURL}
			return r.printer().document(answer, func(tui.Styles) string {
				return repoconfig.SchemaURL + "\n"
			})
		}),
	}

	return describe(cmd, meta{
		Output: output{Mode: outputDocument, Kind: "configSchema"},
		Needs:  needs{},
		Examples: []example{
			{"Where the schema is", "kitchen config schema"},
			{"The URL alone, for a script", "kitchen config schema --json | jq -r .schema"},
		},
	})
}
