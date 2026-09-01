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

// Command gen-claim-matrix writes the matrix of what every claim provider
// declares — what a preview gets, whether the environment still idles,
// whether deploys recreate — into docs/api/claims.md, between the page's
// generated markers.
//
// The declarations are Go values next to the provisioners
// (internal/provider/declarations gathers them), and prose about them
// drifts: this repository's answer is to hold the machine-checkable half in
// a test, the way ui/src/lib/design.test.ts holds the design guide and
// gen-ui-policy holds the dashboard's copy of the enforcement table. The
// table is generated here, and internal/provider/declarations' test fails
// when the checked-in page and a fresh render differ.
//
// Only the block between the markers is written; the prose around it is
// the page's own. Run it with `make claim-matrix`.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Bermos/Kitchen/internal/provider/declarations"
)

// defaultOut is the page, relative to the repository root — which is where
// `make claim-matrix` runs this from.
const defaultOut = "docs/api/claims.md"

func main() {
	out := flag.String("o", defaultOut, "the page carrying the generated markers to write the matrix into")
	flag.Parse()

	if err := run(*out); err != nil {
		fmt.Fprintf(os.Stderr, "gen-claim-matrix: %v\n", err)
		os.Exit(1)
	}
}

func run(out string) error {
	raw, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	spliced, err := declarations.Splice(string(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", out, err)
	}
	if spliced == string(raw) {
		fmt.Printf("gen-claim-matrix: %s is up to date (%d declarations)\n", out, len(declarations.All()))
		return nil
	}
	if err := os.WriteFile(out, []byte(spliced), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", out, err)
	}
	fmt.Printf("gen-claim-matrix: %s (%d declarations)\n", out, len(declarations.All()))
	return nil
}
