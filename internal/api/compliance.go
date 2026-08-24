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
	"context"
	"net/http"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
	"github.com/Bermos/Kitchen/internal/attestation"
	"github.com/Bermos/Kitchen/internal/controller"
	"github.com/Bermos/Kitchen/internal/provider"
	"github.com/Bermos/Kitchen/internal/retention"
)

// The compliance read surface: what the platform is producing, and what has
// been attached to one artifact.
//
// The evidence endpoint is a convenience and says so. Everything it returns is
// in the registry, keyed to the artifact's digest, and can be read with cosign
// or anything else that speaks OCI referrers without Kitchen being involved —
// which is the point of storing it there. What this adds is the verification
// against the platform's own key and a shape the dashboard can render.

// EvidenceReader reads what is attached to an artifact's digest, and attaches
// what somebody submits.
//
// The write half is here rather than in a second interface because it is the
// same registry, reached with the same credential, for the same artifact — and
// because a caller holding one and not the other would be a caller that can
// show evidence it cannot add to.
type EvidenceReader interface {
	Evidence(ctx context.Context, imageRef string, verifiers ...attestation.Verifier) (attestation.EvidenceSet, error)
	Attach(ctx context.Context, imageRef string, envelope attestation.Envelope, predicateType string) (string, error)
}

// EvidenceFactory builds the reader for one registry out of the docker config
// that registry is reached with.
type EvidenceFactory func(dockerConfig []byte, server string) (EvidenceReader, error)

func defaultEvidenceReader(dockerConfig []byte, server string) (EvidenceReader, error) {
	auth, err := attestation.AuthFromDockerConfig(dockerConfig, server)
	if err != nil {
		return nil, err
	}
	return &attestation.Store{Auth: auth}, nil
}

// complianceBody is what the platform says about its own evidence production.
type complianceBody struct {
	Audit struct {
		Enabled       bool  `json:"enabled"`
		Recording     bool  `json:"recording"`
		RetentionDays int32 `json:"retentionDays"`
		Sequence      int64 `json:"sequence"`
		// Immutable is whether the store has taken the audit table's
		// mutation privileges away from the platform's own credential, so
		// that a compromised operator or API can append to the log and
		// cannot rewrite it. False is a smaller claim, not a fault, and
		// ImmutabilityMessage says why.
		Immutable           bool   `json:"immutable"`
		ImmutabilityMessage string `json:"immutabilityMessage,omitempty"`
		Message             string `json:"message,omitempty"`
	} `json:"audit"`
	Attestation struct {
		Enabled bool   `json:"enabled"`
		Signing bool   `json:"signing"`
		KeyID   string `json:"keyID,omitempty"`
		// PublicKey is the PEM of the verification key.
		//
		// Handing it out is not the API reading a credential back: a public
		// key is public by construction, and evidence signed under a key
		// nobody can obtain is evidence nobody can check. It is here so that
		// an auditor can take it away and run `cosign verify-attestation
		// --key` against the registry, with Kitchen out of the loop.
		PublicKey string `json:"publicKey,omitempty"`
		Message   string `json:"message,omitempty"`
	} `json:"attestation"`
	// Policy is whether decisions are being stored. The engine always
	// evaluates — what needs the store is keeping a replayable record, and an
	// installation without one is told so here rather than discovering it at
	// its first audit.
	Policy struct {
		Storing bool   `json:"storing"`
		Message string `json:"message,omitempty"`
	} `json:"policy"`
}

