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
	"strings"
	"testing"

	"github.com/Bermos/Kitchen/internal/api"
)

// The tests that keep `kitchen schema` honest.
//
// The schema is the CLI's contract with anything that is not a person, and a
// published surface is only worth reading if it cannot fall behind the real
// one. These are what stop it: a command added without saying what it does,
// what it calls or how to run it fails here, and so does a command that claims
// to call an endpoint the API does not serve.

// tree is the whole command tree, as the schema sees it.
func tree(t *testing.T) *schema {
	t.Helper()
	document, err := describeTree(newRoot(&Runtime{}), nil)
	if err != nil {
		t.Fatalf("building the schema: %v", err)
	}
	if len(document.Commands) == 0 {
		t.Fatal("the schema is empty")
	}
	return document
}

// Every command describes itself well enough to be run by something that has
// never seen it: a summary, a description, what it needs, what it answers, and
// at least one command line that works.
func TestEveryCommandPublishesItself(t *testing.T) {
	for _, command := range tree(t).Commands {
		t.Run(command.Path, func(t *testing.T) {
			if command.Summary == "" {
				t.Error("no summary")
			}
			if command.Description == "" {
				t.Error("no description: `kitchen schema` is the only documentation a machine reads")
			}
			if command.Output.Mode == "" {
				t.Error("no output mode: a caller cannot tell a document from a stream")
			}
			switch command.Output.Mode {
			case outputDocument, outputStream, outputNone:
			default:
				t.Errorf("output mode %q is not one of %s, %s, %s",
					command.Output.Mode, outputDocument, outputStream, outputNone)
			}
			if len(command.Examples) == 0 {
				t.Error("no examples")
			}
			for _, example := range command.Examples {
				if example.Description == "" || example.Command == "" {
					t.Errorf("an example with a missing half: %+v", example)
				}
				if !strings.HasPrefix(example.Command, "kitchen ") &&
					!strings.Contains(example.Command, "| kitchen ") {
					t.Errorf("the example is not a command line: %q", example.Command)
				}
			}
			for _, flag := range command.Flags {
				if flag.Usage == "" {
					t.Errorf("--%s has no usage", flag.Name)
				}
				if flag.Type == "" {
					t.Errorf("--%s has no type", flag.Name)
				}
			}
			// A command that talks to the platform says which endpoints it
			// touches. That is the honest answer to "what will this do", and
			// it is what the next test checks against the API itself.
			if command.Needs.Auth && len(command.Calls) == 0 {
				t.Error("it needs a credential but names no endpoint")
			}
		})
	}
}

// The link between this CLI and the API it is a client of.
//
// Every endpoint a command claims to call has to be a row of the API's own
// enforcement table (internal/api/policy.go), which is where every route is
// registered from. A route renamed, moved or removed fails here — which is the
// point: the CLI is a client of that table, and it should not be possible to
// change the table and leave the CLI describing a platform that no longer
// exists.
func TestEveryCallNamesARealAPIRoute(t *testing.T) {
	policy, err := api.PolicyTable()
	if err != nil {
		t.Fatalf("reading the API's route table: %v", err)
	}
	routes := map[string]struct{}{}
	for _, route := range policy.Routes {
		routes[route.Pattern] = struct{}{}
	}

	for _, command := range tree(t).Commands {
		for _, call := range command.Calls {
			if !strings.HasPrefix(call, "GET "+apiPrefix) &&
				!strings.HasPrefix(call, "POST "+apiPrefix) &&
				!strings.HasPrefix(call, "PUT "+apiPrefix) &&
				!strings.HasPrefix(call, "PATCH "+apiPrefix) &&
				!strings.HasPrefix(call, "DELETE "+apiPrefix) {
				// The three that are not routes of this API: the dashboard's
				// public /config.json, the token exchange at the identity
				// provider, and `kitchen api`'s "whatever you name".
				continue
			}
			if _, ok := routes[call]; !ok {
				t.Errorf("%s says it calls %q, which is not a route the API serves.\n"+
					"Either the endpoint moved — in which case this command has to move with it — "+
					"or the annotation is wrong.", command.Path, call)
			}
		}
	}
}

