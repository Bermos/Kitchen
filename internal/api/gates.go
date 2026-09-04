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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/controller"
)

// Gate results that were produced somewhere else.
//
// A great many organisations already run their scanners in the application's
// own CI, on the pull request, minutes before Kitchen ever sees the commit.
// Making them run again on the platform would be slower, would spend the
// compute twice, and would produce a second answer to the same question — so
// results can be submitted instead of re-derived.
//
// What changes is not the predicate but the **attribution**. A result Kitchen
// produced is a claim about an artifact; a result somebody submitted is a claim
// about an artifact *and* a claim about who said so, and merging the two into
// one word would let an application team assert its own clean scan and have it
// read exactly like the platform's. So the statement records `reportedBy` — the
// authenticated identity that submitted it — and the Build records the source
// as `external`. A policy that trusts only what the platform ran can say so.
//
// The platform's signature still goes on it, and means what it always means:
// that these bytes were submitted by that identity at that moment and have not
// changed since. It is not a claim that the findings are true. Nothing can sign
// that.

// Where a gate result came from, in the two words the Build records. They are
// the operator's constants; spelled here so a reader of this file does not have
// to go looking for what "external" is checked against.
const (
	gateSourcePlatform = "platform"
	gateSourceExternal = "external"
)

// gateSubmission is the request body: what ran, and what it found.
type gateSubmission struct {
	// Gate names the gate, and shares a namespace with the platform's own
	// configured gates on purpose — a policy asking whether "trivy" ran should
	// not have to ask twice.
	Gate string `json:"gate"`

	// Workload is which image of the unit the gate ran over, absent for the
	// project's own — which is the only image a single-workload project has
	// and what every submission before #300 meant. A result submitted about
	// one image of a unit says nothing about the others, so it is recorded
	// against the one it names and nothing else.
	Workload string `json:"workload,omitempty"`

	// Version is the gate's, which is what makes a finding reproducible. A
	// scanner whose vulnerability database moves hourly is a different gate
	// every hour under the same version string, and recording what was
	// claimed is still better than recording nothing.
	Version string `json:"version,omitempty"`

	// Format names the shape of the findings, for a reader that has to parse
	// them. Nothing here validates it.
	Format string `json:"format,omitempty"`

	// FinishedAt is when the gate ran, wherever it ran. It is the submitter's
	// claim, not the platform's observation, and it is recorded as such —
	// the moment of submission is recorded separately and is the platform's
	// own.
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	// Findings is the gate's raw output, carried unmodified. It is raw JSON
	// so that a report keeps the exact shape its tool produced: re-encoding
	// somebody's evidence into a shape of the platform's choosing is the
	// platform editing evidence.
	Findings json.RawMessage `json:"findings"`
}

// maxSubmittedFindings bounds a submission.
//
// It is the one endpoint whose body is not a handful of fields, which is why
// it does not take the API's usual megabyte: a container scan of an ordinary
// Node application runs to several. The number matches what the operator will
// read back for a gate it ran itself, so that where a result came from does
// not change how large it is allowed to be.
const maxSubmittedFindings = 16 << 20

// submitGate ingests a gate result produced outside the platform.
func (s *Server) submitGate(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, req.PathValue("name"), build); err != nil {
		s.writeError(w, err)
		return
	}
	submission := gateSubmission{}
	if err := decodeFindings(req, &submission); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	// Which image of the unit the result is about. It is read out of the body
	// rather than the path because it is part of what is being asserted: the
	// same gate, the same commit and a different image is a different claim.
	subjectArtifact, refusal := requestedArtifact(build, submission.Workload, "a gate result could be about")
	if refusal != nil {
		writeJSON(w, refusal.status, errorBody{Error: refusal.message})
		return
	}
	artifact := subjectArtifact.Artifact
	if submission.Gate == "" {
		writeJSON(w, http.StatusBadRequest, errorBody{
			Error: "a gate result has to say which gate produced it"})
		return
	}
	if len(submission.Findings) == 0 {
		writeJSON(w, http.StatusBadRequest, errorBody{
			Error: "a gate result has to carry findings — a gate that reports nothing has not run"})
		return
	}

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		s.writeError(w, err)
		return
	}
	signer, err := controller.SigningKeyFor(ctx, s.Client, kitchen)
	if err != nil || signer == nil {
		// Storing an unsigned result would leave something in the registry
		// that looks like evidence and is not. Refusing is the honest answer.
		writeJSON(w, http.StatusConflict, errorBody{Error: "this platform holds no signing key, " +
			"so a submitted gate result could not be turned into evidence"})
		return
	}

	caller, _ := CallerFrom(ctx)
	reporter := callerName(caller)
	statement, err := attestation.NewStatement(
		artifact.Repository, artifact.Digest, attestation.PredicateQualityGate,
		submittedGateRecord(submission, reporter))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{Error: err.Error()})
		return
	}
	envelope, err := attestation.Sign(ctx, statement, signer)
	if err != nil {
		s.writeError(w, err)
		return
	}

	writer, err := s.evidenceFor(ctx, build)
	if err != nil {
		s.writeError(w, err)
		return
	}
	subject := attestation.ArtifactRef(artifact.Repository, artifact.Digest)
	manifest, err := writer.Attach(ctx, subject, envelope, attestation.PredicateQualityGate)
	if err != nil {
		s.log().Error(err, "attaching a submitted gate result failed", "build", build.Name)
		writeJSON(w, http.StatusBadGateway, errorBody{
			Error: "the result could not be attached to the artifact: " + err.Error(),
		})
		return
	}

	now := metav1.Now()
	recordSubmittedGate(build, subjectArtifact, submission, reporter, manifest, now)
	if err := s.Client.Status().Update(ctx, build); err != nil {
		// The evidence is attached and is the thing that matters; the Build's
		// summary of it is not. Saying so beats answering an error for a
		// write that did happen.
		s.log().Error(err, "recording a submitted gate result on the build failed", "build", build.Name)
	}

	accepted := map[string]any{
		"gate":          submission.Gate,
		"predicateType": attestation.PredicateQualityGate,
		"manifest":      manifest,
		"reportedBy":    reporter,
		"subject":       subject,
	}
	if subjectArtifact.Workload != "" {
		accepted["workload"] = subjectArtifact.Workload
	}
	writeJSON(w, http.StatusCreated, accepted)
}

