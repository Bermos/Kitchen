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

// The scheduled backup's account is enumerated too, and this keeps that
// enumeration honest — the same check as TestRestoreRoleCoversTheArchive,
// against the other end of the same archive.
//
// The failure it exists to prevent is quieter than the restore's. A kind added
// to the archive and forgotten here does not fail the run: the exporter reads
// what it is allowed to read, the archive is written, the manifest says so,
// the read-back verifies, and the platform reports a healthy backup that is
// silently missing a kind. Nobody finds out until the restore, on the day the
// cluster is gone.
//
// The check is textual because the resource names in the template are plain
// literals; there is no templating in the list it reads.
func TestBackupRoleCoversTheArchive(t *testing.T) {
	role, err := os.ReadFile(filepath.Join("..", "..", "charts", "kitchen", "templates", "backup-rbac.yaml"))
	if err != nil {
		t.Fatalf("reading the backup role: %v", err)
	}
	rules := string(role)

	for _, kind := range Kinds {
		if !strings.Contains(rules, "- "+kind.Plural+"\n") && !strings.Contains(rules, `"`+kind.Plural+`"`) {
			t.Errorf("the archive carries %s but the scheduled backup's role does not grant %q, so a "+
				"scheduled archive would silently carry none of them. Add it to "+
				"charts/kitchen/templates/backup-rbac.yaml", kind.Kind, kind.Plural)
		}
	}

	// The two cluster-scoped reads a run cannot do without: the singleton is
	// where spec.backup says where to write to, and every Secret in the
	// platform namespace is the half of the archive that cannot be rebuilt
	// from source.
	for _, needed := range []string{"kitchens", "secrets"} {
		if !strings.Contains(rules, `"`+needed+`"`) && !strings.Contains(rules, "- "+needed+"\n") {
			t.Errorf("a scheduled backup cannot run without reading %q", needed)
		}
	}

	// It writes nothing. The archive goes to the destination and the run's
	// result goes on the pod's termination message, so a verb that could
	// change the cluster has no business in this file.
	for _, verb := range []string{`"create"`, `"update"`, `"patch"`, `"delete"`, `"*"`} {
		if strings.Contains(rules, verb) {
			t.Errorf("the scheduled backup's role grants %s: a backup reads, and writes nothing to the "+
				"cluster it is backing up", verb)
		}
	}
}
