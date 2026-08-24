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
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/Bermos/Kitchen/internal/version"
)

// `kitchen schema` — the whole CLI, as one JSON document.
//
// This is the command that makes the rest of them usable by something that has
// never seen this program: every command, every flag with its type and its
// default, which API endpoints each one calls, what shape it answers with, the
// exit codes, the environment variables and the files. One request, no
// guessing, no scraping of `--help`.
//
// **It is derived rather than written.** The commands are walked, the flags are
// read off pflag, and the output shapes are read off the Go structs the CLI
// actually decodes into (by reflection, so a field added to an answer appears
// here without anybody remembering to add it). What cannot be derived — which
// endpoints a command calls, what it needs, how to run it — rides on the
// command itself as metadata, and a test walks the tree and fails if a command
// carries none. So the published surface and the real one cannot drift: there
// is no second list to keep in step, which is the same reason the API's
// enforcement table is a table and the dashboard's copy of it is generated.

// helpCommand is cobra's own: a command and a flag, neither of which is part
// of the surface this document publishes — `kitchen schema` is what a machine
// reads instead of --help.
const helpCommand = "help"

// schema is the published document.
type schema struct {
	CLI     string `json:"cli"`
	Version string `json:"version"`
	Summary string `json:"summary"`
	// Protocol is how to read anything this CLI writes.
	Protocol protocol `json:"protocol"`
	// GlobalFlags are accepted by every command.
	GlobalFlags []flagSpec `json:"globalFlags"`
	// Environment is every variable the CLI reads.
	Environment []variableSpec `json:"environment"`
	// Files is the state it keeps, and where.
	Files []fileSpec `json:"files"`
	// ExitCodes is the contract a caller branches on.
	ExitCodes []exitCodeSpec `json:"exitCodes"`
	// Commands is every command, flat, sorted by path — flat because a caller
	// looking for "how do I deploy" wants to scan a list rather than walk a
	// tree.
	Commands []commandSpec `json:"commands"`
	// Shapes describes every JSON shape the commands answer with, by the name
	// each command's output names.
	Shapes map[string]shapeSpec `json:"shapes"`
}

// protocol is the contract between this CLI and whatever is reading it.
type protocol struct {
	JSON     string `json:"json"`
	Stream   string `json:"stream"`
	Errors   string `json:"errors"`
	Progress string `json:"progress"`
	Input    string `json:"input"`
	Discover string `json:"discover"`
}

// commandSpec is one command.
type commandSpec struct {
	// Path is how it is invoked, spaces and all: "kitchen env set".
	Path        string     `json:"path"`
	Summary     string     `json:"summary"`
	Description string     `json:"description"`
	Aliases     []string   `json:"aliases,omitempty"`
	Arguments   string     `json:"arguments,omitempty"`
	Flags       []flagSpec `json:"flags,omitempty"`
	Calls       []string   `json:"calls,omitempty"`
	Output      output     `json:"output"`
	Needs       needs      `json:"needs"`
	Examples    []example  `json:"examples,omitempty"`
	// Group is true for a command that only holds others — `kitchen env` —
	// and so is not something to run.
	Group bool `json:"group,omitempty"`
}

// flagSpec is one flag, with everything needed to write it correctly without
// trying it first.
type flagSpec struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand,omitempty"`
	// Type is pflag's own name for it: string, bool, int, duration,
	// stringArray. A stringArray is repeatable.
	Type       string `json:"type"`
	Default    string `json:"default,omitempty"`
	Usage      string `json:"usage"`
	Env        string `json:"env,omitempty"`
	Repeatable bool   `json:"repeatable,omitempty"`
}

// variableSpec is one environment variable the CLI reads.
type variableSpec struct {
	Name    string `json:"name"`
	Meaning string `json:"meaning"`
}

// fileSpec is one file the CLI keeps.
type fileSpec struct {
	Path    string `json:"path"`
	Holds   string `json:"holds"`
	Secret  bool   `json:"secret"`
	Written string `json:"writtenBy"`
}

// shapeSpec is one JSON shape, as its fields.
type shapeSpec struct {
	Description string      `json:"description,omitempty"`
	Fields      []fieldSpec `json:"fields"`
}

// fieldSpec is one field of a shape.
type fieldSpec struct {
	Name string `json:"name"`
	// Type is this document's own small vocabulary: string, integer, number,
	// boolean, timestamp, object, a shape's name, or any of those with "[]"
	// after it.
	Type     string `json:"type"`
	Optional bool   `json:"optional,omitempty"`
}

