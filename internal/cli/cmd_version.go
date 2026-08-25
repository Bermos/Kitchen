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
	"fmt"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
	"github.com/Bermos/Kitchen/internal/version"
)

// versionInfo is what `kitchen version` answers with: the client's own
// identity, and nothing the platform was asked for.
type versionInfo struct {
	Version string `json:"version"`
	Go      string `json:"go"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

// `kitchen version` — which release of the CLI this is.
//
// cobra's own --version prints a sentence and ignores --json, which is exactly
// the shape this CLI promises never to put on stdout. So the version is a
// command like everything else: one document, one shape, readable by whatever
// is deciding whether the binary on this machine is the one the installation
// expects.
func newVersionCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Report which release of the CLI this is",
		Long: strings.TrimSpace(`
Print the release this binary was built from, with the toolchain and platform
it was built for.

It asks the platform nothing — this is the client's own version. One tag
versions the chart, both images and the CLI, so this number and the one in the
dashboard's sidebar are directly comparable, and a client older than the
installation it talks to is how a command for a route that does not exist yet
comes to fail.

A binary installed with "go install" reports the module version the toolchain
recorded in it; one built by the Makefile reports what the linker stamped; a
plain "go build" in a working directory has neither and says "dev".`),
		Args: cobra.NoArgs,
		RunE: run(func(_ *cobra.Command, _ []string) error {
			answer := versionInfo{
				Version: version.Version,
				Go:      runtime.Version(),
				OS:      runtime.GOOS,
				Arch:    runtime.GOARCH,
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				return fmt.Sprintf("kitchen %s (%s, %s/%s)\n",
					s.Accent.Render(answer.Version), answer.Go, answer.OS, answer.Arch)
			})
		}),
	}

	return describe(cmd, meta{
		Output: output{Mode: outputDocument, Kind: "version"},
		Needs:  needs{},
		Examples: []example{
			{"Which release this is", "kitchen version"},
			{"The number on its own", "kitchen version --json | jq -r .version"},
		},
	})
}
