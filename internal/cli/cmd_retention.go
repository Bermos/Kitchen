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
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// The retention model, from a terminal.
//
// This is the read, and it is the one worth having: "how long do you keep
// container logs, and can you show me nothing older than that is there" is a
// question asked from outside the platform, usually with a deadline attached,
// and `kitchen retention --json` is a complete answer to it in one call —
// every class, the rule in force, where the number came from, and the oldest
// row the last sweep found.
//
// Changing it stays with `kitchen api`, deliberately. A retention change is
// rare, it is a records decision rather than an operational one, and putting
// the audit floor's override behind a flag would make typing it easier than
// thinking about it:
//
//	kitchen api PATCH /platform/retention --data '{"buildLogs": 90}'
//	kitchen api PATCH /platform/retention --data '{"audit": 60,
//	  "auditFloorOverride": {"reason": "...", "approvedBy": "..."}}'
//
// See docs/api/platform.md for the bodies and what the floor refuses.

func newRetentionCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "retention",
		Short: "How long the platform keeps each class, and how far back each one goes",
		Long: strings.TrimSpace(`
The platform's retention model: one row per class of what it keeps.

Each row is the rule in force, where that number came from, and what the last
retention sweep measured — how much the class holds and the oldest row that
survived the horizon. The oldest column is the claim retention actually
makes: nothing of this class is older than this.

The audit class has a documented floor of 90 days. An installation keeping
less than that has an override on record with a reason and an approver, and
this prints it — an override nobody can see is not an override, it is a
setting.

Changing any of it is left to the API, which is where the bodies are spelled
out in full:
  kitchen api PATCH /platform/retention --data '{"buildLogs": 90}'
See docs/api/platform.md.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			answer, err := client.platformRetention(ctx)
			if err != nil {
				return err
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				return renderRetention(s, answer)
			})
		}),
	}

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/platform/retention"},
		Output: output{Mode: outputDocument, Kind: "retention"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"Every class, with what the last sweep measured", "kitchen retention --json"},
			{"Keep build logs for 90 days",
				`kitchen api PATCH /platform/retention --data '{"buildLogs": 90}'`},
		},
	})
}

// renderRetention draws the table a person reads.
func renderRetention(s tui.Styles, answer *retention) string {
	lines := []string{}
	if answer.Message != "" {
		// Said first: a table of retentions nothing is enforcing means
		// something quite different from the same table under a store.
		lines = append(lines, answer.Message, "")
	}

	rows := make([][]string, 0, len(answer.Classes))
	for _, class := range answer.Classes {
		oldest := "—"
		if class.Oldest != nil {
			oldest = class.Oldest.Local().Format("2006-01-02")
		}
		rows = append(rows, []string{
			class.Class,
			strconv.Itoa(int(class.Days)) + "d",
			class.Source,
			oldest,
			strconv.FormatInt(class.Rows, 10),
			enforcedLabel(class),
		})
	}
	lines = append(lines, s.Table([]string{"CLASS", "KEPT", "SET BY", "OLDEST", "ROWS", "STATE"}, rows))

	if answer.LastSweep != nil {
		lines = append(lines, "", "last swept "+answer.LastSweep.Local().Format("2006-01-02 15:04:05"))
	}
	if answer.AuditFloorOverridden && answer.AuditFloorOverride != nil {
		lines = append(lines, "",
			"audit records are kept for less than the "+
				strconv.Itoa(int(answer.AuditFloorDays))+"-day floor, on "+
				answer.AuditFloorOverride.ApprovedBy+"'s authority:",
			"  "+answer.AuditFloorOverride.Reason)
	}
	return strings.Join(lines, "\n") + "\n"
}

func enforcedLabel(class retentionClass) string {
	if class.Message != "" {
		return class.Message
	}
	if !class.Enforced {
		return "not measured"
	}
	if class.Expired > 0 {
		// Normal in small numbers — a day-partitioned table keeps at most the
		// partition the horizon falls inside — and worth showing, because a
		// number that stays large is the store holding data past its date.
		return "enforced, " + strconv.FormatInt(class.Expired, 10) + " past the horizon"
	}
	return "enforced"
}
