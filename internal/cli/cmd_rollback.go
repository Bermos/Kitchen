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

// `kitchen rollback` — point an environment at another release.
//
// Rollback is not a special operation on this platform: a Release is an
// immutable snapshot of an image digest and the configuration it runs with, so
// putting an older one back *is* putting back exactly what was running. Which
// is why this command and a promotion are the same request, and why the only
// interesting part is working out which release "back" means — the
// environment's own history says, newest first.

func newRollbackCommand(r *Runtime) *cobra.Command {
	var (
		environmentName string
		to              string
		yes             bool
	)

	cmd := &cobra.Command{
		Use:   "rollback [RELEASE]",
		Short: "Put an environment back on an earlier release",
		Long: strings.TrimSpace(`
Move an environment to another release.

With no release named it goes back one: the environment's history records every
release that stopped being current, newest first, and the newest of those is
what it was running before. Naming one explicitly moves it there, which is also
how a promotion is done — the platform does not distinguish, because a release
is an immutable snapshot either way.

The default environment is the project's production one. "kitchen releases"
lists what there is to move to.`),
		Args: cobra.MaximumNArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if to != "" && to != args[0] {
					return fail(codeUsage, "the release is named twice, differently").
						withHint("pass it as an argument or as --to, not both")
				}
				to = args[0]
			}
			return rollback(commandContext(cmd), r, environmentName, to, yes)
		}),
	}

	cmd.Flags().StringVarP(&environmentName, "environment", "e", "",
		"the environment to move. The default is the project's production environment")
	cmd.Flags().StringVar(&to, "to", "", "the release to move it to. The default is the one it was on before")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask first")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/projects/{name}",
			"GET /api/v1/environments/{name}",
			"GET /api/v1/releases/{name}/config-diff",
			"PATCH /api/v1/environments/{name}",
		},
		Output: output{Mode: outputDocument, Kind: "environment",
			Note: "the environment as it is after the move — or, when the environment declares " +
				"requirements, the promotion the move became (shape `promotion`, answered 202)"},
		Needs: needs{Auth: true, Project: true},
		Examples: []example{
			{"Roll production back one release", "kitchen rollback --yes --json"},
			{"Move an environment to a named release", "kitchen rollback shop-rel-41 --yes --json"},
			{"Roll a preview back", "kitchen rollback --environment shop-pr-42 --yes --json"},
		},
	})
}

func rollback(parent context.Context, r *Runtime, environmentName, to string, yes bool) error {
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
			return failf(codeNotFound, "%s has no production environment to roll back", name).
				withHint("`kitchen environments` lists what there is, and --environment names one")
		}
		environmentName = found.ProductionEnvironment
	}

	current, err := client.environment(ctx, environmentName)
	if err != nil {
		return err
	}
	if to == "" {
		if len(current.History) == 0 {
			return failf(codeNotFound, "%s has only ever been on %s", environmentName, current.Release).
				withHint("there is nothing to go back to; `kitchen releases` lists what else exists, " +
					"and `kitchen rollback <release>` moves it there")
		}
		to = current.History[0].Release
	}
	if to == current.Release {
		return failf(codeConflict, "%s is already on %s", environmentName, to)
	}

	// What the move would change, before it is made (#181). This is the same
	// answer the dashboard's second step renders, and it is here for the same
	// reason: a confirmation that reveals nothing the release list had not
	// already shown is a speed bump rather than a safety mechanism.
	//
	// It is printed only when there is somebody to read it. `--yes` and
	// `--json` both mean nobody is being asked, and a diff nobody reads is a
	// round trip spent on a stream something else is parsing.
	if !yes && !r.jsonOut {
		printConfigDiff(ctx, r, client, to, current.Release)
	}
	if err := confirm(r, fmt.Sprintf("Move %s from %s to %s?", environmentName, current.Release, to), yes); err != nil {
		return err
	}

	outcome, err := client.moveEnvironment(ctx, environmentName, to)
	if err != nil {
		return err
	}
	// An environment that declares requirements answers with the promotion
	// the move became rather than the move itself: the policy engine decides,
	// and `kitchen promotions` is where the verdict lands.
	if outcome.Promotion != nil {
		accepted := outcome.Promotion
		return r.printer().document(accepted, func(s tui.Styles) string {
			return fmt.Sprintf("%s %s: %s → %s awaits the environment's requirements "+
				"(`kitchen promotions %s` follows it)\n",
				s.OK.Render("Promotion"), s.Title.Render(accepted.Name),
				s.Accent.Render(accepted.Release), accepted.Environment, accepted.Name)
		})
	}
	moved := outcome.Environment
	return r.printer().document(moved, func(s tui.Styles) string {
		return fmt.Sprintf("%s %s → %s %s\n",
			s.OK.Render("Moved"), s.Title.Render(moved.Name), s.Accent.Render(moved.Release),
			s.Phase(moved.Phase))
	})
}

