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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen audit-pack` — take one project's audit pack and write it to disk.
//
// The dashboard has the button, and the button is the point of the feature —
// an auditor should not need a terminal. This exists for the other half of
// the same problem: an institution that produces evidence quarterly produces
// it on a schedule, and a pack that only happens when somebody remembers to
// click is a pack somebody will one day not remember to click. `kitchen
// audit-pack --json` in a cron job writes the two files, prints the digest,
// and gives a scheduled process something to check.
//
// It writes **two** files by default, and that is the whole of why it is a
// command rather than a `kitchen api` invocation: the pack and the signature
// over it are one deliverable and are useless apart, and a caller assembling
// them by hand out of two `curl`s is a caller who will eventually ship the
// pack without the envelope. `--format html` writes the reader's version
// instead, and `--no-signature` is for a platform that holds no signing key,
// where the API answers 409 and the pack stands on its own.
//
// Both ends of the window are required, because the API requires them: a pack
// that ended "now" could not be reproduced. A bare date is accepted and read
// as midnight UTC, since that is what a quarter's boundaries actually are.

// auditPackTaken is what the command answers with: what was written, and
// enough about it to check the pack without opening it.
type auditPackTaken struct {
	Project string `json:"project"`
	// From and To are the window as the platform recorded it, read back off
	// the pack rather than echoed from the flags.
	From string `json:"from"`
	To   string `json:"to"`

	// File is the pack, and Signature the DSSE envelope over it — empty when
	// the platform holds no key, or when --no-signature said not to ask.
	File      string `json:"file"`
	Signature string `json:"signature,omitempty"`
	Bytes     int64  `json:"bytes"`

	// Digest is the sha256 of the bytes on disk, computed here rather than
	// taken from the header: the point of the number is that it describes the
	// file, and a number the server sent describes what the server had.
	Digest string `json:"digest"`
	// ServedDigest is what the platform said it was serving. Equal to Digest
	// unless something between the two changed the bytes, which is a finding
	// rather than a detail.
	ServedDigest string `json:"servedDigest"`

	// Signed is whether an envelope exists, and Message why it does not.
	Signed  bool   `json:"signed"`
	Message string `json:"message,omitempty"`

	// Truncated says the pack answers for less than it was asked for —
	// retention removed part of the window, or a section hit its cap.
	Truncated bool `json:"truncated"`
	// Coverage is the pack's own account of that, verbatim.
	Coverage string `json:"coverage,omitempty"`

	// Sections is what is in it, by count, so a scheduled job can tell an
	// empty quarter from a failed export.
	Sections map[string]int `json:"sections"`
}