// getCompliance reports the audit log and the signing identity.
func (s *Server) getCompliance(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err != nil {
		s.writeError(w, err)
		return
	}

	body := complianceBody{}
	body.Audit.Enabled = kitchen.Spec.Compliance.Audit.Enabled
	// The audit class of the retention model rather than the spec field: the
	// two are the same until somebody sets spec.retention.audit, and after
	// that this has to answer with the one being enforced.
	body.Audit.RetentionDays = retention.Resolve(kitchen).Days(retention.ClassAudit)
	body.Attestation.Enabled = kitchen.Spec.Compliance.Attestation.Enabled

	if status := kitchen.Status.Compliance; status != nil {
		if status.Audit != nil {
			body.Audit.Recording = status.Audit.Recording
			body.Audit.Sequence = status.Audit.Sequence
			body.Audit.Message = status.Audit.Message
			body.Audit.Immutable = status.Audit.Immutable
			body.Audit.ImmutabilityMessage = status.Audit.ImmutabilityMessage
		}
		if status.Attestation != nil {
			body.Attestation.Signing = status.Attestation.Signing
			body.Attestation.KeyID = status.Attestation.KeyID
			body.Attestation.Message = status.Attestation.Message
		}
		if status.Policy != nil {
			body.Policy.Storing = status.Policy.Storing
			body.Policy.Message = status.Policy.Message
		}
	}

	// Read fresh rather than from status: the key is the one thing here a
	// caller might act on, and a stale copy of it would send someone off to
	// verify against a key that has been rotated away.
	if key, err := controller.SigningKeyFor(ctx, s.Client, kitchen); err == nil && key != nil {
		if pem, err := key.PublicPEM(); err == nil {
			body.Attestation.PublicKey = string(pem)
			body.Attestation.KeyID = key.KeyID()
			body.Attestation.Signing = true
		}
	}

	writeJSON(w, http.StatusOK, body)
}

// buildAttestations serves the evidence attached to a build's artifact.
func (s *Server) buildAttestations(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()

	build := &kitchenv1alpha1.Build{}
	if err := s.get(ctx, req.PathValue("name"), build); err != nil {
		s.writeError(w, err)
		return
	}
	artifact := build.Status.Artifact
	if artifact == nil || artifact.Digest == "" {
		// A build with no artifact digest is not an error and not an empty
		// evidence set either — it is a build nothing can be said about, and
		// saying which of the two it is matters.
		writeJSON(w, http.StatusConflict, errorBody{Error: "this build produced no artifact digest, " +
			"so there is nothing evidence could be attached to"})
		return
	}

	reader, err := s.evidenceFor(ctx, build)
	if err != nil {
		s.writeError(w, err)
		return
	}

	verifiers := []attestation.Verifier{}
	kitchen := &kitchenv1alpha1.Kitchen{}
	if err := s.Client.Get(ctx, types.NamespacedName{Name: controller.KitchenSingletonName}, kitchen); err == nil {
		if key, err := controller.SigningKeyFor(ctx, s.Client, kitchen); err == nil && key != nil {
			verifiers = append(verifiers, key)
		}
	}

	set, err := reader.Evidence(ctx, artifact.Repository+"@"+artifact.Digest, verifiers...)
	if err != nil {
		s.log().Error(err, "reading an artifact's evidence failed", "build", build.Name)
		writeJSON(w, http.StatusBadGateway, errorBody{
			Error: "the registry could not be asked what is attached to this artifact: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, set)
}

// evidenceFor resolves how to reach the registry a build pushed to, through
// the project's own registry Connection — the same credential the build used.
func (s *Server) evidenceFor(ctx context.Context, build *kitchenv1alpha1.Build) (EvidenceReader, error) {
	project := &kitchenv1alpha1.Project{}
	if err := s.get(ctx, build.Spec.ProjectRef.Name, project); err != nil {
		return nil, err
	}
	connection := &kitchenv1alpha1.Connection{}
	if err := s.get(ctx, project.Spec.Registry.ConnectionRef.Name, connection); err != nil {
		return nil, err
	}
	target, err := provider.Registry(connection)
	if err != nil {
		return nil, apierrors.NewBadRequest(err.Error())
	}
	secret := &corev1.Secret{}
	key := types.NamespacedName{Namespace: s.Namespace, Name: connection.Spec.CredentialsSecretRef.Name}
	if err := s.Client.Get(ctx, key, secret); err != nil {
		return nil, err
	}

	factory := s.EvidenceReaders
	if factory == nil {
		factory = defaultEvidenceReader
	}
	return factory(secret.Data[corev1.DockerConfigJsonKey], target.Server)
}
