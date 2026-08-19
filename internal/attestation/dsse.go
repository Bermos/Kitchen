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

package attestation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// PayloadType is what a DSSE envelope carrying an in-toto Statement declares.
//
// It is not decoration. The payload type is inside the pre-authentication
// encoding, so it is signed along with the payload — which is what stops a
// signature over an SBOM from being lifted onto a provenance statement. Any
// envelope format that puts the type outside the signed bytes loses that, and
// is why this is DSSE rather than something smaller written here.
const PayloadType = "application/vnd.in-toto+json"

// Signature is one signer's assertion over an envelope's payload.
type Signature struct {
	// KeyID identifies the key, so a verifier holding several knows which to
	// try. It is advisory: a verifier still has to check the signature.
	KeyID string `json:"keyid,omitempty"`
	// Sig is the raw signature, base64 (standard encoding, padded), as DSSE
	// specifies.
	Sig string `json:"sig"`
}

// Envelope is a DSSE envelope: a payload, what sort of payload it is, and the
// signatures over both.
//
// Multiple signatures over one payload are native to the format and Kitchen
// uses that rather than routing around it — the build system and an approval
// service each assert their own part of the same statement, and a verifier
// decides how many it needs.
type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

// PAE is DSSE's pre-authentication encoding: the bytes that are actually
// signed.
//
//	"DSSEv1" SP len(type) SP type SP len(payload) SP payload
//
// The lengths are what make it unambiguous — without them a payload could be
// crafted whose bytes read as a different (type, payload) pair, and two
// different statements would have one signature between them.
func PAE(payloadType string, payload []byte) []byte {
	encoded := make([]byte, 0, len(payload)+len(payloadType)+32)
	encoded = append(encoded, "DSSEv1 "...)
	encoded = append(encoded, strconv.Itoa(len(payloadType))...)
	encoded = append(encoded, ' ')
	encoded = append(encoded, payloadType...)
	encoded = append(encoded, ' ')
	encoded = append(encoded, strconv.Itoa(len(payload))...)
	encoded = append(encoded, ' ')
	return append(encoded, payload...)
}

// Sign wraps a statement in an envelope signed by every signer given.
//
// The payload is marshalled once and every signer signs those same bytes, so
// the envelope has one payload with several signatures rather than several
// envelopes that happen to say the same thing.
func Sign(ctx context.Context, statement Statement, signers ...Signer) (Envelope, error) {
	if len(signers) == 0 {
		return Envelope{}, errors.New("an attestation needs at least one signer")
	}
	payload, err := json.Marshal(statement)
	if err != nil {
		return Envelope{}, fmt.Errorf("the attestation statement could not be encoded: %w", err)
	}

	envelope := Envelope{
		PayloadType: PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures:  make([]Signature, 0, len(signers)),
	}
	message := PAE(PayloadType, payload)
	for _, signer := range signers {
		signature, err := signer.Sign(ctx, message)
		if err != nil {
			return Envelope{}, fmt.Errorf("signing the attestation with key %s failed: %w", signer.KeyID(), err)
		}
		envelope.Signatures = append(envelope.Signatures, Signature{
			KeyID: signer.KeyID(),
			Sig:   base64.StdEncoding.EncodeToString(signature),
		})
	}
	return envelope, nil
}

// ErrNoAcceptedSignature is what an envelope no known key signed comes back
// as. It is a distinct error because "the evidence is not signed by anyone we
// trust" and "the evidence is malformed" are different findings.
var ErrNoAcceptedSignature = errors.New("no signature on this attestation was made by an accepted key")

// Statement decodes the envelope's payload.
//
// It does not verify anything. Reading an unverified statement is a legitimate
// thing to want — listing what evidence exists before deciding what to trust —
// but it is a different act from believing it, and the two have different
// method names here so a caller cannot do one while meaning the other.
func (e Envelope) Statement() (Statement, error) {
	if e.PayloadType != PayloadType {
		return Statement{}, fmt.Errorf(
			"this envelope carries %q, not an in-toto statement (%q)", e.PayloadType, PayloadType)
	}
	payload, err := base64.StdEncoding.DecodeString(e.Payload)
	if err != nil {
		return Statement{}, fmt.Errorf("the attestation payload is not valid base64: %w", err)
	}
	statement := Statement{}
	if err := json.Unmarshal(payload, &statement); err != nil {
		return Statement{}, fmt.Errorf("the attestation payload is not an in-toto statement: %w", err)
	}
	if statement.Type != StatementType {
		return Statement{}, fmt.Errorf(
			"the attestation payload declares _type %q, not %q", statement.Type, StatementType)
	}
	return statement, nil
}

// Verify checks the envelope against the verifiers given and returns the
// statement it carries, once at least one signature is accepted.
//
// One accepted signature is the threshold because DSSE's own model is that a
// verifier decides its policy: an installation that requires two independent
// signers expresses that by asking twice, over the same envelope, with one
// verifier each. Building the threshold in here would make that impossible to
// express.
func (e Envelope) Verify(verifiers ...Verifier) (Statement, error) {
	if len(verifiers) == 0 {
		return Statement{}, errors.New("verifying an attestation needs at least one key")
	}
	payload, err := base64.StdEncoding.DecodeString(e.Payload)
	if err != nil {
		return Statement{}, fmt.Errorf("the attestation payload is not valid base64: %w", err)
	}
	message := PAE(e.PayloadType, payload)

	for _, signature := range e.Signatures {
		raw, err := base64.StdEncoding.DecodeString(signature.Sig)
		if err != nil {
			continue
		}
		for _, verifier := range verifiers {
			// The key id is a hint, not a filter: an envelope signed by a
			// key that recorded no id must still verify against that key.
			if signature.KeyID != "" && verifier.KeyID() != "" && signature.KeyID != verifier.KeyID() {
				continue
			}
			if err := verifier.Verify(message, raw); err == nil {
				return e.Statement()
			}
		}
	}
	return Statement{}, ErrNoAcceptedSignature
}
