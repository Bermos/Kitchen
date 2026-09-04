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

package notify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

// The signature is the only thing a receiver has to get right, and the only
// thing this platform can get wrong in a way nobody notices until it matters:
// a signature that verifies for everything is a signature that proves nothing.
// So the tests are the four things that must not verify as well as the one
// that must.

var (
	signingKey = []byte("a-signing-key-nobody-else-has")
	signedAt   = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	signedBody = []byte(`{"version":"v1","id":"abc","type":"deploy.succeeded"}`)
)

func TestSignatureRoundTrips(t *testing.T) {
	header := Sign(signingKey, signedAt, signedBody)
	if !strings.HasPrefix(header, SignatureScheme+"=") {
		t.Fatalf("signature %q does not name its scheme", header)
	}
	if !Verify(signingKey, header, signedAt, signedBody) {
		t.Fatal("a signature this package produced does not verify against this package")
	}
}

// TestSignatureIsTheDocumentedConstruction: docs/api/notifications.md tells a
// relay author to HMAC-SHA256 the string `v1:<timestamp>:<body>`, and somebody
// will write that in Python without reading this package. If the two ever part
// company, every relay on every installation stops verifying at once.
func TestSignatureIsTheDocumentedConstruction(t *testing.T) {
	mac := hmac.New(sha256.New, signingKey)
	mac.Write([]byte("v1:1788523200:"))
	mac.Write(signedBody)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))

	if got := Sign(signingKey, signedAt, signedBody); got != want {
		t.Fatalf("signature is %q, but the documented construction gives %q", got, want)
	}
	if signedAt.Unix() != 1788523200 {
		t.Fatalf("the fixture's timestamp is %d, not the one hashed above", signedAt.Unix())
	}
}

func TestSignatureRefusesWhatItShould(t *testing.T) {
	header := Sign(signingKey, signedAt, signedBody)

	t.Run("a changed body", func(t *testing.T) {
		tampered := []byte(`{"version":"v1","id":"abc","type":"build.failed"}`)
		if Verify(signingKey, header, signedAt, tampered) {
			t.Fatal("a body somebody edited on the way verified")
		}
	})
	t.Run("another key", func(t *testing.T) {
		if Verify([]byte("some-other-subscriptions-key"), header, signedAt, signedBody) {
			t.Fatal("a signature verified under a key that did not make it")
		}
	})
	t.Run("a replayed timestamp", func(t *testing.T) {
		// The timestamp is inside the signed string, so a captured request
		// replayed with a fresh header does not verify. This is what lets a
		// receiver refuse anything older than a few minutes.
		if Verify(signingKey, header, signedAt.Add(time.Hour), signedBody) {
			t.Fatal("a signature verified against a timestamp it was not made with")
		}
	})
	t.Run("another scheme", func(t *testing.T) {
		_, value, _ := strings.Cut(header, "=")
		if Verify(signingKey, "v2="+value, signedAt, signedBody) {
			t.Fatal("a signature named as another scheme verified")
		}
		if Verify(signingKey, value, signedAt, signedBody) {
			t.Fatal("a bare hash with no scheme verified")
		}
	})
}

// TestBackoffIsBoundedAndGrows pins the ladder the retry contract is written
// in: doubling, and capped, so that a receiver that is gone cannot hold a
// delivery open for hours.
func TestBackoffIsBoundedAndGrows(t *testing.T) {
	want := []time.Duration{
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		80 * time.Second,
		160 * time.Second,
	}
	for i, expected := range want {
		if got := Backoff(int32(i + 1)); got != expected {
			t.Errorf("Backoff(%d) = %s, want %s", i+1, got, expected)
		}
	}
	if got := Backoff(20); got != 10*time.Minute {
		t.Errorf("Backoff(20) = %s, want the ten-minute cap", got)
	}
	if got := Backoff(0); got != 10*time.Second {
		t.Errorf("Backoff(0) = %s, want the first rung rather than nothing", got)
	}
}
