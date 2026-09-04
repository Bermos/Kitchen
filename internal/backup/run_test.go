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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	"github.com/Bermos/Kitchen/internal/backup/destination"
)

// A scheduled run, against a destination that is a map rather than a bucket.
//
// The whole of what these tests are for is the *order* — upload, read back,
// and only then prune. It is the one property of this feature that cannot be
// checked by looking at the result: a run that pruned first and then failed
// its upload leaves a Result that looks exactly like a run that did it in the
// right order and failed, and the difference is last week's archive.

// store is a Destination held in memory. It records the order of the calls it
// received, which is the thing under test.
type store struct {
	prefix  string
	objects map[string][]byte
	written map[string]time.Time
	calls   []string

	// Failures the test wants: each is checked before the call does anything.
	putErr    error
	getErr    error
	deleteErr error
	listErr   error

	// truncate cuts every uploaded object to this many bytes, which is how a
	// store that accepted an upload and did not keep all of it behaves.
	truncate int
}

var _ destination.Destination = (*store)(nil)

func newStore() *store {
	return &store{
		prefix:  "prod/",
		objects: map[string][]byte{},
		written: map[string]time.Time{},
	}
}

func (f *store) Put(_ context.Context, name string, size int64, body io.Reader) (destination.Object, error) {
	f.calls = append(f.calls, "put")
	if f.putErr != nil {
		return destination.Object{}, f.putErr
	}
	content, err := io.ReadAll(body)
	if err != nil {
		return destination.Object{}, err
	}
	if int64(len(content)) != size {
		return destination.Object{}, fmt.Errorf("the run declared %d bytes and sent %d", size, len(content))
	}
	if f.truncate > 0 && len(content) > f.truncate {
		content = content[:f.truncate]
	}
	key := f.prefix + name
	f.objects[key] = content
	if _, already := f.written[key]; !already {
		f.written[key] = time.Now().UTC()
	}
	return destination.Object{Key: key, Size: size}, nil
}

func (f *store) List(context.Context) ([]destination.Object, error) {
	f.calls = append(f.calls, "list")
	if f.listErr != nil {
		return nil, f.listErr
	}
	objects := make([]destination.Object, 0, len(f.objects))
	for key, content := range f.objects {
		objects = append(objects, destination.Object{
			Key: key, Size: int64(len(content)), Modified: f.written[key],
		})
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	return objects, nil
}

func (f *store) Get(_ context.Context, key string, limit int64) (io.ReadCloser, error) {
	f.calls = append(f.calls, "get")
	if f.getErr != nil {
		return nil, f.getErr
	}
	content, held := f.objects[key]
	if !held {
		return nil, fmt.Errorf("no object at %s", key)
	}
	if limit > 0 && int64(len(content)) > limit {
		content = content[:limit]
	}
	return io.NopCloser(bytes.NewReader(content)), nil
}

func (f *store) Delete(_ context.Context, key string) error {
	f.calls = append(f.calls, "delete")
	if f.deleteErr != nil {
		return f.deleteErr
	}
	delete(f.objects, key)
	delete(f.written, key)
	return nil
}

func (f *store) String() string { return "store://kitchen-backups/prod" }

// keys is what the destination holds, sorted.
func (f *store) keys() []string {
	keys := make([]string, 0, len(f.objects))
	for key := range f.objects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// seed puts an archive of an earlier day at the destination, without going
// through a run.
func (f *store) seed(name string, written time.Time) {
	key := f.prefix + name
	f.objects[key] = []byte("an older archive")
	f.written[key] = written
}

// newRun is one scheduled run against the fixture platform.
func newRun(t *testing.T, target destination.Destination, retention RetentionPolicy) *Run {
	t.Helper()
	return &Run{
		Exporter: &Exporter{
			Client:      newClient(t, platform()...),
			Namespace:   testNamespace,
			Version:     "0.9.0",
			ClusterName: "prod",
			BaseDomain:  "apps.example.com",
			Accounts:    fixtureAccounts(),
		},
		Destination: target,
		Retention:   retention,
		Scratch:     t.TempDir(),
	}
}

// The order is the design: a prune that runs before the new archive has been
// read back is a system that deletes last week's backup on the night this
// week's fails.
func TestRunUploadsVerifiesThenPrunes(t *testing.T) {
	target := newStore()
	run := newRun(t, target, RetentionPolicy{})

	result, err := run.Do(context.Background())
	if err != nil {
		t.Fatalf("the run failed: %v", err)
	}

	wanted := []string{"put", "get", "list"}
	if strings.Join(target.calls, ",") != strings.Join(wanted, ",") {
		t.Fatalf("a run is upload, read back, prune — in that order; got %v", target.calls)
	}
	if !result.Verified {
		t.Error("a run that uploaded and did not read the archive back has not verified anything")
	}
	if result.Archive == "" || result.Bytes == 0 {
		t.Errorf("the result names neither the archive nor its size: %+v", result)
	}
	if result.Objects == 0 || result.Secrets == 0 || result.AccountRows == 0 {
		t.Errorf("the result is read off the manifest that came back, and says nothing: %+v", result)
	}
	if result.Archives != 1 {
		t.Errorf("one archive was written and the destination reports %d", result.Archives)
	}
	if !IsArchiveName(filepath.Base(result.Archive)) {
		t.Errorf("a scheduled archive has to be named the way a downloaded one is, got %q", result.Archive)
	}
}

// The read-back is a real check and not a formality: an object that arrived
// truncated parses as no manifest at all, and the run has to say so.
func TestRunFailsWhenTheArchiveCannotBeReadBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target *store
		says   string
	}{
		{"the store kept only part of it", func() *store { f := newStore(); f.truncate = 32; return f }(),
			"is not the archive that was just written"},
		{"the store would not hand it back", func() *store {
			f := newStore()
			f.getErr = errors.New("403 Forbidden")
			return f
		}(), "could not be read back"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newRun(t, tc.target, RetentionPolicy{KeepLast: ptr.To(int32(1))})
			result, err := run.Do(context.Background())
			if err == nil {
				t.Fatal("an archive nothing has read back is an untested restore, and is a failed run")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the failure should name what went wrong, got %v", err)
			}
			if result.Verified {
				t.Error("the result claims a verification that did not happen")
			}
			for _, call := range tc.target.calls {
				if call == "delete" || call == "list" {
					t.Fatalf("nothing is pruned before an archive verifies; calls were %v", tc.target.calls)
				}
			}
		})
	}
}

