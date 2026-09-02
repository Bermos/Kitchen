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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The restore account's role is enumerated, and this is what keeps the
// enumeration honest.
//
// It is narrow on purpose: the chart's own comment argues that a restore
// applies an enumerable list of kinds, unlike a self-update, and so does not
// need cluster-admin. That argument is only true while somebody maintains the
// list — and the failure mode when they do not is the worst-shaped one the
// platform has. A kind added to the archive and forgotten in the role does
// nothing at all until a restore runs, which happens on the day the cluster
// is gone, and then it fails part-way through with a Forbidden, having
// already written half the objects.
//
// That is not hypothetical: adding Addon to Kinds passed every test and every
// chart render, and failed in the kind job's restore, after the archive had
// been taken and eleven secrets restored. Two other kinds — Exception and
// AccessReview — turned out to have been missing since they were added.
//
// The check is textual because the resource names in the template are plain
// literals; there is no templating in the list it reads.
func TestRestoreRoleCoversTheArchive(t *testing.T) {
	role, err := os.ReadFile(filepath.Join("..", "..", "charts", "kitchen", "templates", "restore.yaml"))
	if err != nil {
		t.Fatalf("reading the restore role: %v", err)
	}
	rules := string(role)

	for _, kind := range Kinds {
		for _, resource := range []string{kind.Plural, kind.Plural + "/status"} {
			if !strings.Contains(rules, "- "+resource+"\n") &&
				!strings.Contains(rules, `"`+resource+`"`) {
				t.Errorf("the archive carries %s but the restore role does not grant %q: a restore would "+
					"fail part-way through with a Forbidden. Add it to charts/kitchen/templates/restore.yaml",
					kind.Kind, resource)
			}
		}
	}
}
