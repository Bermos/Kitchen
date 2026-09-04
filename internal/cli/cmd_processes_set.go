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

// `kitchen processes set` and `kitchen processes rm` — declaring a project's
// workloads from a terminal (#310).
//
// #299 filed no command for this on purpose: the list is a project setting on
// PATCH /projects/{name}, a repository declares it in kitchen.json, and a
// flag-shaped spelling of a list of records was worse than the JSON body.
//
// **That reasoning holds exactly as long as there is a file.** A project whose
// source is an image has no repository, so it has no kitchen.json, and the
// API, the dashboard and this command are not a fallback for it — they are the
// only route. "Reachable through `kitchen api`" is a lower bar than the
// platform sets for anything a person does routinely, so these two commands
// exist and the decision is recorded in docs/CLI.md rather than left to be
// inferred from an absence.
//
// They are shaped to keep #299's objection answered rather than to ignore it:
// one workload at a time, named by its argument, with the rest of the list read
// and sent back untouched. Nothing here composes a list of records on a command
// line — `kitchen api PATCH /projects/<name> --data @processes.json` still does
// that, and is still the honest form for declaring six workloads at once.
//
// The whole list travels on every write, because the route replaces it. That is
// the same read-modify-write `kitchen env set` does, and it has the same
// property: the workloads this command is not changing go back exactly as they
// came, which is why the project view had to report `replicas: 0` and a
// workload's declared `previews` before this could exist.

// processEdits is what `processes set` was asked to change about one workload.
// Every field is a flag, and a flag nobody passed changes nothing — which is
// what `cobra.Flags().Changed` answers and why the values are read through it
// rather than compared to a zero value somebody might have typed on purpose.
type processEdits struct {
	kind        string
	command     []string
	args        []string
	port        int32
	replicas    int32
	singleton   bool
	cpu         string
	memory      string
	schedule    string
	concurrency string
	timeout     string
	previews    string

	image           string
	imageConnection string

	buildRoot       string
	buildStrategy   string
	buildDockerfile string
	buildTarget     string

	healthPath            string
	healthPort            int32
	healthPeriod          int32
	healthTimeout         int32
	healthFailures        int32
	healthStartupFailures int32
	noHealth              bool
}

// The three answers --previews takes. A workload that has said nothing takes
// its type's default — off for a worker and a scheduled job, on for a service
// and a task — and `default` is how a declaration is taken back off, which a
// bool flag cannot say.
const (
	previewsYes     = "yes"
	previewsNo      = "no"
	previewsDefault = "default"
)