// A failed upload prunes nothing at all. This is the night the feature exists
// to survive.
func TestRunPrunesNothingWhenTheUploadFails(t *testing.T) {
	target := newStore()
	target.seed("kitchen-backup-prod-2026-08-01T030000Z.tar.gz", time.Now().Add(-30*24*time.Hour))
	target.putErr = errors.New("the bucket refused the credential")

	run := newRun(t, target, RetentionPolicy{KeepLast: ptr.To(int32(1))})
	if _, err := run.Do(context.Background()); err == nil {
		t.Fatal("an upload that failed is a failed run")
	}
	if len(target.keys()) != 1 {
		t.Fatalf("last month's archive was deleted on the night this month's failed: %v", target.keys())
	}
}

// Retention keeps the newest, always keeps what this run just wrote, and
// touches nothing it did not write itself.
func TestRunPrunesByRetention(t *testing.T) {
	target := newStore()
	for day, name := range map[int]string{
		30: "kitchen-backup-prod-2026-08-01T030000Z.tar.gz",
		20: "kitchen-backup-prod-2026-08-11T030000Z.tar.gz",
		10: "kitchen-backup-prod-2026-08-21T030000Z.tar.gz",
	} {
		target.seed(name, time.Now().Add(-time.Duration(day)*24*time.Hour))
	}
	// A bucket is somebody's bucket. Neither of these is this platform's, and
	// neither may be deleted by a retention this platform applies.
	target.seed("README.txt", time.Now().Add(-90*24*time.Hour))
	target.objects[target.prefix+"an-operators-own-copy.tar.gz"] = []byte("theirs")

	run := newRun(t, target, RetentionPolicy{KeepLast: ptr.To(int32(2))})
	result, err := run.Do(context.Background())
	if err != nil {
		t.Fatalf("the run failed: %v", err)
	}

	if result.Pruned != 2 {
		t.Errorf("keepLast 2 over four archives prunes two, pruned %d", result.Pruned)
	}
	if result.Archives != 2 {
		t.Errorf("two archives should be left, the destination reports %d", result.Archives)
	}
	if _, held := target.objects[result.Archive]; !held {
		t.Error("the archive this run just verified was pruned by its own retention")
	}
	for _, key := range []string{target.prefix + "README.txt", target.prefix + "an-operators-own-copy.tar.gz"} {
		if _, held := target.objects[key]; !held {
			t.Errorf("retention deleted %s, which this platform did not write", key)
		}
	}
}

