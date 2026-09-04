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
	"strconv"
	"strings"
	"time"
)

// The signature, which is the whole of what a receiver has to implement.
//
// Every request carries `X-Kitchen-Signature: v1=<hex>`, an HMAC-SHA256 over
// the string `v1:<timestamp>:<body>` keyed with the subscription's secret,
// where `<timestamp>` is the integer in `X-Kitchen-Timestamp` and `<body>` is
// the request body byte for byte.
//
// Three decisions in that, each of which a receiver author would otherwise
// have to guess at:
//
//   - **The timestamp is inside the signed string**, so a captured request
//     cannot be replayed a week later against a receiver that checks how old
//     it is. A receiver should reject anything older than a few minutes; the
//     window is the receiver's call, because only it knows how far its clock
//     can be from the platform's.
//   - **The scheme is named in the value** (`v1=`), so a second scheme can be
//     sent alongside the first one day rather than instead of it. A receiver
//     matches on the prefix rather than assuming the whole value is a hash.
//   - **The body is signed as bytes, not as JSON.** Nothing re-serializes the
//     payload between attempts — the delivery object holds the exact bytes —
//     so a receiver may verify before it parses, which is the order that
//     keeps a malformed body from being parsed by an unauthenticated caller.
const (
	// SignatureScheme is the prefix on the signature header's value.
	SignatureScheme = "v1"

	// HeaderSignature carries the HMAC, HeaderTimestamp the integer it is
	// bound to, and the rest describe the delivery.
	HeaderSignature = "X-Kitchen-Signature"
	HeaderTimestamp = "X-Kitchen-Timestamp"
	HeaderEvent     = "X-Kitchen-Event"
	HeaderEventID   = "X-Kitchen-Event-Id"
	HeaderDelivery  = "X-Kitchen-Delivery"
	HeaderAttempt   = "X-Kitchen-Attempt"

	// SecretKey is the key the signing secret is stored under in its Secret.
	SecretKey = "secret"
)

// Sign returns the value for HeaderSignature: the scheme, an `=`, and the hex
// HMAC over `v1:<timestamp>:<body>`.
func Sign(secret []byte, timestamp time.Time, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(SignatureScheme))
	mac.Write([]byte{':'})
	mac.Write([]byte(strconv.FormatInt(timestamp.UTC().Unix(), 10)))
	mac.Write([]byte{':'})
	mac.Write(body)
	return SignatureScheme + "=" + hex.EncodeToString(mac.Sum(nil))
}

// Verify is the receiver's half, and it is here so that the documentation and
// the tests check the same implementation the operator sends. A relay in
// another language reimplements this in about ten lines; docs/api/notifications.md
// spells it out.
//
// The comparison is constant-time, which matters: a receiver that compares
// signatures with `==` leaks the correct one a byte at a time to anybody
// willing to send a few million requests.
func Verify(secret []byte, header string, timestamp time.Time, body []byte) bool {
	scheme, value, found := strings.Cut(strings.TrimSpace(header), "=")
	if !found || scheme != SignatureScheme {
		return false
	}
	expected := Sign(secret, timestamp, body)
	_, want, _ := strings.Cut(expected, "=")
	return hmac.Equal([]byte(value), []byte(want))
}

// Backoff is how long to wait after an attempt that failed, doubling from ten
// seconds and capped at ten minutes.
//
// Five attempts — the default ladder — are four waits of 10s, 20s, 40s and
// 80s: two and a half minutes, which is a receiver being restarted or
// redeployed. It is deliberately not hours: a notification that arrives long
// after the deploy it reports on is worse than a dead letter, because nobody
// reads it as history.
func Backoff(attempt int32) time.Duration {
	const (
		base    = 10 * time.Second
		ceiling = 10 * time.Minute
	)
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 12 {
		// 10s << 12 is already past the cap; the shift is bounded so it
		// cannot overflow on a hand-edited attempt count.
		attempt = 12
	}
	wait := base << (attempt - 1)
	if wait > ceiling {
		return ceiling
	}
	return wait
}