func newProcessSetCommand(r *Runtime) *cobra.Command {
	edits := processEdits{}

	cmd := &cobra.Command{
		Use:   "set NAME",
		Short: "Declare or change one of the project's workloads",
		Long: strings.TrimSpace(`
Declare a workload of this project, or change one it already has.

The workload list is a project setting, and this rewrites one entry of it: the
project is read, the entry named by NAME is added or edited, and the whole list
goes back. Every other workload is sent exactly as it was read, so nothing here
can lose one.

A new workload needs --type; an existing one keeps whatever it has for every
flag that is not passed.

  kitchen processes set worker --type worker --command node --command worker.js
  kitchen processes set api --type service --port 8080 --build-root services/api
  kitchen processes set cache --type service --port 6379 --image docker.io/library/redis:7.4
  kitchen processes set nightly --type cron --schedule "0 3 * * *" --timeout 30m
  kitchen processes set worker --replicas 0

--command and --arg are exec form: one word per occurrence, never a shell line,
so an argument with a space in it is one --arg and nothing is split or quoted.
Passing either replaces the whole list; passing it once with an empty string
clears it.

--image and --build-root are answers to one question and each clears the other:
a workload is built from this repository or it runs an image somebody else
published, never both. --replicas 0 is a workload declared and parked, which is
how one is turned off without losing its command.

**This is the whole surface for a project with no repository.** A project whose
source is an image has no kitchen.json to declare workloads in, so this command
and the dashboard are how its unit is described at all. On a project that has a
repository the file still wins: it is read at every build and its "processes"
replace the project's, so a change made here holds until the next build. Change
the file instead — "kitchen builds <name>" reports which settings it declares.

The list reaches an environment through the next release, like the port and the
replica count: what is running keeps the workloads its own release declared.`),
		Args: cobra.ExactArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return setProcess(commandContext(cmd), r, args[0], edits, cmd.Flags().Changed)
		}),
	}

	flags := cmd.Flags()
	flags.StringVar(&edits.kind, "type", "", "worker, service, cron or task. Required for a workload the project does not have yet")
	flags.StringArrayVar(&edits.command, "command", nil, "one word of the command that replaces the image's entrypoint, repeatable")
	flags.StringArrayVar(&edits.args, "arg", nil, "one argument, repeatable. Exec form: never split, never quoted")
	flags.Int32Var(&edits.port, "port", 0, "the port a service listens on, and the port its siblings reach it on")
	flags.Int32Var(&edits.replicas, "replicas", 1, "how many copies of a worker or a service run. 0 parks it")
	flags.BoolVar(&edits.singleton, "singleton", false, "two of this workload must never run at once, so deploys stop the old copy first")
	flags.StringVar(&edits.cpu, "cpu", "", "the CPU one replica, or one run, asks for — a Kubernetes quantity such as 250m")
	flags.StringVar(&edits.memory, "memory", "", "the memory one replica, or one run, asks for — 512Mi")
	flags.StringVar(&edits.schedule, "schedule", "", "the five-field cron expression a scheduled job runs on, read in UTC")
	flags.StringVar(&edits.concurrency, "concurrency", "", "what happens when a run is due and the last one has not finished: Allow, Forbid or Replace")
	flags.StringVar(&edits.timeout, "timeout", "", "how long one run of a scheduled job or a deploy task may take, as a duration: 30m")
	flags.StringVar(&edits.previews, "previews", "", "whether it runs in preview environments: yes, no, or default for its type's own answer")
	flags.StringVar(&edits.image, "image", "", "an image this platform did not build: repository:tag, or repository@sha256:...")
	flags.StringVar(&edits.imageConnection, "image-connection", "", "the connection that image is pulled with. Leave it out for a public image")
	flags.StringVar(&edits.buildRoot, "build-root", "", "the directory of the repository this workload is built from")
	flags.StringVar(&edits.buildStrategy, "build-strategy", "", "how that directory is built: auto, dockerfile or buildpacks")
	flags.StringVar(&edits.buildDockerfile, "build-dockerfile", "", "the Dockerfile, relative to this workload's own build root")
	flags.StringVar(&edits.buildTarget, "build-target", "", "the stage of that Dockerfile to ship")
	flags.StringVar(&edits.healthPath, "health-path", "", "the path the platform asks this workload before calling it working. Empty is a plain connect")
	flags.Int32Var(&edits.healthPort, "health-port", 0, "the port that check is made against. A worker publishes none of its own, so it has to say")
	flags.Int32Var(&edits.healthPeriod, "health-period", 0, "seconds between checks. 0 takes the platform's own")
	flags.Int32Var(&edits.healthTimeout, "health-timeout", 0, "seconds one check may take. 0 takes the platform's own")
	flags.Int32Var(&edits.healthFailures, "health-failures", 0, "failed checks before it is restarted. 0 takes the platform's own")
	flags.Int32Var(&edits.healthStartupFailures, "health-startup-failures", 0, "failed checks allowed while it is starting. 0 takes the platform's own")
	flags.BoolVar(&edits.noHealth, "no-health", false, "take the health check off: its liveness becomes whether its process is still running")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/projects/{name}",
			"PATCH /api/v1/projects/{name}",
		},
		Output: output{Mode: outputDocument, Kind: "processList",
			Note: "the project's whole declared workload list after the write"},
		Needs: needs{Auth: true, Project: true},
		Examples: []example{
			{"Declare a queue worker", "kitchen processes set worker --type worker --command node --command worker.js --json"},
			{"Run an image this platform did not build",
				"kitchen processes set cache --type service --port 6379 --image docker.io/library/redis:7.4 --json"},
			{"Declare a nightly job", `kitchen processes set nightly --type cron --schedule "0 3 * * *" --timeout 30m --json`},
			{"Park a worker without losing its command", "kitchen processes set worker --replicas 0 --json"},
		},
	})
}