func newSchemaCommand(r *Runtime) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema [COMMAND ...]",
		Short: "Publish the whole CLI as one JSON document",
		Long: strings.TrimSpace(`
Print every command, flag, output shape and exit code as JSON.

This is the machine-readable surface of the CLI: a caller that has never run
kitchen before can read this one document and drive all of it — what each
command does, what it needs, which API endpoints it calls, what shape it
answers with, and what each exit status means.

It answers JSON whether or not --json was passed, because that is the whole
point of it. Naming a command narrows the document to that command and its
subcommands.`),
		Args: cobra.ArbitraryArgs,
		RunE: run(func(cmd *cobra.Command, args []string) error {
			document, err := describeTree(cmd.Root(), args)
			if err != nil {
				return err
			}
			encoder := json.NewEncoder(r.Stdout)
			encoder.SetEscapeHTML(false)
			if !r.jsonOut {
				// A person asking for the schema is reading it; a pipe gets
				// the same document on one line.
				encoder.SetIndent("", "  ")
			}
			if err := encoder.Encode(document); err != nil {
				return fail(codeFailed, "writing the schema: "+err.Error())
			}
			return nil
		}),
	}

	return describe(cmd, meta{
		Output: output{Mode: outputDocument, Kind: "schema", Note: "always JSON, with or without --json"},
		Needs:  needs{},
		Examples: []example{
			{"The whole surface", "kitchen schema"},
			{"One command's flags", "kitchen schema deploy"},
			{"Every command's name and summary",
				"kitchen schema | jq -r '.commands[] | \"\\(.path): \\(.summary)\"'"},
		},
	})
}

// describeTree builds the document, optionally narrowed to one command.
func describeTree(root *cobra.Command, path []string) (*schema, error) {
	from := root
	if len(path) > 0 {
		found, _, err := root.Find(path)
		if err != nil || found == root {
			return nil, failf(codeUsage, "no such command: %s", strings.Join(path, " ")).
				withHint("`kitchen schema` with no arguments lists every one")
		}
		from = found
	}

	document := &schema{
		CLI:         root.Name(),
		Version:     version.Version,
		Summary:     root.Short,
		Protocol:    publishedProtocol,
		GlobalFlags: flagSpecs(root.PersistentFlags(), nil),
		Environment: publishedVariables,
		Files:       publishedFiles,
		ExitCodes:   exitCodes,
		Shapes:      map[string]shapeSpec{},
	}

	var walk func(cmd *cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Hidden || cmd.Name() == helpCommand || cmd.Name() == "completion" {
			return
		}
		if cmd != root || from != root {
			document.Commands = append(document.Commands, describeCommand(cmd, root))
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(from)

	sort.Slice(document.Commands, func(i, j int) bool {
		return document.Commands[i].Path < document.Commands[j].Path
	})
	for _, command := range document.Commands {
		if command.Output.Kind != "" {
			addShape(command.Output.Kind, document.Shapes)
		}
	}
	return document, nil
}

// describeCommand is one command as the document publishes it.
func describeCommand(cmd *cobra.Command, root *cobra.Command) commandSpec {
	spec := commandSpec{
		Path:        cmd.CommandPath(),
		Summary:     cmd.Short,
		Description: cmd.Long,
		Aliases:     cmd.Aliases,
		Arguments:   arguments(cmd),
		Flags:       flagSpecs(cmd.Flags(), root.PersistentFlags()),
		Group:       !cmd.Runnable() || cmd.HasSubCommands(),
	}
	if m, ok := metaOf(cmd); ok {
		spec.Calls, spec.Output, spec.Needs, spec.Examples = m.Calls, m.Output, m.Needs, m.Examples
	}
	return spec
}

// arguments is the positional part of a command's use line — "NAME=VALUE ..." —
// which is the half cobra does not model and a caller still has to get right.
func arguments(cmd *cobra.Command) string {
	_, rest, found := strings.Cut(cmd.Use, " ")
	if !found {
		return ""
	}
	return strings.TrimSpace(rest)
}

// flagSpecs reads a flag set, skipping --help and anything inherited from the
// root: the global flags are published once, under globalFlags, rather than
// repeated on all eighteen commands.
func flagSpecs(flags *pflag.FlagSet, global *pflag.FlagSet) []flagSpec {
	specs := []flagSpec{}
	flags.VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden || flag.Name == helpCommand {
			return
		}
		if global != nil && global.Lookup(flag.Name) != nil {
			return
		}
		spec := flagSpec{
			Name:      flag.Name,
			Shorthand: flag.Shorthand,
			Type:      flag.Value.Type(),
			Default:   flag.DefValue,
			Usage:     flag.Usage,
		}
		if strings.HasSuffix(spec.Type, "Array") || strings.HasSuffix(spec.Type, "Slice") {
			spec.Repeatable = true
		}
		if variable, ok := flag.Annotations[envAnnotation]; ok && len(variable) > 0 {
			spec.Env = variable[0]
		}
		specs = append(specs, spec)
	})
	if len(specs) == 0 {
		return nil
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })
	return specs
}

