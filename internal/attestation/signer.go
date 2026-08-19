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
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
)

// Signer makes one signature over the bytes it is handed. It is an interface
// with one method because that is the whole of what a signing backend has to
// do, and because key custody is the one part of this scheme an institution
// cannot delegate: an adopter whose rules put the key in an HSM implements
// this against the HSM and changes nothing else.
//
// It takes a context because a backend that is not a local key is a network
// call, and a build must not block on one forever.
type Signer interface {
	// KeyID identifies the key the signature will be made with, and is what
	// lands in the envelope's `keyid`.
	KeyID() string
	// Sign returns the raw signature over message. The message is already
	// the DSSE pre-authentication encoding; a backend must not hash-and-sign
	// something else.
	Sign(ctx context.Context, message []byte) ([]byte, error)
}

// Verifier checks one signature. It has no context because every verification
// Kitchen does is against material it already holds — a backend that had to
// call out to verify would be checking something other than the signature.
type Verifier interface {
	KeyID() string
	Verify(message, signature []byte) error
}

// Secret keys the signing keypair is stored under. They are `private.pem` and
// `public.pem` rather than the `tls.key`/`tls.crt` pair a Kubernetes TLS
// secret uses, because this is not a TLS keypair and a secret that looked like
// one would eventually be mounted as one.
const (
	SecretKeyPrivate = "private.pem"
	SecretKeyPublic  = "public.pem"
)

// ECDSAKey is a signing backend over a local ECDSA P-256 key: the default, and
// the one that needs nothing an installation does not already have.
//
// P-256 with SHA-256 and ASN.1 signatures is what the rest of the supply-chain
// tooling defaults to, which matters more here than any argument about curves:
// evidence nothing else can check is not evidence.
type ECDSAKey struct {
	private *ecdsa.PrivateKey
	public  *ecdsa.PublicKey
	keyID   string
}

// GenerateECDSAKey makes a new keypair and returns it along with the PEM
// encodings to store.
func GenerateECDSAKey() (*ECDSAKey, []byte, []byte, error) {
	private, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generating a signing key failed: %w", err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encoding the signing key failed: %w", err)
	}
	publicDER, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("encoding the verification key failed: %w", err)
	}

	key := &ECDSAKey{private: private, public: &private.PublicKey, keyID: keyIDFor(publicDER)}
	return key,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: publicDER}),
		nil
}

// LoadECDSAKey reads a keypair back out of its PEM encodings. The public half
// is optional: it is derived from the private key when absent, and checked
// against it when present, so a secret whose two halves have drifted apart
// fails here rather than producing signatures nothing can verify.
func LoadECDSAKey(privatePEM, publicPEM []byte) (*ECDSAKey, error) {
	block, _ := pem.Decode(privatePEM)
	if block == nil {
		return nil, errors.New("the signing key is not PEM-encoded")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the signing key is not a PKCS#8 private key: %w", err)
	}
	private, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("the signing key is a %T, and attestations are signed with ECDSA", parsed)
	}
	if private.Curve != elliptic.P256() {
		return nil, fmt.Errorf("the signing key is on %s, and attestations are signed on P-256", private.Curve.Params().Name)
	}

	publicDER, err := x509.MarshalPKIXPublicKey(&private.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("encoding the verification key failed: %w", err)
	}
	if len(publicPEM) > 0 {
		stored, err := ParsePublicKey(publicPEM)
		if err != nil {
			return nil, err
		}
		if !stored.public.Equal(&private.PublicKey) {
			return nil, errors.New(
				"the stored public key does not belong to the stored private key: replace the secret rather than one half of it")
		}
	}
	return &ECDSAKey{private: private, public: &private.PublicKey, keyID: keyIDFor(publicDER)}, nil
}

// ParsePublicKey reads a verification key. It is what a verifier holds when it
// has the public half and nothing else, which is the position everyone outside
// the platform is in.
func ParsePublicKey(publicPEM []byte) (*ECDSAKey, error) {
	block, _ := pem.Decode(publicPEM)
	if block == nil {
		return nil, errors.New("the verification key is not PEM-encoded")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the verification key is not a PKIX public key: %w", err)
	}
	public, ok := parsed.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("the verification key is a %T, and attestations are signed with ECDSA", parsed)
	}
	return &ECDSAKey{public: public, keyID: keyIDFor(block.Bytes)}, nil
}

// keyIDFor is the SHA-256 of the key's PKIX DER encoding, hex-encoded. It
// identifies the key without revealing anything the public key does not, and
// two installations that happen to hold the same public key agree on it.
func keyIDFor(publicDER []byte) string {
	sum := sha256.Sum256(publicDER)
	return hex.EncodeToString(sum[:])
}

// KeyID identifies the key.
func (k *ECDSAKey) KeyID() string { return k.keyID }

// PublicPEM is the verification half, for publishing. A public key is not a
// credential and this is not the API reading one back: evidence signed by a
// key nobody can obtain is evidence nobody can check.
func (k *ECDSAKey) PublicPEM() ([]byte, error) {
	if k.public == nil {
		return nil, errors.New("this key has no public half")
	}
	der, err := x509.MarshalPKIXPublicKey(k.public)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}

// Sign signs the DSSE pre-authentication encoding.
func (k *ECDSAKey) Sign(_ context.Context, message []byte) ([]byte, error) {
	if k.private == nil {
		return nil, errors.New("this key can verify but not sign: it has no private half")
	}
	digest := sha256.Sum256(message)
	return ecdsa.SignASN1(rand.Reader, k.private, digest[:])
}

// Verify checks a signature over the DSSE pre-authentication encoding.
func (k *ECDSAKey) Verify(message, signature []byte) error {
	if k.public == nil {
		return errors.New("this key cannot verify: it has no public half")
	}
	digest := sha256.Sum256(message)
	if !ecdsa.VerifyASN1(k.public, digest[:], signature) {
		return errors.New("the signature was not made by this key")
	}
	return nil
}

// Public exposes the verification key, so that a caller holding a signer can
// hand out what checks its output without holding the private half.
func (k *ECDSAKey) Public() crypto.PublicKey { return k.public }

// Compile-time proof that one type satisfies both halves. The signer and the
// verifier are separate interfaces because most backends only ever implement
// one of them, and a local key is the case where they coincide.
var (
	_ Signer   = (*ECDSAKey)(nil)
	_ Verifier = (*ECDSAKey)(nil)
)
