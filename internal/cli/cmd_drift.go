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

// Compliance drift, from a terminal.
//
// "What is running right now that no longer meets its bar?" is the question
// almost no institution can answer, and it is one command here. It is also the
// one worth having in a pipeline: a nightly job that runs `kitchen drift
// --json` and opens a ticket on a non-empty answer is the whole of continuous
// vulnerability management for an estate this platform deploys.
//
// The exit code stays zero for a non-empty answer, deliberately. Drift is a
// finding, not a failure of the command — and a command that failed on a
// finding would be turned off the first week it found something.

func newDriftCommand(r *Runtime) *cobra.Command {
	var environment string
	var all bool

	cmd := &cobra.Command{
		Use:   "drift",
		Short: "Deployed releases that no longer meet their environment's bar",
		Long: strings.TrimSpace(`
What is deployed right now that would not be allowed to deploy today.

Every currently-deployed release is re-evaluated on a schedule against a
current vulnerability database, through the same policy code path a promotion
uses. This reads the result, and draws the distinction the whole thing exists
for:

  newly-failing        a rule that did not fire when this release was
                       promoted fires now — nothing about the artifact
                       changed, the world did
  waived-at-promotion  a rule that fired at promotion too and was waived by a
                       break-glass exception which has since expired
  waived               still clearing the bar, but only because an exception
                       is waiving what fires — compliant by grace, and dated
  not-evaluated        no current re-evaluation stands for this pair: either
                       nothing has ever re-checked it, or the last scan did
                       not run. That is a finding about the platform, not
                       about the release, and it is never counted as
                       compliant

Compliant pairs are left out unless --all asks for them.`),
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
			if all {
				query.Set("all", "true")
			}

			answer, err := client.complianceDrift(ctx, query)
			if err != nil {
				return err
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				lines := []string{}
				if !answer.Rescanning {
					// Said first, because an empty table under a pass that is
					// off means "nobody is looking", not "nothing is wrong".
					note := answer.Message
					if note == "" {
						note = "continuous re-evaluation is not running"
					}
					lines = append(lines, note, "")
				}
				if len(answer.Items) == 0 {
					lines = append(lines, "Nothing deployed is drifting.")
					return strings.Join(lines, "\n") + "\n"
				}
				rows := make([][]string, 0, len(answer.Items))
				for _, item := range answer.Items {
					scanned := noValue
					if item.ScannedAt != nil {
						scanned = item.ScannedAt.Local().Format("2006-01-02 15:04")
					}
					rules := make([]string, 0, len(item.Rules))
					for _, rule := range item.Rules {
						// The grant is the reader's next stop — renew it,
						// resolve it, or fix the finding — so the row names
						// whichever of the two there is: the one waiving the
						// rule now, or the one that waived it at promotion and
						// has since run out.
						rules = append(rules, rule.Rule+" ("+rule.Since+driftGrant(rule)+")")
					}
					status := item.Status
					if item.ScanFailed != "" {
						// The row is answering with something older than the
						// failure, so the failure travels with the word.
						status += " · last scan failed"
					}
					rows = append(rows, []string{
						item.Project, item.Environment, item.Release, status,
						scanned, strconv.Itoa(int(item.Findings)), strings.Join(rules, ", "),
					})
				}
				lines = append(lines, s.Table([]string{"PROJECT", "ENVIRONMENT", "RELEASE",
					"STATUS", "SCANNED", "FINDINGS", "RULES"}, rows))
				return strings.Join(lines, "\n")
			})
		}),
	}
	cmd.Flags().StringVar(&environment, "environment", "", "only this environment")

	cmd.Flags().BoolVar(&all, "all", false, "include the pairs that are still compliant")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/compliance/drift"},
		Output: output{Mode: outputDocument, Kind: "drift"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"Everything deployed that no longer clears its bar", "kitchen drift --json"},
			{"One project's whole estate, compliant rows included",
				"kitchen drift --project shop --all --json"},
		},
	})
}

// driftGrant names the exception on a rule, where there is one: the grant
// waiving it now, or — for a rule now firing unwaived — the grant that waived
// it at promotion and has since run out. The two are different facts and the
// answer keeps them in different fields; a table column has room for one, and
// only ever one of them is set.
func driftGrant(rule driftRule) string {
	if rule.Exception != "" {
		return ", waived by " + rule.Exception
	}
	if rule.WaivedAtPromotion != "" {
		return ", was waived by " + rule.WaivedAtPromotion
	}
	return ""
}