// The parts of the document that are prose rather than a walk over something.

var publishedProtocol = protocol{
	JSON: "--json makes stdout carry JSON and nothing else. The same flag on every command; " +
		"KITCHEN_JSON=1 is the same thing for a whole session.",
	Stream: "A command whose output mode is \"stream\" writes one JSON object per line (NDJSON) " +
		"as things happen, rather than one document at the end.",
	Errors: "A failure is a single {\"error\": {\"code\", \"message\", \"hint\", \"status\", \"doing\"}} " +
		"document — on stdout under --json, on stderr otherwise — and a non-zero exit status. " +
		"The code and the status name the same thing; see exitCodes.",
	Progress: "Progress and warnings go to stderr, never to stdout: as {\"type\":\"note\"|\"warning\"} " +
		"objects under --json, and as plain sentences otherwise.",
	Input: "Nothing ever blocks on a prompt. Every question has a flag that answers it, and " +
		"--no-input (implied whenever stdin is not a terminal) turns a question into a failure naming " +
		"that flag. Bubble Tea's interactive views are only ever drawn when stdout is a terminal and " +
		"--json is off, so they cannot appear in a pipe.",
	Discover: "`kitchen schema` publishes this document; `kitchen api METHOD PATH` reaches any endpoint " +
		"no command covers yet. Between them there is nothing the platform's API can do that the " +
		"command line cannot.",
}

var publishedVariables = []variableSpec{
	{"KITCHEN_API", "The installation to talk to. The --api flag overrides it; the linked directory and the stored current installation are the fallbacks"},
	{"KITCHEN_PROJECT", "The project to act on, overriding the linked one"},
	{"KITCHEN_TOKEN", "A platform token to use as it is. Skips the stored credential and the key exchange entirely — what CI should use when it exchanges its own key"},
	{"KITCHEN_API_KEY", "An API key to exchange for a token. Used without `kitchen login` having been run"},
	{"KITCHEN_JSON", "1, true, yes or on turns --json on for every command"},
	{"KITCHEN_NO_INPUT", "1, true, yes or on turns --no-input on for every command"},
	{"KITCHEN_CONFIG_HOME", "Where the credential file lives, instead of the user's configuration directory"},
}

var publishedFiles = []fileSpec{
	{
		Path:    "$KITCHEN_CONFIG_HOME/auth.json, or <user config dir>/kitchen/auth.json",
		Holds:   "One entry per installation: the issuer, the API key, and the last exchanged token with its expiry",
		Secret:  true,
		Written: "kitchen login, and any command that exchanges a token",
	},
	{
		Path:    "<working copy>/.kitchen/project.json",
		Holds:   "The project this directory deploys to, and the installation it is on. No credential",
		Secret:  false,
		Written: "kitchen link",
	},
}

