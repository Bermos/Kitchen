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

package platformhost

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The list in this package is only worth having if it is the same list the
// platform actually publishes. These tests read the chart — the templates that
// declare HTTPRoutes and the helpers their hostnames come from — and refuse a
// route whose hostname is a label nobody reserved.
//
// The chart is Helm, so nothing in it can import this package. Reading it is
// the substitute, and it is the whole point: a new platform route lands as a
// new template, and a new template with an unreserved hostname fails here.

const chartTemplates = "../../charts/kitchen/templates"

// operatorPublished are the labels the chart never publishes because the
// operator does: the preview gate's route and the registry's are written by
// KitchenReconciler, which needs the shared Gateway to exist first. They are
// pinned against the operator's own constants by
// TestTheOperatorPublishesOnlyReservedHostnames in internal/controller.
var operatorPublished = []string{PreviewGate, Registry}

func TestEveryHostnameTheChartPublishesIsReserved(t *testing.T) {
	found := chartRouteLabels(t)
	if len(found) == 0 {
		t.Fatal("no HTTPRoute hostnames were found in the chart: this test has stopped reading it")
	}
	for label, where := range found {
		if !IsReserved(label) {
			t.Errorf("%s publishes %s.<baseDomain> and %q is not reserved: "+
				"a project of that name would claim the same hostname — add it to platformhost",
				where, label, label)
		}
	}
}

// The other direction: a label nothing publishes should not be reserved, or
// the list grows into a deny-list of names nobody may use for no reason.
func TestEveryReservedLabelIsPublishedBySomething(t *testing.T) {
	published := map[string]bool{}
	for label := range chartRouteLabels(t) {
		published[label] = true
	}
	for _, label := range operatorPublished {
		published[label] = true
	}
	for _, label := range Reserved() {
		if !published[label] {
			t.Errorf("%q is reserved but nothing publishes %s.<baseDomain>: "+
				"either the route went away and the reservation should too, or this test "+
				"can no longer see where it is declared", label, label)
		}
	}
	for label := range published {
		if !IsReserved(label) {
			t.Errorf("%q is published and not reserved", label)
		}
	}
}

// chartRouteLabels reads every `hostnames:` block in the chart's templates and
// answers with the label each hostname resolves to by default, against the
// template that declares it.
func chartRouteLabels(t *testing.T) map[string]string {
	t.Helper()

	helpers := chartHelpers(t)
	labels := map[string]string{}

	err := filepath.WalkDir(chartTemplates, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// The generated CRDs are a copy of upstream schemas, not a route.
		if entry.IsDir() || !strings.HasSuffix(path, ".yaml") || filepath.Base(path) == "crds.yaml" {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, expression := range hostnameExpressions(string(content)) {
			label, ok := defaultLabel(expression, helpers)
			if !ok {
				t.Errorf("%s publishes %q and this test cannot work out which label that is: "+
					"either spell the hostname as <label>.<baseDomain>, or teach this test the shape",
					path, expression)
				continue
			}
			labels[label] = path
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the chart's templates: %v", err)
	}
	return labels
}

// hostnameExpressions pulls the entries out of every `hostnames:` list in one
// template.
func hostnameExpressions(content string) []string {
	expressions := make([]string, 0, 2)
	inList := false
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "hostnames:" {
			inList = true
			continue
		}
		if !inList {
			continue
		}
		if !strings.HasPrefix(trimmed, "- ") {
			inList = false
			continue
		}
		expressions = append(expressions, strings.TrimPrefix(trimmed, "- "))
	}
	return expressions
}

var includeCall = regexp.MustCompile(`include\s+"([^"]+)"`)

// defaultLabel expands a hostname expression through the chart's helpers and
// answers with the reserved label its default resolves to. Every platform
// hostname is a single label in front of the base domain, which in a helper is
// spelled `printf "<label>.%s"` — so an expansion carrying exactly one such
// prefix has named its label.
func defaultLabel(expression string, helpers map[string]string) (string, bool) {
	expanded := expand(expression, helpers, 0)
	var found []string
	for _, label := range Reserved() {
		if strings.Contains(expanded, label+".%s") {
			found = append(found, label)
		}
	}
	// A helper that reaches two labels cannot be read as naming one; the
	// caller reports it rather than guessing.
	if len(found) != 1 {
		return "", false
	}
	return found[0], true
}

// expand substitutes the bodies of the helpers an expression includes, so that
// a hostname assembled through three helpers still shows the prefix its
// default is built from.
func expand(expression string, helpers map[string]string, depth int) string {
	if depth > 8 {
		return expression
	}
	return includeCall.ReplaceAllStringFunc(expression, func(call string) string {
		name := includeCall.FindStringSubmatch(call)[1]
		body, ok := helpers[name]
		if !ok {
			return call
		}
		return expand(body, helpers, depth+1)
	})
}

var (
	defineStart = regexp.MustCompile(`\{\{-?\s*define\s+"([^"]+)"\s*-?\}\}`)
	action      = regexp.MustCompile(`\{\{-?\s*(\w+)`)
)

// chartHelpers reads _helpers.tpl into name -> body. Bodies nest — an `if`
// inside a `define` has an `end` of its own — so the closing `end` is found by
// counting the actions that open a block rather than by taking the first one.
func chartHelpers(t *testing.T) map[string]string {
	t.Helper()

	content, err := os.ReadFile(filepath.Join(chartTemplates, "_helpers.tpl"))
	if err != nil {
		t.Fatalf("reading the chart's helpers: %v", err)
	}
	text := string(content)

	helpers := map[string]string{}
	for _, match := range defineStart.FindAllStringSubmatchIndex(text, -1) {
		name := text[match[2]:match[3]]
		body, ok := blockBody(text[match[1]:])
		if !ok {
			t.Fatalf("%q in _helpers.tpl has no closing end", name)
		}
		helpers[name] = body
	}
	if len(helpers) == 0 {
		t.Fatal("no helpers were found in _helpers.tpl: this test has stopped reading it")
	}
	return helpers
}

// blockBody returns everything up to the `end` that closes the block already
// open at the start of rest.
func blockBody(rest string) (string, bool) {
	depth := 1
	for _, position := range action.FindAllStringSubmatchIndex(rest, -1) {
		switch rest[position[2]:position[3]] {
		case "if", "with", "range", "define", "block":
			depth++
		case "end":
			depth--
			if depth == 0 {
				return rest[:position[0]], true
			}
		}
	}
	return "", false
}
