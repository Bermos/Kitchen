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
	"time"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// Quality gates, from a terminal — which is where the interesting half of them
// happens.
//
// A great many organisations already run their scanners in the application's
// own CI, on the pull request, minutes before the platform ever sees the
// commit. `kitchen gates submit` is how that result reaches the artifact
// instead of being run again: the pipeline that already has the report pipes it
// in, the platform signs it, and it is attached to the artifact's digest beside
// everything else.
//
// What the platform will not do is pretend it watched. A submitted result is
// recorded as reported by the identity that sent it, and a policy that trusts
// only what the platform ran itself can say so — which is the whole reason the
// two are never one word.

func newGatesCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "gates",
		Aliases: []string{"gate"},
		Short:   "Quality gates over a build's artifact",
		Long: strings.TrimSpace(`
What ran over a build's artifact, and how to submit a result something else
produced.

A gate records findings and never a verdict. "Completed" means the gate ran,
whatever it found — a scanner reporting a hundred critical vulnerabilities has
completed, because it did its job. "Failed" means the gate did not run at all
and nothing is known either way, which is the state a compliance system can sit
in while looking green.

Whether findings are disqualifying is a question about the environment being
deployed to, and it is answered at promotion rather than here.`),
	}
	cmd.AddCommand(newGatesListCommand(r), newGatesSubmitCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{},
		Examples: []example{{"What ran over a build", "kitchen gates list shop-bld-7 --json"}},
	})
}

func newGatesListCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list <build>",
		Short: "What ran over a build's artifact",
		Long: strings.TrimSpace(`
List the quality gates that ran over a build's artifact.

It shows what each gate did, not what it found: the findings are in the gate's
attestation, which "kitchen attestations" reads out of the registry.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			found, err := client.build(ctx, args[0])
			if err != nil {
				return err
			}
			// Every image the commit produced, not the project's own alone: a
			// gate is a claim about an image and a unit ships several, so a
			// listing that showed one image's runs would read as the unit's.
			runs := gateRuns(found)
			answer := list[gateRun]{Items: runs}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(runs) == 0 {
					return "No gates have run over this artifact.\n"
				}
				rows := make([][]string, 0, len(runs))
				for _, run := range runs {
					signed := "not signed"
					if run.Attested {
						signed = "signed"
					}
					reported := run.Source
					if run.ReportedBy != "" {
						reported += " (" + run.ReportedBy + ")"
					}
					rows = append(rows, []string{run.Workload, run.Name, run.Phase, reported, signed})
				}
				return s.Table([]string{"IMAGE", "GATE", "PHASE", "REPORTED", "EVIDENCE"}, rows)
			})
		}),
	}

	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/builds/{name}"},
		Output:   output{Mode: outputDocument, Kind: "gateList"},
		Needs:    needs{Auth: true},
		Examples: []example{{"What ran over a build", "kitchen gates list shop-bld-7 --json"}},
	})
}

// gateRuns is every gate run of one commit, each naming the image it ran
// over. A single-workload project answers exactly what it answered before,
// with `web` in front of it.
func gateRuns(found *build) []gateRun {
	runs := make([]gateRun, 0, len(found.Gates))
	for _, ran := range found.Gates {
		runs = append(runs, gateRun{Workload: "web", gate: ran})
	}
	for _, workload := range found.Workloads {
		for _, ran := range workload.Gates {
			runs = append(runs, gateRun{Workload: workload.Name, gate: ran})
		}
	}
	return runs
}

func newGatesSubmitCommand(r *Runtime) *cobra.Command {
	var gateName, version, format, findings, workload string

	cmd := &cobra.Command{
		Use:   "submit <build>",
		Short: "Submit a gate result produced somewhere else",
		Long: strings.TrimSpace(`
Submit a quality gate result that was produced outside the platform.

This is the path for a scanner an application's own CI already ran: the report
is attached to the build's artifact as a signed attestation rather than being
run again. The findings are sent as the exact bytes the tool wrote — this
command does not reshape them, because re-encoding somebody's evidence is
editing it.

The platform's signature means what it always means: that these bytes were
submitted by this identity at this moment and have not changed since. It is not
a claim that the findings are true, and the result is recorded as reported by
whoever sent it, so a policy that trusts only what the platform ran itself can
tell the difference.

Do not pass a scanner the flag that makes it exit non-zero on findings. A
result is a set of facts; whether they are disqualifying is decided at
promotion.

A commit that builds more than one image produces one artifact per workload,
and a result is a claim about one image. --workload names which it ran over;
without it the result is recorded against the project's own image.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if gateName == "" {
				return fail(codeUsage, "--gate names which gate produced this result")
			}
			body, err := requestBody(r, findings)
			if err != nil {
				return err
			}
			if len(body) == 0 {
				return fail(codeUsage,
					"--findings carries the gate's output: the JSON itself, @file, or - for stdin").
					withHint("a gate result with no findings is a gate that has not run")
			}

			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			submission := gateSubmission{
				Gate:     gateName,
				Workload: workload,
				Version:  version,
				Format:   format,
				Findings: json.RawMessage(body),
			}
			now := time.Now().UTC()
			submission.FinishedAt = &now

			accepted, err := client.submitGate(ctx, args[0], submission)
			if err != nil {
				return err
			}
			return r.printer().document(accepted, func(_ tui.Styles) string {
				return accepted.Gate + " recorded against " + accepted.Subject +
					", reported by " + accepted.ReportedBy + "\n"
			})
		}),
	}
	cmd.Flags().StringVar(&gateName, "gate", "", "which gate produced this result, e.g. trivy")
	cmd.Flags().StringVar(&version, "gate-version", "", "the gate's own version, which is what makes a finding reproducible")
	cmd.Flags().StringVar(&format, "format", "", "the shape of the findings, e.g. trivy-json or sarif")
	cmd.Flags().StringVar(&findings, "findings", "",
		"the gate's output: the JSON itself, @file, or - for stdin")
	cmd.Flags().StringVar(&workload, "workload", "",
		"which image of the unit the gate ran over, e.g. api — the project's own image by default")

	return describe(cmd, meta{
		Calls:  []string{"POST /api/v1/builds/{name}/gates"},
		Output: output{Mode: outputDocument, Kind: "gateAccepted"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"Submit a scan the pipeline already ran",
				"trivy image --format json shop:latest | kitchen gates submit shop-bld-7 --gate trivy --findings -"},
		},
	})
}