// The exit codes are a contract, so they have to be a set: one code per
// meaning, one meaning per code, and every one of them documented.
func TestExitCodesAreAContract(t *testing.T) {
	byCode := map[int]string{}
	byName := map[string]int{}
	for _, spec := range exitCodes {
		if spec.Meaning == "" {
			t.Errorf("%s (%d) has no meaning", spec.Name, spec.Code)
		}
		if previous, taken := byCode[spec.Code]; taken {
			t.Errorf("%d is both %s and %s", spec.Code, previous, spec.Name)
		}
		if _, taken := byName[spec.Name]; taken {
			t.Errorf("%s appears twice", spec.Name)
		}
		byCode[spec.Code], byName[spec.Name] = spec.Name, spec.Code
	}

	// Every code a failure can carry has a row, and every row's code is the
	// status exitFor answers with.
	for _, code := range []string{
		codeFailed, codeUsage, codeUnauthenticated, codeForbidden, codeNotFound,
		codeConflict, codeUnavailable, codeUnreachable, codeBuildFailed,
		codeNotLinked, codeTimedOut, codeDeployFailed, codeInterrupted,
	} {
		status, documented := byName[code]
		if !documented {
			t.Errorf("the failure code %q is in no row of the table", code)
			continue
		}
		if exitFor(code) != status {
			t.Errorf("%q exits %d but the table says %d", code, exitFor(code), status)
		}
	}
	if exitFor("something nothing returns") != exitFailed {
		t.Error("an unknown code has to exit non-zero")
	}
}

// The shapes are read off the structs the CLI decodes into, so a field cannot
// be published that the answer does not have. This checks the derivation
// itself — the part that would fail quietly.
func TestShapesDescribeTheRealAnswers(t *testing.T) {
	document := tree(t)

	for _, command := range document.Commands {
		if command.Output.Kind == "" {
			continue
		}
		if _, ok := document.Shapes[command.Output.Kind]; !ok {
			t.Errorf("%s answers with %q, which no shape describes", command.Path, command.Output.Kind)
		}
	}

	project, ok := document.Shapes["project"]
	if !ok {
		t.Fatal("no project shape")
	}
	fields := map[string]string{}
	for _, field := range project.Fields {
		fields[field.Name] = field.Type
	}
	for name, wanted := range map[string]string{
		"name": "string", "role": "string", "previews": "boolean",
		"createdAt": "timestamp", "env": "envVar[]", "replicas": "integer",
	} {
		if fields[name] != wanted {
			t.Errorf("project.%s is %q, wanted %q", name, fields[name], wanted)
		}
	}
	if _, ok := document.Shapes["envVar"]; !ok {
		t.Error("the shape a project's env refers to was not described too")
	}

	// A list is an object with items, never a bare array: the API answers that
	// way so a cursor can be added, and the CLI has to say so.
	projects, ok := document.Shapes["projectList"]
	if !ok || len(projects.Fields) != 1 || projects.Fields[0].Name != "items" {
		t.Errorf("unexpected projectList shape: %+v", projects)
	}
	if projects.Fields[0].Type != "project[]" {
		t.Errorf("projectList.items is %q", projects.Fields[0].Type)
	}
}

// Naming a command narrows the document, which is how a caller reads one
// command's flags without the other seventeen.
func TestSchemaNarrowsToOneCommand(t *testing.T) {
	root := newRoot(&Runtime{})

	narrowed, err := describeTree(root, []string{"env"})
	if err != nil {
		t.Fatalf("narrowing: %v", err)
	}
	for _, command := range narrowed.Commands {
		if !strings.HasPrefix(command.Path, "kitchen env") {
			t.Errorf("%s is not under `kitchen env`", command.Path)
		}
	}
	if len(narrowed.Commands) != 4 {
		t.Errorf("wanted `kitchen env` and its three subcommands, got %d", len(narrowed.Commands))
	}

	if _, err := describeTree(root, []string{"teleport"}); err == nil {
		t.Error("a command that does not exist should not narrow to an empty document")
	}
}

// The global flags are published once rather than repeated on every command,
// and they carry the environment variable each one answers to.
func TestGlobalFlagsArePublishedOnceWithTheirVariables(t *testing.T) {
	document := tree(t)

	variables := map[string]string{}
	for _, flag := range document.GlobalFlags {
		variables[flag.Name] = flag.Env
	}
	for name, wanted := range map[string]string{
		"api": "KITCHEN_API", "project": "KITCHEN_PROJECT",
		"json": "KITCHEN_JSON", "no-input": "KITCHEN_NO_INPUT",
	} {
		if variables[name] != wanted {
			t.Errorf("--%s answers to %q, wanted %q", name, variables[name], wanted)
		}
	}

	for _, command := range document.Commands {
		for _, flag := range command.Flags {
			if _, global := variables[flag.Name]; global {
				t.Errorf("%s repeats the global flag --%s", command.Path, flag.Name)
			}
			if flag.Name == "help" {
				t.Errorf("%s publishes --help, which is not part of the surface", command.Path)
			}
		}
	}
}
