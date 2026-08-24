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

package vex

import (
	"strings"
	"testing"
	"time"
)

// What the reader has to get right: both spellings of an identifier, the four
// statuses, the five justifications, and a refusal that says what to do.

const document = `{
  "@context": "https://openvex.dev/ns/v0.2.0",
  "@id": "https://shop.example/vex/2026-08-24",
  "author": "security@shop.example",
  "role": "Product Security",
  "timestamp": "2026-08-24T09:00:00Z",
  "version": 1,
  "statements": [
    {
      "vulnerability": {"@id": "https://nvd.nist.gov/vuln/detail/CVE-2026-1", "name": "CVE-2026-1"},
      "products": [{"@id": "pkg:oci/shop@sha256:abc"}],
      "status": "not_affected",
      "justification": "vulnerable_code_not_in_execute_path",
      "impact_statement": "the parser is never reached from our entry points"
    },
    {
      "vulnerability": "CVE-2026-2",
      "products": ["pkg:oci/shop@sha256:abc"],
      "status": "affected",
      "action_statement": "upgrade to 2.4.1",
      "supplier": "vendor@upstream.example",
      "timestamp": "2026-08-20T09:00:00Z"
    }
  ]
}`

func parsed(t *testing.T, raw string) Document {
	t.Helper()
	doc, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestBothIdentifierSpellingsRead(t *testing.T) {
	doc := parsed(t, document)
	if err := Validate(doc); err != nil {
		t.Fatalf("a well formed document must validate: %v", err)
	}
	if len(doc.Statements) != 2 {
		t.Fatalf("want two statements, got %d", len(doc.Statements))
	}

	// v0.2.0's object spelling: the name is what a rule matches on, not the
	// NVD URL that happens to be its @id.
	if got := doc.Statements[0].Vulnerability.String(); got != "CVE-2026-1" {
		t.Errorf("object vulnerability read as %q", got)
	}
	// The first specification's bare string, still in circulation.
	if got := doc.Statements[1].Vulnerability.String(); got != "CVE-2026-2" {
		t.Errorf("string vulnerability read as %q", got)
	}
	for index, statement := range doc.Statements {
		products := ProductIdentifiers(statement)
		if len(products) != 1 || products[0] != "pkg:oci/shop@sha256:abc" {
			t.Errorf("statement %d products read as %v", index, products)
		}
	}
}

func TestAuthorshipIsPerStatement(t *testing.T) {
	doc := parsed(t, document)
	// A document collects statements, and one of them may be somebody else's:
	// attributing a vendor's assertion to whoever aggregated it names the
	// wrong party.
	if got := AuthorOf(doc, doc.Statements[0]); got != "security@shop.example" {
		t.Errorf("a statement with no supplier is the document author's, got %q", got)
	}
	if got := AuthorOf(doc, doc.Statements[1]); got != "vendor@upstream.example" {
		t.Errorf("a statement with a supplier is theirs, got %q", got)
	}
	if got := TimestampOf(doc, doc.Statements[0]); got != "2026-08-24T09:00:00Z" {
		t.Errorf("an undated statement takes the document's date, got %q", got)
	}
	if got := TimestampOf(doc, doc.Statements[1]); got != "2026-08-20T09:00:00Z" {
		t.Errorf("a dated statement keeps its own, got %q", got)
	}
}

func TestNotAffectedWithoutAnEnumeratedJustificationIsRefused(t *testing.T) {
	raw := `{
	  "@context": "https://openvex.dev/ns/v0.2.0",
	  "author": "security@shop.example",
	  "statements": [{
	    "vulnerability": "CVE-2026-1",
	    "status": "not_affected",
	    "impact_statement": "we looked at it and it is fine"
	  }]
	}`
	err := Validate(parsed(t, raw))
	if err == nil {
		t.Fatal("free text alone must not be accepted as a justification")
	}
	// The refusal has to be actionable: it names the enumeration and says
	// where the prose belongs.
	for _, want := range []string{"not_affected", "vulnerable_code_not_present", "impact_statement"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must mention %q: %v", want, err)
		}
	}

	for _, justification := range Justifications {
		doc := parsed(t, raw)
		doc.Statements[0].Justification = justification
		if err := Validate(doc); err != nil {
			t.Errorf("justification %q is in the enumeration: %v", justification, err)
		}
	}
}

func TestWhatElseIsRefused(t *testing.T) {
	cases := map[string]string{
		"a document that is not OpenVEX": `{"@context":"https://example.com/ns","author":"a",
			"statements":[{"vulnerability":"CVE-1","status":"fixed"}]}`,
		"a document with no author": `{"@context":"https://openvex.dev/ns/v0.2.0",
			"statements":[{"vulnerability":"CVE-1","status":"fixed"}]}`,
		"a document with no statements": `{"@context":"https://openvex.dev/ns/v0.2.0","author":"a",
			"statements":[]}`,
		"a statement naming no vulnerability": `{"@context":"https://openvex.dev/ns/v0.2.0","author":"a",
			"statements":[{"status":"fixed"}]}`,
		"a status outside the enumeration": `{"@context":"https://openvex.dev/ns/v0.2.0","author":"a",
			"statements":[{"vulnerability":"CVE-1","status":"probably_fine"}]}`,
		"an expiry nothing can parse": `{"@context":"https://openvex.dev/ns/v0.2.0","author":"a",
			"statements":[{"vulnerability":"CVE-1","status":"fixed","expires":"next tuesday"}]}`,
	}
	for name, raw := range cases {
		if err := Validate(parsed(t, raw)); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestOlderContextsAreStillOpenVEX(t *testing.T) {
	// The context carries the specification version, so it is matched by
	// prefix: a document written against an earlier draft is still read.
	for _, context := range []string{
		"https://openvex.dev/ns",
		"https://openvex.dev/ns/v0.0.1",
		"https://openvex.dev/ns/v0.2.0",
	} {
		if !IsOpenVEX(context) {
			t.Errorf("%q must be recognised as OpenVEX", context)
		}
	}
	if IsOpenVEX("https://cyclonedx.org/bom") {
		t.Error("a bill of materials is not a VEX document")
	}
}

func TestExpiryPrefersTheStatementOverTheDocument(t *testing.T) {
	doc := parsed(t, document)
	if !Expiry(doc, doc.Statements[0]).IsZero() {
		t.Error("a document that says nothing about expiry never expires")
	}

	doc.Expires = "2026-12-01T00:00:00Z"
	if got := Expiry(doc, doc.Statements[0]); !got.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("the document's expiry applies to its statements, got %v", got)
	}

	doc.Statements[0].Expires = "2026-09-01T00:00:00Z"
	if got := Expiry(doc, doc.Statements[0]); !got.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("a statement's own expiry wins, got %v", got)
	}
}

func TestUnknownTermsAreCarriedRatherThanRefused(t *testing.T) {
	// OpenVEX is JSON-LD and a document may carry terms this reader has never
	// heard of. Refusing one would make the platform the arbiter of somebody
	// else's vocabulary — and what is signed is the whole document anyway.
	raw := `{"@context":"https://openvex.dev/ns/v0.2.0","author":"a","tooling":"vexctl",
		"some_future_term":{"nested":true},
		"statements":[{"vulnerability":"CVE-1","status":"fixed","another_term":[1,2]}]}`
	if err := Validate(parsed(t, raw)); err != nil {
		t.Fatalf("an unknown term must not be a refusal: %v", err)
	}
}
