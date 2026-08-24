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

package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `kitchen audit-pack` writes a deliverable, so what these tests assert is
// what is on disk: two files, the right names, and a digest computed from the
// bytes that were actually written rather than echoed from the platform.

// pack is a plausible document: enough of the real shape for the command to
// summarize, and stable bytes so the digest is checkable.
const packDocument = `{"schema":"https://kitchen.bermos.dev/audit-pack/v1","project":"shop",` +
	`"range":{"from":"2026-01-01T00:00:00Z","to":"2026-04-01T00:00:00Z"},` +
	`"verification":{"signed":true},` +
	`"retention":{"truncated":false,"message":"the whole of the requested window is inside what the store still holds"},` +
	`"inventory":{"environments":[{},{}],"releases":[{}],"claims":[]},` +
	`"changeLog":[{}],"promotions":[{}],"decisions":{"items":[{},{}],"truncated":false},` +
	`"attestations":[{}],"exceptions":[],` +
	`"auditLog":{"items":[{},{},{}],"truncated":false},"signedRecords":{"items":[{}]}}`

const packEnvelope = `{"payloadType":"application/vnd.in-toto+json","payload":"e30=",` +
	`"signatures":[{"keyid":"k1","sig":"c2ln"}]}`

func packPlatform(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.platform.auditPack = []byte(packDocument)
	h.platform.auditPackEnvelope = []byte(packEnvelope)
	h.platform.auditPackHTML = []byte("<!doctype html><html><body>audit pack</body></html>")
	return h
}

