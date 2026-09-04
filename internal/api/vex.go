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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/audit"
	"github.com/Bermos/Kitchen/internal/clickhouse"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/policy"
	"github.com/Bermos/Kitchen/internal/vex"
)

// Exploitability assertions: OpenVEX in, and the findings they modify out.
//
// A rescan that reports the same two hundred findings every day, of which four
// matter, is a control that gets switched off — or, worse, rubber-stamped. VEX
// is what makes the other one hundred and ninety-six stop asking: the vendor's
// or the team's word that the vulnerable code is not present, not reachable,
// or already mitigated here.
//
// Two properties keep that from being a way to make a scanner quiet.
//
// **Attribution.** §7.5's model for a submitted gate result applies here
// unchanged and matters more: a result somebody submitted is a claim about an
// artifact *and* a claim about who said so. The predicate is the OpenVEX
// document byte for byte — it is a standard with its own URI and its own
// tooling, so the platform adds nothing to it — and the attribution goes where
// the platform's own claims go: `submittedBy` on the Build's index and an
// audit record naming the identity, the author and every vulnerability the
// document touches. The signature means these bytes were submitted by that
// identity at that moment and have not changed since. It is not a claim that
// the assertion is true. Nothing can sign that.
//
// **Nothing is applied silently.** A suppressed finding is still a finding:
// GET /builds/{name}/vex answers the artifact's statements *joined to the
// findings they modify*, so "why is this CVE not blocking?" has an answer with
// a name and a date on it, on a screen, without reading a policy bundle.

// maxVEXDocument bounds a submission. A VEX document is a list of assertions
// rather than a scanner's output, so it does not need the 16 MiB a gate result
// takes — but an aggregated document covering a large dependency tree is not
// small either, and refusing one at the API's usual megabyte would be refusing
// the case this exists for.
const maxVEXDocument = 4 << 20

// vexSubmission is the request body. The document is carried as raw JSON and
// is what gets signed: decoding and re-encoding it would make the platform the
// author of a claim it received, and a predicate that does not reproduce byte
// for byte is a different claim from the one somebody made.
type vexSubmission struct {
	Document json.RawMessage `json:"document"`

	// Workload is which image of the unit the assertion is about, absent for
	// the project's own — the only image a single-workload project has, and
	// what every submission before #300 meant. A statement suppresses a
	// finding on *an image*: "this CVE does not apply" said about the API is
	// not a claim about the worker, which may not even carry the package.
	Workload string `json:"workload,omitempty"`
}

