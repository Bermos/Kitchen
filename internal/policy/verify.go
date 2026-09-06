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
	"fmt"
)

// Content addressing is a claim about bytes, and it is only worth something
// while somebody re-derives the address on read. A bundle and an input are
// each named by a digest and then stored beside it — in ClickHouse tables
// that are append-only by design but ordinary tables all the same — so the
// pairing has to be checked wherever one of them is loaded by the digest that
// names it. These three helpers are that check, in one place, so that a load
// site can neither skip it nor word it its own way.

// ErrDigestMismatch reports content that does not hash to the digest it was
// filed under. It is a sentinel because the finding is categorically unlike
// "it is not there": content that is present and does not match is a store
// that has been written to, and a caller distinguishes the two.
var ErrDigestMismatch = errors.New("digest mismatch")

// VerifyBundle checks that a bundle is the bundle a digest names. It is what
// makes a bundle read back out of the decision store evidence rather than a
// row somebody could have replaced.
func VerifyBundle(digest string, bundle Bundle) error {
	if derived := Digest(bundle); derived != digest {
		return fmt.Errorf("%w: the policy bundle held as %s content-addresses to %s",
			ErrDigestMismatch, digest, derived)
	}
	return nil
}

// VerifyBundleContent is VerifyBundle over a bundle's stored JSON encoding —
// the `content` column beside the digest — for the write path, which holds
// the bytes and never unmarshals them.
func VerifyBundleContent(digest, content string) error {
	bundle := Bundle{}
	if err := json.Unmarshal([]byte(content), &bundle); err != nil {
		return fmt.Errorf("the policy bundle named %s is not a readable bundle: %w", digest, err)
	}
	return VerifyBundle(digest, bundle)
}

// VerifyInput checks that an input is the input a digest names.
//
// It re-derives the digest from the *parsed* input — the value the engine is
// about to be handed — rather than from the stored bytes, so that what is
// verified is what is evaluated. That makes Input's JSON encoding part of its
// contract: a field added without `omitempty` would re-derive to a different
// digest for every decision already stored, and every one of them would stop
// replaying.
func VerifyInput(digest string, in Input) error {
	derived, err := in.Digest()
	if err != nil {
		return fmt.Errorf("the stored input named %s could not be re-encoded: %w", digest, err)
	}
	if derived != digest {
		return fmt.Errorf("%w: the input held as %s content-addresses to %s",
			ErrDigestMismatch, digest, derived)
	}
	return nil
}
