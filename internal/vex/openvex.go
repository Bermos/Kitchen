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

// Package vex reads OpenVEX documents: the vendor's or the team's word that a
// vulnerability the scanner found is not exploitable in this artifact.
//
// It is a reader and a validator and deliberately not a writer. A VEX document
// is somebody's assertion, and the platform's job is to carry it verbatim,
// sign that these bytes were submitted by that identity at that moment, and let
// the environment's policy decide whether to believe it. Re-encoding a
// document through these structs would make the platform the author of a
// claim it merely received — the same rule §6.5 of docs/COMPLIANCE.md keeps
// for a bill of materials.
//
// # Why the platform is stricter than the specification
//
// OpenVEX allows `not_affected` to be explained by an `impact_statement` in
// free text instead of by a justification from its enumeration. Kitchen
// refuses that at ingest, because a suppression is only auditable if the
// reason it was granted is a value somebody can count: "the vulnerable code
// is not in the execute path" is a claim a reviewer can check against the
// artifact, and "we looked at it and it's fine" is a sentence. Free text is
// still carried — in `status_notes` and `impact_statement`, beside the
// justification rather than instead of it.
package vex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ContextPrefix is what every OpenVEX document's `@context` begins with. It is
// matched by prefix rather than by equality because the context carries the
// specification version — `https://openvex.dev/ns/v0.2.0` today, bare
// `https://openvex.dev/ns` in the first drafts — and a document written
// against an older version is still a document, read by the same fields.
const ContextPrefix = "https://openvex.dev/ns"

// Context is the version Kitchen writes about and validates against.
const Context = "https://openvex.dev/ns/v0.2.0"

// The four OpenVEX statuses. They are the whole vocabulary: a status outside
// them is refused rather than carried, because a rule that matched on a
// status nobody defined would be matching on a typo.
const (
	// StatusNotAffected: the vulnerability is present in the bill of
	// materials and cannot be exploited here. The only status that suppresses
	// anything, and the only one that has to say why.
	StatusNotAffected = "not_affected"
	// StatusAffected: it is present and it is exploitable. An honest VEX
	// document says this more often than the other three.
	StatusAffected = "affected"
	// StatusFixed: this artifact already carries the fix.
	StatusFixed = "fixed"
	// StatusUnderInvestigation: somebody is looking. It is a promise to
	// answer later, not an answer, and it suppresses nothing.
	StatusUnderInvestigation = "under_investigation"
)

// Statuses is the enumeration, for a message that has to list it.
var Statuses = []string{StatusNotAffected, StatusAffected, StatusFixed, StatusUnderInvestigation}

// The OpenVEX justification enumeration: the five reasons a product can be
// `not_affected` by a vulnerability it contains.
//
// The list is closed by the specification and closed here. It is what makes
// suppression reviewable in aggregate — "how many of our waivers are
// `vulnerable_code_not_present`, and who says so" is a question with an
// answer, and it would not be one if the reason were prose.
const (
	JustificationComponentNotPresent   = "component_not_present"
	JustificationVulnerableCodeAbsent  = "vulnerable_code_not_present"
	JustificationNotInExecutePath      = "vulnerable_code_not_in_execute_path"
	JustificationCannotBeControlled    = "vulnerable_code_cannot_be_controlled_by_adversary"
	JustificationInlineMitigationsHold = "inline_mitigations_already_exist"
)

// Justifications is the enumeration in the specification's own order.
var Justifications = []string{
	JustificationComponentNotPresent,
	JustificationVulnerableCodeAbsent,
	JustificationNotInExecutePath,
	JustificationCannotBeControlled,
	JustificationInlineMitigationsHold,
}

// Justified reports whether a justification is one of the five. It is the one
// implementation: the ingest refusal, the read surface's `justified` flag and
// the default bundle's own list all mean the same thing by it.
func Justified(justification string) bool {
	for _, known := range Justifications {
		if justification == known {
			return true
		}
	}
	return false
}

// KnownStatus reports whether a status is one of the four.
func KnownStatus(status string) bool {
	for _, known := range Statuses {
		if status == known {
			return true
		}
	}
	return false
}

