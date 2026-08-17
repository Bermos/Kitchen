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

package flows

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Path normalisation: the stateless half of the route templating the follower
// does at ingest.
//
// `/users/12345` is not a path. Per-route numbers without templating grow a
// series per user id — in a column the store keeps a dictionary for, and in
// the ordering key of both rollups — and no two requests ever group, so the
// breakdown the golden signals exist to give answers nothing. Templating turns
// the unbounded set into a route table.
//
// It happens here rather than in SQL because the budget in budget.go needs
// per-environment state, and because the store should never see the unbounded
// set at all: normalising after the insert is normalising after the damage.
//
// The shape mirrors the log-pattern normaliser in clickhouse/loganalytics.go —
// an ordered table of narrowing rules, first match winning — for the same
// reason it has one: the variable parts of a message and the variable parts of
// a path are the same problem, and one taste for it is easier to reason about
// than two.

const (
	// The placeholders a classified segment is recorded as. They are spelled
	// the way a route template is spelled in every framework someone deploying
	// here has already used, so the route table reads as the routes the
	// application declares rather than as a scheme this package invented.
	segmentID   = ":id"
	segmentUUID = ":uuid"
	segmentHash = ":hash"

	// overflowSegment stands for "and more, which is not worth a series of its
	// own". It is both the tail of a path deeper than the cap and, in
	// budget.go, the whole route of a request past an environment's budget:
	// the two are the same statement about the same thing.
	overflowSegment = "…"
	overflowRoute   = "/" + overflowSegment

	// maxRouteDepth caps how many segments a template keeps. Twelve is past
	// any route a person wrote; beyond it a path is describing a tree someone
	// is walking, not an endpoint an application serves.
	maxRouteDepth = 12

	// maxSegmentLength caps one segment of a template. A segment this long
	// that matched none of the rules below is not a route name, but it is
	// still worth showing the beginning of — a truncated segment says what it
	// was, where `:hash` would only say that it was long.
	maxSegmentLength = 64

	// maxRawPathBytes is where the raw `path` column is truncated. It is
	// generous by design: the raw path is what makes a mis-templated route
	// diagnosable, and 512 bytes of it is the cheapest column in the table.
	maxRawPathBytes = 512
)

// segmentRules classify one whole path segment, in order, first match winning.
//
// Order carries the correctness here the way it does in patternNormalisers.
// Every scheme below is a subset of a wider one — a UUID is hex with dashes, a
// sixteen-digit id is valid hex, a ULID is alphanumeric — so a rule tried out
// of turn does not merely mislabel one shape, it swallows the narrower ones
// whole and every identifier in the store reads as `:hash`.
var segmentRules = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	// A UUID, in the only spelling anything emits it in.
	{regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`), segmentUUID},
	// A plain number, which is what a sequential key looks like. It has to
	// precede the hex rule: `12345678` satisfies both, and it is an id.
	{regexp.MustCompile(`^\d+$`), segmentID},
	// Hex from eight characters up: a short digest, an object id, a commit.
	{regexp.MustCompile(`^(0x)?[0-9a-fA-F]{8,}$`), segmentHash},
	// A ULID: 26 characters of Crockford base32, which omits I, L, O and U.
	{regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`), segmentHash},
	// A KSUID: 27 characters of base62.
	{regexp.MustCompile(`^[0-9A-Za-z]{27}$`), segmentHash},
}

// assetExtension is what the last dot-part of a filename has to look like for
// the segment to be a file at all: short, and starting with a letter, so that
// `v1.2.3` and `2026.05.01` are not read as files with extensions `3` and
// `01`.
var assetExtension = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,4}$`)

// hexToken is the commonest spelling of a bundler's content hash.
var hexToken = regexp.MustCompile(`^[0-9a-fA-F]{6,}$`)

const (
	// minTokenLength and minTokenDistinct are where a segment stops reading as
	// a name and starts reading as a token. Both bounds are needed: length
	// alone catches long slugs, and variety alone catches short ones.
	minTokenLength   = 16
	minTokenDistinct = 12
)

// templatePath turns one request path into the route template it belongs to.
//
// The path it is given has already had its query string removed and is never
// given one back: that is a privacy rule (§3.2) rather than a cardinality one,
// and the query is dropped in splitURL so that no caller can reintroduce it.
func templatePath(path string) string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return "/"
	}
	parts := strings.Split(trimmed, "/")
	segments := make([]string, 0, min(len(parts), maxRouteDepth))
	for _, part := range parts {
		// An empty part is a `//` somebody typed, not a segment of the route.
		if part == "" {
			continue
		}
		if len(segments) == maxRouteDepth {
			return "/" + strings.Join(segments, "/") + overflowRoute
		}
		segments = append(segments, templateSegment(part))
	}
	if len(segments) == 0 {
		return "/"
	}
	return "/" + strings.Join(segments, "/")
}

// templateSegment classifies one segment, leaving anything it does not
// recognise as itself: a route template is only useful if the parts that are
// route names survive as route names.
func templateSegment(segment string) string {
	for _, rule := range segmentRules {
		if rule.pattern.MatchString(segment) {
			return rule.replacement
		}
	}
	if asset, ok := hashedAsset(segment); ok {
		return asset
	}
	if opaque(segment) {
		return segmentHash
	}
	return capSegment(segment)
}

