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
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen processes` — the project's workloads besides its web process.
//
// A process is read per *environment*, never per project, and that is not an
// accident of which endpoint was available: what an environment runs is its
// release's process list, so an environment on an older release runs that
// release's processes. A project-shaped answer would describe something that
// may not be running anywhere.
//
// Declaring them is deliberately not here. The list is a project setting
// alongside the port and the replica count, written through
// PATCH /projects/{name}, and a flag-shaped spelling of a list of records with
// commands, schedules and resources in it would be worse than the JSON body:
// `kitchen api PATCH /projects/shop --data @processes.json` is the honest
// form, and it is in the examples below so that nobody has to work it out.
//
// The same decision covers the rest of that body — the health check, the
// command and arguments, the singleton and not-request-driven declarations.
// docs/CLI.md records it in the decisions table rather than leaving it to be
// inferred from the absence of a command.

func newProcessesCommand(r *Runtime) *cobra.Command {
	var environmentName string

	cmd := &cobra.Command{
		Use:     "processes",
		Aliases: []string{"process", "ps"},
		Short:   "The workloads an environment runs besides its web process",
		Long: strings.TrimSpace(`
List what an environment runs besides its web process.

A worker runs continuously and is never addressed — no URL, no route. A service
runs continuously and *is* addressed, by the rest of this environment and by
nothing outside the cluster: its siblings read its address as
KITCHEN_SERVICE_<NAME>, with _HOST and _PORT beside it. A scheduled job runs on
a cron expression, in UTC, and each firing is a run with its own logs.

Publishing stays the exception the project declares. The web process is the one
workload with a URL; nothing here gets a route, and a service that should be on
the internet is the web process.

What is listed is the *release's* workload list, so an environment that has been
rolled back lists the workloads that release declared — and, for one built from
its own directory, the image that release built. A preview lists the project's
whole list, with the ones it does not run marked suspended: a worker and a
scheduled job run in previews only if they were opted in, and a service runs
unless it was opted out. That default is what makes a preview of a several-
workload unit the whole unit rather than a quarter of it.

Changing the list is a project setting, written with the rest of them:

  kitchen api PATCH /projects/shop --data '{"processes":[
    {"name":"worker","type":"worker","command":["node","worker.js"],"replicas":2,
     "health":{"path":"/healthz","port":9000}},
    {"name":"api","type":"service","port":8080,
     "build":{"rootDirectory":"services/api"}},
    {"name":"nightly-report","type":"cron","schedule":"0 3 * * *","command":["node","report.js"]}
  ]}'

A worker's health check must name the port it is made against, because a worker
publishes none of its own; a service falls back to its own port; a scheduled
process takes no health check at all, since how a run went is its exit status.

A workload with a "build" is built from its own directory of the repository, so
one commit produces several images that ship as one release and roll back
together. Without one it runs the project's image with another command, which
is one build rather than two. The strategy is dockerfile or buildpacks — there
is no auto for a workload — and a scheduled process is refused a build: give it
to the worker or service that ships the image and run the schedule on that.

"dockerfileTarget" on that build is which stage of the workload's Dockerfile to
ship, which is the case one multi-stage file yielding an API, a worker and a
migration runner is for. A workload that names none is built to the project's
stage, not to the file's last one; a stage on a buildpacks workload fails the
build naming that workload. What each image was actually built to is on the
build: kitchen builds <name> --json reports it per workload.

A workload that must never run twice — a poller, a scheduler, an ingest loop —
says so with "singleton": true, which deploys it by stopping the old copy
before starting the new one instead of overlapping the two. It refuses more
than one replica, and a scheduled process is refused it: whether two of its
runs may overlap is concurrencyPolicy.