// vexStatementView is one statement as the read surface shows it.
//
// `Justified`, `Expired` and `Verified` are facts, not a verdict: they say
// what the platform can establish about the statement, and whether it actually
// suppresses anything here is the environment's bundle's question — the same
// division gates keep. A statement that fails one of them is shown, not
// hidden, because a suppression nobody can see is the failure mode this whole
// endpoint exists to prevent, and so is a *refused* suppression nobody can
// see.
type vexStatementView struct {
	Vulnerability string   `json:"vulnerability"`
	Status        string   `json:"status"`
	Justification string   `json:"justification,omitempty"`
	Products      []string `json:"products,omitempty"`
	// Justified is whether the justification is one of OpenVEX's five. A
	// not_affected statement without one suppresses nothing, anywhere.
	Justified bool   `json:"justified"`
	Author    string `json:"author,omitempty"`
	// SubmittedBy is the identity that handed the document to the platform,
	// which is a different fact from who the document says wrote it.
	SubmittedBy string `json:"submittedBy,omitempty"`
	DocumentID  string `json:"documentID,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
	ExpiresAt   string `json:"expiresAt,omitempty"`
	Expired     bool   `json:"expired"`
	// Verified is whether a key this platform holds accepted the envelope.
	// False is either "attached by something else" or "this platform holds no
	// key", and `verification` on the answer says which.
	Verified        bool   `json:"verified"`
	StatusNotes     string `json:"statusNotes,omitempty"`
	ImpactStatement string `json:"impactStatement,omitempty"`
	ActionStatement string `json:"actionStatement,omitempty"`
}

// effective is whether the statement is one the platform would let a policy
// act on at all: a justified not_affected, still current, whose envelope
// verified. It is deliberately not called "suppresses".
func (v vexStatementView) effective() bool {
	return v.Status == vex.StatusNotAffected && v.Justified && !v.Expired && v.Verified
}

// vexFindingView is one finding from the artifact's newest vulnerability scan,
// beside the statement covering it.
type vexFindingView struct {
	Vulnerability string `json:"vulnerability"`
	Severity      string `json:"severity,omitempty"`
	Package       string `json:"package,omitempty"`
	Version       string `json:"version,omitempty"`
	FixedIn       string `json:"fixedIn,omitempty"`
	// VEX is the statement about this finding, where there is one. It is
	// present whatever the statement says: a finding somebody marked
	// `affected` is worth showing as such.
	VEX *vexStatementView `json:"vex,omitempty"`
}

// vexBody is the whole answer.
type vexBody struct {
	Subject string `json:"subject"`
	// Verification says whether signatures were checked at all — `verified`
	// for an evidence set gathered against a key, `listed` for one gathered
	// without one. A reader that could not tell the two apart would
	// eventually treat one as the other.
	Verification string             `json:"verification"`
	Statements   []vexStatementView `json:"statements"`
	// Findings is the newest vulnerability scan's, joined to the statements.
	// Empty means nothing has scanned this artifact since it was built, which
	// is not the same as nothing being wrong.
	Findings []vexFindingView `json:"findings"`
	// Caveat is the sentence a screen prints when the answer is weaker than
	// it looks.
	Caveat string `json:"caveat,omitempty"`
}

// listVEX answers what has been asserted about this artifact's exploitability,
// and which of the scanner's findings each assertion is about.
func (s *Server) listVEX(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, req.PathValue("name"), build); err != nil {
		s.writeError(w, err)
		return
	}
	// Which image of the unit is being asked about. Absent is the project's
	// own, which is what this endpoint has always answered and the only
	// answer a single-workload project has.
	subject, refusal := requestedArtifact(build, req.URL.Query().Get("workload"), "a VEX statement could be about")
	if refusal != nil {
		writeJSON(w, refusal.status, errorBody{Error: refusal.message})
		return
	}
	artifact := subject.Artifact

	set, err := s.artifactEvidence(ctx, build, artifact)
	if err != nil {
		s.log().Error(err, "reading an artifact's evidence failed", "build", build.Name)
		writeJSON(w, http.StatusBadGateway, errorBody{
			Error: "the registry could not be asked what is attached to this artifact: " + err.Error(),
		})
		return
	}

	now := time.Now().UTC()
	body := vexBody{
		Subject:      attestation.ArtifactRef(artifact.Repository, artifact.Digest),
		Verification: "verified",
		Statements:   vexStatements(set, build, subject.Workload, now),
		Findings:     []vexFindingView{},
	}
	if !set.Verified {
		body.Verification = "listed"
		body.Caveat = "this platform holds no signing key, so the statements are listed without verification — " +
			"and a policy that requires a verified statement will honour none of them"
	}
	body.Findings = joinFindings(scanFindings(set), body.Statements)
	writeJSON(w, http.StatusOK, body)
}

// vexStatements flattens every OpenVEX document attached to the artifact.
//
// It shows what the materializer hides and hides nothing the materializer
// shows: policy.VEXFrom drops an expired statement because an evaluation must
// not act on one, and this keeps it with `expired: true` because a reader
// asking why a finding came back needs to see the assertion that ran out. The
// two are the same reading of the same documents, disagreeing only about a
// statement neither of them believes.
func vexStatements(
	set attestation.EvidenceSet, build *kitchenv1alpha1.Build, workload string, at time.Time,
) []vexStatementView {
	submitters := map[string]string{}
	for _, ingested := range build.Status.VEX {
		// The index is the unit's and the evidence set is one image's, so a
		// document filed about another image of the same unit must not lend
		// its submitter to this one — the same `@id` filed twice is two
		// assertions about two artifacts.
		if ingested.Workload != workload {
			continue
		}
		if ingested.DocumentID != "" {
			submitters[ingested.DocumentID] = ingested.SubmittedBy
		}
	}

	views := []vexStatementView{}
	for _, entry := range set.Attestations {
		if !vex.IsOpenVEX(entry.PredicateType) {
			continue
		}
		document, err := vex.Parse(entry.Statement.Predicate)
		if err != nil {
			continue
		}
		// A document with no `@id` is indexed under its envelope's digest, so
		// that is what it is looked up by here. Keying only on `@id` would
		// drop `submittedBy` for exactly the documents whose attribution is
		// hardest to recover any other way — the audit record would still have
		// it and this surface would not, which is the wrong half to lose.
		submitter := submitters[document.ID]
		if submitter == "" {
			submitter = submitters[entry.Digest]
		}
		for _, statement := range document.Statements {
			identifier := statement.Vulnerability.String()
			if identifier == "" {
				continue
			}
			expiry := vex.Expiry(document, statement)
			view := vexStatementView{
				Vulnerability:   identifier,
				Status:          statement.Status,
				Justification:   statement.Justification,
				Products:        vex.ProductIdentifiers(statement),
				Justified:       vex.Justified(statement.Justification),
				Author:          vex.AuthorOf(document, statement),
				SubmittedBy:     submitter,
				DocumentID:      document.ID,
				Timestamp:       vex.TimestampOf(document, statement),
				Verified:        entry.Verified,
				StatusNotes:     statement.StatusNotes,
				ImpactStatement: statement.ImpactStatement,
				ActionStatement: statement.ActionStatement,
			}
			if !expiry.IsZero() {
				view.ExpiresAt = expiry.Format(time.RFC3339)
				view.Expired = !at.Before(expiry)
			}
			views = append(views, view)
		}
	}
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].Vulnerability != views[j].Vulnerability {
			return views[i].Vulnerability < views[j].Vulnerability
		}
		return views[i].Author < views[j].Author
	})
	return views
}

// scanFindings reads the newest vulnerability-scan attestation's normalized
// findings. The platform wrote that list when it signed the scan (§9.6), so
// nothing is re-derived here — a second normalizer would be a second opinion
// about what the scanner said.
//
// Which scan is the newest is policy.NewestVulnerabilityScan's answer and not
// a second one: the artifact accumulates a scan per rescan interval, and this
// screen exists so a person can see the findings the policy engine judged.
// Reading all of them here would print a persistent CVE once per day it has
// been up — N duplicate rows, N duplicate Vue keys, N lines from
// `kitchen vex list` — on the one view whose whole purpose is that a
// suppression is legible.
func scanFindings(set attestation.EvidenceSet) []vexFindingView {
	findings := []vexFindingView{}
	newest, scanned := policy.NewestVulnerabilityScan(set.Attestations)
	if !scanned {
		return findings
	}
	predicate := struct {
		Findings []struct {
			Vulnerability string `json:"vulnerability"`
			Severity      string `json:"severity"`
			Package       string `json:"package"`
			Version       string `json:"version"`
			FixedIn       string `json:"fixedIn"`
		} `json:"findings"`
	}{}
	if err := json.Unmarshal(set.Attestations[newest].Statement.Predicate, &predicate); err != nil {
		return findings
	}
	for _, finding := range predicate.Findings {
		findings = append(findings, vexFindingView{
			Vulnerability: finding.Vulnerability,
			Severity:      finding.Severity,
			Package:       finding.Package,
			Version:       finding.Version,
			FixedIn:       finding.FixedIn,
		})
	}
	return findings
}

// joinFindings puts each finding beside the statement about it, preferring one
// the platform would let a policy act on — a justified, current, verified
// not_affected — over one it would not. A finding covered by nothing comes
// back with no statement at all, which is the ordinary case and the one a
// reader should be able to see at a glance.
func joinFindings(findings []vexFindingView, statements []vexStatementView) []vexFindingView {
	best := map[string]vexStatementView{}
	for _, statement := range statements {
		existing, seen := best[statement.Vulnerability]
		if !seen || (statement.effective() && !existing.effective()) {
			best[statement.Vulnerability] = statement
		}
	}
	for index := range findings {
		if statement, found := best[findings[index].Vulnerability]; found {
			covering := statement
			findings[index].VEX = &covering
		}
	}
	return findings
}

// vexAccepted is the answer to a submission.
type vexAccepted struct {
	DocumentID    string `json:"documentID,omitempty"`
	PredicateType string `json:"predicateType"`
	Manifest      string `json:"manifest"`
	Subject       string `json:"subject"`
	// Workload is which image of the unit the assertion was filed about,
	// absent for the project's own.
	Workload        string   `json:"workload,omitempty"`
	Author          string   `json:"author"`
	SubmittedBy     string   `json:"submittedBy"`
	Statements      int      `json:"statements"`
	Vulnerabilities []string `json:"vulnerabilities"`
}

// submitVEX ingests an OpenVEX document and attaches it to the artifact.
func (s *Server) submitVEX(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, req.PathValue("name"), build); err != nil {
		s.writeError(w, err)
		return
	}
	submission := vexSubmission{}
	if err := decodeVEX(req, &submission); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	subject, refusal := requestedArtifact(build, submission.Workload, "a VEX statement could be about")
	if refusal != nil {
		writeJSON(w, refusal.status, errorBody{Error: refusal.message})
		return
	}
	artifact := subject.Artifact
	if len(submission.Document) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody{
			Error: "`document` carries the OpenVEX document itself, as JSON"})
		return
	}
	document, err := vex.Parse(submission.Document)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	// Everything OpenVEX requires, plus the one thing it does not: a
	// not_affected statement has to give a justification from the
	// enumeration, because a suppression whose reason cannot be counted
	// cannot be reviewed. The message says so and says where prose belongs.
	if err := vex.Validate(document); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		s.writeError(w, err)
		return
	}
	settings := kitchen.Spec.Compliance.VEX
	if settings != nil && !settings.Enabled {
		writeJSON(w, http.StatusConflict, errorBody{
			Error: "this platform does not admit VEX statements: " +
				"compliance.vex.enabled is off, so every finding counts"})
		return
	}
	if !settings.AdmitsAuthor(document.Author) {
		writeJSON(w, http.StatusForbidden, errorBody{Error: fmt.Sprintf(
			"this platform admits VEX documents from %s, and this one is authored by %q",
			strings.Join(settings.TrustedAuthors, ", "), document.Author)})
		return
	}
	signer, err := controller.SigningKeyFor(ctx, s.Client, kitchen)
	if err != nil || signer == nil {
		// An unsigned document in the registry would look like evidence and
		// not be. Refusing is the honest answer, and it is the same one a
		// submitted gate result gets.
		writeJSON(w, http.StatusConflict, errorBody{Error: "this platform holds no signing key, " +
			"so a submitted VEX document could not be turned into evidence"})
		return
	}

	caller, _ := CallerFrom(ctx)
	submitter := callerName(caller)
	vulnerabilities := documentVulnerabilities(document)

	// Recorded before it is attached, and fail-closed. An assertion that
	// makes findings stop counting is exactly the kind of write whose record
	// an auditor asks for first, and over-recording — a record whose write
	// then failed — is the acceptable direction.
	if !s.recorded(w, req, audit.Transition{
		Object:    build,
		Kind:      audit.KindBuild,
		Operation: clickhouse.AuditCreate,
		Project:   build.Spec.ProjectRef.Name,
		Reason: fmt.Sprintf("%s submitted an OpenVEX document authored by %s, covering %s",
			submitter, document.Author, strings.Join(vulnerabilities, ", ")),
		Details: map[string]any{
			"documentID":      document.ID,
			"author":          document.Author,
			"submittedBy":     submitter,
			"artifact":        artifact.Repository + "@" + artifact.Digest,
			"statements":      len(document.Statements),
			"vulnerabilities": vulnerabilities,
			"assertions":      documentAssertions(document),
		},
	}) {
		return
	}

	// The predicate type is the document's own `@context`, not the constant.
	// OpenVEX versions itself through that URI and vex.Validate admits any of
	// them, so signing a v0.1.0 document under v0.2.0 would be the platform
	// asserting a version the author did not write — an edit to somebody
	// else's assertion, in the one field that says which vocabulary to read it
	// with. Every reader here matches by prefix (vex.IsOpenVEX), so nothing
	// downstream needs the versions to be the same.
	predicateType := strings.TrimSpace(document.Context)
	statement, err := attestation.NewStatement(
		artifact.Repository, artifact.Digest, predicateType, submission.Document)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		s.writeError(w, err)
		return
	}
	// The envelope's digest is how an `@id`-less document is indexed, so it is
	// taken before the attach rather than from it: Attach answers with the
	// attachment *manifest*, which holds every envelope attached to the
	// artifact so far and therefore moves every time anything else is
	// attached.
	envelopeDigest, err := attestation.EnvelopeDigest(envelope)
	if err != nil {
		s.writeError(w, err)
		return
	}
	writer, err := s.evidenceFor(ctx, build)
	if err != nil {
		s.writeError(w, err)
		return
	}
	subjectRef := attestation.ArtifactRef(artifact.Repository, artifact.Digest)
	manifest, err := writer.Attach(ctx, subjectRef, envelope, predicateType)
	if err != nil {
		s.log().Error(err, "attaching a VEX document failed", "build", build.Name)
		writeJSON(w, http.StatusBadGateway, errorBody{
			Error: "the document could not be attached to the artifact: " + err.Error(),
		})
		return
	}

	now := metav1.Now()
	recordIngestedVEX(build, subject, document, vulnerabilities, submitter, manifest, envelopeDigest, now)
	if err := s.Client.Status().Update(ctx, build); err != nil {
		// The evidence is attached and is the thing that matters; the Build's
		// index of it is not. Saying so beats answering an error for a write
		// that did happen.
		s.log().Error(err, "recording an ingested VEX document on the build failed", "build", build.Name)
	}

	writeJSON(w, http.StatusCreated, vexAccepted{
		DocumentID:      document.ID,
		PredicateType:   predicateType,
		Manifest:        manifest,
		Subject:         subjectRef,
		Workload:        subject.Workload,
		Author:          document.Author,
		SubmittedBy:     submitter,
		Statements:      len(document.Statements),
		Vulnerabilities: vulnerabilities,
	})
}

// documentVulnerabilities is what a document is about, deduplicated and
// ordered — the line an audit record and an answer both need.
func documentVulnerabilities(document vex.Document) []string {
	seen := map[string]struct{}{}
	identifiers := []string{}
	for _, statement := range document.Statements {
		identifier := statement.Vulnerability.String()
		if identifier == "" {
			continue
		}
		if _, already := seen[identifier]; already {
			continue
		}
		seen[identifier] = struct{}{}
		identifiers = append(identifiers, identifier)
	}
	sort.Strings(identifiers)
	return identifiers
}

// documentAssertions is what the audit record keeps of the document itself:
// per vulnerability, the status and the justification claimed for it. The
// document is in the registry and this is not a copy of it — it is the part an
// auditor reading the log alone would otherwise have to fetch a registry to
// see, which is the part that says what was waived and why.
func documentAssertions(document vex.Document) []map[string]string {
	assertions := make([]map[string]string, 0, len(document.Statements))
	for _, statement := range document.Statements {
		assertion := map[string]string{
			"vulnerability": statement.Vulnerability.String(),
			"status":        statement.Status,
		}
		if statement.Justification != "" {
			assertion["justification"] = statement.Justification
		}
		if author := vex.AuthorOf(document, statement); author != document.Author {
			assertion["author"] = author
		}
		assertions = append(assertions, assertion)
	}
	return assertions
}

// recordIngestedVEX indexes the document on the Build, replacing the row for a
// document of the same `@id` rather than growing one per submission — a
// corrected document is the same assertion restated, and two rows would make
// the register read as two waivers.
func recordIngestedVEX(
	build *kitchenv1alpha1.Build,
	subject kitchenv1alpha1.BuildArtifact,
	document vex.Document,
	vulnerabilities []string,
	submitter, manifest, envelopeDigest string,
	now metav1.Time,
) {
	ingested := kitchenv1alpha1.VEXStatus{
		DocumentID:      document.ID,
		Workload:        subject.Workload,
		Author:          document.Author,
		SubmittedBy:     submitter,
		Statements:      int32(len(document.Statements)),
		Vulnerabilities: vulnerabilities,
		Manifest:        manifest,
		IngestedAt:      &now,
	}
	if ingested.DocumentID == "" {
		// A document with no `@id` is indexed under its envelope's own digest:
		// a row nothing can be matched back to the registry is not an index of
		// anything, and this is the one name for it that a reader of the
		// evidence set also holds (attestation.Evidence.Digest). The manifest
		// digest is not — it names the whole accumulating attachment and moves
		// whenever anything else is attached, so a row keyed on it would be
		// unmatchable by the second submission and would grow a duplicate.
		ingested.DocumentID = envelopeDigest
	}
	replaced := false
	for index, existing := range build.Status.VEX {
		// Document and image together: the same document filed about two
		// images of one unit is two assertions, and one row for both would
		// make the register read as a waiver of something it never covered.
		if existing.DocumentID == ingested.DocumentID && existing.Workload == ingested.Workload {
			build.Status.VEX[index] = ingested
			replaced = true
			break
		}
	}
	if !replaced {
		build.Status.VEX = append(build.Status.VEX, ingested)
	}

	if build.Status.Artifact == nil {
		return
	}
	// One index entry per predicate type, carrying the newest manifest: the
	// attachment manifest holds every envelope attached so far, so the newest
	// digest names all of them and a row per document would be a growing list
	// of the same answer.
	//
	// The match is on IsOpenVEX rather than on one constant, because the
	// predicate type is the submitted document's own `@context` and an
	// installation can be sent both v0.1.0 and v0.2.0 documents. They are one
	// kind of evidence in one index row, carrying whichever version arrived
	// last — which is what the row is for.
	for index, evidence := range build.Status.Artifact.Evidence {
		if vex.IsOpenVEX(evidence.PredicateType) {
			build.Status.Artifact.Evidence[index].PredicateType = predicateTypeOf(document)
			build.Status.Artifact.Evidence[index].Manifest = manifest
			return
		}
	}
	build.Status.Artifact.Evidence = append(build.Status.Artifact.Evidence, kitchenv1alpha1.ArtifactEvidence{
		PredicateType: predicateTypeOf(document),
		Manifest:      manifest,
		// The platform signed it; somebody else made the claim — the same
		// distinction a submitted gate result draws, and the reason neither
		// says simply "attested". `builder` and `platform` are the two words
		// this field has, and a document nobody built is the platform's.
		Source: "platform",
	})
}

// predicateTypeOf is the predicate type a document is attested under: its own
// `@context`, which is how OpenVEX versions itself. attestation.PredicateOpenVEX
// is the current one and is the fallback for a document whose context somehow
// reached here empty — vex.Validate refuses that, so this is a belt.
func predicateTypeOf(document vex.Document) string {
	if context := strings.TrimSpace(document.Context); context != "" {
		return context
	}
	return attestation.PredicateOpenVEX
}

// decodeVEX reads a submission under this endpoint's own limit, refusing
// fields it does not know — the document inside is read leniently, because
// OpenVEX is JSON-LD and may carry terms nobody here has heard of, but the
// envelope around it is Kitchen's and a typo in it is an error rather than a
// silently ignored instruction.
func decodeVEX(req *http.Request, into *vexSubmission) error {
	decoder := json.NewDecoder(io.LimitReader(req.Body, maxVEXDocument))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("a JSON body is required")
		}
		return fmt.Errorf("unreadable JSON body: %w", err)
	}
	return nil
}
