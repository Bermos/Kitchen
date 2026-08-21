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
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// The decision register, from a terminal.
//
// Every verdict the platform reaches — a promotion allowed, a rescan gone
// non-compliant, a replay — is a stored decision citing the bundle digest and
// the input digest it was computed from. `list` and `show` read that record;
// `replay` is the auditor's move: re-run a historical decision from its
// stored inputs and see the same verdict come out, years later, with the
// ConfigMap the bundle came from long gone.

func newDecisionsCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "decisions",
		Aliases: []string{"decision"},
		Short:   "The policy decisions the platform has stored",
		Long: strings.TrimSpace(`
What the policy engine decided, and the proof it can be decided again.

A decision is stored with everything needed to reproduce it: the policy
bundle by digest, the fully materialized input by digest and in full, and
which rules fired. Nothing in a decision was fetched during evaluation — the
engine compiles bundles with no network builtins at all — which is what makes
replaying one meaningful.`),
	}
	cmd.AddCommand(newDecisionsListCommand(r), newDecisionsShowCommand(r), newDecisionsReplayCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{},
		Examples: []example{{"Blocked promotions, newest first", "kitchen decisions list --verdict blocked --json"}},
	})
}

func newDecisionsListCommand(r *Runtime) *cobra.Command {
	var environment, release, verdict, kind string
	var limit int

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List stored decisions, newest first",
		Long: strings.TrimSpace(`
List the policy decisions the platform has stored, newest first.

The filters compose: the global --project, --environment and --release narrow
to a pair or an artifact's history, --verdict to an outcome (allowed,
allowed-with-exception, blocked), --kind to why the engine was asked
(promotion, rescan, replay). Without --project the answer spans every project
the caller may see — this command reads the register rather than the linked
directory, so the link is deliberately not consulted.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			query := url.Values{}
			for name, value := range map[string]string{
				"project":     strings.TrimSpace(r.projectFlag),
				"environment": environment,
				"release":     release,
				"verdict":     verdict,
				"kind":        kind,
			} {
				if value != "" {
					query.Set(name, value)
				}
			}
			if limit > 0 {
				query.Set("limit", strconv.Itoa(limit))
			}

			found, err := client.decisions(ctx, query)
			if err != nil {
				return err
			}
			answer := list[decision]{Items: found}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(found) == 0 {
					return "No decisions stored.\n"
				}
				rows := make([][]string, 0, len(found))
				for _, d := range found {
					fired := strconv.Itoa(len(d.RulesFired))
					rows = append(rows, []string{
						d.Timestamp.Local().Format("2006-01-02 15:04"),
						d.Kind, d.Project, d.Environment, d.Release, d.Verdict, fired, d.ID,
					})
				}
				return s.Table([]string{"WHEN", "KIND", "PROJECT", "ENVIRONMENT", "RELEASE",
					"VERDICT", "FIRED", "ID"}, rows)
			})
		}),
	}
	cmd.Flags().StringVar(&environment, "environment", "", "only decisions about this environment")
	cmd.Flags().StringVar(&release, "release", "", "only decisions about this release")
	cmd.Flags().StringVar(&verdict, "verdict", "", "only this outcome: allowed, allowed-with-exception or blocked")
	cmd.Flags().StringVar(&kind, "kind", "", "only this question: promotion, rescan or replay")
	cmd.Flags().IntVar(&limit, "limit", 0, "how many to answer, newest first (server default 100)")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/decisions"},
		Output: output{Mode: outputDocument, Kind: "decisionList"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"Blocked promotions, newest first", "kitchen decisions list --verdict blocked --json"},
			{"Everything decided about one environment",
				"kitchen decisions list --environment shop-production --json"},
		},
	})
}

func newDecisionsShowCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "One decision whole, with its reproduction inputs",
		Long: strings.TrimSpace(`
Read one stored decision whole: the verdict, every rule that fired — waived
ones included, naming the exception that waived them — and the full
materialized input the decision can be replayed from.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			found, err := client.decision(ctx, args[0])
			if err != nil {
				return err
			}
			return r.printer().document(found, func(s tui.Styles) string {
				lines := []string{
					found.Kind + " of " + found.Release + " for " + found.Environment +
						": " + found.Verdict,
					"decided " + found.Timestamp.Local().Format("2006-01-02 15:04:05") +
						" by " + found.DecidedBy,
					"bundle " + found.BundleDigest,
					"input  " + found.InputDigest,
				}
				if found.DataSnapshot != "" {
					lines = append(lines, "data   "+found.DataSnapshot)
				}
				if len(found.RulesFired) == 0 {
					lines = append(lines, "no rules fired")
				}
				for _, rule := range found.RulesFired {
					line := "fired  " + rule.Rule + ": " + rule.Message
					if rule.Waived {
						line += " (waived by " + rule.Exception + ")"
					}
					lines = append(lines, line)
				}
				return strings.Join(lines, "\n") + "\n"
			})
		}),
	}

	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/decisions/{id}"},
		Output:   output{Mode: outputDocument, Kind: "decision"},
		Needs:    needs{Auth: true},
		Examples: []example{{"One decision whole", "kitchen decisions show 0d9a1f7e-… --json"}},
	})
}

func newDecisionsReplayCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "replay <id>",
		Short: "Re-evaluate a stored decision from its stored inputs",
		Long: strings.TrimSpace(`
Re-run a historical decision: the exact bundle bytes and the exact input the
original cited, through the same engine, compared against the original
verdict.

The replay is itself stored as a decision of kind replay, so the check has a
record too. The command succeeds whether or not the verdicts match — the
answer's "match" field is the finding, and a false one means the stored
decision does not reproduce, which is exactly what the command exists to
surface.

Replaying needs developer on the decision's project: it writes a decision.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			answer, err := client.replayDecision(ctx, args[0])
			if err != nil {
				return err
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				if answer.Match {
					return "reproduced: " + answer.Original.Verdict +
						" then, " + answer.Replay.Verdict + " now (replay stored as " +
						answer.Decision + ")\n"
				}
				return "DID NOT REPRODUCE: " + answer.Original.Verdict +
					" then, " + answer.Replay.Verdict + " now (replay stored as " +
					answer.Decision + ")\n"
			})
		}),
	}

	return describe(cmd, meta{
		Calls:    []string{"POST /api/v1/decisions/{id}/replay"},
		Output:   output{Mode: outputDocument, Kind: "decisionReplay"},
		Needs:    needs{Auth: true},
		Examples: []example{{"Prove a decision reproduces", "kitchen decisions replay 0d9a1f7e-… --json"}},
	})
}
