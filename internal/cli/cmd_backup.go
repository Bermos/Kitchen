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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Bermos/Kitchen/internal/backup"
	"github.com/Bermos/Kitchen/internal/cli/tui"
)

// `kitchen backup` — take the platform's backup and write it to a file.
//
// This is the command issue #74 asked for by name, and the reason it is worth
// having on top of the dashboard's button is scheduling: a backup that only
// happens when somebody remembers to click is not a backup. With `--json` it
// answers one object naming the file it wrote and what is in it, which is
// enough for a cron job to check that the archive it just took carries the
// accounts as well as the objects.
//
// It carries the POST alone. Reading what an archive *would* hold, without
// taking one, is `kitchen api GET /platform/backup` — the fallback CLAUDE.md
// keeps for exactly this: a route reachable from a terminal on the day it
// lands, without a command for every one.
//
// There is no `kitchen restore`, and there cannot be. A restore happens into a
// cluster whose accounts database is gone, so the credentials this command
// authenticates with are inside the archive and there is nobody left to run
// it. The chart renders a Job instead; docs/BACKUP.md is the procedure.

// backupTaken is what `kitchen backup` answers with: where the archive went,
// and the manifest it carries — read back out of the file rather than
// reported from memory, so the summary describes what is actually on disk.
type backupTaken struct {
	// File the archive was written to, and how big it is.
	File  string `json:"file"`
	Bytes int64  `json:"bytes"`

	// PlatformVersion is the release that wrote it, and so the only release it
	// restores into without --force.
	PlatformVersion string `json:"platformVersion"`
	ClusterName     string `json:"clusterName,omitempty"`
	BaseDomain      string `json:"baseDomain,omitempty"`
	CreatedAt       string `json:"createdAt"`

	// Objects is every custom resource in the archive and Secrets the
	// credentials beside them.
	Objects int `json:"objects"`
	Secrets int `json:"secrets"`

	// AccountRows is the identity provider's half — the accounts, sessions and
	// OAuth clients no sweep of custom resources could recover. Zero with a
	// message is an archive that carries none, and the message says whether
	// that is because there are none or because the database could not be
	// reached.
	AccountRows     int64  `json:"accountRows"`
	AccountsMessage string `json:"accountsMessage,omitempty"`

	// Excluded is what the archive deliberately does not carry, in the
	// platform's own words rather than this command's.
	Excluded []string `json:"excluded"`
}

func newBackupCommand(r *Runtime) *cobra.Command {
	var (
		// Not `output`: that is the name of the type a command's answer shape
		// is declared with, three lines further down.
		path  string
		force bool
	)

	cmd := &cobra.Command{
		Use:   "backup [FILE]",
		Short: "Take a backup of the platform and write it to a file",
		Long: strings.TrimSpace(`
Export the platform's state as one archive: every Kitchen object, every secret
in the platform namespace, and the identity provider's database.

Telemetry is not in it. Logs, metrics, traces and the audit log live in
ClickHouse, which is not backed up and is not expected to survive — the archive
says so in its own manifest, and this command prints it.

The archive is a credential. It holds every secret the platform has, in the
clear, so keep it where you would keep the cluster's root credentials and keep
it off the cluster it came from. Taking one is recorded in the platform's audit
log.

With no file named it is written into the current directory, under the name the
platform suggests — which carries the installation and the day. An existing
file is never overwritten without --force.

Restoring is not a command: it happens into a cluster whose accounts database
is gone, so the credential this command signs in with is inside the archive and
there is nobody left to run it. The chart renders a Job for it instead; see
docs/BACKUP.md.`),
		Args: cobra.MaximumNArgs(1),
		RunE: run(func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if path != "" && path != args[0] {
					return fail(codeUsage, "the file is named twice, differently").
						withHint("pass it as an argument or as --output, not both")
				}
				path = args[0]
			}
			return takeBackup(commandContext(cmd), r, path, force)
		}),
	}

	cmd.Flags().StringVarP(&path, "output", "o", "",
		"where to write the archive. The default is the name the platform suggests, "+
			"in the current directory")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite the file if it is already there")

	return describe(cmd, meta{
		Calls: []string{"POST /api/v1/platform/backup"},
		Output: output{
			Mode: outputDocument,
			Kind: "backupTaken",
			Note: "the archive is on disk at `file`; the counts are read back out of it, not remembered",
		},
		Needs: needs{Auth: true},
		Examples: []example{
			{"Take a backup into the current directory", "kitchen backup --json"},
			{"Take one to a named file, overwriting it", "kitchen backup /backups/kitchen.tar.gz --force --json"},
		},
	})
}

