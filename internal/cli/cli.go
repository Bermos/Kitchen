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

// Package cli is `kitchen`, the command line client for a Kitchen
// installation.
//
// It is a client of the REST API in docs/API.md and nothing else: it holds no
// kubeconfig, talks to no cluster, and knows nothing about the platform that
// the API did not just tell it. Everything it can do, the dashboard can do,
// because both are clients of the same routes — which is the premise the whole
// platform is built on (CLAUDE.md, "Nothing needs kubectl").
//
// # Meant to be driven
//
// The CLI is built to be run by a person *and* by something that is not a
// person, and the second one is the harder requirement, so it comes first:
//
//   - **`kitchen schema` is the whole surface, as JSON.** Every command, every
//     flag with its type and default, which endpoints each one calls, what
//     shape it answers with, the exit codes, the environment variables and the
//     files — in one document, derived from the commands themselves rather
//     than written alongside them. A caller that has never seen this CLI can
//     read that and drive all of it.
//   - **`--json` on every command**, with a shape that does not depend on
//     whether a terminal was attached.
//   - **Nothing ever blocks on a prompt it was not given a flag for.** Every
//     question has a flag, and --no-input (implied whenever stdin is not a
//     terminal) turns a question into a failure that names the flag.
//   - **Exit codes are a contract**, one per kind of failure, published in the
//     schema and never reused for anything else.
//   - **`kitchen api` reaches whatever has no command yet**, so a route added
//     to the API is usable from the CLI the day it lands rather than the day
//     somebody writes a subcommand for it.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Bermos/Kitchen/internal/version"
)

// Main runs the CLI against the real process and answers its exit status. It
// is the only function in this package that reads the process's own
// environment; everything below takes a Runtime.
func Main() int {
	working, err := os.Getwd()
	if err != nil {
		// A process whose working directory has been removed can still be
		// told everything by flags, so this is a note rather than the end.
		working = "."
	}
	runtime := &Runtime{
		Stdin:         os.Stdin,
		Stdout:        os.Stdout,
		Stderr:        os.Stderr,
		Getenv:        os.Getenv,
		WorkingDir:    working,
		Terminal:      term.IsTerminal(int(os.Stdout.Fd())),
		StdinTerminal: term.IsTerminal(int(os.Stdin.Fd())),
	}

	// Ctrl-C ends the command rather than the shell's idea of it: a followed
	// build stops being followed, and the build carries on — which the exit
	// status says, so nothing mistakes an interrupted follow for a failed
	// deploy.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return Execute(ctx, runtime, os.Args[1:])
}

// Execute runs one command line against a Runtime and answers the exit status.
// Tests call this.
func Execute(ctx context.Context, r *Runtime, args []string) int {
	root := newRoot(r)
	root.SetArgs(args)
	root.SetOut(r.Stdout)
	root.SetErr(r.Stderr)
	if r.Stdin != nil {
		root.SetIn(r.Stdin)
	}

	err := root.ExecuteContext(ctx)
	if err == nil {
		return exitOK
	}

	// A command line cobra could not even parse never reached the code that
	// reads --json off the flag set, and a caller who asked for JSON must get
	// JSON for the refusal too — a machine driving this CLI meets an unknown
	// flag more often than it meets anything else.
	if !r.jsonOut && (wantsJSON(args) || truthy(r.env("KITCHEN_JSON"))) {
		r.jsonOut, r.out = true, nil
	}

	// Every command in this package answers with a *failure. Anything else got
	// here from cobra's own parsing — an unknown flag, a missing argument, an
	// unknown subcommand — which is a usage error and exits 2.
	var f *failure
	if !errors.As(err, &f) {
		f = fail(codeUsage, err.Error()).
			withHint("run `kitchen " + strings.Join(args, " ") + " --help`, " +
				"or `kitchen schema` for every command and flag as JSON")
	}
	if errors.Is(ctx.Err(), context.Canceled) && f.Code != codeInterrupted {
		f = fail(codeInterrupted, "interrupted").
			withHint("anything already started on the platform is still running")
	}
	r.printer().failure(f)
	return f.exitCode()
}

