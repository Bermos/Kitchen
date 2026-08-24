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

// The criticality mapping, from a terminal.
//
// Both halves are reads and both are worth having in a pipeline. `kitchen
// criticality --json` is the operational-resilience register an institution
// would otherwise assemble by hand from four systems, and it is a nightly job
// away from being kept current. `kitchen criticality dependents --provider
// neon --json` is the question asked during an incident, when the person
// asking has a terminal and not a browser.
//
// Designating a project or an environment has no command, deliberately: it is
// a rare, deliberate write made by somebody who is not in a terminal at the
// time, and it goes through `kitchen api`.
//
//	kitchen api PATCH /projects/shop --data '{"criticality": "critical", "rto": "1h", "rpo": "5m"}'
//	kitchen api PATCH /environments/shop-production/requirements --data '{"criticality": "critical", "rto": "15m"}'

func newCriticalityCommand(r *Runtime) *cobra.Command {
	var minimum string

	cmd := &cobra.Command{
		Use:   "criticality",
		Short: "What supports each designated function, and what breaks without a third party",
		Long: strings.TrimSpace(`
The function-to-resource mapping: every designated function with the
environments, releases, resource claims, connections, domains and third
parties standing behind it.

Kitchen does not decide what is critical and does not set the tolerances —
the institution does, and this reads back what it declared. What the platform
contributes is the mapping, which is derived from the reconciled graph on
every request rather than maintained anywhere.

--criticality narrows to a designation and worse; the global --project
narrows to one project. Projects nobody has designated are counted rather
than listed, because a short map means one thing on a four-project estate and
another on a ninety-project one.

Designating something is a deliberate write left to the API:
  kitchen api PATCH /projects/<name> --data '{"criticality": "critical", "rto": "1h"}'
See docs/api/criticality.md.`),
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
			if minimum != "" {
				query.Set("criticality", minimum)
			}

			answer, err := client.criticalityMap(ctx, query)
			if err != nil {
				return err
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				return renderCriticalityMap(s, answer)
			})
		}),
	}
	cmd.Flags().StringVar(&minimum, "criticality", "",
		"only this designation and worse: nonCritical, important or critical")
	cmd.AddCommand(newDependentsCommand(r))

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/compliance/criticality"},
		Output: output{Mode: outputDocument, Kind: "criticalityMap"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"Everything supporting a critical function",
				"kitchen criticality --criticality critical --json"},
			{"One project's whole map", "kitchen criticality -p shop --json"},
		},
	})
}

// renderCriticalityMap draws the map as an outline rather than a table: the
// answer is a tree, and a table of it would either lose the nesting or repeat
// the project name on every row.
func renderCriticalityMap(s tui.Styles, answer *criticalityMap) string {
	lines := []string{}
	if len(answer.Functions) == 0 {
		lines = append(lines, "Nothing is designated.")
		if answer.Undesignated > 0 {
			lines = append(lines, strconv.Itoa(answer.Undesignated)+
				" project(s) carry no designation — Kitchen does not decide what is critical.")
		}
		return strings.Join(lines, "\n") + "\n"
	}

	for _, function := range answer.Functions {
		lines = append(lines, s.Title.Render(function.Project)+"  "+
			designationOf(function.Criticality, function.RTO, function.RPO))

		rows := make([][]string, 0, len(function.Environments))
		for _, env := range function.Environments {
			designation := designationOf(env.Criticality, env.RTO, env.RPO)
			if len(env.Inherited) > 0 {
				designation += " (inherited: " + strings.Join(env.Inherited, ", ") + ")"
			}
			rows = append(rows, []string{env.Name, env.Type, designation, env.Release,
				strings.Join(env.Domains, ", ")})
		}
		if len(rows) > 0 {
			lines = append(lines, s.Table(
				[]string{"ENVIRONMENT", "TYPE", "DESIGNATION", "RELEASE", "DOMAINS"}, rows))
		}

		claims := make([][]string, 0, len(function.Claims))
		for _, claim := range function.Claims {
			claims = append(claims, []string{claim.Name, claim.Type, claim.Provider,
				claim.DataClass, claim.Residency})
		}
		if len(claims) > 0 {
			lines = append(lines, s.Table(
				[]string{"CLAIM", "TYPE", "PROVIDER", "CLASS", "RESIDENCY"}, claims))
		}
		if len(function.ThirdParties) > 0 {
			lines = append(lines, "third parties: "+strings.Join(function.ThirdParties, ", "))
		}
		lines = append(lines, "")
	}
	if answer.Undesignated > 0 {
		lines = append(lines, strconv.Itoa(answer.Undesignated)+
			" further project(s) carry no designation at all.")
	}
	return strings.Join(lines, "\n") + "\n"
}

