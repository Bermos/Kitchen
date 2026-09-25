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
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// The architecture overview, from a terminal.
//
// The dashboard draws it; this answers it as data. The graph is an edge list
// with the nodes beside it, which is the shape that pipes into anything — a
// `jq` over the edges answers "who depends on this" without the diagram — and
// `--traffic` adds the flow collector's edges laid over the same nodes, each
// saying whether a declared edge explains it.

func newTopologyCommand(r *Runtime) *cobra.Command {
	var traffic string

	cmd := &cobra.Command{
		Use:   "topology",
		Short: "What the projects are made of and what they depend on",
		Long: strings.TrimSpace(`
The architecture overview: every environment, resource, offering, domain and
connection of the projects you can see, and the edges between them — an
environment using a resource, binding another project's offering, published
at a domain.

The global --project narrows to one project and everything one edge away from
it. --traffic adds what the platform observed over that window: each talking
pair, attributed onto the same nodes, and whether a declared edge explains it.
An "undeclared" pair is one project's workload calling another's with nothing
saying it may.

Nothing here is written; the graph is read off the objects it is made of, and
is changed by changing them. See docs/api/topology.md.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			project := strings.TrimSpace(r.projectFlag)
			answer, err := client.topology(ctx, queryOf([2]string{"project", project}))
			if err != nil {
				return err
			}
			if traffic != "" {
				since, err := instant(traffic)
				if err != nil {
					return err
				}
				observed, err := client.topologyTraffic(ctx,
					queryOf([2]string{"project", project}, [2]string{"since", since}))
				if err != nil {
					return err
				}
				answer.Traffic = observed
			}
			return r.printer().document(answer, func(s tui.Styles) string {
				return renderTopology(s, answer)
			})
		}),
	}
	cmd.Flags().StringVar(&traffic, "traffic", "",
		"also read the observed traffic since this: an RFC 3339 timestamp, or a duration ago such as 15m")

	return describe(cmd, meta{
		Calls:  []string{"GET /api/v1/topology", "GET /api/v1/topology/traffic"},
		Output: output{Mode: outputDocument, Kind: "topology"},
		Needs:  needs{Auth: true},
		Examples: []example{
			{"The whole graph as data", "kitchen topology --json"},
			{"Who calls whom without a binding, in the last hour",
				`kitchen topology --traffic 1h --json | jq '.traffic.edges[] | select(.status == "undeclared")'`},
			{"One project and its neighbours", "kitchen topology -p shop"},
		},
	})
}

// renderTopology draws the graph as the edges it is, one per line, with the
// nodes named the way the dashboard names them.
func renderTopology(s tui.Styles, answer *topology) string {
	names := map[string]string{}
	for _, node := range answer.Nodes {
		names[node.ID] = topologyName(node)
	}
	if answer.Traffic != nil {
		for _, node := range answer.Traffic.Nodes {
			names[node.ID] = topologyName(node)
		}
	}
	name := func(id string) string {
		if name, ok := names[id]; ok {
			return name
		}
		return id
	}

	lines := []string{}
	if len(answer.Edges) == 0 {
		lines = append(lines, "Nothing depends on anything yet.")
	} else {
		rows := make([][]string, 0, len(answer.Edges))
		for _, edge := range answer.Edges {
			state := edge.State
			if edge.Reason != "" {
				state += " — " + edge.Reason
			}
			rows = append(rows, []string{name(edge.From), edge.Kind, name(edge.To), state})
		}
		lines = append(lines, s.Table([]string{"FROM", "EDGE", "TO", "STATE"}, rows))
	}

	if answer.Traffic != nil {
		lines = append(lines, "", s.Title.Render("Observed")+"  "+s.Subtle.Render(
			answer.Traffic.Since.Local().Format("15:04")+" – "+answer.Traffic.Until.Local().Format("15:04")))
		if len(answer.Traffic.Edges) == 0 {
			lines = append(lines, "No flow data in this window.")
		} else {
			rows := make([][]string, 0, len(answer.Traffic.Edges))
			for _, edge := range answer.Traffic.Edges {
				status := edge.Status
				if status == "undeclared" {
					status = s.Warn.Render(status)
				}
				errors := "—"
				if edge.Errors > 0 {
					errors = s.Bad.Render(fmt.Sprint(edge.Errors))
				}
				rows = append(rows, []string{name(edge.From), name(edge.To), status,
					fmt.Sprintf("%.2f/s", edge.RPS), errors})
			}
			lines = append(lines, s.Table([]string{"FROM", "TO", "STATUS", "RATE", "5XX"}, rows))
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// topologyName is a node as a person would say it: an offering by its
// project, since two projects may offer the same name.
func topologyName(node topologyNode) string {
	if node.Kind == "offering" {
		return node.Project + "/" + node.Name
	}
	return node.Name
}