// submittedGateRecord is the predicate. It is the same shape a gate the
// platform ran produces, plus who reported it and when the platform received
// it — and, like that one, it carries no verdict.
func submittedGateRecord(submission gateSubmission, reporter string) map[string]any {
	record := map[string]any{
		"gate":     submission.Gate,
		"findings": submission.Findings,
		// Who made the claim. The platform signed it; it did not witness it.
		"reportedBy": reporter,
		"reportedAt": time.Now().UTC().Format(time.RFC3339),
		"external":   true,
	}
	if submission.Version != "" {
		record["version"] = submission.Version
	}
	if submission.Format != "" {
		record["format"] = submission.Format
	}
	if submission.FinishedAt != nil {
		record["finishedAt"] = submission.FinishedAt.UTC().Format(time.RFC3339)
	}
	return record
}

// recordSubmittedGate puts the result on the Build, beside the gates the
// platform ran itself.
func recordSubmittedGate(
	build *kitchenv1alpha1.Build,
	subject kitchenv1alpha1.BuildArtifact,
	submission gateSubmission,
	reporter, manifest string,
	now metav1.Time,
) {
	status := kitchenv1alpha1.QualityGateStatus{
		Name:          submission.Gate,
		Phase:         kitchenv1alpha1.GateCompleted,
		Source:        gateSourceExternal,
		ReportedBy:    reporter,
		PredicateType: attestation.PredicateQualityGate,
		Attested:      &now,
		FinishedAt:    &now,
	}
	if submission.FinishedAt != nil {
		finished := metav1.NewTime(*submission.FinishedAt)
		status.FinishedAt = &finished
	}
	subject.Artifact.Evidence = append(subject.Artifact.Evidence, kitchenv1alpha1.ArtifactEvidence{
		PredicateType: attestation.PredicateQualityGate,
		Manifest:      manifest,
		// The platform signed it; somebody else made the claim. This is
		// the same distinction `source` draws on the gate itself, and it
		// is why neither says simply "attested".
		Source: "platform",
	})
	// The row goes beside the platform's own runs over the *same* image: the
	// Build's own list for the web process, the workload's row otherwise. A
	// result about the API submitted into the worker's row would be the
	// self-marked homework this endpoint's attribution exists to prevent.
	rows := build.GatesFor(subject.Workload)
	for index, existing := range *rows {
		if existing.Name != status.Name {
			continue
		}
		// A gate the platform ran is not replaced by a submission claiming
		// the same name. Both attestations are attached to the artifact and
		// both are readable; what the Build shows is the one the platform can
		// vouch for having observed.
		if existing.Source == gateSourcePlatform && existing.Phase == kitchenv1alpha1.GateCompleted {
			return
		}
		(*rows)[index] = status
		return
	}
	*rows = append(*rows, status)
}

// decodeFindings reads a gate submission, under this endpoint's own limit
// rather than the API's usual one, and refusing fields it does not know for
// the same reason every other body does: a typo in a field name should be an
// error, not a silently ignored instruction.
func decodeFindings(req *http.Request, into *gateSubmission) error {
	decoder := json.NewDecoder(io.LimitReader(req.Body, maxSubmittedFindings))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("a JSON body is required")
		}
		return fmt.Errorf("unreadable JSON body: %w", err)
	}
	return nil
}
