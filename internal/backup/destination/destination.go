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

// Package destination is where a scheduled archive goes.
//
// It is an interface with one implementation, and that is the point rather
// than an accident: "S3, maybe others" is a shape here rather than a promise,
// so a second backend is a new value in BackupDestinationType *plus* a type
// satisfying Destination — the rule the claim providers keep, applied to the
// one thing on the platform whose absence is discovered during a recovery.
//
// Everything above it — the ordering of a run, the retention — is written
// against the interface, so a second backend inherits the whole of it.
package destination

import (
	"context"
	"io"
	"time"
)

// Object is one archive at a destination.
type Object struct {
	// Key as the destination holds it, prefix included.
	Key string
	// Size in bytes.
	Size int64
	// Modified is when the destination says it was written.
	Modified time.Time
}

// Destination is a place archives are written to, listed from, read back out
// of, and pruned.
//
// The four methods are exactly what one run needs and nothing more. Read them
// in the order a run uses them: Put uploads, Get reads the manifest back to
// prove the upload arrived, List and Delete are the prune.
type Destination interface {
	// Put writes one archive under name — the archive's filename, not a key:
	// the prefix is the destination's own business. Size is the archive's
	// exact length, because a store that has to buffer an unknown length is a
	// store that holds a platform's entire archive in memory.
	Put(ctx context.Context, name string, size int64, body io.Reader) (Object, error)

	// List answers every object under the destination's prefix, archives and
	// anything else alike. Filtering to what this platform wrote is the
	// prune's job, and it is deliberately not done here: a caller listing a
	// destination should see what is actually in it.
	List(ctx context.Context) ([]Object, error)

	// Get reads an object back, at most limit bytes of it. The limit is what
	// makes verification cheap: the manifest is the first entry in the tar,
	// so the first few tens of kilobytes are enough to prove the object at
	// the far end is the archive that was just written.
	Get(ctx context.Context, key string, limit int64) (io.ReadCloser, error)

	// Delete removes one object.
	Delete(ctx context.Context, key string) error

	// String describes the destination and never its credential. It is what
	// goes on status.backup.destination and into a log line.
	String() string
}

// VerifyBytes is how much of an uploaded archive is read back to confirm it
// arrived. The manifest is the first entry in the tar and is a few kilobytes
// of JSON before compression, so this is generous by an order of magnitude
// and still one range request.
const VerifyBytes = 64 << 10
