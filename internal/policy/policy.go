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

// Package policy is the platform's one policy engine: an embedded Rego
// evaluator that decides whether an artifact may move into an environment,
// and nothing else decides that anywhere.
//
// Promotion, scheduled re-evaluation and replay all come through Evaluate,
// which is what makes their verdicts comparable — the same bundle and the
// same input produce the same answer whichever of the three asked, and a
// decision that cannot be reproduced from what was stored is a decision that
// was never really made.
//
// Two properties are load-bearing and both are enforced rather than assumed:
//
//   - **Inputs are fully materialized before evaluation.** The Input struct
//     is everything a policy may look at, assembled by the caller from stored
//     evidence. There is no way for a rule to fetch anything: the engine
//     compiles bundles against a capability set with http.send, every net.*
//     builtin and opa.runtime removed, so a bundle that tries is refused at
//     compile time (a test proves it).
//   - **Everything is named by digest.** A bundle is content-addressed
//     (Digest), the input is content-addressed (Input.Digest), and a stored
//     decision carries both — which is the whole of what replay needs.
package policy

import (
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// ruleIDPattern matches the rule id literal inside a deny entry. See RuleIDs.
var ruleIDPattern = regexp.MustCompile(`"rule"\s*:\s*"([^"]+)"`)

// Bundle is one policy bundle: file path to file content. The .rego files are
// the rules; an optional data.json is the bundle's own static data, served to
// the rules as `data`.
//
// A bundle is a value, not a location. However it arrived — compiled in,
// read from a ConfigMap, read back out of the decision store — two bundles
// with the same files are the same bundle, and Digest is what says so.
type Bundle map[string]string

// Digest content-addresses a bundle: a sha256 over its (path, content) pairs,
// sorted by path, each part length-prefixed so that no arrangement of file
// names and contents can collide with another. Rendered `sha256:<hex>`, the
// same form every artifact digest uses, because it is used the same way — an
// environment's requirements pin a bundle by this string.
func Digest(bundle Bundle) string {
	paths := make([]string, 0, len(bundle))
	for path := range bundle {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	sum := sha256.New()
	var length [8]byte
	write := func(value string) {
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		sum.Write(length[:])
		sum.Write([]byte(value))
	}
	for _, path := range paths {
		write(path)
		write(bundle[path])
	}
	return "sha256:" + hex.EncodeToString(sum.Sum(nil))
}

// The built-in default bundle. It ships inside the operator so that an
// installation has rules to require on day one, and it is addressed by digest
// like any other bundle — an environment that pins it keeps the exact version
// it pinned even across an operator upgrade that changes the default, because
// the old content is persisted to the decision store on first use.
//
//go:embed bundles/default/*.rego
var defaultBundleFS embed.FS

// DefaultBundle is the built-in bundle, rebuilt from the embedded files on
// each call so a caller cannot mutate the package's copy.
func DefaultBundle() Bundle {
	bundle := Bundle{}
	entries, err := fs.ReadDir(defaultBundleFS, "bundles/default")
	if err != nil {
		// The files are compiled in; this cannot fail short of a broken build.
		panic(fmt.Sprintf("the built-in policy bundle is unreadable: %v", err))
	}
	for _, entry := range entries {
		content, err := fs.ReadFile(defaultBundleFS, "bundles/default/"+entry.Name())
		if err != nil {
			panic(fmt.Sprintf("the built-in policy bundle is unreadable: %v", err))
		}
		bundle[entry.Name()] = string(content)
	}
	return bundle
}

// RuleIDs lists the stable rule identifiers a bundle can fire, read from its
// sources. A deny entry is `{"rule": "<id>", "message": ...}`, and the id is
// a literal by contract — a rule whose id was computed would be a rule no
// requirement could name — so reading the literals out of the source is
// faithful, and it is how the bundle listing can say what a bundle enforces
// without evaluating it against anything.
func RuleIDs(bundle Bundle) []string {
	seen := map[string]struct{}{}
	for path, content := range bundle {
		if !strings.HasSuffix(path, ".rego") {
			continue
		}
		for _, match := range ruleIDPattern.FindAllStringSubmatch(content, -1) {
			seen[match[1]] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
