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
	"fmt"
	"regexp"
	"time"
)

// Filename names an archive after the installation and the day it was taken,
// so that an archive found on a disk months later says which platform it is
// from before anybody opens it.
//
// It lives here rather than in the API because a file taken from the browser
// and a file taken by the schedule have to be indistinguishable: they are the
// same archive, they restore identically, and the pruning rule below has to
// recognise both.
func Filename(clusterName, baseDomain string, now time.Time) string {
	name := clusterName
	if name == "" {
		name = baseDomain
	}
	if name == "" {
		name = "kitchen"
	}
	return fmt.Sprintf("kitchen-backup-%s-%s.tar.gz", sanitizeName(name), now.UTC().Format("2006-01-02T150405Z"))
}

// archiveName is Filename's own shape, and the whole of what a prune is
// allowed to consider.
//
// A destination is somebody's bucket, and it is entirely reasonable for it to
// hold other things — an operator's own copy of an archive under another name,
// a README saying what the bucket is. Retention deletes objects this platform
// wrote and nothing else, so a bucket that is shared does not lose what it
// was already keeping.
var archiveName = regexp.MustCompile(`^kitchen-backup-[A-Za-z0-9.\-]*-\d{4}-\d{2}-\d{2}T\d{6}Z\.tar\.gz$`)

// IsArchiveName reports whether a key's last segment is a name this platform
// wrote. It is the prune's filter, and it is deliberately strict.
func IsArchiveName(name string) bool {
	return archiveName.MatchString(name)
}

// sanitizeName keeps a base domain or a cluster name to what a filename can
// carry. It comes off the Kitchen object rather than from a request, but a
// header value with a quote in it is a header value with two meanings — and
// an object key with a slash in it is a key in a place nobody meant.
func sanitizeName(name string) string {
	cleaned := make([]rune, 0, len(name))
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
			cleaned = append(cleaned, r)
		default:
			cleaned = append(cleaned, '-')
		}
	}
	return string(cleaned)
}