// IsOpenVEX reports whether a predicate type is an OpenVEX document in any
// version. It is how the evidence set is filtered, and it matches by prefix
// for the reason ContextPrefix does.
func IsOpenVEX(predicateType string) bool {
	return strings.HasPrefix(predicateType, ContextPrefix)
}

// Identifier is OpenVEX's "either a string or an object" spelling.
//
// The first version of the specification wrote a vulnerability as the bare
// string `"CVE-2026-1"` and a product as a bare package URL; v0.2.0 writes
// both as objects carrying `@id` and, for a vulnerability, `name`. Both are
// still in circulation — a document is written by whatever tool the author
// runs — so both are read, and String() is the one spelling everything
// downstream matches on.
type Identifier struct {
	ID   string `json:"@id,omitempty"`
	Name string `json:"name,omitempty"`
}

// UnmarshalJSON reads either spelling.
func (i *Identifier) UnmarshalJSON(raw []byte) error {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		i.Name = strings.TrimSpace(text)
		return nil
	}
	object := struct {
		ID   string `json:"@id"`
		Name string `json:"name"`
	}{}
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("an OpenVEX identifier is a string or an object with @id: %w", err)
	}
	i.ID, i.Name = strings.TrimSpace(object.ID), strings.TrimSpace(object.Name)
	return nil
}

// String is the identifier as a rule sees it: the name where the document
// gave one, and the `@id` otherwise. For a vulnerability that is `CVE-2026-1`
// either way; for a product it is the package URL.
func (i Identifier) String() string {
	if i.Name != "" {
		return i.Name
	}
	return i.ID
}

// Document is one OpenVEX document.
//
// Unknown fields are tolerated rather than refused, which is the opposite of
// every other body this API decodes. OpenVEX is JSON-LD: a document may carry
// terms this reader has never heard of, and refusing one would make the
// platform the arbiter of somebody else's vocabulary. What is *read* is
// exactly the fields below; what is *signed* is the whole document, verbatim.
type Document struct {
	Context     string `json:"@context"`
	ID          string `json:"@id,omitempty"`
	Author      string `json:"author"`
	Role        string `json:"role,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
	LastUpdated string `json:"last_updated,omitempty"`
	Tooling     string `json:"tooling,omitempty"`

	// Expires is Kitchen's extension and not OpenVEX's — see Expiry.
	Expires string `json:"expires,omitempty"`

	Statements []Statement `json:"statements"`
}

// Statement is one assertion inside a document: this vulnerability, in these
// products, has this status, for this reason.
type Statement struct {
	ID            string       `json:"@id,omitempty"`
	Vulnerability Identifier   `json:"vulnerability"`
	Products      []Identifier `json:"products,omitempty"`
	Status        string       `json:"status"`
	Justification string       `json:"justification,omitempty"`
	// StatusNotes and ImpactStatement are the document's free text. They are
	// carried and shown and they are never a justification: see the package
	// comment for why that distinction is the whole point.
	StatusNotes     string `json:"status_notes,omitempty"`
	ImpactStatement string `json:"impact_statement,omitempty"`
	ActionStatement string `json:"action_statement,omitempty"`
	// Supplier is who made *this* statement, where a document collects
	// statements from more than one source. Absent, the document's author is
	// the author of every statement in it.
	Supplier    string `json:"supplier,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
	LastUpdated string `json:"last_updated,omitempty"`

	// Expires is Kitchen's extension and not OpenVEX's — see Expiry.
	Expires string `json:"expires,omitempty"`
}

// Parse reads a document. It does not validate it: a caller that is going to
// store one calls Validate too, and a caller reading back what is already
// attached takes what it finds.
func Parse(raw []byte) (Document, error) {
	document := Document{}
	if err := json.Unmarshal(raw, &document); err != nil {
		return Document{}, fmt.Errorf("this is not a readable OpenVEX document: %w", err)
	}
	return document, nil
}

