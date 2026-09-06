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
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
)

// Encrypting an archive on its way to a destination.
//
// The archive is every credential the platform holds, and an archive at a
// destination is one nobody is watching: it sits in somebody's bucket for as
// long as the retention keeps it, reachable by whoever holds that bucket's
// credential and by whoever the bucket's own policy has ever admitted. Asking
// the store to encrypt it (`serverSideEncryption`) is worth doing and is not
// this: a store that encrypts at rest still decrypts for anybody it answers,
// which means the confidentiality of every platform credential rests on the
// bucket's configuration. Encrypting here rests it on a key that is not in the
// bucket, is not in the archive, and never leaves this cluster except in the
// operator's own hands.
//
// The construction is the conventional one for a stream nobody can hold in
// memory, and every part of it is stdlib:
//
//   - A random 16-byte salt per archive, and a per-archive subkey derived from
//     the operator's key with HKDF-SHA256. Two archives under one key
//     therefore share no keystream, and a key is used for one message per
//     archive rather than for thousands.
//   - AES-256-GCM over 16 KiB chunks, each with the chunk's counter in the
//     nonce, so a chunk cannot be reordered, dropped or replayed into another
//     position.
//   - The final chunk says so *in its nonce*, which is what makes truncation
//     an error rather than a shorter archive. A stream that ends without one
//     is refused.
//
// The chunk is small deliberately. A scheduled run verifies its upload by
// reading the first 64 KiB back off the destination and parsing the manifest
// out of it (destination.VerifyBytes), and that only works if a prefix of an
// encrypted archive decrypts to a prefix of the plaintext — which is what
// per-chunk framing buys, at 20 bytes per 16 KiB.
//
// An archive written before any of this existed is a gzip stream, and a
// restore has to keep reading one. The two are told apart by the header:
// EncryptedMagic is the first bytes of an encrypted archive and is not a gzip
// magic, so Sniff answers without a manifest field, a filename convention or
// anything else that could be lost on the way to the bucket.

const (
	// EncryptedMagic opens an encrypted archive. It is the whole of how a
	// restore tells one from the plain gzip an older release wrote: gzip
	// starts 0x1f 0x8b, this does not, and neither can be mistaken for the
	// other by a reader that has only the bytes.
	EncryptedMagic = "kitchen-backup-enc-v1\n"

	// EncryptionKeyBytes is the key's length: AES-256.
	EncryptionKeyBytes = 32

	// encryptionSaltBytes is the per-archive salt the subkey is derived from.
	encryptionSaltBytes = 16

	// encryptionChunkBytes is one chunk's plaintext. See the note above on
	// why it is small: the upload's verification reads a prefix.
	encryptionChunkBytes = 16 << 10

	// encryptionInfo binds the derived subkey to this use. A key that was
	// ever used for something else derives a different subkey there.
	encryptionInfo = "kitchen backup archive v1"

	// maxEncryptedChunk bounds what a reader will allocate for one chunk. A
	// frame claiming more than this is a corrupt or hostile archive, not a
	// chunk — the writer never emits one.
	maxEncryptedChunk = encryptionChunkBytes + 1024
)

// ErrNoEncryptionKey is an encrypted archive met with no key to open it. It is
// its own error because the restore path answers it with a sentence naming
// where the key should have come from, which is a different sentence from
// "this archive is corrupt".
var ErrNoEncryptionKey = errors.New(
	"this archive is encrypted and no key was supplied to open it")

// ParseEncryptionKey reads a key as an operator writes one down: base64, as
// `openssl rand -base64 32` prints it, or hex, as `openssl rand -hex 32` does.
// Both spellings reach 32 bytes and anything else is refused here rather than
// at 02:00 on the night the archive was needed.
func ParseEncryptionKey(written string) ([]byte, error) {
	trimmed := strings.TrimSpace(written)
	if trimmed == "" {
		return nil, errors.New("the backup encryption key is empty")
	}
	if decoded, err := hex.DecodeString(trimmed); err == nil && len(decoded) == EncryptionKeyBytes {
		return decoded, nil
	}
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err := encoding.DecodeString(trimmed); err == nil && len(decoded) == EncryptionKeyBytes {
			return decoded, nil
		}
	}
	// Raw bytes, which is what a Secret written with --from-file holds.
	if len(trimmed) == EncryptionKeyBytes {
		return []byte(trimmed), nil
	}
	return nil, fmt.Errorf(
		"the backup encryption key must be %d bytes, written as base64 or hex — `openssl rand -base64 %d`",
		EncryptionKeyBytes, EncryptionKeyBytes)
}

