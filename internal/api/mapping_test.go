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

package api

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The requirement mapping is docs/COMPLIANCE.md §17, and "kept current as each
// issue in this suite ships" is the acceptance criterion it is easiest to fail
// silently: the code keeps working while the document quietly stops describing
// it, and a mapping read as a claim about a platform that no longer exists is
// worse than no mapping at all.
//
// So the two links a test can hold are held here, in the same shape the CLI
// holds its own — every command names the endpoints it calls, and a route that
// moves fails the CLI's tests rather than only the API's.

// compliancePaths are the route prefixes this suite owns. A route under one of
// them is part of the evidence surface and has to be named in the mapping; a
// route anywhere else is ordinary platform surface and is not this document's
// business.
//
// Prefixes rather than a list of paths, deliberately: a list would go stale the
// moment somebody adds `GET /api/v1/compliance/something`, which is exactly the
// case this exists to catch.
var compliancePaths = []string{
	"/api/v1/audit",
	"/api/v1/compliance",
	"/api/v1/decisions",
	"/api/v1/exceptions",
	"/api/v1/access/",
	"/api/v1/policy/bundles",
	"/api/v1/platform/retention",
	"/api/v1/promotions/",
	"/api/v1/projects/{name}/audit-pack",
	"/api/v1/projects/{name}/exceptions",
	"/api/v1/projects/{name}/promotions",
	"/api/v1/builds/{name}/attestations",
	"/api/v1/builds/{name}/gates",
	"/api/v1/builds/{name}/vex",
	"/api/v1/environments/{name}/requirements",
	"/api/v1/environments/{name}/eligibility",
}

func TestTheMappingCoversEveryComplianceSurface(t *testing.T) {
	document, err := os.ReadFile(filepath.Join("..", "..", "docs", "COMPLIANCE.md"))
	if err != nil {
		t.Fatalf("the compliance design document must exist: %v", err)
	}
	whole := string(document)

	mapping, ok := section(whole, "## 17.")
	if !ok {
		t.Fatal("docs/COMPLIANCE.md has no §17 — the requirement mapping is the deliverable of #143, " +
			"and everything below depends on it being there to point at")
	}

	// Every endpoint this suite answers a requirement with. An endpoint the
	// mapping does not name is an endpoint an examiner has no route to.
	policy, err := PolicyTable()
	if err != nil {
		t.Fatalf("the enforcement table must be readable: %v", err)
	}
	unmapped := []string{}
	for _, route := range policy.Routes {
		path := route.Pattern
		if _, after, found := strings.Cut(path, " "); found {
			path = after
		}
		if !isCompliancePath(path) {
			continue
		}
		// The document writes an endpoint without the version prefix every
		// other page also drops, so that is what is looked for.
		if !names(mapping, strings.TrimPrefix(path, "/api/v1")) {
			unmapped = append(unmapped, route.Pattern)
		}
	}
	if len(unmapped) > 0 {
		t.Errorf("docs/COMPLIANCE.md §17 names no answer for %s — a compliance endpoint that is in "+
			"the API and not in the mapping is one nobody reading the mapping can find. Add it to §17.4 "+
			"or to §17.6, or take its path back out of compliancePaths here if it is not evidence surface",
			strings.Join(unmapped, ", "))
	}

	// And every component. The phases table (§15) names each one with the
	// issue that built it, so it is the list rather than a second list kept
	// here — a phase that ships a component without a row in §17.4 fails.
	phases, ok := section(whole, "## 15.")
	if !ok {
		t.Fatal("docs/COMPLIANCE.md has no §15 — the phases table is where the component list comes from")
	}
	missing := []string{}
	for _, issue := range issueNumbers(phases) {
		if !strings.Contains(mapping, issue) {
			missing = append(missing, issue)
		}
	}
	if len(missing) > 0 {
		t.Errorf("docs/COMPLIANCE.md §17 has no row for %s — every shipped component appears in the "+
			"mapping table is #143's first acceptance criterion, and a component nobody mapped reads "+
			"as one that answers nothing", strings.Join(missing, ", "))
	}
}

// isCompliancePath reports whether a route belongs to the evidence surface.
func isCompliancePath(path string) bool {
	for _, prefix := range compliancePaths {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// names reports whether the text mentions this endpoint as a whole path rather
// than as the head of a longer one. Without the boundary, `/audit` would be
// satisfied by `/audit-pack` — which is the one pair in this suite where the
// mistake is easy to make and the meaning is entirely different.
func names(text, path string) bool {
	found := regexp.MustCompile(regexp.QuoteMeta(path) + `([^A-Za-z0-9/_{}-]|$)`)
	return found.MatchString(text)
}

// issueNumbers pulls the `#123` references out of a stretch of the document, in
// order and once each.
func issueNumbers(text string) []string {
	found := regexp.MustCompile(`#\d+`).FindAllString(text, -1)
	seen := map[string]bool{}
	numbers := []string{}
	for _, number := range found {
		if seen[number] {
			continue
		}
		seen[number] = true
		numbers = append(numbers, number)
	}
	return numbers
}

// section returns the text of the heading starting with prefix, up to the next
// second-level heading or the end of the document.
func section(document, prefix string) (string, bool) {
	start := strings.Index(document, "\n"+prefix)
	if start < 0 {
		return "", false
	}
	rest := document[start+1:]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		return rest[:end], true
	}
	return rest, true
}
