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
	"encoding/json"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// Exploitability assertions, from a terminal.
//
// A scanner matching a bill of materials against today's vulnerability
// database reports everything it finds, and most of it is present but not
// reachable. Without a way to say so, the daily report becomes noise, the noise
// becomes rubber-stamping, and the control becomes decorative. `kitchen vex
// submit` is how the vendor's or the security team's word that a finding does
// not apply here reaches the artifact.
//
// `kitchen vex list` is the other half and the more important one: it shows
// every finding beside the statement covering it, so a suppression is
// something a person can see, date and attribute rather than something that
// quietly happened inside a policy bundle.

func newVEXCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "vex",
		Aliases: []string{"openvex"},
		Short:   "Exploitability assertions about a build's artifact",
		Long: strings.TrimSpace(`
What has been asserted about a build's artifact being exploitable, and how to
assert it.

A VEX statement does not change what a scanner found. It says whether the
finding applies here — the component is not present, the vulnerable code is
not reachable, a mitigation is already in place — and a "not_affected"
statement must give one of OpenVEX's five justifications, because a
suppression whose reason cannot be counted cannot be reviewed.

Whether a statement suppresses anything is still the target environment's
question. Its policy decides whose word it takes, whether an unverified
statement counts at all, and how old a statement may be.`),
	}
	cmd.AddCommand(newVEXListCommand(r), newVEXSubmitCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{},
		Examples: []example{{"What has been asserted about an artifact", "kitchen vex list shop-bld-7 --json"}},
	})
}

func newVEXListCommand(r *Runtime) *cobra.Command {
	var workload string

	cmd := &cobra.Command{
		Use:   "list <build>",
		Short: "The artifact's VEX statements, beside the findings they modify",
		Long: strings.TrimSpace(`
List the OpenVEX statements attached to a build's artifact, joined to the
vulnerability-scan findings each one is about.

A suppressed finding is still a finding and is still listed, with the
statement suppressing it, who authored it and who submitted it. A statement
that has expired, that was never justified from the enumeration, or whose
signature no key this platform holds accepted is listed too, and marked — the
point of the view is that nothing here is applied silently.

A statement suppresses a finding on an image, and a commit that builds more
than one image has one artifact per workload. --workload names which to read;
without it you get the project's own image.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			answer, err := client.vex(ctx, args[0], workload)
			if err != nil {
				return err
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				return vexTable(s, answer)
			})
		}),
	}
	cmd.Flags().StringVar(&workload, "workload", "",
		"which image of the unit to read, e.g. api — the project's own image by default")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/builds/{name}/vex"},
		Output: output{Mode: outputDocument, Kind: "vexAnswer"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"What has been asserted about an artifact", "kitchen vex list shop-bld-7 --json"},
			{"What has been asserted about one workload's image",
				"kitchen vex list shop-bld-7 --workload worker --json"},
		},
	})
}

// vexTable draws the join: the findings first, because "why is this not
// blocking?" is the question somebody came here with.
func vexTable(s tui.Styles, answer *vexAnswer) string {
	out := &strings.Builder{}
	if answer.Caveat != "" {
		out.WriteString(answer.Caveat + "\n\n")
	}
	if len(answer.Findings) == 0 && len(answer.Statements) == 0 {
		return "Nothing has been asserted about this artifact, and nothing has scanned it.\n"
	}
	if len(answer.Findings) > 0 {
		rows := make([][]string, 0, len(answer.Findings))
		for _, finding := range answer.Findings {
			rows = append(rows, []string{
				finding.Vulnerability, finding.Severity, finding.Package, vexNote(finding.VEX),
			})
		}
		out.WriteString(s.Table([]string{"FINDING", "SEVERITY", "PACKAGE", "ASSERTED"}, rows))
	}
	if len(answer.Statements) > 0 {
		rows := make([][]string, 0, len(answer.Statements))
		for _, statement := range answer.Statements {
			rows = append(rows, []string{
				statement.Vulnerability, statement.Status, statement.Justification,
				statement.Author, vexState(statement),
			})
		}
		out.WriteString("\n" + s.Table(
			[]string{"VULNERABILITY", "STATUS", "JUSTIFICATION", "AUTHOR", "STATE"}, rows))
	}
	return out.String()
}

// vexNote is the one column that says whether a finding has anything said
// about it, and by whom.
func vexNote(statement *vexStatement) string {
	if statement == nil {
		return noValue
	}
	note := statement.Status
	if statement.Author != "" {
		note += " (" + statement.Author + ")"
	}
	if state := vexState(*statement); state != "current" {
		note += " — " + state
	}
	return note
}

// vexState is what the platform can establish, in one word. It is never
// "suppressed": that is the environment's word, not this one's.
func vexState(statement vexStatement) string {
	switch {
	case statement.Expired:
		return "expired"
	case !statement.Verified:
		return "unverified"
	case statement.Status == "not_affected" && !statement.Justified:
		return "unjustified"
	default:
		return "current"
	}
}

func newVEXSubmitCommand(r *Runtime) *cobra.Command {
	var document, workload string

	cmd := &cobra.Command{
		Use:   "submit <build>",
		Short: "Attach an OpenVEX document to a build's artifact",
		Long: strings.TrimSpace(`
Attach an OpenVEX document to a build's artifact as a signed attestation.

The document is sent as the exact bytes its author wrote — this command does
not reshape it, because re-encoding somebody's assertion is editing it — and
it is stored under OpenVEX's own predicate type, so "cosign download
attestation" reads it back with the platform out of the loop.

A "not_affected" statement must carry a justification from OpenVEX's
enumeration: component_not_present, vulnerable_code_not_present,
vulnerable_code_not_in_execute_path,
vulnerable_code_cannot_be_controlled_by_adversary or
inline_mitigations_already_exist. Free text belongs in impact_statement or
status_notes, beside the justification rather than instead of it, and a
document that gives only free text is refused.

The document names its own author. The platform records, separately, the
identity that submitted it — those are two different facts and only the second
is the platform's own observation. Submitting is an admin's write on the
project, because an assertion whose effect is to stop a finding counting is
nearer to approving a break-glass exception than to reporting a scan.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			body, err := requestBody(r, document)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fail(codeUsage,
					"--document carries the OpenVEX document: the JSON itself, @file, or - for stdin").
					withHint("a submission with no document asserts nothing")
			}

			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			accepted, err := client.submitVEX(ctx, args[0], vexSubmission{
				Document: json.RawMessage(body),
				Workload: workload,
			})
			if err != nil {
				return err
			}
			return r.printer().document(accepted, func(_ tui.Styles) string {
				return "attached " + accepted.PredicateType + " to " + accepted.Subject +
					", authored by " + accepted.Author + ", submitted by " + accepted.SubmittedBy +
					"\ncovering " + strings.Join(accepted.Vulnerabilities, ", ") + "\n"
			})
		}),
	}
	cmd.Flags().StringVar(&document, "document", "",
		"the OpenVEX document: the JSON itself, @file, or - for stdin")
	cmd.Flags().StringVar(&workload, "workload", "",
		"which image of the unit the assertion is about, e.g. worker — the project's own image by default")

	return describe(cmd, meta{
		Calls:  []string{"POST /api/v1/builds/{name}/vex"},
		Output: output{Mode: outputDocument, Kind: "vexAccepted"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"Attach a VEX document the security team wrote",
				"kitchen vex submit shop-bld-7 --document @not-affected.openvex.json"},
			{"Assert about one workload's image",
				"kitchen vex submit shop-bld-7 --workload worker --document @not-affected.openvex.json"},
		},
	})
}