// newRoot builds the command tree. Every command is registered here, which is
// also what `kitchen schema` walks: there is one list, and a command that is
// not on it does not exist for either reader.
func newRoot(r *Runtime) *cobra.Command {
	root := &cobra.Command{
		Use:   "kitchen",
		Short: "Deploy to a Kitchen installation from the command line",
		Long: strings.TrimSpace(`
kitchen is the command line client for a Kitchen installation: link a working
directory to a project, deploy the current commit, follow what it does, read
the logs, change the environment variables, roll back.

It is a client of the platform's REST API. Everything it can do is a route the
dashboard uses too, and everything the API answers can be had as JSON with
--json. Run "kitchen schema" for the whole surface — commands, flags, output
shapes and exit codes — in one machine-readable document.`),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version.Version,
		// The default is to print help and exit 0, which reads as success to
		// anything checking a status. A command that is not a command is a
		// usage error.
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				// The help is for a person, so it stays off stdout when
				// somebody asked for JSON: the answer to `kitchen --json` is
				// the error envelope, and nothing else may share that stream.
				if !r.jsonOut {
					_ = cmd.Help()
				}
				return fail(codeUsage, "no command").
					withHint("`kitchen --help` lists them, `kitchen schema` publishes them as JSON")
			}
			return fail(codeUsage, "unknown command \""+args[0]+"\"").
				withHint("`kitchen --help` lists the commands, `kitchen schema` publishes them as JSON")
		},
	}

	flags := root.PersistentFlags()
	flags.StringVar(&r.apiFlag, "api", "",
		"the installation to talk to, e.g. https://kitchen.example.com [KITCHEN_API]")
	flags.StringVarP(&r.projectFlag, "project", "p", "",
		"the project to act on, overriding the linked one [KITCHEN_PROJECT]")
	flags.BoolVar(&r.jsonOut, "json", false,
		"answer with JSON on stdout and nothing else [KITCHEN_JSON]")
	flags.BoolVar(&r.plain, "plain", false,
		"never colour or lay out the output, even on a terminal")
	flags.BoolVar(&r.noInput, "no-input", false,
		"never ask a question; a missing answer is a failure naming the flag [KITCHEN_NO_INPUT]")
	flags.DurationVar(&r.timeout, "timeout", 0,
		"give up after this long, e.g. 30m. The default waits as long as it takes")

	// The flags that answer to an environment variable say so in one place, so
	// the schema can publish the pairing rather than a reader having to find
	// it in the usage string.
	mustAnnotate(flags, "api", "KITCHEN_API")
	mustAnnotate(flags, "project", "KITCHEN_PROJECT")
	mustAnnotate(flags, "json", "KITCHEN_JSON")
	mustAnnotate(flags, "no-input", "KITCHEN_NO_INPUT")

	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		// An environment variable is a weaker statement than a flag, so it
		// only speaks where the flag was not given.
		if !cmd.Flags().Changed("json") && truthy(r.env("KITCHEN_JSON")) {
			r.jsonOut = true
		}
		if !cmd.Flags().Changed("no-input") && truthy(r.env("KITCHEN_NO_INPUT")) {
			r.noInput = true
		}
		// Nothing can answer a question when stdin is not a terminal, so a
		// command that would ask one must fail rather than hang. This is what
		// makes the CLI safe to run from a script that forgot --no-input.
		if !r.StdinTerminal {
			r.noInput = true
		}
		r.out = nil
		return nil
	}

	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fail(codeUsage, err.Error()).
			withHint("`kitchen schema` publishes every flag with its type and default")
	})

	root.AddCommand(
		newLoginCommand(r),
		newLogoutCommand(r),
		newWhoamiCommand(r),
		newLinkCommand(r),
		newStatusCommand(r),
		newDeployCommand(r),
		newCancelCommand(r),
		newLogsCommand(r),
		newEnvCommand(r),
		newRollbackCommand(r),
		newPromoteCommand(r),
		newPromotionsCommand(r),
		newProjectsCommand(r),
		newBuildsCommand(r),
		newAttestationsCommand(r),
		newGatesCommand(r),
		newVEXCommand(r),
		newDecisionsCommand(r),
		newExceptionsCommand(r),
		newAccessCommand(r),
		newDriftCommand(r),
		newCriticalityCommand(r),
		newRetentionCommand(r),
		newReleasesCommand(r),
		newEnvironmentsCommand(r),
		newBackupCommand(r),
		newAPICommand(r),
		newSchemaCommand(r),
	)
	return root
}

// wantsJSON reads --json off a command line nothing has parsed. It is only
// ever consulted for a failure that happened before parsing, where the flag
// set has nothing to say.
func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || arg == "--json=true" {
			return true
		}
	}
	return false
}

// truthy reads the environment's idea of a boolean. Anything a person would
// write meaning yes counts; everything else, including the empty string, is
// no.
func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// The metadata a command publishes beyond what cobra already knows about it.
//
// It rides on the cobra command as an annotation rather than in a table keyed
// by command name, so it cannot go stale when a command is renamed or moved: a
// command carries its own description of itself, and the schema is a walk over
// the tree.

// meta is what `kitchen schema` says about a command that cobra cannot.
type meta struct {
	// Calls is every API endpoint this command may call, in the "METHOD /path"
	// form docs/API.md lists them in. It is the honest answer to "what will
	// this do to my platform", and it is what ties a command to the route
	// table it depends on.
	Calls []string `json:"calls,omitempty"`
	// Output is what stdout carries when the command succeeds.
	Output output `json:"output"`
	// Needs is what has to be true before the command can run.
	Needs needs `json:"needs"`
	// Examples are runnable command lines, machine-first: every one of them
	// works with no terminal, no prompt and no linked directory.
	Examples []example `json:"examples,omitempty"`
}

