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

// `kitchen redeploy` — the same commit, today's settings (#392).
//
// It belongs to the deploy family and it is the member for the case `deploy`
// cannot serve: the code is right and a *setting* was wrong. A release freezes
// the configuration it was cut with, so correcting the setting changes nothing
// that is already running, and `kitchen deploy` with no new commit rebuilds
// the same commit into the release it already has. This asks the platform for
// a new release from the commit that is already there.
//
// It asks before it does it, like `rollback` does, and for the same reason:
// this replaces what is serving. The question says what the change is —
// the same commit, today's settings — because that is the one thing somebody
// about to press return may have wrong.

func newRedeployCommand(r *Runtime) *cobra.Command {
	var (
		environmentName string
		yes             bool
	)

	cmd := &cobra.Command{
		Use:   "redeploy",
		Short: "Deploy the commit that is already there, with the project's settings as they stand",
		Long: strings.TrimSpace(`
Cut a new release from the commit an environment is already running, carrying
the project's configuration as it stands now, and deploy it.

A release is an immutable snapshot of an image and the settings it was built
with, which is what makes a rollback exact — and what makes a corrected setting
reach nothing that is already running. Rebuilding the commit does not help: the
same commit resolves to the release it already has. This is the way to apply a
setting without a commit to carry it.

The image does not change, and neither does the commit. What changes is the
configuration, and the release that was running is left exactly as it was, so
rolling back to it still puts back what was there.

The default environment is the project's production one.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			return redeploy(commandContext(cmd), r, environmentName, yes)
		}),
	}

	cmd.Flags().StringVarP(&environmentName, "environment", "e", "",
		"the environment to redeploy. The default is the project's production environment")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask first")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/projects/{name}",
			"GET /api/v1/environments/{name}",
			"POST /api/v1/environments/{name}/redeploy",
		},
		Output: output{Mode: outputDocument, Kind: "redeployed",
			Note: "the release that was made and where it is going, answered 202. " +
				"`promotion` is set instead of a move when the environment declares requirements"},
		Needs: needs{Auth: true, Project: true},
		Examples: []example{
			{"Apply a corrected setting to production", "kitchen redeploy --yes --json"},
			{"Redeploy a preview", "kitchen redeploy --environment shop-pr-42 --yes --json"},
		},
	})
}

func redeploy(parent context.Context, r *Runtime, environmentName string, yes bool) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	if environmentName == "" {
		name, err := r.projectName()
		if err != nil {
			return err
		}
		found, err := client.project(ctx, name)
		if err != nil {
			return err
		}
		if found.ProductionEnvironment == "" {
			return failf(codeNotFound, "%s has no production environment to redeploy", name).
				withHint("`kitchen environments` lists what there is, and --environment names one")
		}
		environmentName = found.ProductionEnvironment
	}

	current, err := client.environment(ctx, environmentName)
	if err != nil {
		return err
	}
	if current.Release == "" {
		return failf(codeNotFound, "%s is not running a release yet, so there is nothing to redeploy",
			environmentName).
			withHint("`kitchen deploy` builds a commit for it")
	}
	if err := confirm(r, fmt.Sprintf(
		"Redeploy %s — the same commit as %s, with the project's settings as they stand now?",
		environmentName, current.Release), yes); err != nil {
		return err
	}

	accepted, err := client.redeployEnvironment(ctx, environmentName)
	if err != nil {
		return err
	}
	return r.printer().document(accepted, func(s tui.Styles) string {
		// An environment that declares requirements answers with the
		// promotion the move became rather than the move itself: the release
		// exists, and the policy engine decides whether it lands.
		if accepted.Promotion != "" {
			return fmt.Sprintf("%s %s: %s awaits %s's requirements (`kitchen promotions %s` follows it)\n",
				s.OK.Render("Cut"), s.Title.Render(accepted.Release),
				s.Accent.Render(accepted.Release), accepted.Environment, accepted.Promotion)
		}
		return fmt.Sprintf("%s %s → %s (the same commit as %s)\n",
			s.OK.Render("Redeploying"), s.Title.Render(accepted.Environment),
			s.Accent.Render(accepted.Release), accepted.PreviousRelease)
	})
}
