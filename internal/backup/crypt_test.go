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

package backup

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

// The archive is every credential the platform holds, and these are the
// properties a reader of the bucket must not be able to get around.

func TestAnEncryptedArchiveComesBackByteForByte(t *testing.T) {
	key := NewEncryptionKey()
	// Past one chunk, deliberately: the framing is where a stream cipher
	// wrapper is usually wrong.
	plain := make([]byte, 3*encryptionChunkBytes+17)
	if _, err := rand.Read(plain); err != nil {
		t.Fatal(err)
	}

	sealed := &bytes.Buffer{}
	writer, err := Encrypt(sealed, key)
	if err != nil {
		t.Fatal(err)
	}
	// In pieces that do not line up with the chunk, because the exporter
	// writes a tar and never counts.
	for offset := 0; offset < len(plain); offset += 1000 {
		if _, err := writer.Write(plain[offset:min(offset+1000, len(plain))]); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(sealed.Bytes(), plain[:64]) {
		t.Fatal("the archive's plaintext is in the ciphertext")
	}
	if !strings.HasPrefix(sealed.String(), EncryptedMagic) {
		t.Fatal("an encrypted archive does not start with the magic a restore looks for")
	}

	reader, err := Decrypt(bytes.NewReader(sealed.Bytes()), key, false)
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, plain) {
		t.Fatalf("the archive came back as %d bytes and went in as %d", len(read), len(plain))
	}
}

func TestAnotherKeyDoesNotOpenAnArchive(t *testing.T) {
	sealed := &bytes.Buffer{}
	writer, err := Encrypt(sealed, NewEncryptionKey())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("the cloudflare token")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := Decrypt(bytes.NewReader(sealed.Bytes()), NewEncryptionKey(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("an archive opened under a key that did not write it")
	}
}

func TestATruncatedArchiveIsRefused(t *testing.T) {
	key := NewEncryptionKey()
	sealed := &bytes.Buffer{}
	writer, err := Encrypt(sealed, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(bytes.Repeat([]byte("secret"), 4000)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	cut := sealed.Bytes()[:sealed.Len()-32]
	reader, err := Decrypt(bytes.NewReader(cut), key, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("an archive missing its tail read as a whole one")
	}

	// The same bytes are *not* an error where the reader was told it holds a
	// prefix on purpose, which is what an upload's verification holds.
	prefix, err := Decrypt(bytes.NewReader(cut), key, true)
	if err != nil {
		t.Fatal(err)
	}
	read, err := io.ReadAll(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(read) == 0 {
		t.Fatal("a prefix of an archive decrypted to nothing")
	}
}

func TestAnAlteredArchiveIsRefused(t *testing.T) {
	key := NewEncryptionKey()
	sealed := &bytes.Buffer{}
	writer, err := Encrypt(sealed, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(bytes.Repeat([]byte("secret"), 4000)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	altered := bytes.Clone(sealed.Bytes())
	altered[len(EncryptedMagic)+encryptionSaltBytes+40] ^= 0xff
	reader, err := Decrypt(bytes.NewReader(altered), key, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(reader); err == nil {
		t.Fatal("an archive somebody edited in the bucket read as the one that was written")
	}
}

func TestSniffTellsAnEncryptedArchiveFromAPlainOne(t *testing.T) {
	sealed := &bytes.Buffer{}
	writer, err := Encrypt(sealed, NewEncryptionKey())
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	encrypted, rest, err := Sniff(bytes.NewReader(sealed.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !encrypted {
		t.Fatal("an encrypted archive was not recognised as one")
	}
	// Nothing is consumed: the reader handed back still carries the header.
	read, err := io.ReadAll(rest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, sealed.Bytes()) {
		t.Fatal("sniffing an archive ate part of it")
	}

	// A gzip stream — which is what every archive written before this was —
	// is not mistaken for one.
	plain := []byte{0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03, 0x01, 0x02}
	encrypted, rest, err = Sniff(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	if encrypted {
		t.Fatal("a plain gzip archive was taken for an encrypted one")
	}
	read, err = io.ReadAll(rest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(read, plain) {
		t.Fatal("sniffing a plain archive ate part of it")
	}
}

func TestAKeyIsReadTheWayAnOperatorWritesOneDown(t *testing.T) {
	key := NewEncryptionKey()
	for _, written := range []string{
		base64.StdEncoding.EncodeToString(key),
		strings.TrimRight(base64.StdEncoding.EncodeToString(key), "="),
		hex.EncodeToString(key),
		"  " + base64.StdEncoding.EncodeToString(key) + "\n",
	} {
		parsed, err := ParseEncryptionKey(written)
		if err != nil {
			t.Fatalf("%q: %v", written, err)
		}
		if !bytes.Equal(parsed, key) {
			t.Fatalf("%q parsed to another key", written)
		}
	}

	for _, refused := range []string{"", "   ", "too short", base64.StdEncoding.EncodeToString([]byte("sixteen bytes!!!"))} {
		if _, err := ParseEncryptionKey(refused); err == nil {
			t.Fatalf("%q was accepted as a key", refused)
		}
	}
}