func TestAuditPackWritesTheDocumentAndTheSignatureBesideIt(t *testing.T) {
	h := packPlatform(t)

	code := h.run("audit-pack", "--project", "shop",
		"--from", "2026-01-01", "--to", "2026-04-01", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	taken := auditPackTaken{}
	h.answer(&taken)

	// Two files, under the names the platform suggested, in the working
	// directory — which is what makes a pack found on a disk months later say
	// which project and which quarter it is.
	pack := filepath.Join(h.work, "kitchen-audit-pack-shop-2026-01-01-2026-04-01.json")
	envelope := filepath.Join(h.work, "kitchen-audit-pack-shop-2026-01-01-2026-04-01.dsse.json")
	if taken.File != pack {
		t.Errorf("wrote the pack to %q, want %q", taken.File, pack)
	}
	if taken.Signature != envelope {
		t.Errorf("wrote the signature to %q, want %q", taken.Signature, envelope)
	}
	written, err := os.ReadFile(taken.File)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != packDocument {
		t.Fatal("the pack on disk is not the bytes the platform served")
	}
	signed, err := os.ReadFile(taken.Signature)
	if err != nil {
		t.Fatal(err)
	}
	if string(signed) != packEnvelope {
		t.Fatal("the envelope on disk is not the one the platform served")
	}

	// The digest describes the file, and the platform's own header is
	// reported beside it so a mismatch is visible rather than assumed away.
	sum := sha256.Sum256([]byte(packDocument))
	if want := "sha256:" + hex.EncodeToString(sum[:]); taken.Digest != want {
		t.Errorf("digest %q, want %q", taken.Digest, want)
	}
	if taken.ServedDigest != taken.Digest {
		t.Errorf("the platform served %q and the file hashes to %q", taken.ServedDigest, taken.Digest)
	}
	if !taken.Signed || taken.Truncated {
		t.Errorf("the fixture is signed and fully covered, got %+v", taken)
	}

	// The counts are read back off the file, so a scheduled job can tell an
	// empty quarter from a failed export.
	for section, want := range map[string]int{
		"environments": 2, "releases": 1, "changes": 1, "promotions": 1,
		"decisions": 2, "attestations": 1, "auditRecords": 3, "signedRecords": 1,
	} {
		if taken.Sections[section] != want {
			t.Errorf("%s counted %d, want %d", section, taken.Sections[section], want)
		}
	}
}

// The window travels to the platform as two RFC 3339 instants, whatever the
// caller typed. A bare date is midnight UTC, which is what a quarter's
// boundaries actually are.
func TestAuditPackSendsTheWindowAsInstants(t *testing.T) {
	h := packPlatform(t)

	if code := h.run("audit-pack", "--project", "shop",
		"--from", "2026-01-01", "--to", "2026-04-01", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	asked := h.platform.sent("GET", "/audit-pack")
	if len(asked) == 0 {
		t.Fatal("the platform saw no request for a pack")
	}
	if !strings.Contains(asked[0].Query, "from=2026-01-01T00%3A00%3A00Z") ||
		!strings.Contains(asked[0].Query, "to=2026-04-01T00%3A00%3A00Z") {
		t.Fatalf("the window must reach the platform as instants, got %q", asked[0].Query)
	}
}

// Both ends are required, and the refusal says why rather than filling one in.
func TestAuditPackRefusesAWindowWithOneEnd(t *testing.T) {
	for _, args := range [][]string{
		{"audit-pack", "--project", "shop", "--json"},
		{"audit-pack", "--project", "shop", "--from", "2026-01-01", "--json"},
		{"audit-pack", "--project", "shop", "--to", "2026-04-01", "--json"},
	} {
		h := packPlatform(t)
		if code := h.run(args...); code != exitUsage {
			t.Fatalf("%v: exit %d, want %d: %s", args, code, exitUsage, h.stdout.String())
		}
		if !strings.Contains(h.stdout.String(), "reproduc") {
			t.Fatalf("%v: the refusal must say why both ends are required, got %s",
				args, h.stdout.String())
		}
	}
}

func TestAuditPackRefusesAnInvertedWindow(t *testing.T) {
	h := packPlatform(t)
	if code := h.run("audit-pack", "--project", "shop",
		"--from", "2026-04-01", "--to", "2026-01-01", "--json"); code != exitUsage {
		t.Fatalf("exit %d, want %d: %s", code, exitUsage, h.stdout.String())
	}
}

// A platform with no signing key: the pack is written, the signature is not
// asked for, and the answer says so rather than leaving a blank field.
func TestAuditPackFromAnUnsignedPlatformWritesThePackAndSaysWhyThereIsNoSignature(t *testing.T) {
	h := packPlatform(t)
	h.platform.auditPack = []byte(strings.Replace(packDocument,
		`"verification":{"signed":true}`,
		`"verification":{"signed":false,"message":"attestation is switched off on this platform"}`, 1))
	h.platform.auditPackEnvelope = nil

	if code := h.run("audit-pack", "--project", "shop",
		"--from", "2026-01-01", "--to", "2026-04-01", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	taken := auditPackTaken{}
	h.answer(&taken)

	if taken.Signed || taken.Signature != "" {
		t.Fatalf("an unsigned platform produces no envelope, got %+v", taken)
	}
	if !strings.Contains(taken.Message, "attestation is switched off") {
		t.Fatalf("the answer must carry the platform's own reason, got %q", taken.Message)
	}
	if _, err := os.Stat(taken.File); err != nil {
		t.Fatalf("the pack itself must still be written: %v", err)
	}
}

// A pack that answers for less than it was asked for is reported as such, so a
// scheduled job can branch on one field.
func TestAuditPackReportsATruncatedWindow(t *testing.T) {
	h := packPlatform(t)
	h.platform.auditPack = []byte(strings.Replace(packDocument,
		`"retention":{"truncated":false,"message":"the whole of the requested window is inside what the store still holds"}`,
		`"retention":{"truncated":true,"message":"retention has already removed part of the window"}`, 1))

	if code := h.run("audit-pack", "--project", "shop",
		"--from", "2026-01-01", "--to", "2026-04-01", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	taken := auditPackTaken{}
	h.answer(&taken)
	if !taken.Truncated || !strings.Contains(taken.Coverage, "retention has already removed") {
		t.Fatalf("a truncated window must be reported with the platform's own words, got %+v", taken)
	}
}

// --format html writes the reader's version and asks for nothing to sign it:
// a rendering is not the document, and there is nothing to sign.
func TestAuditPackHTMLWritesOnePageAndNoSignature(t *testing.T) {
	h := packPlatform(t)

	if code := h.run("audit-pack", "--project", "shop", "--format", "html",
		"--from", "2026-01-01", "--to", "2026-04-01", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	taken := auditPackTaken{}
	h.answer(&taken)

	if !strings.HasSuffix(taken.File, ".html") || taken.Signature != "" {
		t.Fatalf("want one HTML file and no envelope, got %+v", taken)
	}
	written, err := os.ReadFile(taken.File)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "audit pack") {
		t.Fatal("the rendering was not written")
	}
}

func TestAuditPackRefusesAFormatItDoesNotWrite(t *testing.T) {
	h := packPlatform(t)
	if code := h.run("audit-pack", "--project", "shop", "--format", "pdf",
		"--from", "2026-01-01", "--to", "2026-04-01", "--json"); code != exitUsage {
		t.Fatalf("exit %d, want %d: %s", code, exitUsage, h.stdout.String())
	}
}

// A file already there is never overwritten without being asked: a pack is a
// deliverable, and quietly replacing one somebody has already cited is the
// worst thing this command could do.
func TestAuditPackWillNotOverwriteWithoutForce(t *testing.T) {
	h := packPlatform(t)
	existing := filepath.Join(h.work, "kitchen-audit-pack-shop-2026-01-01-2026-04-01.json")
	if err := os.WriteFile(existing, []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := h.run("audit-pack", "--project", "shop",
		"--from", "2026-01-01", "--to", "2026-04-01", "--json"); code != exitConflict {
		t.Fatalf("exit %d, want %d: %s", code, exitConflict, h.stdout.String())
	}
	kept, err := os.ReadFile(existing)
	if err != nil || string(kept) != "older" {
		t.Fatalf("the file that was there must be untouched, got %q (%v)", kept, err)
	}

	if code := h.run("audit-pack", "--project", "shop", "--force",
		"--from", "2026-01-01", "--to", "2026-04-01", "--json"); code != 0 {
		t.Fatalf("exit %d: %s", code, h.stderr.String())
	}
	replaced, err := os.ReadFile(existing)
	if err != nil || string(replaced) != packDocument {
		t.Fatalf("--force must replace it, got %q (%v)", replaced, err)
	}
}

// The route refuses a member, and the CLI passes that through with the exit
// code a caller branches on rather than an unexplained failure.
func TestAuditPackPassesTheRefusalThrough(t *testing.T) {
	h := newHarness(t)
	if code := h.run("audit-pack", "--project", "shop",
		"--from", "2026-01-01", "--to", "2026-04-01", "--json"); code != exitForbidden {
		t.Fatalf("exit %d, want %d: %s", code, exitForbidden, h.stdout.String())
	}
	if !strings.Contains(h.stdout.String(), "operator") {
		t.Fatalf("the refusal must name the role it wanted, got %s", h.stdout.String())
	}
}