// The change kinds a configDiff reports, in the direction of the write.
const (
	changeAdded     = "added"
	changeRemoved   = "removed"
	changeChanged   = "changed"
	changeUnchanged = "unchanged"
)

// printConfigDiff writes the configuration difference between the release an
// environment is going to and the one it is on, above the confirmation.
//
// A diff that cannot be read is not a reason to refuse the move: the endpoint
// is a viewer's read that could fail on its own — an old platform that does
// not serve it, a release whose snapshot predates the field — and stopping a
// rollback for it would be the safety feature making the outage longer. It
// says so and lets the confirmation stand.
//
// No value appears here, because none is fetched. See the configDiff type.
func printConfigDiff(ctx context.Context, r *Runtime, client *client, to, from string) {
	diff, err := client.releaseConfigDiff(ctx, to, from)
	if err != nil {
		r.printer().note("could not read what changes between %s and %s: %v", from, to, err)
		return
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%s → %s\n", from, to)

	moved := 0
	for _, v := range diff.Variables {
		if v.Change == changeUnchanged {
			continue
		}
		moved++
		fmt.Fprintf(&out, "  %s %-32s %s\n", changeMark(v.Change), v.Name, variableDetail(v))
	}
	for _, f := range diff.Runtime {
		if !f.Changed {
			continue
		}
		moved++
		fmt.Fprintf(&out, "  ~ %-32s %s → %s\n", f.Field, orUnset(f.From), orUnset(f.To))
	}
	for _, p := range diff.Processes {
		if p.Change == changeUnchanged {
			continue
		}
		moved++
		detail := p.Change
		if p.Schedule != "" {
			detail += " · " + p.Schedule
		}
		fmt.Fprintf(&out, "  %s %-32s %s\n", changeMark(p.Change), p.Name, detail)
	}
	if moved == 0 {
		fmt.Fprint(&out, "  the configuration is identical; only the image changes")
	}
	r.printer().note("%s", strings.TrimRight(out.String(), "\n"))
}

// changeMark is the diff vocabulary, the same signs the dashboard's rows are
// marked with.
func changeMark(change string) string {
	switch change {
	case changeChanged:
		return "~"
	case changeRemoved:
		return "−"
	case changeAdded:
		return "+"
	default:
		return "="
	}
}

// variableDetail says what kind of change it is, and where the source moved.
// There is no value to print — the API never reads one back — and the source
// is the part somebody acts on anyway.
func variableDetail(v variableChange) string {
	source := func(kind string) string {
		switch kind {
		case "secret":
			return "a secret"
		case "claim":
			return "a claim binding"
		case "value":
			return "a value"
		default:
			return "unset"
		}
	}
	switch v.Change {
	case changeRemoved:
		return source(v.AgainstSource) + " → unset"
	case changeAdded:
		return "unset → " + source(v.Source)
	case changeChanged:
		if v.PreviewOnly {
			return "only the preview override differs"
		}
		if v.Source != v.AgainstSource {
			return source(v.AgainstSource) + " → " + source(v.Source)
		}
		if v.Ref != nil && v.AgainstRef != nil && *v.Ref != *v.AgainstRef {
			return fmt.Sprintf("%s/%s → %s/%s", v.AgainstRef.Name, v.AgainstRef.Key, v.Ref.Name, v.Ref.Key)
		}
		return "the value differs"
	default:
		return "unchanged"
	}
}

func orUnset(value string) string {
	if value == "" {
		return "unset"
	}
	return value
}