It replaces the whole list, and it reaches an environment through the next
release — what is running keeps its own workloads until something builds.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			name, err := environmentFor(ctx, r, client, environmentName)
			if err != nil {
				return err
			}
			processes, err := client.environmentProcesses(ctx, name)
			if err != nil {
				return err
			}
			answer := list[process]{Items: processes}
			return r.printer().document(answer, func(s tui.Styles) string {
				return renderProcesses(s, processes)
			})
		}),
	}
	cmd.Flags().StringVarP(&environmentName, "environment", "e", "",
		"the environment to read. The default is the project's production environment")
	cmd.AddCommand(newProcessRunsCommand(r), newProcessRunCommand(r))

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/environments/{name}/processes",
			"GET /api/v1/projects/{name}",
		},
		Output: output{Mode: outputDocument, Kind: "processList"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"What production runs besides the web process", "kitchen processes --json"},
			{"A preview's, including what it will not run", "kitchen processes --environment shop-pr-42 --json"},
			{"Where one workload's siblings reach it",
				"kitchen processes --json | jq '.items[] | select(.address) | {name, address}'"},
		},
	})
}

func newProcessRunsCommand(r *Runtime) *cobra.Command {
	var (
		environmentName string
		limit           int
	)

	cmd := &cobra.Command{
		Use:   "runs PROCESS",
		Short: "A scheduled job's recent runs, newest first",
		Long: strings.TrimSpace(`
List the recent runs of one scheduled job.

What is listed is what the cluster still holds: the platform keeps the last few
finished runs of a schedule and collects the rest. A run's *output* outlives it
by the whole container-log retention, so a run that has been collected can
still be read:

  kitchen logs --run <name>

A worker and a service have no runs — they are already running, and their
replicas are on "kitchen processes".`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			name, err := environmentFor(ctx, r, client, environmentName)
			if err != nil {
				return err
			}
			runs, err := client.processRuns(ctx, name, args[0])
			if err != nil {
				return err
			}
			if limit > 0 && len(runs) > limit {
				runs = runs[:limit]
			}
			answer := list[processRun]{Items: runs}
			return r.printer().document(answer, func(s tui.Styles) string {
				if len(runs) == 0 {
					return "No runs yet.\n"
				}
				rows := make([][]string, 0, len(runs))
				for _, run := range runs {
					rows = append(rows, []string{
						run.Name, s.Phase(run.Phase), runStarted(run), runTook(run), run.Message,
					})
				}
				return s.Table([]string{"RUN", "PHASE", "STARTED", "TOOK", "MESSAGE"}, rows)
			})
		}),
	}
	cmd.Flags().StringVarP(&environmentName, "environment", "e", "",
		"the environment to read. The default is the project's production environment")
	cmd.Flags().IntVarP(&limit, "limit", "n", 20, "how many to show, newest first. 0 shows all of them")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/environments/{name}/processes/{process}/runs",
			"GET /api/v1/projects/{name}",
		},
		Output: output{Mode: outputDocument, Kind: "processRunList"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"The nightly report's last runs", "kitchen processes runs nightly-report --json"},
		},
	})
}

func newProcessRunCommand(r *Runtime) *cobra.Command {
	var environmentName string

	cmd := &cobra.Command{
		Use:   "run PROCESS",
		Short: "Run a scheduled job now, off its schedule",
		Long: strings.TrimSpace(`
Start one run of a scheduled job immediately.

It is a copy of what the schedule would have run — the same image, command,
resources and timeout — so this is the schedule firing early, not a different
thing that happens to look like it. The run's concurrency policy still applies:
a job set to Forbid that is already running gets a second run that the
scheduler drops.

It answers as soon as the run is created, not when it finishes. Follow it with:

  kitchen logs --run <name> --follow`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			client, err := r.client()
			if err != nil {
				return err
			}
			ctx, cancel := r.context(commandContext(cmd))
			defer cancel()

			name, err := environmentFor(ctx, r, client, environmentName)
			if err != nil {
				return err
			}
			started, err := client.startProcessRun(ctx, name, args[0])
			if err != nil {
				return err
			}
			return r.printer().document(started, func(s tui.Styles) string {
				return fmt.Sprintf("Started %s.\n%s\n",
					s.OK.Render(started.Name),
					s.Subtle.Render("kitchen logs --run "+started.Name+" --follow"))
			})
		}),
	}
	cmd.Flags().StringVarP(&environmentName, "environment", "e", "",
		"the environment to run it in. The default is the project's production environment")

	return describe(cmd, meta{
		Calls: []string{
			"POST /api/v1/environments/{name}/processes/{process}/runs",
			"GET /api/v1/projects/{name}",
		},
		Output: output{Mode: outputDocument, Kind: "processRun"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"Run the nightly report now", "kitchen processes run nightly-report --json"},
		},
	})
}

