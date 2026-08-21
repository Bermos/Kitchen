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
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// The exception register, from a terminal.
//
// `list` and `show` are the reads: every break-glass grant — who asked, who
// approved, which rules it waives, until when, and which promotions relied on
// it. The writes stay with `kitchen api`, deliberately: granting an exception
// is rare, two-person and worth typing out in full, and resolving one is an
// auditable act with a reason — neither is a move to make muscle-memory of.
//
//	kitchen api POST /projects/shop/exceptions --data '{"environment": "shop-production",
//	  "ruleIDs": ["max-severity"], "reason": "hotfix for INC-421", "approvedBy": "cto@example.com",
//	  "incidentRef": "INC-421", "expiresAt": "2026-08-22T09:00:00Z"}'
//	kitchen api PATCH /exceptions/shop-exc-x7k2p --data '{"resolved": true, "reason": "patched in 1.4.2"}'

func newExceptionsCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "exceptions",
		Aliases: []string{"exception"},
		Short:   "The break-glass exceptions the platform has on record",
		Long: strings.TrimSpace(`
The exception register: every break-glass grant, active and historical.

An exception waives named policy rules for one project's environment, for a
bounded time, on two people's word — the requester's and an approver's whose
required seniority rises with the duration. The rules still evaluate and
still report; the exception changes the verdict, never the facts, and every
use of one is a privileged audit record plus a break-glass attestation on
the artifact it carried.

Granting and resolving are deliberate, spelled-out writes left to the API:
  kitchen api POST /projects/<project>/exceptions --data '{...}'
  kitchen api PATCH /exceptions/<name> --data '{"resolved": true, "reason": "..."}'
See docs/api/exceptions.md for the bodies.`),
	}
	cmd.AddCommand(newExceptionsListCommand(r), newExceptionsShowCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{},
		Examples: []example{{"Every active exception", "kitchen exceptions list --json"}},
	})
}

func newExceptionsListCommand(r *Runtime) *cobra.Command {
	var environment string
	var historical bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List exceptions: active by default, everything with --historical",
		Long: strings.TrimSpace(`
List break-glass exceptions, soonest to expire first.

Active grants by default — the ones currently changing verdicts. --historical
adds the expired and the resolved, which are retained on purpose: the
register is an answer to "what was ever waived here, by whom, and what went
out under it", not only to "what is waived now". The global --project and
--environment narrow the answer; without --project it spans every project
the caller may see.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			query := url.Values{}
			if project := strings.TrimSpace(r.projectFlag); project != "" {
				query.Set("project", project)
			}
			if environment != "" {
				query.Set("environment", environment)
			}
			if historical {
				query.Set("historical", "true")
			}

			found, err := client.exceptions(ctx, query)
			if err != nil {
				return err
			}
			answer := list[exception]{Items: found}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(found) == 0 {
					if historical {
						return "No exceptions on record.\n"
					}
					return "No active exceptions. (--historical lists past ones.)\n"
				}
				rows := make([][]string, 0, len(found))
				for _, e := range found {
					rows = append(rows, []string{
						e.Name, e.Project, e.Environment, strings.Join(e.RuleIDs, ","),
						e.ApprovedBy, e.ExpiresAt.Local().Format("2006-01-02 15:04"), e.Phase,
					})
				}
				return s.Table([]string{"NAME", "PROJECT", "ENVIRONMENT", "RULES",
					"APPROVED BY", "EXPIRES", "PHASE"}, rows)
			})
		}),
	}
	cmd.Flags().StringVar(&environment, "environment", "", "only exceptions scoped to this environment")
	cmd.Flags().BoolVar(&historical, "historical", false, "include expired and resolved exceptions")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/exceptions"},
		Output: output{Mode: outputDocument, Kind: "exceptionList"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"Every active exception", "kitchen exceptions list --json"},
			{"The whole register for one project", "kitchen exceptions list -p shop --historical --json"},
		},
	})
}

func newExceptionsShowCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "One exception whole: the grant, and what went out under it",
		Long: strings.TrimSpace(`
Read one exception whole: who requested it and who approved, which rules it
waives and where, its reason and incident, when it expires, and every
promotion that relied on it — the usedBy list is the register's answer to
"what actually shipped under this grant".`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			found, err := client.exception(ctx, args[0])
			if err != nil {
				return err
			}
			return r.printer().document(found, func(s tui.Styles) string {
				lines := []string{
					found.Name + ": " + found.Phase,
					"waives  " + strings.Join(found.RuleIDs, ", ") + " for " + found.Environment,
					"reason  " + found.Reason,
					"asked   " + found.RequestedBy,
					"approved " + found.ApprovedBy,
					"expires " + found.ExpiresAt.Local().Format("2006-01-02 15:04:05"),
				}
				if found.Release != "" {
					lines = append(lines, "release "+found.Release)
				}
				if found.IncidentRef != "" {
					lines = append(lines, "incident "+found.IncidentRef)
				}
				if len(found.UsedBy) > 0 {
					lines = append(lines, "used by "+strings.Join(found.UsedBy, ", "))
				}
				if found.ResolvedBy != "" {
					lines = append(lines, "resolved by "+found.ResolvedBy)
				}
				return strings.Join(lines, "\n") + "\n"
			})
		}),
	}

	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/exceptions/{name}"},
		Output:   output{Mode: outputDocument, Kind: "exception"},
		Needs:    needs{Auth: true},
		Examples: []example{{"One exception whole", "kitchen exceptions show shop-exc-x7k2p --json"}},
	})
}