// output is the shape of a command's answer.
type output struct {
	// Mode is "document" (one JSON object), "stream" (NDJSON, one object per
	// line, ending in a result or error event) or "none".
	Mode string `json:"mode"`
	// Kind names a shape in the schema's `shapes` section, where one applies.
	Kind string `json:"kind,omitempty"`
	// Note is anything about the answer a caller cannot see from the shape.
	Note string `json:"note,omitempty"`
}

// needs is what a command requires of the world before it can run.
type needs struct {
	// Auth is whether it talks to the API at all.
	Auth bool `json:"auth"`
	// Project is whether it has to resolve a project — and so whether it
	// fails with notLinked in a directory that is not linked.
	Project bool `json:"project"`
	// Git is whether it reads the working copy. Every command that does can
	// be told the same thing with flags instead.
	Git bool `json:"git,omitempty"`
}

// example is one runnable command line.
type example struct {
	Description string `json:"description"`
	Command     string `json:"command"`
}

// The output modes and the shape names, spelled once so a typo in an
// annotation cannot invent a mode nothing documents.
const (
	outputDocument = "document"
	outputStream   = "stream"
	outputNone     = "none"

	metaAnnotation = "kitchen.meta"
	envAnnotation  = "kitchen.env"
)

// describe attaches a command's metadata and returns it, so a command
// definition reads as one expression.
func describe(cmd *cobra.Command, m meta) *cobra.Command {
	encoded, err := json.Marshal(m)
	if err != nil {
		// meta is a fixed struct of strings and booleans; this cannot fail at
		// runtime, and a panic here fails the test that walks the tree.
		panic("encoding the metadata for " + cmd.Name() + ": " + err.Error())
	}
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[metaAnnotation] = string(encoded)
	return cmd
}

// metaOf reads back what describe attached. A command with none is a command
// that is not published — which the schema test refuses to let happen.
func metaOf(cmd *cobra.Command) (meta, bool) {
	encoded, ok := cmd.Annotations[metaAnnotation]
	if !ok {
		return meta{}, false
	}
	m := meta{}
	if err := json.Unmarshal([]byte(encoded), &m); err != nil {
		return meta{}, false
	}
	return m, true
}

// mustAnnotate records the environment variable a flag falls back to.
func mustAnnotate(flags interface {
	SetAnnotation(name, key string, values []string) error
}, name, variable string,
) {
	if err := flags.SetAnnotation(name, envAnnotation, []string{variable}); err != nil {
		panic("annotating the flag " + name + ": " + err.Error())
	}
}

// run wraps a command body so that everything leaving it is a *failure with an
// exit status, and so no command has to remember to convert its errors.
func run(body func(cmd *cobra.Command, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if err := body(cmd, args); err != nil {
			return asFailure(err)
		}
		return nil
	}
}

// commandContext is the context Execute was started with — the one that ends
// when somebody interrupts the process. A command that ran outside Execute
// (which only happens in a test that builds a command by hand) gets a
// background one rather than a nil.
func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

// ask puts a question to whoever is there, and refuses when nobody is.
//
// It is the only place in the CLI that reads stdin for an answer, which is
// what makes "never blocks on a prompt" checkable rather than a promise: every
// caller passes the flag that would have answered it, and that flag is what
// the failure names.
func ask(r *Runtime, question, flag string) (string, error) {
	if r.noInput || !r.StdinTerminal {
		return "", fail(codeUsage, "cannot ask: "+question).
			withHint("pass " + flag + " — this command is not asking anything, since " +
				"stdin is not a terminal or --no-input was given")
	}
	_, _ = fmt.Fprint(r.Stderr, question+" ")
	answer, err := readLine(r.Stdin)
	if err != nil {
		return "", fail(codeUsage, "reading the answer: "+err.Error())
	}
	return strings.TrimSpace(answer), nil
}

// confirm asks a yes/no question. `yes` short-circuits it, which is how every
// destructive command stays runnable without a terminal.
func confirm(r *Runtime, question string, yes bool) error {
	if yes {
		return nil
	}
	answer, err := ask(r, question+" [y/N]", "--yes")
	if err != nil {
		return err
	}
	switch strings.ToLower(answer) {
	case "y", "yes":
		return nil
	default:
		return fail(codeFailed, "cancelled")
	}
}

// readLine reads one line without pulling in a buffered reader that would
// swallow the rest of stdin — which matters because some commands read a
// credential from the same stream afterwards.
func readLine(in io.Reader) (string, error) {
	line := make([]byte, 0, 64)
	one := make([]byte, 1)
	for {
		n, err := in.Read(one)
		if n > 0 {
			if one[0] == '\n' {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			line = append(line, one[0])
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return strings.TrimSuffix(string(line), "\r"), nil
			}
			return "", err
		}
	}
}

// since renders how long ago something was, in the two words a person reads at
// a glance. It is only ever used in text output — a JSON answer carries the
// timestamp itself, which is the thing that can be computed with.
func since(at time.Time) string {
	if at.IsZero() {
		return ""
	}
	elapsed := time.Since(at)
	switch {
	case elapsed < time.Minute:
		return fmt.Sprintf("%ds ago", int(elapsed.Seconds()))
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	}
}

// short is a commit as it is written when it is being read rather than
// resolved.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