func takeBackup(parent context.Context, r *Runtime, path string, force bool) error {
	client, err := r.client()
	if err != nil {
		return err
	}
	ctx, cancel := r.context(parent)
	defer cancel()

	// A temporary file first, renamed once the archive is whole. A backup that
	// failed part-way must not be left sitting at the name a restore would
	// reach for — a truncated archive is refused on the way back in, but only
	// by somebody who tries, and that is the wrong moment to find out.
	directory := r.WorkingDir
	if path != "" {
		directory = filepath.Dir(absolute(r, path))
	}
	if directory == "" {
		directory = "."
	}
	partial, err := os.CreateTemp(directory, ".kitchen-backup-*.partial")
	if err != nil {
		return failf(codeFailed, "cannot write into %s: %v", directory, err)
	}
	// 0600 from the start: os.CreateTemp already does that, and it matters
	// more here than the comment costs.
	defer func() {
		_ = partial.Close()
		_ = os.Remove(partial.Name())
	}()

	suggested, written, err := client.download(ctx,
		"taking a backup", "POST", "/platform/backup", nil, partial)
	if err != nil {
		return err
	}
	if err := partial.Close(); err != nil {
		return failf(codeFailed, "the archive was not written completely: %v", err)
	}

	target := absolute(r, path)
	if path == "" {
		name := suggested
		if name == "" {
			// The platform names the download after the installation and the
			// day. A platform that did not is not a reason to refuse to write
			// the archive that is already in hand.
			name = "kitchen-backup.tar.gz"
		}
		target = filepath.Join(directory, name)
	}
	if !force {
		if _, err := os.Stat(target); err == nil {
			return failf(codeConflict, "%s is already there", target).
				withHint("--force overwrites it, or name another file")
		} else if !errors.Is(err, os.ErrNotExist) {
			return failf(codeFailed, "cannot write %s: %v", target, err)
		}
	}

	// The manifest is read back off the file rather than remembered, so what is
	// reported is what is actually on disk — and reading it is also the
	// cheapest proof that the archive is not truncated.
	file, err := os.Open(partial.Name())
	if err != nil {
		return failf(codeFailed, "cannot read the archive back: %v", err)
	}
	manifest, err := backup.ReadManifest(file)
	_ = file.Close()
	if err != nil {
		return failf(codeFailed, "the platform answered with something that is not a backup archive: %v", err)
	}

	if err := os.Rename(partial.Name(), target); err != nil {
		return failf(codeFailed, "cannot move the archive into place: %v", err)
	}

	taken := backupTaken{
		File:            target,
		Bytes:           written,
		PlatformVersion: manifest.PlatformVersion,
		ClusterName:     manifest.ClusterName,
		BaseDomain:      manifest.BaseDomain,
		CreatedAt:       manifest.CreatedAt.UTC().Format(time.RFC3339),
		Secrets:         manifest.Secrets,
		AccountsMessage: manifest.AccountsMessage,
		Excluded:        manifest.Excluded,
	}
	for _, count := range manifest.Resources {
		taken.Objects += count
	}
	if manifest.Accounts != nil {
		taken.AccountRows = manifest.Accounts.Rows
	}

	return r.printer().document(taken, func(s tui.Styles) string {
		lines := []string{fmt.Sprintf("%s %s\n", s.OK.Render("Wrote"), s.Title.Render(taken.File))}
		lines = append(lines, fmt.Sprintf("  %d objects, %d secrets, %d account rows\n",
			taken.Objects, taken.Secrets, taken.AccountRows))
		if taken.AccountsMessage != "" {
			lines = append(lines, "  "+s.Warn.Render("no accounts: "+taken.AccountsMessage)+"\n")
		}
		lines = append(lines, "  "+s.Subtle.Render(
			"every secret this platform has, in the clear — keep it off the cluster it came from")+"\n")
		return strings.Join(lines, "")
	})
}

// absolute resolves a path the caller gave against the runtime's working
// directory, so that a relative --output lands where the person running the
// command is rather than where the process happens to be.
func absolute(r *Runtime, path string) string {
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	if r.WorkingDir == "" {
		return path
	}
	return filepath.Join(r.WorkingDir, path)
}