func newProcessRemoveCommand(r *Runtime) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:     "rm NAME [NAME ...]",
		Aliases: []string{"remove"},
		Short:   "Take workloads off the project",
		Long: strings.TrimSpace(`
Remove workloads from the project's declaration.

A name the project does not declare is a failure rather than a silent success,
so a typo cannot read as a removal.

The removal reaches an environment through the next release: the environment
reconciler tears down whatever it materialized that the current release no
longer names, so what is running now keeps running until something builds.`),
		Args: cobra.MinimumNArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			return removeProcesses(commandContext(cmd), r, args, yes)
		}),
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask first")

	return describe(cmd, meta{
		Calls: []string{
			"GET /api/v1/projects/{name}",
			"PATCH /api/v1/projects/{name}",
		},
		Output: output{Mode: outputDocument, Kind: "processList"},
		Needs:  needs{Auth: true, Project: true},
		Examples: []example{
			{"Remove one, without being asked", "kitchen processes rm nightly --yes --json"},
		},
	})
}

// setProcess reads the project, rewrites one entry of its workload list and
// sends the whole list back. `changed` is the command's own `Flags().Changed`,
// which is the only way to tell a flag nobody passed from one passed with the
// value it defaults to — `--replicas 1` and no `--replicas` at all are
// different requests.
func setProcess(parent context.Context, r *Runtime, name string, edits processEdits, changed func(string) bool) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	projectName, err := r.projectName()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	found, err := client.project(ctx, projectName)
	if err != nil {
		return err
	}

	writes := declaredProcessWrites(found.Processes)
	at := -1
	for i := range writes {
		if writes[i].Name == name {
			at = i
			break
		}
	}
	if at < 0 {
		if !changed("type") {
			return failf(codeUsage, "%s declares no workload called %q", projectName, name).
				withHint("--type says what a new one is: worker, service, cron or task")
		}
		writes = append(writes, processWrite{Name: name})
		at = len(writes) - 1
	}
	if err := applyProcessEdits(&writes[at], edits, changed); err != nil {
		return err
	}

	updated, err := client.setProcesses(ctx, projectName, writes)
	if err != nil {
		return err
	}
	return printDeclaredProcesses(r, updated)
}

// applyProcessEdits writes the flags that were passed onto one workload, and
// refuses the two values it can judge without the platform. Everything else is
// the API's to refuse: there is one validator for what a workload is, and a
// second copy of it here would be a second thing to be wrong.
func applyProcessEdits(target *processWrite, edits processEdits, changed func(string) bool) error {
	if changed("type") {
		target.Type = strings.TrimSpace(edits.kind)
	}
	if changed("command") {
		target.Command = execWords(edits.command)
	}
	if changed("arg") {
		target.Args = execWords(edits.args)
	}
	if changed("port") {
		target.Port = edits.port
	}
	if changed("replicas") {
		target.Replicas = &edits.replicas
	}
	if changed("singleton") {
		target.Singleton = edits.singleton
	}
	if changed("cpu") {
		target.CPU = strings.TrimSpace(edits.cpu)
	}
	if changed("memory") {
		target.Memory = strings.TrimSpace(edits.memory)
	}
	if changed("schedule") {
		target.Schedule = strings.TrimSpace(edits.schedule)
	}
	if changed("concurrency") {
		target.ConcurrencyPolicy = strings.TrimSpace(edits.concurrency)
	}
	if changed("timeout") {
		target.Timeout = strings.TrimSpace(edits.timeout)
	}
	if changed("previews") {
		switch strings.TrimSpace(edits.previews) {
		case previewsYes:
			target.Previews = boolPointer(true)
		case previewsNo:
			target.Previews = boolPointer(false)
		case previewsDefault:
			target.Previews = nil
		default:
			return failf(codeUsage, "--previews takes %s, %s or %s (got %q)",
				previewsYes, previewsNo, previewsDefault, edits.previews)
		}
	}
	if err := applyProcessImageEdits(target, edits, changed); err != nil {
		return err
	}
	applyProcessBuildEdits(target, edits, changed)
	applyProcessHealthEdits(target, edits, changed)
	return nil
}