// NewEncryptionKey mints one. Nothing on the platform calls this: the key is
// the operator's, supplied once and never read back, for the reason a
// notification subscription's signing key is theirs. It is here so that tests
// and documentation agree about what a key is.
func NewEncryptionKey() []byte {
	key := make([]byte, EncryptionKeyBytes)
	// crypto/rand.Read never fails on any supported platform; it panics
	// inside the standard library rather than returning an error.
	_, _ = rand.Read(key)
	return key
}

// Encrypt wraps a writer so that everything written to it lands encrypted.
//
// The returned writer must be closed: the final chunk — the one that says it
// is the final chunk — is written there, and an archive without it is refused
// on the way back in. Closing does not close the underlying writer.
func Encrypt(w io.Writer, key []byte) (io.WriteCloser, error) {
	salt := make([]byte, encryptionSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("the archive's salt could not be generated: %w", err)
	}
	gcm, err := archiveCipher(key, salt)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write([]byte(EncryptedMagic)); err != nil {
		return nil, fmt.Errorf("the archive's header could not be written: %w", err)
	}
	if _, err := w.Write(salt); err != nil {
		return nil, fmt.Errorf("the archive's header could not be written: %w", err)
	}
	return &encryptingWriter{writer: w, gcm: gcm, buffer: make([]byte, 0, encryptionChunkBytes)}, nil
}

// Decrypt reads an encrypted archive back.
//
// partial is what an upload's verification needs: it reads the first tens of
// kilobytes of the object off the destination and parses the manifest out of
// them, so the stream it holds is a prefix by design and its truncation is not
// a fault. Everywhere else it is false, and a stream that stops before the
// final chunk is an error — which is the point of framing the final chunk at
// all.
func Decrypt(r io.Reader, key []byte, partial bool) (io.Reader, error) {
	header := make([]byte, len(EncryptedMagic)+encryptionSaltBytes)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, fmt.Errorf("this is not a Kitchen backup archive: %w", err)
	}
	if string(header[:len(EncryptedMagic)]) != EncryptedMagic {
		return nil, errors.New("this archive is not encrypted")
	}
	gcm, err := archiveCipher(key, header[len(EncryptedMagic):])
	if err != nil {
		return nil, err
	}
	return &decryptingReader{reader: r, gcm: gcm, partial: partial}, nil
}

// Sniff answers whether a stream is an encrypted archive, and hands back a
// reader that still carries the bytes it had to look at.
//
// This is how a restore tells an encrypted archive from the plain gzip an
// older release wrote, and it is deliberately a property of the bytes rather
// than of a manifest field or a filename: the manifest is *inside* the
// encryption, and a filename is whatever somebody renamed the object to.
func Sniff(r io.Reader) (bool, io.Reader, error) {
	buffered := bufio.NewReaderSize(r, len(EncryptedMagic)+encryptionSaltBytes+512)
	head, err := buffered.Peek(len(EncryptedMagic))
	switch {
	case errors.Is(err, io.EOF):
		// A stream shorter than the magic is not encrypted, and whatever it
		// is the archive reader will say so in its own words.
		return false, buffered, nil
	case err != nil:
		return false, nil, fmt.Errorf("the archive could not be read: %w", err)
	}
	return string(head) == EncryptedMagic, buffered, nil
}