// packSummary is the slice of the pack this command reads back off the file.
// It is deliberately narrow: the command's job is to write the document, not
// to interpret it, and a struct that mirrored the whole pack would be a second
// copy of the API's shape to keep in step.
type packSummary struct {
	Project string `json:"project"`
	Range   struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"range"`
	Verification struct {
		Signed  bool   `json:"signed"`
		Message string `json:"message"`
	} `json:"verification"`
	Retention struct {
		Truncated bool   `json:"truncated"`
		Message   string `json:"message"`
	} `json:"retention"`
	Inventory struct {
		Environments []struct{} `json:"environments"`
		Releases     []struct{} `json:"releases"`
		Claims       []struct{} `json:"claims"`
	} `json:"inventory"`
	ChangeLog  []struct{} `json:"changeLog"`
	Promotions []struct{} `json:"promotions"`
	Decisions  struct {
		Items     []struct{} `json:"items"`
		Truncated bool       `json:"truncated"`
	} `json:"decisions"`
	Attestations []struct{} `json:"attestations"`
	Exceptions   []struct{} `json:"exceptions"`
	AuditLog     struct {
		Items     []struct{} `json:"items"`
		Truncated bool       `json:"truncated"`
	} `json:"auditLog"`
	SignedRecords struct {
		Items []struct{} `json:"items"`
	} `json:"signedRecords"`
}

// describe fills in what the command reports from what the document says.
// The counts are the file's own, so an empty quarter and a failed export are
// two different answers rather than the same one.
func (p packSummary) describe(taken *auditPackTaken) {
	taken.From, taken.To = p.Range.From, p.Range.To
	taken.Signed = p.Verification.Signed
	taken.Truncated = p.Retention.Truncated || p.Decisions.Truncated || p.AuditLog.Truncated
	taken.Coverage = p.Retention.Message
	taken.Sections = map[string]int{
		"environments":  len(p.Inventory.Environments),
		"releases":      len(p.Inventory.Releases),
		"claims":        len(p.Inventory.Claims),
		"changes":       len(p.ChangeLog),
		"promotions":    len(p.Promotions),
		"decisions":     len(p.Decisions.Items),
		"attestations":  len(p.Attestations),
		"exceptions":    len(p.Exceptions),
		"auditRecords":  len(p.AuditLog.Items),
		"signedRecords": len(p.SignedRecords.Items),
	}
	if !taken.Signed && taken.Message == "" {
		taken.Message = p.Verification.Message
	}
}

func newAuditPackCommand(r *Runtime) *cobra.Command {
	var (
		from        string
		to          string
		format      string
		directory   string
		noSignature bool
		force       bool
	)

	cmd := &cobra.Command{
		Use:   "audit-pack",
		Short: "Export one project's audit pack for a window",
		Long: strings.TrimSpace(`
One project's whole compliance answer for one window, as files on disk.

What is in it: the inventory of environments, releases, resource claims,
connections and domains; the change log with the author and the approvers of
every release; the promotions and the policy decisions behind them, with the
full inputs they can be replayed from; the evidence attached to each artifact;
the break-glass exceptions; the recertification cycles that reviewed this
project's access; what is running that no longer meets its bar; the project's
slice of the tamper-evident audit log; and every signed statement the platform
holds that has no registry to live in, carried byte for byte.

Two files by default: the pack, and the DSSE envelope that signs it. They are
one deliverable — the envelope's subject is the pack's sha256 — and the pack's
own verification block carries the four commands that check them with this
platform switched off.

Both ends of the window are required. A pack that ended "now" could not be
reproduced, and reproducibility is the point: two exports of the same window
are the same bytes unless the evidence changed. A bare date is read as
midnight UTC.

--format html writes the reader's version instead of the machine-readable one:
one self-contained page, printable, unsigned, carrying the pack's digest so a
printout ties back to the bytes.

Taking a pack is recorded in the platform's audit log, under the kind
EvidenceExport.`),
		Args: cobra.NoArgs,
		RunE: run(func(cmd *cobra.Command, _ []string) error {
			return takeAuditPack(commandContext(cmd), r, packRequest{
				from:        from,
				to:          to,
				format:      format,
				directory:   directory,
				noSignature: noSignature,
				force:       force,
			})
		}),
	}

	cmd.Flags().StringVar(&from, "from", "",
		"the start of the window, inclusive: a date (2026-01-01) or an RFC 3339 timestamp")
	cmd.Flags().StringVar(&to, "to", "",
		"the end of the window, exclusive: a date or an RFC 3339 timestamp")
	cmd.Flags().StringVar(&format, "format", packFormatJSON,
		"json for the machine-readable pack, html for the reader's rendering")
	cmd.Flags().StringVarP(&directory, "output", "o", "",
		"where to write. The default is the current directory; the files are named after "+
			"the project and the window")
	cmd.Flags().BoolVar(&noSignature, "no-signature", false,
		"write the pack alone. Only for a platform that holds no signing key — everywhere else "+
			"the envelope is half the deliverable")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite files that are already there")

	return describe(cmd, meta{
		Calls: []string{"GET /api/v1/projects/{name}/audit-pack"},
		Output: output{
			Mode: outputDocument,
			Kind: "auditPackTaken",
			Note: "the documents are on disk at `file` and `signature`; `digest` is computed " +
				"from the bytes that were written, not taken from the platform's header",
		},
		Needs: needs{Auth: true, Project: true},
		Examples: []example{
			{"A quarter of one project, into the current directory",
				"kitchen audit-pack --project shop --from 2026-01-01 --to 2026-04-01 --json"},
			{"The reader's version of the same window",
				"kitchen audit-pack --project shop --from 2026-01-01 --to 2026-04-01 --format html"},
			{"Fail a scheduled job when the window is not fully covered",
				"kitchen audit-pack --project shop --from 2026-01-01 --to 2026-04-01 --json | " +
					"jq -e '.truncated | not'"},
		},
	})
}

// packRequest is what the flags said.
type packRequest struct {
	from        string
	to          string
	format      string
	directory   string
	noSignature bool
	force       bool
}

// The two renderings this command writes. `dsse` is not among them because it
// is not a rendering somebody asks for: it is the signature over the pack, and
// it is fetched with the pack rather than instead of it.
const (
	packFormatJSON = "json"
	packFormatHTML = "html"
	packFormatDSSE = "dsse"
)

// asked is the request with its flags checked: the project resolved and the
// window read into instants. It is a step of its own because a command whose
// validation and whose work are one function is a command nobody can read.
type packAsk struct {
	project   string
	format    string
	from      time.Time
	to        time.Time
	directory string
}

func checkPackRequest(r *Runtime, ask packRequest) (packAsk, error) {
	project, err := r.projectName()
	if err != nil {
		return packAsk{}, err
	}
	format := strings.ToLower(strings.TrimSpace(ask.format))
	if format != packFormatJSON && format != packFormatHTML {
		return packAsk{}, failf(codeUsage, "--format is json or html (got %q)", ask.format).
			withHint("the signature is written beside a json pack; there is nothing to sign an " +
				"HTML rendering with, because a rendering is not the document")
	}
	from, err := packInstant("from", ask.from)
	if err != nil {
		return packAsk{}, err
	}
	to, err := packInstant("to", ask.to)
	if err != nil {
		return packAsk{}, err
	}
	if !to.After(from) {
		return packAsk{}, failf(codeUsage, "--to must be after --from (%s is not after %s)",
			to.Format(time.RFC3339), from.Format(time.RFC3339))
	}

	directory := ask.directory
	if directory == "" {
		directory = r.WorkingDir
	}
	if directory == "" {
		directory = "."
	}
	return packAsk{
		project:   project,
		format:    format,
		from:      from,
		to:        to,
		directory: absolute(r, directory),
	}, nil
}

func takeAuditPack(parent context.Context, r *Runtime, ask packRequest) error {
	checked, err := checkPackRequest(r, ask)
	if err != nil {
		return err
	}

	client, err := r.client()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	query := url.Values{}
	query.Set("from", checked.from.Format(time.RFC3339))
	query.Set("to", checked.to.Format(time.RFC3339))
	if checked.format == packFormatHTML {
		query.Set("format", packFormatHTML)
	}

	path := "/projects/" + url.PathEscape(checked.project) + "/audit-pack"
	packFile, served, err := downloadPack(ctx, client,
		"exporting an audit pack", path, query, checked.directory, ask.force)
	if err != nil {
		return err
	}

	taken := auditPackTaken{
		Project:      checked.project,
		From:         checked.from.Format(time.RFC3339),
		To:           checked.to.Format(time.RFC3339),
		File:         packFile,
		ServedDigest: served,
		Sections:     map[string]int{},
	}
	if info, err := os.Stat(packFile); err == nil {
		taken.Bytes = info.Size()
	}
	taken.Digest, err = digestOfFile(packFile)
	if err != nil {
		return err
	}
	if served != "" && served != taken.Digest {
		// Not fatal, and loud: the file is on disk and can be inspected, but
		// something between the platform and this process changed the bytes,
		// which is precisely the thing the digest exists to notice.
		taken.Message = fmt.Sprintf(
			"the platform served %s and what was written hashes to %s — the pack on disk is not "+
				"the document the platform signed", served, taken.Digest)
	}

	if checked.format == packFormatJSON {
		summary, err := readPackSummary(packFile)
		if err != nil {
			return err
		}
		summary.describe(&taken)

		if !ask.noSignature && taken.Signed {
			signature := url.Values{}
			for key, values := range query {
				signature[key] = values
			}
			signature.Set("format", packFormatDSSE)
			envelope, _, err := downloadPack(ctx, client,
				"signing an audit pack", path, signature, checked.directory, ask.force)
			if err != nil {
				return err
			}
			taken.Signature = envelope
		}
	}

	return r.printer().document(taken, func(s tui.Styles) string {
		lines := []string{fmt.Sprintf("%s %s\n", s.OK.Render("Wrote"), s.Title.Render(taken.File))}
		if taken.Signature != "" {
			lines = append(lines, fmt.Sprintf("       %s\n", s.Title.Render(taken.Signature)))
		}
		lines = append(lines, "  "+s.Subtle.Render(taken.Digest)+"\n")
		lines = append(lines, fmt.Sprintf("  %s → %s\n", taken.From, taken.To))
		if len(taken.Sections) > 0 {
			lines = append(lines, fmt.Sprintf(
				"  %d changes, %d promotions, %d decisions, %d exceptions, %d audit records\n",
				taken.Sections["changes"], taken.Sections["promotions"], taken.Sections["decisions"],
				taken.Sections["exceptions"], taken.Sections["auditRecords"]))
		}
		if !taken.Signed && taken.Message != "" {
			lines = append(lines, "  "+s.Warn.Render("unsigned: "+taken.Message)+"\n")
		}
		if taken.Truncated {
			note := taken.Coverage
			if note == "" {
				note = "part of this window is no longer in the platform's store"
			}
			lines = append(lines, "  "+s.Warn.Render(note)+"\n")
		}
		return strings.Join(lines, "")
	})
}

// packInstant reads one end of the window. A bare date is midnight UTC, which
// is what a quarter's boundaries actually are; anything else has to be a full
// RFC 3339 timestamp, because "the 1st" in an unnamed zone is two different
// instants and an export must not have to guess which.
func packInstant(flag, value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, fail(codeUsage,
			"an audit pack needs both ends of its window: --from and --to").
			withHint("a pack that ended \"now\" could not be reproduced, so the platform " +
				"refuses one. Dates are accepted: --from 2026-01-01 --to 2026-04-01")
	}
	if day, err := time.Parse("2006-01-02", value); err == nil {
		return day.UTC(), nil
	}
	instant, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, failf(codeUsage,
			"--%s must be a date (2026-01-01) or an RFC 3339 timestamp (got %q)", flag, value)
	}
	return instant.UTC(), nil
}

// downloadPack fetches one rendering into the directory, under the name the
// platform suggests, and answers where it went and what digest the platform
// said it was serving.
//
// A temporary file first, renamed once the body is whole, for the reason the
// backup does it: a document that failed part-way must not be left sitting
// under the name somebody will later hand to an examiner.
func downloadPack(
	ctx context.Context,
	client *client,
	doing, path string,
	query url.Values,
	directory string,
	force bool,
) (string, string, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return "", "", failf(codeFailed, "cannot write into %s: %v", directory, err)
	}
	partial, err := os.CreateTemp(directory, ".kitchen-audit-pack-*.partial")
	if err != nil {
		return "", "", failf(codeFailed, "cannot write into %s: %v", directory, err)
	}
	defer func() {
		_ = partial.Close()
		_ = os.Remove(partial.Name())
	}()

	suggested, _, headers, err := client.downloadWithHeaders(ctx, doing, "GET", path, query, partial)
	if err != nil {
		return "", "", err
	}
	if err := partial.Close(); err != nil {
		return "", "", failf(codeFailed, "the document was not written completely: %v", err)
	}

	name := suggested
	if name == "" {
		name = "kitchen-audit-pack.json"
	}
	target := filepath.Join(directory, name)
	if !force {
		if _, err := os.Stat(target); err == nil {
			return "", "", failf(codeConflict, "%s is already there", target).
				withHint("--force overwrites it, or --output names another directory")
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", "", failf(codeFailed, "cannot write %s: %v", target, err)
		}
	}
	if err := os.Rename(partial.Name(), target); err != nil {
		return "", "", failf(codeFailed, "cannot move the document into place: %v", err)
	}
	return target, headers.Get("X-Kitchen-Pack-Digest"), nil
}

// digestOfFile hashes what is on disk. The pack's identity is the sha256 of
// its bytes, and the bytes that matter are the ones that were written.
func digestOfFile(path string) (string, error) {
	content, err := os.ReadFile(path) //nolint:gosec // the path is one this command just wrote
	if err != nil {
		return "", failf(codeFailed, "cannot read %s back: %v", path, err)
	}
	sum := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// readPackSummary reads back the few fields the command reports. It is read
// off the file rather than remembered, which is also the cheapest proof that
// what was written is a whole document rather than a truncated one.
func readPackSummary(path string) (packSummary, error) {
	content, err := os.ReadFile(path) //nolint:gosec // the path is one this command just wrote
	if err != nil {
		return packSummary{}, failf(codeFailed, "cannot read %s back: %v", path, err)
	}
	summary := packSummary{}
	if err := json.Unmarshal(content, &summary); err != nil {
		return packSummary{}, failf(codeFailed,
			"the platform answered with something that is not an audit pack: %v", err)
	}
	return summary, nil
}
