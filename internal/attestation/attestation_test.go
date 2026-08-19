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
	"strings"
	"testing"
)

func TestPAEMatchesTheSpecificationsExample(t *testing.T) {
	// DSSE's own worked example. It is here as a literal because the whole
	// point of the encoding is that everyone computes the same bytes: a
	// signature nothing else can check is not evidence.
	got := string(PAE("http://example.com/HelloWorld", []byte("hello world")))
	want := "DSSEv1 29 http://example.com/HelloWorld 11 hello world"
	if got != want {
		t.Errorf("PAE = %q, want %q", got, want)
	}
}

// The lengths are what stop one field from reading as another. Without them
// these two would encode identically.
func TestPAEDistinguishesPayloadsThatShareTheirBytes(t *testing.T) {
	left := PAE("a b", []byte("c"))
	right := PAE("a", []byte("b c"))
	if string(left) == string(right) {
		t.Error("two different (type, payload) pairs encode the same")
	}
}

func statement(t *testing.T) Statement {
	t.Helper()
	made, err := NewStatement(
		"registry.example.com/shop",
		"sha256:aaaa000000000000000000000000000000000000000000000000000000000000",
		PredicateBuildRecord,
		map[string]any{"project": "shop", "build": "shop-bld-1"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return made
}

func TestSignAndVerifyRoundTrip(t *testing.T) {
	key, _, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}

	envelope, err := Sign(context.Background(), statement(t), key)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.PayloadType != PayloadType {
		t.Errorf("envelope declares %q, want %q", envelope.PayloadType, PayloadType)
	}
	if len(envelope.Signatures) != 1 || envelope.Signatures[0].KeyID != key.KeyID() {
		t.Fatalf("envelope carries %d signatures, want one from %s", len(envelope.Signatures), key.KeyID())
	}

	verified, err := envelope.Verify(key)
	if err != nil {
		t.Fatal(err)
	}
	if verified.PredicateType != PredicateBuildRecord {
		t.Errorf("verified statement asserts %q, want %q", verified.PredicateType, PredicateBuildRecord)
	}
	if !verified.Describes("sha256:aaaa000000000000000000000000000000000000000000000000000000000000") {
		t.Error("the verified statement does not describe the artifact it was made about")
	}
}

func TestVerifyRejectsAnotherKeysSignature(t *testing.T) {
	signer, _, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	stranger, _, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}

	envelope, err := Sign(context.Background(), statement(t), signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := envelope.Verify(stranger); err == nil {
		t.Error("an envelope verified against a key that did not sign it")
	}
}

func TestVerifyRejectsAnEditedPayload(t *testing.T) {
	key, _, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Sign(context.Background(), statement(t), key)
	if err != nil {
		t.Fatal(err)
	}

	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		t.Fatal(err)
	}
	edited := Statement{}
	if err := json.Unmarshal(payload, &edited); err != nil {
		t.Fatal(err)
	}
	edited.Subject[0].Digest["sha256"] = strings.Repeat("b", 64)
	reencoded, err := json.Marshal(edited)
	if err != nil {
		t.Fatal(err)
	}
	envelope.Payload = base64.StdEncoding.EncodeToString(reencoded)

	if _, err := envelope.Verify(key); err == nil {
		t.Error("an envelope whose subject was swapped still verified")
	}
}

// The payload type is inside the signed bytes, which is what stops a signature
// over one kind of evidence being presented as another.
func TestVerifyRejectsARelabelledEnvelope(t *testing.T) {
	key, _, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Sign(context.Background(), statement(t), key)
	if err != nil {
		t.Fatal(err)
	}
	envelope.PayloadType = "application/vnd.something-else+json"

	if _, err := envelope.Verify(key); err == nil {
		t.Error("an envelope whose payload type was changed still verified")
	}
}

func TestVerifyAcceptsOneOfSeveralSignatures(t *testing.T) {
	build, _, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	approval, _, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}

	envelope, err := Sign(context.Background(), statement(t), build, approval)
	if err != nil {
		t.Fatal(err)
	}
	if len(envelope.Signatures) != 2 {
		t.Fatalf("two signers produced %d signatures", len(envelope.Signatures))
	}
	// A verifier holding only one of the keys still accepts the envelope, and
	// an installation that wants both asks twice with one key each.
	if _, err := envelope.Verify(approval); err != nil {
		t.Errorf("an envelope both keys signed was refused by one of them: %v", err)
	}
}

func TestStatementRequiresADigestSubject(t *testing.T) {
	if _, err := NewStatement("registry.example.com/shop", "latest", PredicateBuildRecord, nil); err == nil {
		t.Error("a statement about a tag was accepted; attestations key on content")
	}
}

func TestLoadKeyRejectsAMismatchedPair(t *testing.T) {
	_, privatePEM, _, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	_, _, strangersPublicPEM, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := LoadECDSAKey(privatePEM, strangersPublicPEM); err == nil {
		t.Error("a secret whose two halves belong to different keys was loaded")
	}
}

func TestLoadKeyRoundTripsThroughPEM(t *testing.T) {
	original, privatePEM, publicPEM, err := GenerateECDSAKey()
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadECDSAKey(privatePEM, publicPEM)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.KeyID() != original.KeyID() {
		t.Errorf("reloaded key has id %s, want %s", loaded.KeyID(), original.KeyID())
	}

	// A verifier that only ever had the public half — everybody outside the
	// platform — must accept what the private half signed.
	public, err := ParsePublicKey(publicPEM)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := Sign(context.Background(), statement(t), loaded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := envelope.Verify(public); err != nil {
		t.Errorf("the published public key could not verify what the platform signed: %v", err)
	}
}

func TestAuthFromDockerConfigReadsBothCredentialForms(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte("robot:hunter2"))
	config := `{"auths": {
	  "explicit.example.com": {"username": "robot", "password": "hunter2"},
	  "packed.example.com": {"auth": "` + encoded + `"}
	}}`

	for _, server := range []string{"explicit.example.com", "packed.example.com"} {
		auth, err := AuthFromDockerConfig([]byte(config), server)
		if err != nil {
			t.Fatalf("%s: %v", server, err)
		}
		resolved, err := auth.Authorization()
		if err != nil {
			t.Fatalf("%s: %v", server, err)
		}
		if resolved.Username != "robot" && resolved.Auth != encoded {
			t.Errorf("%s resolved to %+v, want the robot credential", server, resolved)
		}
	}
}

// A registry the config says nothing about is an anonymous pull, not an error.
func TestAuthFromDockerConfigIsAnonymousForAnUnknownRegistry(t *testing.T) {
	auth, err := AuthFromDockerConfig([]byte(`{"auths":{}}`), "elsewhere.example.com")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := auth.Authorization()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Username != "" || resolved.Auth != "" {
		t.Errorf("an unknown registry resolved to %+v, want anonymous", resolved)
	}
}

func TestKitchenPredicateRecognisesItsOwnVocabulary(t *testing.T) {
	if !KitchenPredicate(PredicateBuildRecord) {
		t.Error("Kitchen's own predicate type was not recognised as its own")
	}
	if KitchenPredicate("https://slsa.dev/provenance/v1") {
		t.Error("a standard predicate type was claimed as Kitchen's")
	}
}