// archiveCipher derives this archive's subkey and returns the AEAD over it.
func archiveCipher(key, salt []byte) (cipher.AEAD, error) {
	if len(key) != EncryptionKeyBytes {
		return nil, fmt.Errorf("the backup encryption key is %d bytes and must be %d",
			len(key), EncryptionKeyBytes)
	}
	derived, err := hkdf.Key(sha256.New, key, salt, encryptionInfo, EncryptionKeyBytes)
	if err != nil {
		return nil, fmt.Errorf("the archive's key could not be derived: %w", err)
	}
	block, err := aes.NewCipher(derived)
	if err != nil {
		return nil, fmt.Errorf("the archive's cipher could not be built: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("the archive's cipher could not be built: %w", err)
	}
	return gcm, nil
}

// chunkNonce is one chunk's nonce: its counter, and whether it is the last.
//
// The final flag lives in the nonce rather than beside it so that it is
// authenticated without a separate field: a reader that meets a final chunk
// where it expected another one, or an archive whose tail was cut off, fails
// to authenticate rather than quietly reading a shorter platform.
func chunkNonce(gcm cipher.AEAD, counter uint64, final bool) []byte {
	nonce := make([]byte, gcm.NonceSize())
	binary.BigEndian.PutUint64(nonce[:8], counter)
	if final {
		nonce[8] = 1
	}
	return nonce
}

// encryptingWriter buffers up to a chunk and seals each one.
type encryptingWriter struct {
	writer  io.Writer
	gcm     cipher.AEAD
	buffer  []byte
	counter uint64
	closed  bool
	err     error
}

func (e *encryptingWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if e.closed {
		return 0, errors.New("this archive has already been closed")
	}
	written := len(p)
	for len(p) > 0 {
		room := encryptionChunkBytes - len(e.buffer)
		take := min(room, len(p))
		e.buffer = append(e.buffer, p[:take]...)
		p = p[take:]
		if len(e.buffer) == encryptionChunkBytes {
			if err := e.seal(false); err != nil {
				e.err = err
				return written - len(p), err
			}
		}
	}
	return written, nil
}

// Close seals what is left and writes the final chunk, which is what says the
// archive is whole.
func (e *encryptingWriter) Close() error {
	if e.err != nil {
		return e.err
	}
	if e.closed {
		return nil
	}
	e.closed = true
	e.err = e.seal(true)
	return e.err
}

func (e *encryptingWriter) seal(final bool) error {
	sealed := e.gcm.Seal(nil, chunkNonce(e.gcm, e.counter, final), e.buffer, nil)
	e.counter++
	e.buffer = e.buffer[:0]
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(len(sealed))) //nolint:gosec // a chunk is bounded above
	if _, err := e.writer.Write(length[:]); err != nil {
		return fmt.Errorf("the archive could not be written: %w", err)
	}
	if _, err := e.writer.Write(sealed); err != nil {
		return fmt.Errorf("the archive could not be written: %w", err)
	}
	return nil
}

// decryptingReader opens one chunk at a time.
type decryptingReader struct {
	reader  io.Reader
	gcm     cipher.AEAD
	partial bool

	plain   []byte
	counter uint64
	done    bool
	err     error
}

func (d *decryptingReader) Read(p []byte) (int, error) {
	for len(d.plain) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			d.err = err
			return 0, err
		}
	}
	n := copy(p, d.plain)
	d.plain = d.plain[n:]
	return n, nil
}

// next reads and opens one chunk.
func (d *decryptingReader) next() error {
	var length [4]byte
	switch _, err := io.ReadFull(d.reader, length[:]); {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return d.truncated(err)
	case err != nil:
		return fmt.Errorf("the archive could not be read: %w", err)
	}
	size := binary.BigEndian.Uint32(length[:])
	if size < uint32(d.gcm.Overhead()) || size > maxEncryptedChunk {
		return errors.New("the archive is truncated or corrupt: a chunk claims an impossible length")
	}
	sealed := make([]byte, size)
	switch _, err := io.ReadFull(d.reader, sealed); {
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return d.truncated(err)
	case err != nil:
		return fmt.Errorf("the archive could not be read: %w", err)
	}

	// A chunk opens as an ordinary one or as the final one, and which it was
	// is authenticated: the counter and the final flag are the nonce.
	plain, err := d.gcm.Open(nil, chunkNonce(d.gcm, d.counter, false), sealed, nil)
	if err != nil {
		final, finalErr := d.gcm.Open(nil, chunkNonce(d.gcm, d.counter, true), sealed, nil)
		if finalErr != nil {
			return errors.New("this archive could not be decrypted: it was written with a different " +
				"key, or it has been altered since it was written")
		}
		plain, d.done = final, true
	}
	d.counter++
	d.plain = plain
	return nil
}

// truncated is a stream that stopped before the final chunk. It is an error
// unless the reader was told it is holding a prefix on purpose.
func (d *decryptingReader) truncated(err error) error {
	if d.partial {
		d.done = true
		return io.EOF
	}
	return fmt.Errorf("this archive is truncated: it ends before the chunk that says it is complete: %w", err)
}