// The other bound: age. Both apply where both are set.
func TestRunPrunesByAge(t *testing.T) {
	target := newStore()
	target.seed("kitchen-backup-prod-2026-06-01T030000Z.tar.gz", time.Now().Add(-95*24*time.Hour))
	target.seed("kitchen-backup-prod-2026-08-21T030000Z.tar.gz", time.Now().Add(-10*24*time.Hour))

	run := newRun(t, target, RetentionPolicy{KeepDays: ptr.To(int32(90))})
	result, err := run.Do(context.Background())
	if err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	if result.Pruned != 1 {
		t.Errorf("one archive is past ninety days, pruned %d", result.Pruned)
	}
	for _, key := range target.keys() {
		if strings.Contains(key, "2026-06-01") {
			t.Error("the archive from June survived a ninety-day retention")
		}
	}
}

// An empty policy keeps everything, and that is the safe default: an archive
// costs pennies, and the failure this feature exists to prevent is having too
// few of them.
func TestRunWithNoRetentionKeepsEverything(t *testing.T) {
	target := newStore()
	target.seed("kitchen-backup-prod-2020-01-01T030000Z.tar.gz", time.Now().Add(-2000*24*time.Hour))

	run := newRun(t, target, RetentionPolicy{})
	result, err := run.Do(context.Background())
	if err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	if result.Pruned != 0 || result.Archives != 2 {
		t.Errorf("nothing is pruned without a retention; pruned %d, %d left", result.Pruned, result.Archives)
	}
	for _, call := range target.calls {
		if call == "delete" {
			t.Fatal("a run with no retention deleted something")
		}
	}
}

// One object nobody could remove must not turn a verified archive into a
// failed backup. The archive is safely at the destination either way.
func TestRunReportsAPruneItCouldNotFinish(t *testing.T) {
	target := newStore()
	target.seed("kitchen-backup-prod-2026-06-01T030000Z.tar.gz", time.Now().Add(-95*24*time.Hour))
	target.deleteErr = errors.New("403 Forbidden")

	run := newRun(t, target, RetentionPolicy{KeepDays: ptr.To(int32(30))})
	result, err := run.Do(context.Background())
	if err == nil {
		t.Fatal("a prune that could not run is reported")
	}
	if !result.Verified || result.Archive == "" {
		t.Errorf("the archive was uploaded and read back; the result should still say so: %+v", result)
	}
}

// The scratch file is not left behind. The archive is every credential the
// platform holds, and a run leaves it on a disk exactly as long as it needs it.
func TestRunLeavesNoStagedArchiveBehind(t *testing.T) {
	target := newStore()
	run := newRun(t, target, RetentionPolicy{})
	if _, err := run.Do(context.Background()); err != nil {
		t.Fatalf("the run failed: %v", err)
	}
	entries, err := os.ReadDir(run.Scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("the staged archive is still in %s: %v", run.Scratch, entries)
	}
}

// The name a run writes is the name the browser's download carries, and the
// name a prune recognises. All three are one function for that reason.
func TestArchiveNaming(t *testing.T) {
	now := time.Date(2026, 8, 19, 3, 1, 2, 0, time.UTC)
	if name := Filename("prod", "apps.example.com", now); name != "kitchen-backup-prod-2026-08-19T030102Z.tar.gz" {
		t.Errorf("the cluster name names the archive, got %q", name)
	}
	if name := Filename("", "apps.example.com", now); !strings.Contains(name, "apps.example.com") {
		t.Errorf("with no cluster name the base domain names it, got %q", name)
	}
	if name := Filename("", "", now); !strings.HasPrefix(name, "kitchen-backup-kitchen-") {
		t.Errorf("with neither, the archive is still named, got %q", name)
	}
	// A name off the Kitchen object rather than a request, but a slash in an
	// object key is a key in a place nobody meant.
	if name := Filename("prod/../etc", "", now); strings.Contains(name, "/") {
		t.Errorf("a name is sanitised into something a key can carry, got %q", name)
	}

	for _, name := range []string{
		"kitchen-backup-prod-2026-08-19T030102Z.tar.gz",
		"kitchen-backup-apps.example.com-2026-08-19T030102Z.tar.gz",
	} {
		if !IsArchiveName(name) {
			t.Errorf("%q is an archive this platform wrote, and a prune has to recognise it", name)
		}
	}
	for _, name := range []string{
		"README.txt",
		"an-operators-own-copy.tar.gz",
		"kitchen-backup-prod.tar.gz",
		"kitchen-backup-prod-2026-08-19T030102Z.tar.gz.bak",
		"backup-prod-2026-08-19T030102Z.tar.gz",
	} {
		if IsArchiveName(name) {
			t.Errorf("%q is not this platform's, and retention must never delete it", name)
		}
	}
}