// applyProcessImageEdits reads --image and --image-connection. A workload is
// built here or published elsewhere and never both, so naming an image clears
// whatever build it had rather than sending a pair the API would refuse.
func applyProcessImageEdits(target *processWrite, edits processEdits, changed func(string) bool) error {
	if !changed("image") && !changed("image-connection") {
		return nil
	}
	if changed("image") {
		reference := strings.TrimSpace(edits.image)
		if reference == "" {
			target.Image = nil
			return nil
		}
		image, err := splitImageReference(reference)
		if err != nil {
			return err
		}
		if target.Image != nil {
			image.Connection = target.Image.Connection
		}
		target.Image = image
		target.Build = nil
	}
	if changed("image-connection") {
		if target.Image == nil {
			return fail(codeUsage, "--image-connection is the credential an image is pulled with, and this workload runs no image of its own").
				withHint("name the image with --image")
		}
		target.Image.Connection = strings.TrimSpace(edits.imageConnection)
	}
	return nil
}

// splitImageReference reads `repository@sha256:...` or `repository:tag` into
// the three fields the API takes. The platform refuses a repository carrying
// its own version, so the split happens here rather than being sent whole.
func splitImageReference(reference string) (*imageWrite, error) {
	if repository, digest, pinned := strings.Cut(reference, "@"); pinned {
		if repository == "" || digest == "" {
			return nil, failf(codeUsage, "--image %q names no repository and digest", reference)
		}
		return &imageWrite{Repository: repository, Digest: digest}, nil
	}
	// A registry host may carry a port — `registry.example.com:5000/thing` —
	// so the tag is what follows the last colon, and only when there is no
	// slash after it.
	at := strings.LastIndex(reference, ":")
	if at < 0 || strings.Contains(reference[at+1:], "/") {
		return nil, failf(codeUsage, "--image %q names no version", reference).
			withHint("a vendored image is pinned: write repository:tag or repository@sha256:...")
	}
	repository, tag := reference[:at], reference[at+1:]
	if repository == "" || tag == "" {
		return nil, failf(codeUsage, "--image %q names no repository and tag", reference)
	}
	return &imageWrite{Repository: repository, Tag: tag}, nil
}

// applyProcessBuildEdits reads the four --build flags. Naming any of them makes
// this workload one built from the repository, so it clears an image the way
// naming an image clears a build.
func applyProcessBuildEdits(target *processWrite, edits processEdits, changed func(string) bool) {
	fields := []string{"build-root", "build-strategy", "build-dockerfile", "build-target"}
	touched := false
	for _, field := range fields {
		touched = touched || changed(field)
	}
	if !touched {
		return
	}
	if target.Build == nil {
		target.Build = &processBuild{}
	}
	if changed("build-root") {
		target.Build.RootDirectory = strings.TrimSpace(edits.buildRoot)
	}
	if changed("build-strategy") {
		target.Build.Strategy = strings.TrimSpace(edits.buildStrategy)
	}
	if changed("build-dockerfile") {
		target.Build.DockerfilePath = strings.TrimSpace(edits.buildDockerfile)
	}
	if changed("build-target") {
		target.Build.DockerfileTarget = strings.TrimSpace(edits.buildTarget)
	}
	target.Image = nil
}

// applyProcessHealthEdits reads the health flags, and --no-health, which is
// what takes a check off: a workload with none is alive for as long as its
// process is running, which is the reading a worker gets by default.
func applyProcessHealthEdits(target *processWrite, edits processEdits, changed func(string) bool) {
	if changed("no-health") && edits.noHealth {
		target.Health = nil
		return
	}
	fields := map[string]func(*processHealth){
		"health-path":             func(h *processHealth) { h.Path = strings.TrimSpace(edits.healthPath) },
		"health-port":             func(h *processHealth) { h.Port = edits.healthPort },
		"health-period":           func(h *processHealth) { h.PeriodSeconds = edits.healthPeriod },
		"health-timeout":          func(h *processHealth) { h.TimeoutSeconds = edits.healthTimeout },
		"health-failures":         func(h *processHealth) { h.FailureThreshold = edits.healthFailures },
		"health-startup-failures": func(h *processHealth) { h.StartupFailureThreshold = edits.healthStartupFailures },
	}
	touched := false
	for field := range fields {
		touched = touched || changed(field)
	}
	if !touched {
		return
	}
	if target.Health == nil {
		target.Health = &processHealth{}
	}
	for field, apply := range fields {
		if changed(field) {
			apply(target.Health)
		}
	}
}

