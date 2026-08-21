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
	"net/url"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen promote` and `kitchen promotions` — the staged pipeline from a
// terminal.
//
// A promotion is a request, not a move: the platform creates the object,
// phase Pending, and the promotion reconciler evaluates the environment's
// requirements against the artifact's stored evidence, records the decision,
// and applies or blocks it. The same digest travels every stage — nothing is
// rebuilt — which is also why `kitchen rollback` against a gated environment
// lands here rather than moving anything itself.

func newPromoteCommand(r *Runtime) *cobra.Command {
	var environmentName, reason string

	cmd := &cobra.Command{
		Use:   "promote <release>",
		Short: "Ask for a release to land on an environment",
		Long: strings.TrimSpace(`
Ask for a release to land on an environment.

The answer is a promotion, phase Pending: the platform evaluates the
environment's requirements against the artifact's attested evidence, stores
the decision, and applies the move only if the policy allows it. A blocked
promotion names the unmet rules; "kitchen promotions <name>" reads what
became of it.

Promoting an older release is how a rollback works against a gated
environment — a release is an immutable snapshot, so nothing is rebuilt
between stages or on the way back.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if environmentName == "" {
				return fail(codeUsage, "the target environment is required").
					withHint("--environment names it; `kitchen environments` lists what there is")
			}
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			project, err := r.projectName()
			if err != nil {
				return err
			}
			accepted, err := client.promote(ctx, project, environmentName, args[0], reason)
			if err != nil {
				return err
			}
			return r.printer().document(accepted, func(s tui.Styles) string {
				return fmt.Sprintf("%s %s: %s → %s (%s)\n",
					s.OK.Render("Requested"), s.Title.Render(accepted.Name),
					s.Accent.Render(accepted.Release), accepted.Environment, accepted.Phase)
			})
		}),
	}
	cmd.Flags().StringVarP(&environmentName, "environment", "e", "", "the environment the release should land on")
	cmd.Flags().StringVar(&reason, "reason", "", "why, in your own words; carried into the audit record")

	return describe(cmd, meta{
		Calls:  []string{"POST /api/v1/projects/{name}/promotions"},
		Output: output{Mode: outputDocument, Kind: "promotion", Note: "the promotion, phase Pending"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"Promote a release to staging", "kitchen promote shop-rel-41 --environment shop-staging --json"},
			{"An emergency move, with its reason on the record",
				"kitchen promote shop-rel-40 -e shop-production --reason \"rolling back INC-441\" --json"},
		},
	})
}

func newPromotionsCommand(r *Runtime) *cobra.Command {
	var environmentName, releaseName, phase string

	cmd := &cobra.Command{
		Use:     "promotions [NAME]",
		Aliases: []string{"promotion"},
		Short:   "What promotions were asked for, and what became of them",
		Long: strings.TrimSpace(`
The project's promotions, newest first — or one of them whole, by name.

Each carries what was asked (which release, into which environment, by whom)
and what the policy decided: the phase, the verdict, and — for a blocked one —
the unmet rules by id. The decision id leads to "kitchen decisions show" for
the full fired rules and the replayable input.`),
		Args: cobra.MaximumNArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			if len(args) == 1 {
				found, err := client.promotion(ctx, args[0])
				if err != nil {
					return err
				}
				return r.printer().document(found, func(s tui.Styles) string {
					lines := []string{
						fmt.Sprintf("%s: %s → %s %s",
							s.Title.Render(found.Name), s.Accent.Render(found.Release),
							found.Environment, s.Phase(found.Phase)),
						"requested by " + found.RequestedBy + " (" + found.Trigger + ")",
					}
					if found.Reason != "" {
						lines = append(lines, "reason "+found.Reason)
					}
					if found.Message != "" {
						lines = append(lines, found.Message)
					}
					for _, rule := range found.UnmetRules {
						lines = append(lines, "unmet  "+rule)
					}
					if found.DecisionID != "" {
						lines = append(lines, "decision "+found.DecisionID)
					}
					return strings.Join(lines, "\n") + "\n"
				})
			}

			project, err := r.projectName()
			if err != nil {
				return err
			}
			query := url.Values{}
			for name, value := range map[string]string{
				"environment": environmentName, "release": releaseName, "phase": phase,
			} {
				if value != "" {
					query.Set(name, value)
				}
			}
			found, err := client.projectPromotions(ctx, project, query)
			if err != nil {
				return err
			}
			answer := list[promotion]{Items: found}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(found) == 0 {
					return "No promotions.\n"
				}
				rows := make([][]string, 0, len(found))
				for _, p := range found {
					rows = append(rows, []string{
						p.CreatedAt.Local().Format("2006-01-02 15:04"),
						p.Release, p.Environment, p.Trigger, p.Phase,
						strings.Join(p.UnmetRules, ","), p.Name,
					})
				}
				return s.Table([]string{"WHEN", "RELEASE", "ENVIRONMENT", "TRIGGER", "PHASE",
					"UNMET", "NAME"}, rows)
			})
		}),
	}
	cmd.Flags().StringVarP(&environmentName, "environment", "e", "", "only promotions into this environment")
	cmd.Flags().StringVar(&releaseName, "release", "", "only promotions of this release")
	cmd.Flags().StringVar(&phase, "phase", "",
		"only this phase: Pending, Evaluating, Allowed, AllowedWithException, Blocked, Applied or Failed")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/projects/{name}/promotions",
			"GET /api/v1/promotions/{name}",
		},
		Output: output{Mode: outputDocument, Kind: "promotionList",
			Note: "one promotion (shape `promotion`) when a name is given"},
		Needs: needs{Auth: true, Project: true},
		Examples: []example{
			{"What is blocked, and by which rules", "kitchen promotions --phase Blocked --json"},
			{"One promotion whole", "kitchen promotions shop-promo-4kd92 --json"},
		},
	})
}
