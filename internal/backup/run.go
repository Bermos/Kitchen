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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Bermos/Kitchen/internal/backup/destination"
)

// A scheduled run: export, upload, verify, prune — in that order, and the
// order is the design.
//
// Pruning last is the whole of it. A prune that runs before the new archive
// has been read back is a system that deletes last week's backup because this
// week's failed, which is the one way a backup system can be worse than not
// having one. Verifying by reading the object back is the same claim one level
// down from docs/BACKUP.md's first line: an untested restore is worth exactly
// nothing, and a scheduled backup nothing has ever read back is untested.

// RetentionPolicy is how much of the destination survives a prune. Both
// bounds apply where both are set.
type RetentionPolicy struct {
	// KeepLast is how many of the newest archives survive.
	KeepLast *int32
	// KeepDays deletes archives older than this many days.
	KeepDays *int32
}

// Any is whether this policy prunes anything at all. An empty policy keeps
// every archive forever, which is the safe default: an archive costs pennies,
// and the failure this feature exists to prevent is having too few of them.
func (p RetentionPolicy) Any() bool {
	return p.KeepLast != nil || p.KeepDays != nil
}

// Run is one scheduled backup.
type Run struct {
	// Exporter writes the archive. It is the same exporter the dashboard's
	// button uses, which is what makes a scheduled archive and a manual one
	// the same file.
	Exporter *Exporter

	// Destination is where the archive goes.
	Destination destination.Destination

	// Retention prunes the destination afterwards, and only afterwards.
	Retention RetentionPolicy

	// Scratch is the directory the archive is staged in. It has to be a real
	// directory rather than memory: the archive is uploaded with its exact
	// length, and it is read back off disk to be verified.
	Scratch string

	// Now is the clock, injectable for tests.
	Now func() time.Time
}

// Result is what one run did, and it is written to the pod's termination
// message as JSON. That is not a new idea here — digestFromTerminationMessage
// already reads a build's result out of a pod's termination message, in two
// formats, for exactly this reason: a Job's outcome has to be readable by the
// operator without keeping the pod's logs.
//
// It is kept small on purpose. A termination message is truncated at 4 KiB,
// and a result that is cut in half is a result nothing can parse.
type Result struct {
	// Archive is the key at the destination, and Bytes its size.
	Archive string `json:"archive,omitempty"`
	Bytes   int64  `json:"bytes,omitempty"`

	// Destination described, never its credential.
	Destination string `json:"destination,omitempty"`

	// StartedAt and FinishedAt bound the run.
	StartedAt  time.Time `json:"startedAt"`
	FinishedAt time.Time `json:"finishedAt"`

	// Verified is whether the archive was read back off the destination and
	// parsed as a manifest for this installation. A run that uploaded and
	// could not verify is a failure, not a warning.
	Verified bool `json:"verified"`

	// Objects, Secrets and AccountRows are what went into the archive, read
	// off the manifest that came back rather than remembered.
	Objects     int   `json:"objects"`
	Secrets     int   `json:"secrets"`
	AccountRows int64 `json:"accountRows"`

	// AccountsMessage explains an archive with no accounts in it — the
	// difference between an installation that has none and one whose
	// database could not be reached, which is not a difference to discover
	// during a restore.
	AccountsMessage string `json:"accountsMessage,omitempty"`

	// Archives is how many archives the destination holds after the prune,
	// and Pruned how many this run removed.
	Archives int32 `json:"archives"`
	Pruned   int32 `json:"pruned"`

	// Error is why the run failed, where it did.
	Error string `json:"error,omitempty"`
}

