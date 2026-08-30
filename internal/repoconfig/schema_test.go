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

package repoconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/Bermos/Kitchen/internal/appconfig"
)

// The published schema is what an editor completes against and what an agent
// writing a kitchen.json reads, so it has to describe the file this package
// actually accepts. It is hand-written rather than generated — the wording is
// half of what it is for — which leaves exactly one way it can rot: a field
// added to the Go types and not to the document, or the reverse.
//
// This is that check. It compares the two on the one thing a machine can
// judge, which is which keys exist; whether the description is any good stays
// a matter for review.

const schemaPath = "../../docs/schemas/kitchen.schema.json"

// schemaNode is as much of JSON Schema as this comparison needs.
type schemaNode struct {
	Type                 string                `json:"type"`
	Ref                  string                `json:"$ref"`
	Properties           map[string]schemaNode `json:"properties"`
	AdditionalProperties json.RawMessage       `json:"additionalProperties"`
	Items                *schemaNode           `json:"items"`
	Defs                 map[string]schemaNode `json:"$defs"`
	Description          string                `json:"description"`
}

func loadSchema(t *testing.T) schemaNode {
	t.Helper()
	raw, err := os.ReadFile(filepath.Clean(schemaPath))
	if err != nil {
		t.Fatalf("read the published schema: %v", err)
	}
	var schema schemaNode
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("the published schema is not valid JSON: %v", err)
	}
	return schema
}

// resolve follows a local $ref, which is the only kind this document uses.
func (s schemaNode) resolve(root schemaNode, t *testing.T) schemaNode {
	t.Helper()
	if s.Ref == "" {
		return s
	}
	name, found := strings.CutPrefix(s.Ref, "#/$defs/")
	if !found {
		t.Fatalf("the schema uses a $ref this test cannot follow: %q", s.Ref)
	}
	target, defined := root.Defs[name]
	if !defined {
		t.Fatalf("the schema refers to $defs/%s, which it does not define", name)
	}
	return target
}

// jsonNames is the set of keys a struct accepts, which for this file is
// exactly the set of keys with a json tag.
func jsonNames(structType reflect.Type) []string {
	names := make([]string, 0, structType.NumField())
	for i := range structType.NumField() {
		tag := structType.Field(i).Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name == "" || name == "-" {
			continue
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// compare checks one struct against one schema object, both ways.
func compare(t *testing.T, where string, structType reflect.Type, node, root schemaNode) {
	t.Helper()
	node = node.resolve(root, t)

	inSchema := make([]string, 0, len(node.Properties))
	for name := range node.Properties {
		inSchema = append(inSchema, name)
	}
	slices.Sort(inSchema)

	inGo := jsonNames(structType)
	for _, name := range inGo {
		if name == "rootDirectory" {
			// The one key the Go struct accepts on purpose and the schema
			// must not offer — see TestTheSchemaDoesNotOfferRootDirectory.
			continue
		}
		if !slices.Contains(inSchema, name) {
			t.Errorf("%s accepts %q and the published schema does not describe it — "+
				"add it to %s, or a file that sets it will be flagged by every editor that reads the schema",
				where, name, schemaPath)
		}
	}
	for _, name := range inSchema {
		if !slices.Contains(inGo, name) {
			t.Errorf("the published schema describes %s.%s and the platform does not accept it — "+
				"a file that sets it is refused with \"unknown field\" after the schema said it was fine",
				where, name)
		}
		if node.Properties[name].resolve(root, t).Description == "" {
			t.Errorf("the published schema describes %s.%s with no description — "+
				"the document is the reference an editor and an agent read", where, name)
		}
	}
}

func TestPublishedSchemaMatchesTheFileTheParserAccepts(t *testing.T) {
	root := loadSchema(t)

	compare(t, "kitchen.json", reflect.TypeFor[File](), root, root)
	compare(t, "build", reflect.TypeFor[FileBuild](), root.Properties["build"], root)
	compare(t, "runtime", reflect.TypeFor[FileRuntime](), root.Properties["runtime"], root)
	compare(t, "runtime.resources", reflect.TypeFor[FileResources](), root.Properties["runtime"].Properties["resources"], root)
	compare(t, "runtime.health", reflect.TypeFor[appconfig.Health](), root.Properties["runtime"].Properties["health"], root)
	compare(t, "processes[]", reflect.TypeFor[appconfig.Process](), *root.Properties["processes"].Items, root)
	compare(t, "processes[].health", reflect.TypeFor[appconfig.Health](), root.Defs["process"].Properties["health"], root)
}

// build.rootDirectory is on the Go struct only so that it can be refused with
// a sentence rather than as an unknown field, so it is the one key that must
// not be in the schema: publishing it would be the document telling an editor
// to offer the field the platform exists to refuse.
func TestTheSchemaDoesNotOfferRootDirectory(t *testing.T) {
	root := loadSchema(t)
	if _, offered := root.Properties["build"].Properties["rootDirectory"]; offered {
		t.Fatalf("the schema offers build.rootDirectory, which every file setting it is refused for")
	}
	if !slices.Contains(jsonNames(reflect.TypeFor[FileBuild]()), "rootDirectory") {
		t.Fatalf("FileBuild no longer carries rootDirectory, so the refusal that names it is dead code")
	}
}

// The schema's own $id has to be where the platform tells people the schema
// is, or the two documents disagree about which one is current.
func TestTheSchemaIsPublishedWhereTheRefusalsSayItIs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Clean(schemaPath))
	if err != nil {
		t.Fatalf("read the published schema: %v", err)
	}
	var document struct {
		ID string `json:"$id"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("the published schema is not valid JSON: %v", err)
	}
	if document.ID != SchemaURL {
		t.Fatalf("the schema calls itself %q and the platform points people at %q", document.ID, SchemaURL)
	}
}