// designationOf words one designation, absences included.
func designationOf(criticality, rto, rpo string) string {
	parts := []string{criticality}
	if rto != "" {
		parts = append(parts, "RTO "+rto)
	}
	if rpo != "" {
		parts = append(parts, "RPO "+rpo)
	}
	return strings.Join(parts, ", ")
}

func newDependentsCommand(r *Runtime) *cobra.Command {
	var connection, provider string

	cmd := &cobra.Command{
		Use:   "dependents",
		Short: "What breaks if one connection, or one third party, is unavailable",
		Long: strings.TrimSpace(`
The mapping walked backwards: every environment that depends on one
Connection, or on every Connection from one third party, with the
designation that applies to each and how the dependency runs.

--tightest-rto in the answer is the smallest recovery objective among them:
how long the third party may be gone before the first tolerance the
institution declared is breached.

Exactly one of --connection or --provider. A connection nothing depends on is
an empty answer and exit 0 — "nothing breaks" is an answer.

The traversal follows the graph the platform reconciles: a project's git
source, its registry, and its resource claims. A third party the application
calls from its own code is not a Connection and is not visible here; the
answer says so in its depth field.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			connection, provider = strings.TrimSpace(connection), strings.TrimSpace(provider)
			switch {
			case connection == "" && provider == "":
				return fail(codeUsage, "ask about something: --connection <name> or --provider <name>").
					withHint("`kitchen connections` lists what the platform holds")
			case connection != "" && provider != "":
				return fail(codeUsage,
					"ask about one thing: --connection and --provider name different subjects")
			}

			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			query := url.Values{}
			if connection != "" {
				query.Set("connection", connection)
			} else {
				query.Set("provider", provider)
			}

			answer, err := client.dependents(ctx, query)
			if err != nil {
				return err
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(answer.Affected) == 0 {
					return "Nothing depends on " + answer.Subject.Name + ".\n"
				}
				rows := make([][]string, 0, len(answer.Affected))
				for _, affected := range answer.Affected {
					rows = append(rows, []string{
						affected.Project, affected.Environment, affected.Type,
						affected.Criticality, affected.RTO, strings.Join(affected.Through, ", "),
					})
				}
				lines := []string{s.Table([]string{"PROJECT", "ENVIRONMENT", "TYPE",
					"DESIGNATION", "RTO", "THROUGH"}, rows)}
				if answer.TightestRTO != "" {
					lines = append(lines,
						"tightest recovery objective among them: "+answer.TightestRTO)
				}
				return strings.Join(lines, "\n") + "\n"
			})
		}),
	}
	cmd.Flags().StringVar(&connection, "connection", "", "one Connection by name")
	cmd.Flags().StringVar(&provider, "provider", "", "every Connection from this third party")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/compliance/dependents"},
		Output: output{Mode: outputDocument, Kind: "dependents"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"What breaks if this database provider is down",
				"kitchen criticality dependents --provider neon --json"},
			{"What one connection carries",
				"kitchen criticality dependents --connection gh --json"},
		},
	})
}