// environmentFor resolves which environment a process command is about: the
// one named, or the linked project's production environment. It is the same
// default `kitchen logs` takes, and for the same reason — in a checkout, the
// environment somebody means is the one their branch deploys to.
func environmentFor(ctx context.Context, r *Runtime, c *client, named string) (string, error) {
	if named != "" {
		return named, nil
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
	return found.ProductionEnvironment, nil
}

// renderProcesses is the list as a person reads it: one line each, with the
// column that matters for its type — a worker's readiness, a schedule's last
// run — folded into one STATE column so the two kinds sit in one table.
func renderProcesses(s tui.Styles, processes []process) string {
	if len(processes) == 0 {
		return "No workloads besides the web process.\n" +
			s.Subtle.Render("`kitchen processes --help` says how to declare one.") + "\n"
	}
	rows := make([][]string, 0, len(processes))
	for _, p := range processes {
		rows = append(rows, []string{p.Name, p.Type, processSchedule(p), processState(s, p), processNote(p)})
	}
	// The last column is headed NOTE rather than LAST RUN because a worker
	// has no runs and still has something to say there — that it must never
	// run twice — and a suspended process has only ever put its reason in it.
	return s.Table([]string{"NAME", "TYPE", "SCHEDULE", "STATE", "NOTE"}, rows)
}

func processSchedule(p process) string {
	if p.Schedule != "" {
		return p.Schedule
	}
	return noValue
}

// processState is the one word the row is scanned for. A suspended process is
// neither healthy nor broken — it is not running on purpose — so it says so
// rather than being painted either colour.
func processState(s tui.Styles, p process) string {
	switch {
	case p.Suspended:
		return s.Subtle.Render("suspended")
	case p.Type == "cron":
		if !p.Healthy {
			return s.Bad.Render("failing")
		}
		if p.Active > 0 {
			return s.Warn.Render("running")
		}
		return s.OK.Render("scheduled")
	case p.ReadyReplicas < p.Replicas:
		return s.Bad.Render(strconv.Itoa(int(p.ReadyReplicas)) + "/" + strconv.Itoa(int(p.Replicas)) + " ready")
	default:
		return s.OK.Render(strconv.Itoa(int(p.ReadyReplicas)) + "/" + strconv.Itoa(int(p.Replicas)) + " ready")
	}
}

// processNote is the last thing that happened to a scheduled process, the
// reason a suspended one is not running, where a service answers, and — for a
// worker, which has none of those — the one thing about it a replica count
// does not say: that two of it must never run at once, so a deploy stops the
// old copy first.
func processNote(p process) string {
	if p.Suspended {
		return p.Reason
	}
	if p.LastRun == nil {
		// A service's address is what somebody wiring two workloads together
		// came here for, and no other column carries it.
		if p.Address != "" {
			return p.Address
		}
		if p.Singleton {
			return "never two at once: deploys stop the old copy first"
		}
		return ""
	}
	note := strings.ToLower(p.LastRun.Phase) + " " + runStarted(*p.LastRun)
	if p.LastRun.Message != "" {
		note += " — " + p.LastRun.Message
	}
	return note
}

func runStarted(run processRun) string {
	if run.StartedAt == nil {
		return noValue
	}
	return since(*run.StartedAt)
}

func runTook(run processRun) string {
	if run.DurationSeconds == nil {
		return noValue
	}
	return strconv.FormatFloat(*run.DurationSeconds, 'f', 1, 64) + "s"
}
