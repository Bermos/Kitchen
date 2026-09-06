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

package policy

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Content addressing is a claim about bytes; these are the checks that make
// it a fact on read. Everything here is about the pairing holding or not —
// what a caller does with the finding is the caller's.

func TestVerifyBundleHoldsTheContentToTheDigestThatNamesIt(t *testing.T) {
	bundle := Bundle{"promotion.rego": "package kitchen.promotion"}
	digest := Digest(bundle)

	if err := VerifyBundle(digest, bundle); err != nil {
		t.Fatalf("a bundle must verify against its own digest: %v", err)
	}

	substituted := Bundle{"promotion.rego": "package kitchen.promotion\n\ndeny := []\n"}
	err := VerifyBundle(digest, substituted)
	if err == nil {
		t.Fatal("other content under the same digest must be refused")
	}
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("the refusal must be a digest mismatch, got %v", err)
	}
	// Both digests, because "it does not match" without saying what it is
	// leaves nobody able to tell a substitution from a bug here.
	if !strings.Contains(err.Error(), digest) || !strings.Contains(err.Error(), Digest(substituted)) {
		t.Fatalf("the refusal names the digest asked for and the digest found, got %v", err)
	}
}

func TestVerifyBundleContentReadsTheStoredEncoding(t *testing.T) {
	bundle := Bundle{"promotion.rego": "package kitchen.promotion"}
	content, err := json.Marshal(map[string]string(bundle))
	if err != nil {
		t.Fatal(err)
	}
	digest := Digest(bundle)

	if err := VerifyBundleContent(digest, string(content)); err != nil {
		t.Fatalf("a bundle's stored encoding must verify against its digest: %v", err)
	}
	if err := VerifyBundleContent("sha256:"+strings.Repeat("b", 64), string(content)); err == nil {
		t.Fatal("a stored encoding filed under another digest must be refused")
	}
	// Content that is not a bundle at all fails as unreadable rather than as
	// a mismatch: there is nothing to have hashed.
	err = VerifyBundleContent(digest, "not json")
	if err == nil || errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("unreadable content is its own failure, got %v", err)
	}
}

func TestVerifyInputHoldsTheInputToTheDigestThatNamesIt(t *testing.T) {
	input := Input{
		Kind:        KindPromotion,
		At:          time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC),
		Parameters:  map[string]string{"require-sbom": "true"},
		Project:     ProjectFacts{Name: "shop"},
		Environment: EnvironmentFacts{Name: "shop-production", Type: "production"},
		Release:     ReleaseFacts{Name: "shop-rel-1"},
	}
	digest, err := input.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyInput(digest, input); err != nil {
		t.Fatalf("an input must verify against its own digest: %v", err)
	}

	// The input the engine would be handed, not the bytes it was read from:
	// a parameter turned off is a different input and hashes to say so.
	tampered := input
	tampered.Parameters = map[string]string{"require-sbom": "false"}
	err = VerifyInput(digest, tampered)
	if err == nil || !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("a substituted input must be refused as a mismatch, got %v", err)
	}
	if !strings.Contains(err.Error(), digest) {
		t.Fatalf("the refusal names the digest asked for, got %v", err)
	}
}