// execWords is a list of words as a flag gathered it, with the empty strings
// dropped — so `--command ""` is a cleared command rather than a command of one
// empty word, which is what an image would be started with.
func execWords(words []string) []string {
	out := make([]string, 0, len(words))
	for _, word := range words {
		if trimmed := strings.TrimSpace(word); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	if len(out) == 0 {
		return []string{}
	}
	return out
}

func boolPointer(value bool) *bool { return &value }

func removeProcesses(parent context.Context, r *Runtime, names []string, yes bool) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	projectName, err := r.projectName()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	found, err := client.project(ctx, projectName)
	if err != nil {
		return err
	}
	declared := declaredProcessWrites(found.Processes)

	going := map[string]bool{}
	for _, name := range names {
		going[name] = true
	}
	kept := make([]processWrite, 0, len(declared))
	for _, workload := range declared {
		if !going[workload.Name] {
			kept = append(kept, workload)
			continue
		}
		delete(going, workload.Name)
	}
	if len(going) > 0 {
		missing := make([]string, 0, len(going))
		for name := range going {
			missing = append(missing, name)
		}
		return failf(codeNotFound, "%s declares no workload called %s",
			projectName, strings.Join(missing, ", ")).
			withHint("`kitchen processes` lists what there is")
	}

	if err := confirm(r, fmt.Sprintf("Remove %s from %s?", strings.Join(names, ", "), projectName), yes); err != nil {
		return err
	}

	updated, err := client.setProcesses(ctx, projectName, kept)
	if err != nil {
		return err
	}
	return printDeclaredProcesses(r, updated)
}

func printDeclaredProcesses(r *Runtime, updated *project) error {
	answer := list[process]{Items: updated.Processes}
	if answer.Items == nil {
		answer.Items = []process{}
	}
	return r.printer().document(answer, func(s tui.Styles) string {
		return renderDeclaredProcesses(s, answer.Items)
	})
}

// renderDeclaredProcesses draws what the project *declares*, which is a
// different table from what an environment is running: there are no replica
// counts that are true yet and no last runs, and the column that matters
// instead is where each workload's image comes from.
func renderDeclaredProcesses(s tui.Styles, workloads []process) string {
	if len(workloads) == 0 {
		return "No workloads besides the web process.\n" +
			s.Subtle.Render("`kitchen processes set --help` says how to declare one.") + "\n"
	}
	rows := make([][]string, 0, len(workloads))
	for _, workload := range workloads {
		rows = append(rows, []string{
			workload.Name,
			workload.Type,
			declaredRuns(workload),
			declaredImage(workload),
			declaredPreviews(workload),
		})
	}
	return s.Table([]string{"NAME", "TYPE", "RUNS", "IMAGE", "PREVIEWS"}, rows)
}

// declaredRuns is the one number or expression that says how much of this
// workload there is: a schedule, a replica count, or the once a task runs.
func declaredRuns(p process) string {
	switch p.Type {
	case processTypeCron:
		return p.Schedule
	case processTypeTask:
		return "once per deploy"
	default:
		count := replicaCount(p)
		if count == 0 {
			return "parked"
		}
		return strconv.Itoa(int(count)) + "×"
	}
}

// declaredImage is where this workload's image comes from: an image somebody
// else published, a directory of this repository, or the project's own image
// run with another command.
func declaredImage(p process) string {
	if p.ImageSource != nil {
		return p.ImageSource.Reference
	}
	if p.Build != nil {
		if p.Build.RootDirectory != "" {
			return p.Build.RootDirectory
		}
		return "built here"
	}
	return "the project's"
}

// declaredPreviews reads the declaration rather than resolving it, and says so
// where there is none: what a type's default is belongs in `--help` and in the
// documentation, not repeated in a column where it would look like something
// the workload said.
func declaredPreviews(p process) string {
	if p.Previews == nil {
		return "default"
	}
	if *p.Previews {
		return previewsYes
	}
	return previewsNo
}
