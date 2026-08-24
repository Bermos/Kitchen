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

package controller

import (
	"strings"
	"testing"
	"time"

	kitchenv1alpha1 "github.com/Bermos/Kitchen/api/v1alpha1"
)

// Reading a finding out of three scanners that agree about nothing.
//
// The shape on the other side is fixed by the default policy bundle:
// `max-severity` reads predicate.findings[] and wants .severity and
// .vulnerability. Every case below is about that contract, plus the one thing
// only Grype gives us — the database its findings were produced against.

const grypeReport = `{
  "matches": [
    {"vulnerability": {"id": "CVE-2026-2", "severity": "Critical", "fix": {"versions": ["1.2.4"]}},
     "artifact": {"name": "libssl", "version": "1.2.3"}},
    {"vulnerability": {"id": "CVE-2026-1", "severity": "Low"},
     "artifact": {"name": "zlib", "version": "1.0.0"}}
  ],
  "descriptor": {"db": {"built": "2026-08-24T01:00:00Z", "checksum": "sha256:deadbeef"}}
}`

const trivyReport = `{
  "SchemaVersion": 2,
  "Results": [
    {"Vulnerabilities": [
      {"VulnerabilityID": "CVE-2026-3", "PkgName": "openssl", "InstalledVersion": "3.0.1",
       "FixedVersion": "3.0.2", "Severity": "HIGH"}
    ]}
  ]
}`

const osvReport = `{
  "results": [
    {"packages": [
      {"package": {"name": "lodash", "version": "4.17.20"},
       "vulnerabilities": [{"id": "GHSA-xxxx", "database_specific": {"severity": "MODERATE"}}]}
    ]}
  ]
}`

func TestNormalizeFindingsReadsEachScannerIntoTheShapeTheRulesJudge(t *testing.T) {
	for _, spec := range []struct {
		name          string
		format        string
		report        string
		want          vulnerabilityFinding
		wantCount     int
		wantSnapshot  string
		snapshotEmpty bool
	}{
		{
			name: "grype", format: scanFormatGrype, report: grypeReport, wantCount: 2,
			want: vulnerabilityFinding{
				Vulnerability: "CVE-2026-1", Severity: "low", Package: "zlib", Version: "1.0.0",
			},
			wantSnapshot: "grype-db:sha256:deadbeef",
		},
		{
			name: "trivy", format: scanFormatTrivy, report: trivyReport, wantCount: 1,
			want: vulnerabilityFinding{
				Vulnerability: "CVE-2026-3", Severity: "high", Package: "openssl",
				Version: "3.0.1", FixedIn: "3.0.2",
			},
			snapshotEmpty: true,
		},
		{
			name: "osv", format: scanFormatOSV, report: osvReport, wantCount: 1,
			want: vulnerabilityFinding{
				Vulnerability: "GHSA-xxxx", Severity: "moderate", Package: "lodash", Version: "4.17.20",
			},
			snapshotEmpty: true,
		},
	} {
		t.Run(spec.name, func(t *testing.T) {
			findings, snapshot := normalizeFindings(spec.format, []byte(spec.report))
			if len(findings) != spec.wantCount {
				t.Fatalf("read %d findings, want %d: %+v", len(findings), spec.wantCount, findings)
			}
			// Sorted by identifier, so the same scan twice produces the same
			// predicate bytes — which is what keeps attaching idempotent.
			if findings[0] != spec.want {
				t.Errorf("read %+v, want %+v", findings[0], spec.want)
			}
			switch {
			case spec.snapshotEmpty && snapshot != "":
				t.Errorf("a report that names no database answered %q", snapshot)
			case !spec.snapshotEmpty && snapshot != spec.wantSnapshot:
				t.Errorf("read snapshot %q, want %q", snapshot, spec.wantSnapshot)
			}
		})
	}
}

func TestNormalizeFindingsRecognisesAReportWhoseFormatIsWrong(t *testing.T) {
	// A scanner is far more likely to be misconfigured about its own format
	// than to emit a report two readers both understand, so an unknown or
	// mistaken format tries every reader rather than refusing.
	findings, snapshot := normalizeFindings("", []byte(grypeReport))
	if len(findings) != 2 || snapshot != "grype-db:sha256:deadbeef" {
		t.Fatalf("an undeclared format was not recognised: %d findings, snapshot %q", len(findings), snapshot)
	}
	findings, _ = normalizeFindings("grype-json", []byte(trivyReport))
	if len(findings) != 0 {
		t.Errorf("a declared format was second-guessed: %+v", findings)
	}
}

func TestNormalizeFindingsAnswersNothingForAReportNobodyCanRead(t *testing.T) {
	// The honest outcome: no normalized findings at all. The raw report is
	// still signed, and a policy that requires a scan then fires — rather than
	// the platform inventing a clean result out of something it could not
	// parse.
	findings, snapshot := normalizeFindings("", []byte("<?xml version=\"1.0\"?><report/>"))
	if len(findings) != 0 || snapshot != "" {
		t.Fatalf("an unreadable report produced %d findings and snapshot %q", len(findings), snapshot)
	}
}

func TestAnAbsentSeverityIsUnknownRatherThanLow(t *testing.T) {
	// The bundle ranks "unknown" at zero, the same as "none". Calling an
	// absent severity "low" would rank it above none for no reason anybody
	// stated.
	findings, _ := normalizeFindings(scanFormatOSV,
		[]byte(`{"results":[{"packages":[{"package":{"name":"p"},"vulnerabilities":[{"id":"X"}]}]}]}`))
	if len(findings) != 1 || findings[0].Severity != "unknown" {
		t.Fatalf("an absent severity read as %+v", findings)
	}
}

func TestTheDataSnapshotSaysWhenItCannotReproduceAScan(t *testing.T) {
	scanner := kitchenv1alpha1.VulnerabilityScannerSpec{
		Image: "aquasec/trivy:0.58.0", Version: "0.58.0",
	}
	at := time.Date(2026, 8, 24, 3, 14, 0, 0, time.UTC)

	// The scanner's own word wins over everything.
	if got := dataSnapshot("trivy-db:2026082400", "grype-db:x", scanner, at); got != "trivy-db:2026082400" {
		t.Errorf("the scanner's own snapshot was overridden: %q", got)
	}
	// Then what the report carries.
	if got := dataSnapshot("", "grype-db:sha256:beef", scanner, at); got != "grype-db:sha256:beef" {
		t.Errorf("the report's snapshot was not used: %q", got)
	}
	// And when neither says: the scanner and the day, marked as what it is. A
	// reader has to be able to tell a snapshot that reproduces a scan from one
	// that merely dates it.
	got := dataSnapshot("", "", scanner, at)
	if !strings.HasPrefix(got, "unpinned:") {
		t.Fatalf("a snapshot nobody could establish is not marked unpinned: %q", got)
	}
	if !strings.Contains(got, "0.58.0") || !strings.Contains(got, "2026-08-24") {
		t.Errorf("the fallback snapshot names neither the scanner nor the day: %q", got)
	}
}