// The shapes, by the name a command's output names. Every entry is a value of
// the type the CLI actually decodes into, so the published fields are the real
// ones — a field added to an answer shows up here without anybody being asked
// to remember.
var publishedShapes = map[string]struct {
	Description string
	Sample      any
}{
	"account":       {"Who a credential belongs to: GET /me", account{}},
	"project":       {"A project, with the calling account's role on it", project{}},
	"projectList":   {"A list of projects", list[project]{}},
	"projectStatus": {"A project with its environments and recent builds", projectStatus{}},
	"projectCreated": {"A project that was just created, what the preflight made of its " +
		"repository, and where the link was written", projectCreated{}},
	"detection": {"What the platform makes of a repository before a project exists. " +
		"`detected` false is advice, not a refusal", detection{}},
	"build":     {"One build. Phase is Queued, Running, Succeeded, Failed or Cancelled", build{}},
	"buildList": {"A list of builds, newest first", list[build]{}},
	"gateList": {"The quality gates that ran over an artifact. Completed means the gate ran, " +
		"whatever it found; Failed means it did not run", list[gate]{}},
	"gateAccepted": {"Where a submitted gate result was attached, and whose word it is recorded as",
		gateAccepted{}},
	"evidenceSet": {"The signed evidence attached to an artifact, read out of the registry. " +
		"`verified` false with attestations present means signatures were not checked, " +
		"not that they failed", evidenceSet{}},
	"decision": {"One stored policy decision, with the bundle digest, input digest and full " +
		"input it can be replayed from. Verdict is allowed, allowed-with-exception or blocked", decision{}},
	"decisionList": {"A list of stored decisions, newest first", list[decision]{}},
	"decisionReplay": {"A stored decision re-evaluated from its stored inputs: both verdicts, " +
		"and whether they match", decisionReplay{}},
	"drift": {"Deployed releases measured against their environment's bar today. Status is " +
		"compliant, waived, newly-failing, waived-at-promotion or not-evaluated; `rescanning` " +
		"false means nothing is checking, which is not the same as nothing being wrong", drift{}},
	"exception": {"One break-glass exception: who asked, who approved, the rules it waives, " +
		"until when, and the promotions that relied on it. Phase is Active, Expired or Resolved", exception{}},
	"exceptionList": {"A list of exceptions, soonest to expire first", list[exception]{}},
	"release":       {"An immutable snapshot of an image and its configuration", release{}},
	"releaseList":   {"A list of releases, newest first", list[release]{}},
	"promotion": {"One request to move a release into an environment, with what the policy " +
		"decided about it. Phase is Pending, Evaluating, Allowed, AllowedWithException, " +
		"Blocked, Applied or Failed; a Blocked one names the unmet rules by id", promotion{}},
	"promotionList":   {"A list of promotions, newest first", list[promotion]{}},
	"environment":     {"One environment. Phase is Pending, Deploying, Live, Degraded or Terminating", environment{}},
	"environmentList": {"A list of environments", list[environment]{}},
	"envVarList":      {"A project's environment variables. Values are never answered", list[envVar]{}},
	"logLine":         {"One log line, from a build or from something running", logLine{}},
	"deployEvent":     {"One event of a followed deploy: build, log, release, environment or result", deployEvent{}},
	"linked":          {"What a directory was linked to, and where the fact was written", linked{}},
	"forgotten":       {"Which installations this machine no longer holds a credential for", forgotten{}},
	"backupTaken":     {"An archive that was taken: where it went, and what the platform put in it", backupTaken{}},
	"schema":          {"This document", schema{}},
}

// addShape puts a named shape into the document, along with every shape it
// refers to.
func addShape(name string, into map[string]shapeSpec) {
	if _, done := into[name]; done {
		return
	}
	published, known := publishedShapes[name]
	if !known {
		return
	}
	into[name] = shapeSpec{Description: published.Description}
	spec := shapeSpec{
		Description: published.Description,
		Fields:      fieldSpecs(reflect.TypeOf(published.Sample), into),
	}
	into[name] = spec
}

// fieldSpecs reads a struct's JSON fields by reflection, adding any named
// struct it refers to as a shape of its own.
func fieldSpecs(structType reflect.Type, into map[string]shapeSpec) []fieldSpec {
	for structType.Kind() == reflect.Pointer {
		structType = structType.Elem()
	}
	if structType.Kind() != reflect.Struct {
		return nil
	}

	fields := []fieldSpec{}
	for i := range structType.NumField() {
		field := structType.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, options, _ := strings.Cut(tag, ",")
		if name == "" {
			name = field.Name
		}
		fields = append(fields, fieldSpec{
			Name:     name,
			Type:     typeName(field.Type, into),
			Optional: strings.Contains(options, "omitempty"),
		})
	}
	return fields
}

// timeType is compared against rather than reflected into: a timestamp is one
// value in JSON, not a struct with a wall clock in it.
var timeType = reflect.TypeOf(time.Time{})

// typeName is this document's small vocabulary for a Go type, and the place
// nested shapes are discovered.
func typeName(t reflect.Type, into map[string]shapeSpec) string {
	switch {
	case t == timeType:
		return "timestamp"
	case t.Kind() == reflect.Pointer:
		return typeName(t.Elem(), into)
	}

	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return typeName(t.Elem(), into) + "[]"
	case reflect.Map:
		return "object<" + typeName(t.Elem(), into) + ">"
	case reflect.Struct:
		return nestedShape(t, into)
	default:
		return "object"
	}
}

// nestedShape names a struct a published shape refers to, and describes it
// too. The name is the Go type's own, lower-cased at the front, which is what
// the published shapes are already called.
func nestedShape(t reflect.Type, into map[string]shapeSpec) string {
	name := t.Name()
	if name == "" {
		return "object"
	}
	name = strings.ToLower(name[:1]) + name[1:]
	if _, done := into[name]; !done {
		// Reserve the name before recursing, so a shape that refers to itself
		// does not describe itself forever.
		into[name] = shapeSpec{}
		into[name] = shapeSpec{Fields: fieldSpecs(t, into)}
	}
	return name
}