// Do takes the backup. The Result is filled in as far as the run got, so a
// failure still says what happened and when.
func (r *Run) Do(ctx context.Context) (Result, error) {
	now := time.Now
	if r.Now != nil {
		now = r.Now
	}
	started := now().UTC()
	result := Result{StartedAt: started, Destination: r.Destination.String()}
	finish := func(err error) (Result, error) {
		result.FinishedAt = now().UTC()
		if err != nil {
			result.Error = err.Error()
		}
		return result, err
	}

	name := Filename(r.Exporter.ClusterName, r.Exporter.BaseDomain, started)

	// 1. Export, onto disk. The upload needs the exact length — a store that
	// has to buffer an unknown one buffers the whole platform's archive in
	// memory — and the verification reads the object back rather than this
	// file, so staging it costs nothing that is not needed anyway.
	staged, err := os.CreateTemp(r.Scratch, ".kitchen-backup-*.partial")
	if err != nil {
		return finish(fmt.Errorf("cannot stage the archive in %s: %w", r.scratch(), err))
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	manifest, err := r.Exporter.WriteTo(ctx, staged)
	if err != nil {
		return finish(fmt.Errorf("the export failed: %w", err))
	}
	if err := staged.Sync(); err != nil {
		return finish(fmt.Errorf("the archive was not written completely: %w", err))
	}
	info, err := staged.Stat()
	if err != nil {
		return finish(fmt.Errorf("the staged archive could not be measured: %w", err))
	}
	if _, err := staged.Seek(0, 0); err != nil {
		return finish(fmt.Errorf("the staged archive could not be re-read: %w", err))
	}
	result.Secrets = manifest.Secrets
	result.AccountsMessage = manifest.AccountsMessage
	for _, count := range manifest.Resources {
		result.Objects += count
	}
	if manifest.Accounts != nil {
		result.AccountRows = manifest.Accounts.Rows
	}

	// 2. Upload.
	object, err := r.Destination.Put(ctx, name, info.Size(), staged)
	if err != nil {
		return finish(err)
	}
	result.Archive = object.Key
	result.Bytes = info.Size()

	// 3. Verify, by reading it back. The manifest is the first entry in the
	// tar, so one ranged request proves the object at the far end is this
	// archive rather than a truncated upload or somebody else's file.
	if err := r.verify(ctx, object.Key, manifest); err != nil {
		return finish(err)
	}
	result.Verified = true

	// 4. Only now, prune.
	kept, pruned, err := r.prune(ctx, now().UTC(), object.Key)
	result.Archives = int32(kept)
	result.Pruned = int32(pruned)
	if err != nil {
		return finish(err)
	}
	return finish(nil)
}

// scratch is the staging directory as a message should name it.
func (r *Run) scratch() string {
	if r.Scratch == "" {
		return os.TempDir()
	}
	return r.Scratch
}

// verify reads the head of the uploaded object back and checks it is the
// archive this run just wrote.
func (r *Run) verify(ctx context.Context, key string, wrote Manifest) error {
	body, err := r.Destination.Get(ctx, key, destination.VerifyBytes)
	if err != nil {
		return fmt.Errorf("the archive was uploaded and could not be read back: %w", err)
	}
	defer func() { _ = body.Close() }()

	read, err := ReadManifest(body)
	if err != nil {
		return fmt.Errorf("the object at %s is not the archive that was just written: %w", key, err)
	}
	if read.PlatformVersion != wrote.PlatformVersion || !read.CreatedAt.Equal(wrote.CreatedAt) {
		return fmt.Errorf(
			"the object at %s carries a manifest for %s taken at %s, and this run wrote %s taken at %s",
			key, read.PlatformVersion, read.CreatedAt.UTC().Format(time.RFC3339),
			wrote.PlatformVersion, wrote.CreatedAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// prune removes archives past the retention, and answers how many are left
// and how many went.
//
// Only objects named the way this platform names an archive are considered.
// A destination is somebody's bucket and may hold other things; retention
// deletes what this platform wrote, and nothing else.
func (r *Run) prune(ctx context.Context, now time.Time, wrote string) (int, int, error) {
	objects, err := r.Destination.List(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("the archive is uploaded and the destination could not be listed to prune it: %w", err)
	}

	archives := make([]destination.Object, 0, len(objects))
	for _, object := range objects {
		if IsArchiveName(filepath.Base(object.Key)) {
			archives = append(archives, object)
		}
	}
	// Newest first. Modified is the destination's own answer where it gives
	// one; the key is the fallback, and it sorts chronologically because the
	// name ends in a sortable timestamp.
	sort.Slice(archives, func(i, j int) bool {
		if !archives[i].Modified.Equal(archives[j].Modified) {
			return archives[i].Modified.After(archives[j].Modified)
		}
		return archives[i].Key > archives[j].Key
	})

	if !r.Retention.Any() {
		return len(archives), 0, nil
	}

	pruned := 0
	kept := 0
	var failures error
	for index, archive := range archives {
		// The archive this run just wrote is never a candidate, whatever the
		// numbers say. A retention of keepLast: 1 that deleted the archive
		// that was just verified would be a schedule that reliably ends with
		// nothing at the destination.
		if archive.Key == wrote {
			kept++
			continue
		}
		expired := false
		if r.Retention.KeepLast != nil && index >= int(*r.Retention.KeepLast) {
			expired = true
		}
		if days := r.Retention.KeepDays; days != nil && !archive.Modified.IsZero() {
			if archive.Modified.Before(now.AddDate(0, 0, -int(*days))) {
				expired = true
			}
		}
		if !expired {
			kept++
			continue
		}
		if err := r.Destination.Delete(ctx, archive.Key); err != nil {
			// A failed delete is reported and does not stop the prune: the
			// archive is safely uploaded, and one object nobody could remove
			// must not turn a successful backup into a failed one.
			kept++
			if failures == nil {
				failures = err
			}
			continue
		}
		pruned++
	}
	return kept, pruned, failures
}