// Validate says what is wrong with a document, in one sentence its submitter
// can act on, or nil.
//
// It is deliberately strict about three things and lenient about everything
// else. A status outside the enumeration, a `not_affected` without an
// enumerated justification, and a timestamp that is not RFC 3339 are all
// refused; an unknown term, a missing optional field and a product nobody
// named are not.
func Validate(document Document) error {
	if !IsOpenVEX(strings.TrimSpace(document.Context)) {
		return fmt.Errorf(
			"an OpenVEX document declares @context %q; this one declares %q",
			Context, document.Context)
	}
	if strings.TrimSpace(document.Author) == "" {
		return errors.New(
			"an OpenVEX document has to name its author: a statement nobody signs their name to " +
				"is not an exception register entry")
	}
	if len(document.Statements) == 0 {
		return errors.New("an OpenVEX document with no statements asserts nothing")
	}
	if err := timestamped("the document's", document.Timestamp); err != nil {
		return err
	}
	if err := timestamped("the document's", document.Expires); err != nil {
		return err
	}
	for index, statement := range document.Statements {
		if err := validateStatement(index, statement); err != nil {
			return err
		}
	}
	return nil
}

func validateStatement(index int, statement Statement) error {
	where := fmt.Sprintf("statement %d", index+1)
	if statement.Vulnerability.String() == "" {
		return fmt.Errorf("%s names no vulnerability", where)
	}
	where = fmt.Sprintf("%s (%s)", where, statement.Vulnerability.String())

	if !KnownStatus(statement.Status) {
		return fmt.Errorf("%s has status %q, and OpenVEX defines %s",
			where, statement.Status, strings.Join(Statuses, ", "))
	}
	if statement.Status == StatusNotAffected && !Justified(statement.Justification) {
		return fmt.Errorf(
			"%s is not_affected and its justification is %q: not_affected requires one of %s. "+
				"Free text belongs in impact_statement or status_notes, beside the justification "+
				"rather than instead of it — a suppression whose reason cannot be counted cannot be reviewed",
			where, statement.Justification, strings.Join(Justifications, ", "))
	}
	if err := timestamped(where+"'s", statement.Timestamp); err != nil {
		return err
	}
	return timestamped(where+"'s", statement.Expires)
}

// timestamped checks one optional RFC 3339 field. An unparseable timestamp is
// refused rather than ignored: every expiry judgement downstream reads these,
// and a date nothing can parse is a statement that never expires.
func timestamped(what, value string) error {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		return fmt.Errorf("%s timestamp %q is not an RFC 3339 time", what, value)
	}
	return nil
}

// Expiry is when a statement stops asserting anything, and the zero time when
// it never does.
//
// **OpenVEX has no expiry field.** `expires` is Kitchen's own term, read off
// the statement first and off the document as a default for all of its
// statements. It is in the signed bytes rather than beside them, because an
// expiry supplied out of band would be an unattributable edit to somebody
// else's assertion — and a reader that has never heard of the term simply
// treats the statement as unbounded, which is the same thing every OpenVEX
// reader does today.
func Expiry(document Document, statement Statement) time.Time {
	for _, candidate := range []string{statement.Expires, document.Expires} {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		if at, err := time.Parse(time.RFC3339, candidate); err == nil {
			return at.UTC()
		}
	}
	return time.Time{}
}

// AuthorOf is whose word one statement is: its own supplier where it names
// one, and the document's author otherwise. Authorship is per statement
// because a document is allowed to collect somebody else's assertions, and an
// aggregator's name on a vendor's statement would attribute it to the wrong
// party.
func AuthorOf(document Document, statement Statement) string {
	if supplier := strings.TrimSpace(statement.Supplier); supplier != "" {
		return supplier
	}
	return strings.TrimSpace(document.Author)
}

// TimestampOf is when one statement was made: its own, or the document's.
func TimestampOf(document Document, statement Statement) string {
	if stamp := strings.TrimSpace(statement.Timestamp); stamp != "" {
		return stamp
	}
	return strings.TrimSpace(document.Timestamp)
}

// ProductIdentifiers is what a statement says it is about, as strings. An
// empty list means the document named no product, which is how a document
// written about one artifact and attached to that artifact's digest reads.
func ProductIdentifiers(statement Statement) []string {
	if len(statement.Products) == 0 {
		return nil
	}
	products := make([]string, 0, len(statement.Products))
	for _, product := range statement.Products {
		if identifier := product.String(); identifier != "" {
			products = append(products, identifier)
		}
	}
	if len(products) == 0 {
		return nil
	}
	return products
}
