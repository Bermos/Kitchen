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

package previewgate

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// The gate keeps no state of its own: a session, an in-flight login and the
// hand-off between the two are all signed blobs it hands to the browser and
// verifies on the way back. Every one of them carries the purpose it was
// minted for, so a value that is valid in one place cannot be replayed in
// another, and the host it belongs to, so a preview's session cannot be
// carried to a different preview.
type purpose string

const (
	// purposeSession is the cookie that says who the visitor is.
	purposeSession purpose = "session"
	// purposeReturn is the URL a login should come back to, signed so the
	// gate's own host cannot be turned into an open redirector.
	purposeReturn purpose = "return"
	// purposeFlow is the in-flight login: the state nonce and the PKCE
	// verifier, kept in a cookie on the gate's host.
	purposeFlow purpose = "flow"
	// purposeHandoff carries a finished login from the gate's host to the
	// preview's, where the session cookie can be set.
	purposeHandoff purpose = "handoff"
)

// claims is the payload of every signed value the gate mints.
type claims struct {
	Purpose purpose `json:"p"`
	// Host the value is valid on, or the host of the URL it points at.
	Host string `json:"h,omitempty"`
	// Subject and Email identify the signed-in platform user.
	Subject string `json:"s,omitempty"`
	Email   string `json:"e,omitempty"`
	// ReturnURL is where a login should resume.
	ReturnURL string `json:"r,omitempty"`
	// Nonce ties an authorization response to the flow that started it.
	Nonce string `json:"n,omitempty"`
	// Verifier is the PKCE code verifier.
	Verifier string `json:"v,omitempty"`
	// ExpiresAt is a Unix timestamp.
	ExpiresAt int64 `json:"x"`
}

var errInvalidToken = errors.New("the token is missing, expired or not ours")

// signer mints and verifies the gate's signed values.
type signer struct {
	key []byte
	// now is time.Now in production and a fixed clock in tests.
	now func() time.Time
}

func newSigner(secret string) *signer {
	return &signer{key: []byte(secret), now: time.Now}
}

// mint returns a signed, URL-safe token: payload.signature.
func (s *signer) mint(c claims, ttl time.Duration) (string, error) {
	c.ExpiresAt = s.now().Add(ttl).Unix()
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + s.sign(encoded), nil
}

// verify checks the signature, the purpose, the host and the expiry, in that
// order, and returns the claims only when all of them hold. Every failure
// returns the same error: a caller that told them apart would be telling an
// attacker apart too.
func (s *signer) verify(token string, want purpose, host string) (claims, error) {
	encoded, signature, found := cut(token, '.')
	if !found {
		return claims{}, errInvalidToken
	}
	if subtle.ConstantTimeCompare([]byte(signature), []byte(s.sign(encoded))) != 1 {
		return claims{}, errInvalidToken
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return claims{}, errInvalidToken
	}
	var c claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return claims{}, errInvalidToken
	}
	if c.Purpose != want {
		return claims{}, errInvalidToken
	}
	if host != "" && c.Host != host {
		return claims{}, errInvalidToken
	}
	if c.ExpiresAt <= s.now().Unix() {
		return claims{}, errInvalidToken
	}
	return c, nil
}

func (s *signer) sign(encoded string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(encoded))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// cut splits on the last separator, so a payload may contain it and the
// signature may not.
func cut(value string, sep byte) (string, string, bool) {
	for i := len(value) - 1; i >= 0; i-- {
		if value[i] == sep {
			return value[:i], value[i+1:], true
		}
	}
	return value, "", false
}

// String makes a token safe to put in a log line without leaking it.
func (c claims) String() string {
	return fmt.Sprintf("%s for %s", c.Purpose, c.Host)
}
