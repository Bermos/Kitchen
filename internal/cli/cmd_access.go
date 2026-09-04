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

// Access, from a terminal.
//
// The reads are here because they are the two questions somebody asks
// repeatedly and wants in a pipe: who holds what (and which of those grants
// look like they belong to nobody), and where the recertification stands.
// `--orphaned` on the first is the whole reason this is a command rather than
// a `kitchen api` line — a monthly job that opens a ticket on a non-empty
// answer is the shape it is for.
//
// The writes stay with `kitchen api`, deliberately, and for the exception
// register's reason: opening a cycle is rare and worth typing out, and
// deciding a grant is an auditable act naming a person, which is not a move to
// make muscle-memory of.
//
//	kitchen api POST /access/reviews --data '{"scope": "all", "reason": "the annual audit"}'
//	kitchen api PATCH /access/reviews/access-review-8x2kd --data '{"decisions":
//	  [{"subject": "user_7", "grant": "shop", "decision": "revoke", "note": "left in June"}],
//	  "close": true}'

// accessInTheDashboard is where the four commands in this file point when the
// platform refuses them, spelled once because it is one screen for all of
// them: the recertification panel reads the survey and the cycles together.
func accessInTheDashboard() *dashboardOnly {
	return onlyInTheDashboard("Platform → Audit, under Access recertification", "/platform/audit")
}

func newAccessCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "access",
		Short: "Who holds what on the platform, and the recertifications of it",
		Long: strings.TrimSpace(`
Who holds what on this platform, and the cycles somebody has reviewed that
against.

A grant is one account's one role in one place — the platform's operator list,
or a project — so an account holding admin on three projects is three rows and
three decisions. An identity is reported as orphaned only when it is both
dormant and unknown to the identity provider: either alone has an innocent
reading, and the pair does not.

Every command here needs the operator role: the answer is the whole
installation's access in one document.

Opening a cycle and deciding a grant are deliberate, spelled-out writes left
to the API:
  kitchen api POST /access/reviews --data '{"scope": "all", "reason": "..."}'
  kitchen api PATCH /access/reviews/<name> --data '{"decisions": [...], "close": true}'
See docs/api/access.md for the bodies.`),
	}
	cmd.AddCommand(newAccessIdentitiesCommand(r), newAccessReviewsCommand(r), newAccessShowCommand(r))

	return describe(cmd, meta{
		Output:   output{Mode: outputNone},
		Needs:    needs{Platform: accessInTheDashboard()},
		Examples: []example{{"Grants that look like they belong to nobody", "kitchen access identities --orphaned --json"}},
	})
}

func newAccessIdentitiesCommand(r *Runtime) *cobra.Command {
	var orphanedOnly bool

	cmd := &cobra.Command{
		Use:   "identities",
		Short: "Every grant on the platform, with what is known about who is behind it",
		Long: strings.TrimSpace(`
One row per grant: the account, where the role is held, what it is, and when
that identity was last recorded doing something.

Two words are worth reading carefully. "inactive" is the audit log's answer,
and the audit log records writes — somebody who only ever reads looks inactive
and is not. "unknown" means the identity provider holds no account for that
subject, and is only ever claimed when the directory actually answered: an
installation federated to an issuer of its own serves none, and nothing is
reported as orphaned there at all.

--orphaned narrows to the pair — dormant and unknown together — which is the
list worth acting on.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			survey, err := client.identities(ctx)
			if err != nil {
				return err
			}
			shown := survey.Identities
			if orphanedOnly {
				kept := make([]identity, 0, len(shown))
				for _, row := range shown {
					if row.Orphaned {
						kept = append(kept, row)
					}
				}
				shown = kept
			}
			survey.Identities = shown

			return r.printer().document(survey, func(s tui.Styles) string {
				if len(shown) == 0 {
					if orphanedOnly {
						return "No grant on this platform is both dormant and unknown to the identity provider.\n"
					}
					return "No grants at all: nobody holds a role here.\n"
				}
				rows := make([][]string, 0, len(shown))
				for _, row := range shown {
					last := "never recorded"
					if row.LastActive != nil {
						last = row.LastActive.Local().Format("2006-01-02")
					}
					rows = append(rows, []string{
						row.name(), row.Grant, row.Role, last, flags(row),
					})
				}
				table := s.Table([]string{"ACCOUNT", "GRANT", "ROLE", "LAST ACTIVE", "FLAGS"}, rows)
				if !survey.DirectoryConsulted {
					table += "\nThe account directory did not answer, so nothing is reported as " +
						"belonging to nobody.\n"
				}
				if survey.Message != "" {
					table += "\n" + survey.Message + "\n"
				}
				return table
			})
		}),
	}
	cmd.Flags().BoolVar(&orphanedOnly, "orphaned", false,
		"only grants that are both dormant and unknown to the identity provider")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/access/identities"},
		Output: output{Mode: outputDocument, Kind: "identitySurvey"},
		Needs:  needs{Auth: true, Platform: accessInTheDashboard()},
		Examples: []example{
			{"Every grant on the platform", "kitchen access identities --json"},
			{"The ones that look like they belong to nobody", "kitchen access identities --orphaned --json"},
		},
	})
}

// flags words what is odd about a grant, in the fewest words that stay
// accurate. An empty cell is a grant with nothing to say about it.
func flags(row identity) string {
	words := []string{}
	if row.Orphaned {
		words = append(words, "orphaned")
	} else {
		if row.Inactive {
			words = append(words, "dormant")
		}
		if row.Unknown {
			words = append(words, "no account")
		}
	}
	return strings.Join(words, ", ")
}

func newAccessReviewsCommand(r *Runtime) *cobra.Command {
	var historical bool

	cmd := &cobra.Command{
		Use:     "reviews",
		Aliases: []string{"review"},
		Short:   "The recertification cycles: open by default, everything with --historical",
		Long: strings.TrimSpace(`
The recertification register, newest first.

Open cycles by default — the ones somebody still owes decisions on.
--historical adds the closed ones, which are retained on purpose: the register
is the answer to "when was access last reviewed here, by whom, and what did
they decide", not only to "what is open now".

A cycle reads Overdue when its due date has passed with decisions still
outstanding. Nothing is revoked and no deployment is refused because of it;
what an overdue cycle costs is that somebody has to look.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			query := url.Values{}
			if historical {
				query.Set("historical", "true")
			}
			found, err := client.accessReviews(ctx, query)
			if err != nil {
				return err
			}
			answer := list[accessReview]{Items: found}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(found) == 0 {
					if historical {
						return "No recertification cycle has ever been opened here.\n"
					}
					return "No recertification is open. (--historical lists the closed ones.)\n"
				}
				rows := make([][]string, 0, len(found))
				for _, review := range found {
					rows = append(rows, []string{
						review.Name, review.Scope, review.Phase,
						strconv.Itoa(int(review.Pending)),
						strconv.Itoa(int(review.Confirmed)),
						strconv.Itoa(int(review.Revoked)),
						review.DueBy.Local().Format("2006-01-02"),
					})
				}
				return s.Table([]string{"NAME", "SCOPE", "PHASE", "PENDING",
					"CONFIRMED", "REVOKED", "DUE"}, rows)
			})
		}),
	}
	cmd.Flags().BoolVar(&historical, "historical", false, "include closed cycles")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/access/reviews"},
		Output: output{Mode: outputDocument, Kind: "accessReviewList"},
		Needs:  needs{Auth: true, Platform: accessInTheDashboard()},
		Examples: []example{
			{"What is open", "kitchen access reviews --json"},
			{"The whole register", "kitchen access reviews --historical --json"},
		},
	})
}

func newAccessShowCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <name>",
		Short: "One recertification cycle whole: the snapshot, every decision, the artefact",
		Long: strings.TrimSpace(`
Read one cycle whole: what was in scope when it opened, what was decided about
each grant and by whom, which decisions were self-reviews, which revocations
were actually carried out — and, for a closed cycle, the retained artefact.

The artefact is a signed statement kept in the telemetry store rather than on
the object, so it survives the object. Its "message" is what to read when a
cycle closed without one: the decisions still stand and are still in the audit
log, but there is nothing portable to hand an auditor.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			found, err := client.accessReview(ctx, args[0])
			if err != nil {
				return err
			}
			return r.printer().document(found, func(s tui.Styles) string {
				lines := []string{
					found.Name + ": " + found.Phase,
					"scope    " + found.Scope,
					"opened   " + found.OpenedBy,
					"due      " + found.DueBy.Local().Format("2006-01-02 15:04"),
					"reviewers " + strings.Join(found.Reviewers, ", "),
				}
				if found.ClosedBy != "" {
					lines = append(lines, "closed   "+found.ClosedBy)
				}
				if found.SelfReviewed > 0 {
					lines = append(lines,
						"self-reviewed "+strconv.Itoa(int(found.SelfReviewed))+" (recorded, not refused)")
				}
				if found.Artifact != nil && found.Artifact.Message != "" {
					lines = append(lines, "artefact "+found.Artifact.Message)
				} else if found.Artifact != nil && found.Artifact.RecordID != "" {
					lines = append(lines, "artefact "+found.Artifact.RecordID+" ("+found.Artifact.Subject+")")
				}

				rows := make([][]string, 0, len(found.Entries))
				for _, entry := range found.Entries {
					decision := entry.Decision
					if decision == "" {
						decision = "undecided"
					}
					if entry.SelfReview {
						decision += " (self)"
					}
					if entry.Decision == "revoke" && !entry.Applied && entry.ApplyMessage != "" {
						decision += " — not applied"
					}
					name := entry.Email
					if name == "" {
						name = entry.Subject
					}
					rows = append(rows, []string{name, entry.Grant, entry.Role, decision, entry.DecidedBy})
				}
				return strings.Join(lines, "\n") + "\n\n" +
					s.Table([]string{"ACCOUNT", "GRANT", "ROLE", "DECISION", "DECIDED BY"}, rows)
			})
		}),
	}

	return describe(cmd, meta{
		Calls:    []string{"GET /api/v1/access/reviews/{name}"},
		Output:   output{Mode: outputDocument, Kind: "accessReview"},
		Needs:    needs{Auth: true, Platform: accessInTheDashboard()},
		Examples: []example{{"One cycle whole", "kitchen access show access-review-8x2kd --json"}},
	})
}
