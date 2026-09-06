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

package controller

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// pinnedReference is what an image this repository compiles in has to look
// like: a repository, a tag, and the digest the tag pointed at when somebody
// last read it. The tag is for a person; the digest is what the kubelet
// resolves, so it is the half that decides what runs.
var pinnedReference = regexp.MustCompile(`^[^\s@]+:[^\s@:/]+@sha256:[0-9a-f]{64}$`)

// TestEveryImageConstantNamesADigest is the half of #427 a machine can hold.
//
// Every image the operator makes the cluster run used to be named by tag
// alone, on repositories nobody here owns — the self-update job's helm among
// them, which runs under an account bound to cluster-admin. A tag is a
// mutable pointer: repointed, it is somebody else's code, and nothing in the
// platform would notice, because moving is what tags are for.
//
// So the rule is that an image constant names a digest, and this is what
// keeps it true of the next one somebody adds. It reads the source rather
// than the values, because a constant is what a reviewer sees and because
// the point is to fail on the constant that was written without one.
//
// What it deliberately does not cover:
//
//   - internal/provider/database's catalogue, whose "tag" is a Postgres
//     major and whose whole purpose is to move: a database created next
//     month should get that major's current patch release, and a digest
//     there would pin twelve references that go stale monthly and freeze
//     new databases on an unpatched Postgres. It is not an image the
//     operator runs, either — CloudNativePG pulls it.
//   - the chart's own images, which are values with a pullPolicy an
//     operator sets, and which release-please moves.
//
// The other half — whether a recorded digest is still the digest that tag
// points at — needs the registry, so it lives in hack/check-image-pins.sh
// and is run by hand.
func TestEveryImageConstantNamesADigest(t *testing.T) {
	root := filepath.Join("..", "..", "internal")

	var checked int
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || filepath.Ext(path) != ".go" {
			return err
		}
		// Test files name images that do not exist on purpose — a fixture's
		// registry.example.com, a digest spelled "feed".
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}

		check := func(name string, value ast.Expr) {
			literal, ok := value.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return
			}
			reference, err := strconv.Unquote(literal.Value)
			if err != nil || reference == "" {
				return
			}
			checked++
			if !pinnedReference.MatchString(reference) {
				t.Errorf("%s: %s is %q, which is not repository:tag@sha256:… — "+
					"read the digest off the registry (hack/check-image-pins.sh, "+
					"or `crane digest`) and write both halves, the way "+
					"internal/controller/images.go does",
					path, name, reference)
			}
		}

		ast.Inspect(file, func(node ast.Node) bool {
			switch declaration := node.(type) {
			// DefaultHelmImage = "alpine/helm:…@sha256:…"
			case *ast.ValueSpec:
				for i, name := range declaration.Names {
					if !namesAnImage(name.Name) || i >= len(declaration.Values) {
						continue
					}
					check(name.Name, declaration.Values[i])
				}
			// {Major: "8", Image: "valkey/valkey:…@sha256:…"}
			case *ast.KeyValueExpr:
				key, ok := declaration.Key.(*ast.Ident)
				if !ok || !namesAnImage(key.Name) {
					return true
				}
				check(key.Name, declaration.Value)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("reading internal/: %v", err)
	}

	// A refactor that renamed every constant out of the shape above would
	// otherwise leave this test passing over nothing at all.
	if checked < 10 {
		t.Errorf("found only %d image constants under internal/, which is fewer than "+
			"this repository has: the search above no longer finds them", checked)
	}
}

// namesAnImage reports whether an identifier is one of the image references
// the rule is about. The suffix is the whole of it: imageAnnotation and
// reasonImagePullBackOff are neither images nor named like one.
func namesAnImage(name string) bool {
	return strings.HasSuffix(name, "Image") || strings.HasSuffix(name, "Images")
}