// hashedAsset recognises the one identifier scheme that does not get a segment
// of its own: a bundler's content hash spliced into a filename's stem, as in
// `app.8f3ab2c1.js`. Every build mints a new one, so left alone a single asset
// is a new route on every release — and an application that serves its own
// static files serves dozens of them.
func hashedAsset(segment string) (string, bool) {
	parts := strings.Split(segment, ".")
	if len(parts) < 2 {
		return "", false
	}
	extension := parts[len(parts)-1]
	if !assetExtension.MatchString(extension) {
		return "", false
	}
	for _, part := range parts[:len(parts)-1] {
		if contentHash(part) {
			return "*." + extension, true
		}
	}
	return "", false
}

// contentHash answers whether one dot-part of a filename is a digest rather
// than a name: hex from six characters, or eight-plus alphanumerics that mix
// letters with digits, which is what the base-36 and base-64 spellings look
// like. A name a person chose is letters or digits, essentially never both.
func contentHash(part string) bool {
	if hexToken.MatchString(part) {
		return true
	}
	if len(part) < 8 {
		return false
	}
	digits, letters := 0, 0
	for _, r := range part {
		switch {
		case isDigit(r):
			digits++
		case isLetter(r):
			letters++
		default:
			return false
		}
	}
	return digits > 0 && letters > 0
}

// opaque answers whether a segment none of the named schemes recognised is an
// identifier all the same: long, mixing letters with digits, and varied enough
// not to be a word. It is the catch-all underneath them, because session keys,
// signed-URL parameters and base64 blobs mint a route each and there is no
// finite list of the shapes they arrive in.
//
// A segment that reads as words and numbers joined by separators is exempt,
// because `getting-started-2026` passes every other test here and is a route.
func opaque(segment string) bool {
	if len(segment) < minTokenLength || wordy(segment) {
		return false
	}
	distinct := make(map[rune]struct{}, len(segment))
	digits, letters := 0, 0
	for _, r := range segment {
		distinct[r] = struct{}{}
		switch {
		case isDigit(r):
			digits++
		case isLetter(r):
			letters++
		}
	}
	return digits > 0 && letters > 0 && len(distinct) >= minTokenDistinct
}

// wordy answers whether a segment is words and numbers joined by separators,
// which is what a slug someone wrote looks like and what a token never does —
// a token's parts mix letters and digits, a slug's parts are one or the other.
func wordy(segment string) bool {
	parts := strings.FieldsFunc(segment, func(r rune) bool { return r == '-' || r == '_' })
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if !uniform(part) {
			return false
		}
	}
	return true
}

// uniform answers whether a part is all letters or all digits, and nothing
// else at all: a punctuation mark inside a separated part is a sign of an
// encoding rather than of prose.
func uniform(part string) bool {
	digits, letters := 0, 0
	for _, r := range part {
		switch {
		case isDigit(r):
			digits++
		case isLetter(r):
			letters++
		default:
			return false
		}
	}
	return (digits == 0) != (letters == 0)
}

func isDigit(r rune) bool { return r >= '0' && r <= '9' }

func isLetter(r rune) bool { return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') }

// capSegment bounds one segment of a template, counting runes rather than
// bytes so that a cap meant to bound a name never cuts a character in half.
func capSegment(segment string) string {
	if utf8.RuneCountInString(segment) <= maxSegmentLength {
		return segment
	}
	return string([]rune(segment)[:maxSegmentLength]) + overflowSegment
}

// truncatePath bounds the raw `path` column, on a rune boundary for the same
// reason: half a character is not a shorter path, it is invalid UTF-8 in a
// column something will later try to display.
func truncatePath(path string) string {
	if len(path) <= maxRawPathBytes {
		return path
	}
	cut := maxRawPathBytes
	for cut > 0 && !utf8.RuneStart(path[cut]) {
		cut--
	}
	return path[:cut]
}

// splitURL takes the authority and the path out of the URL Hubble recorded.
//
// Cilium's L7 parser writes the whole request URL — `http://shop.example.com/x`
// — because Envoy's access record carries the authority separately from the
// target. The authority is what attribution keys on (hosts.go); the path is
// what gets templated.
//
// The query string is dropped here and reaches nothing downstream. That is
// §3.2's privacy rule, not an optimisation: a request path is a thing an
// application chose to publish, and a query string is whatever a person typed
// into it. Recommending Hubble's own `redact.http.urlQuery` in the
// prerequisites is defence in depth for the same rule; this is the layer that
// does not depend on the cluster being configured correctly.
func splitURL(raw string) (authority, path string) {
	if parsed, err := url.Parse(raw); err == nil {
		// EscapedPath and never RawQuery or Fragment — url.Parse has already
		// separated them, and nothing here puts them back.
		return parsed.Host, orRoot(parsed.EscapedPath())
	}

	// A URL net/url refuses is still a request that was served, and dropping
	// the row would under-report exactly the malformed traffic someone would
	// want to see. Everything before the first `?` or `#` is the target, and
	// everything after the scheme and before the first `/` is the authority.
	rest := raw
	if cut := strings.IndexAny(rest, "?#"); cut >= 0 {
		rest = rest[:cut]
	}
	scheme := strings.Index(rest, "://")
	if scheme < 0 {
		return "", orRoot(rest)
	}
	rest = rest[scheme+len("://"):]
	if cut := strings.IndexByte(rest, '/'); cut >= 0 {
		return rest[:cut], orRoot(rest[cut:])
	}
	return rest, "/"
}

// orRoot spells the empty path the way every other layer spells it. A request
// for `http://host` asked for `/`, and a row saying it asked for nothing would
// be a second value meaning the same route.
func orRoot(path string) string {
	if path == "" {
		return "/"
	}
	return path
}
