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
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen logs` — the lines, from a build or from something running.
//
// Both come out of the same store and the same endpoint shape, and both follow
// the same way: `Accept: text/event-stream` turns the bounded page into the
// page followed by everything that arrives after it. So --follow is one header
// rather than a second code path, and a line is the same object either way.

func newLogsCommand(r *Runtime) *cobra.Command {
	var (
		environmentName string
		buildName       string
		follow          bool
		limit           int
		since           string
		until           string
		search          string
		container       string
		processName     string
		runName         string
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Read an environment's or a build's logs",
		Long: strings.TrimSpace(`
Read the logs of something the platform is running, or of a build.

With neither --environment nor --build it reads the linked project's production
environment. Lines come back oldest first, newest kept: a log reads forwards,
and --limit keeps the last N of them.

--follow leaves the connection open and prints every line that arrives after
the page, until the command is interrupted or --timeout runs out.

--since and --until take an RFC 3339 timestamp or a duration ("15m", "2h"),
which is read as "that long ago".

--process narrows an environment's lines to one of the project's workers or
scheduled jobs, and --run to one firing of a schedule. A run's output outlives
the run: the platform stops keeping the Job long before it stops keeping the
lines, so last month's failed report is still readable by name.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			return readLogs(commandContext(cmd), r, logOptions{
				environment: environmentName, build: buildName, follow: follow,
				limit: limit, since: since, until: until, search: search, container: container,
				process: processName, run: runName,
			})
		}),
	}

	flags := cmd.Flags()
	flags.StringVarP(&environmentName, "environment", "e", "",
		"the environment to read. The default is the project's production environment")
	flags.StringVarP(&buildName, "build", "b", "", "a build to read instead of an environment")
	flags.BoolVarP(&follow, "follow", "f", false, "keep the connection open and print lines as they arrive")
	flags.IntVarP(&limit, "limit", "n", 200, "how many lines the page holds, newest kept. The API caps it at 5000")
	flags.StringVar(&since, "since", "", "only lines after this: an RFC 3339 timestamp, or a duration ago such as 15m")
	flags.StringVar(&until, "until", "", "only lines before this: an RFC 3339 timestamp, or a duration ago")
	flags.StringVar(&search, "search", "", "only lines whose message contains this, case-insensitively")
	flags.StringVar(&container, "container", "", "only one container of the pod")
	flags.StringVar(&processName, "process", "",
		"only one of the project's workers or scheduled jobs. `kitchen processes` lists them")
	flags.StringVar(&runName, "run", "",
		"only one run of a scheduled job. `kitchen processes runs <name>` lists them")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/environments/{name}/logs",
			"GET /api/v1/builds/{name}/logs",
			"GET /api/v1/projects/{name}",
		},
		Output: output{
			Mode: outputStream, Kind: "logLine",
			Note: "one JSON object per line, oldest first, in both the bounded and the followed form",
		},
		Needs: needs{Auth: true, Project: true},
		Examples: []example{
			{"The last 100 lines of production, as JSON",
				"kitchen logs --limit 100 --json"},
			{"Follow a preview environment for ten minutes",
				"kitchen logs --environment shop-pr-42 --follow --timeout 10m --json"},
			{"Errors in the last hour", "kitchen logs --since 1h --search error --json"},
			{"A build's output", "kitchen logs --build shop-bld-abc123def456-xk2p9 --json"},
			{"What last night's report job printed",
				"kitchen logs --process nightly-report --run shop-production-nightly-report-29387520 --json"},
		},
	})
}

// logOptions is what the flags said.
type logOptions struct {
	environment string
	build       string
	follow      bool
	limit       int
	since       string
	until       string
	search      string
	container   string
	process     string
	run         string
}

func readLogs(parent context.Context, r *Runtime, options logOptions) error {
	if options.environment != "" && options.build != "" {
		return fail(codeUsage, "--environment and --build name two different things to read").
			withHint("pass one of them")
	}

	client, err := r.client()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	path, err := logPath(ctx, r, client, options)
	if err != nil {
		return err
	}
	since, err := instant(options.since)
	if err != nil {
		return err
	}
	until, err := instant(options.until)
	if err != nil {
		return err
	}

	query := queryOf(
		[2]string{"limit", strconv.Itoa(options.limit)},
		[2]string{"since", since},
		[2]string{"until", until},
		[2]string{"search", options.search},
		[2]string{"container", options.container},
		[2]string{"process", options.process},
		[2]string{"run", options.run},
	)

	printer := r.printer()
	write := func(line logLine) error {
		return printer.event(line, func(s tui.Styles) string { return renderLogLine(s, line, true) })
	}

	if options.follow {
		err := client.followLogs(ctx, path, query, write)
		if ctx.Err() != nil {
			// A tail ends because somebody stopped it or the clock ran out.
			// Neither is a failure of the tail.
			return interrupted(ctx, "the logs are still being written")
		}
		return err
	}

	lines, err := client.logsPage(ctx, path, query)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if err := write(line); err != nil {
			return err
		}
	}
	return nil
}

// logPath resolves what is being read. The default — the project's production
// environment — is the one a person means when they type `kitchen logs` in a
// checkout, and it is one read to find out.
func logPath(ctx context.Context, r *Runtime, c *client, options logOptions) (string, error) {
	switch {
	case options.build != "":
		return "/builds/" + options.build + "/logs", nil
	case options.environment != "":
		return "/environments/" + options.environment + "/logs", nil
	}

	name, err := r.projectName()
	if err != nil {
		return "", err
	}
	found, err := c.project(ctx, name)
	if err != nil {
		return "", err
	}
	if found.ProductionEnvironment == "" {
		return "", failf(codeNotFound, "%s has no production environment yet", name).
			withHint("it appears once a build of " + found.ProductionBranch +
				" has landed; `kitchen environments` lists what there is, and --environment reads one")
	}
	return "/environments/" + found.ProductionEnvironment + "/logs", nil
}

// instant reads what a flag said about a moment: a timestamp as the API wants
// it, or a duration read as "that long ago", which is what somebody types when
// they mean the last ten minutes.
func instant(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if at, err := time.Parse(time.RFC3339, value); err == nil {
		return at.UTC().Format(time.RFC3339), nil
	}
	ago, err := time.ParseDuration(value)
	if err != nil {
		return "", failf(codeUsage, "%q is neither an RFC 3339 timestamp nor a duration", value).
			withHint("write 2026-08-19T09:00:00Z, or 15m for fifteen minutes ago")
	}
	if ago < 0 {
		ago = -ago
	}
	return time.Now().Add(-ago).UTC().Format(time.RFC3339), nil
}

// renderLogLine is one line as a person reads it: the time, the severity when
// the line had one, and the message. `withSource` adds which container it came
// from, which matters when the lines are an environment's (several pods) and
// does not when they are one build's.
func renderLogLine(s tui.Styles, line logLine, withSource bool) string {
	parts := []string{s.Subtle.Render(line.Timestamp.Local().Format("15:04:05"))}
	if withSource && line.Container != "" {
		parts = append(parts, s.Subtle.Render(line.Container))
	}
	if line.Level != "" {
		parts = append(parts, s.Level(line.Level))
	}
	parts = append(parts, line.Message)
	return strings.Join(parts, " ")
}
