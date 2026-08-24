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
	"encoding/json"
	"sort"
	"strings"
)

// Reading a finding out of a scanner's report.
//
// The platform does not transcode evidence — §6.5 of docs/COMPLIANCE.md is
// emphatic that a bill of materials rewritten by something that did not scan
// the image is a claim by the transcoder — and this is the one place that rule
// is bent, deliberately and in one direction only.
//
// The reason is that phase 3 already fixed the contract. The default bundle's
// `max-severity` rule reads `predicate.findings[]` and wants `.severity` and
// `.vulnerability`, and a rule cannot be written once against three scanners
// whose reports agree about nothing. So the signed statement carries **both**:
// the scanner's own bytes verbatim under `report`, and the platform's reading
// of them under `findings`. The first is the scanner's word; the second is the
// platform's, and the platform's signature covers exactly that division.
//
// Nothing is dropped in the reading that a rule could want and nothing is
// invented: a report shape nobody here recognises yields no normalized
// findings at all, and a policy that requires a scan then fires on an artifact
// whose evidence the platform could not read — which is the honest outcome and
// not the quiet one.

// vulnerabilityFinding is one finding as the policy engine sees it. The field
// names are the contract `max-severity` matches on; adding to them is safe,
// renaming them is not.
type vulnerabilityFinding struct {
	// Vulnerability is the identifier: a CVE, a GHSA, whatever the scanner's
	// database calls it.
	Vulnerability string `json:"vulnerability"`
	// Severity is lowercased to the vocabulary the bundle ranks — none,
	// negligible, unknown, low, medium, high, critical — and left as the
	// scanner wrote it when it is not one of those, because a severity nobody
	// recognises must read as unranked rather than as low.
	Severity string `json:"severity"`
	// Package and Version locate the finding in the bill of materials, and
	// FixedIn is the answer to "what do I do about it".
	Package string `json:"package,omitempty"`
	Version string `json:"version,omitempty"`
	FixedIn string `json:"fixedIn,omitempty"`
}

// The report shapes the platform can read. A scanner outside this list still
// produces a signed attestation carrying its bytes; what it does not produce
// is a findings list any rule can judge.
const (
	scanFormatGrype = "grype-json"
	scanFormatTrivy = "trivy-json"
	scanFormatOSV   = "osv-json"
)

// normalizeFindings reads a scanner's report into the shape the policy engine
// judges, and answers what it could establish about the vulnerability database
// behind it.
//
// `format` is the scanner's declared shape. Empty, or a shape nobody here
// knows, means every reader is tried in turn and the first that finds anything
// wins — a scanner is far more likely to be misconfigured about its own format
// than to emit a report that two readers both understand.
func normalizeFindings(format string, raw []byte) ([]vulnerabilityFinding, string) {
	readers := map[string]func([]byte) ([]vulnerabilityFinding, string){
		scanFormatGrype: grypeFindings,
		scanFormatTrivy: trivyFindings,
		scanFormatOSV:   osvFindings,
	}
	if reader, known := readers[strings.ToLower(strings.TrimSpace(format))]; known {
		return reader(raw)
	}
	for _, reader := range []func([]byte) ([]vulnerabilityFinding, string){
		grypeFindings, trivyFindings, osvFindings,
	} {
		if findings, snapshot := reader(raw); len(findings) > 0 || snapshot != "" {
			return findings, snapshot
		}
	}
	return []vulnerabilityFinding{}, ""
}

// grypeFindings reads Grype's JSON. It is the only one of the three that says
// which database it matched against, which is why the snapshot half of this
// package's contract is not a fiction: `descriptor.db` carries the build date
// and the checksum, and the checksum is what makes a rescan reproducible
// rather than merely repeatable.
func grypeFindings(raw []byte) ([]vulnerabilityFinding, string) {
	report := struct {
		Matches []struct {
			Vulnerability struct {
				ID       string `json:"id"`
				Severity string `json:"severity"`
				Fix      struct {
					Versions []string `json:"versions"`
				} `json:"fix"`
			} `json:"vulnerability"`
			Artifact struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"artifact"`
		} `json:"matches"`
		Descriptor struct {
			DB struct {
				Built    string `json:"built"`
				Checksum string `json:"checksum"`
			} `json:"db"`
		} `json:"descriptor"`
	}{}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, ""
	}
	findings := make([]vulnerabilityFinding, 0, len(report.Matches))
	for _, match := range report.Matches {
		findings = append(findings, vulnerabilityFinding{
			Vulnerability: match.Vulnerability.ID,
			Severity:      normalizeSeverity(match.Vulnerability.Severity),
			Package:       match.Artifact.Name,
			Version:       match.Artifact.Version,
			FixedIn:       strings.Join(match.Vulnerability.Fix.Versions, ", "),
		})
	}
	snapshot := ""
	switch db := report.Descriptor.DB; {
	case db.Checksum != "":
		snapshot = "grype-db:" + db.Checksum
	case db.Built != "":
		snapshot = "grype-db:" + db.Built
	}
	return sortFindings(findings), snapshot
}

// trivyFindings reads Trivy's JSON. Trivy's report says nothing about its own
// database, so the snapshot for a Trivy scan comes from the scanner writing
// one to KITCHEN_DATA_SNAPSHOT, or from the fallback the sweep records — see
// rescan.go.
func trivyFindings(raw []byte) ([]vulnerabilityFinding, string) {
	report := struct {
		Results []struct {
			Vulnerabilities []struct {
				VulnerabilityID  string `json:"VulnerabilityID"`
				PkgName          string `json:"PkgName"`
				InstalledVersion string `json:"InstalledVersion"`
				FixedVersion     string `json:"FixedVersion"`
				Severity         string `json:"Severity"`
			} `json:"Vulnerabilities"`
		} `json:"Results"`
	}{}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, ""
	}
	findings := []vulnerabilityFinding{}
	for _, result := range report.Results {
		for _, entry := range result.Vulnerabilities {
			findings = append(findings, vulnerabilityFinding{
				Vulnerability: entry.VulnerabilityID,
				Severity:      normalizeSeverity(entry.Severity),
				Package:       entry.PkgName,
				Version:       entry.InstalledVersion,
				FixedIn:       entry.FixedVersion,
			})
		}
	}
	return sortFindings(findings), ""
}

// osvFindings reads OSV-Scanner's JSON. OSV records no severity of its own —
// the ecosystem's advisory does, under database_specific — so a finding with
// nothing there is recorded as "unknown" rather than as low, which is what the
// bundle's rank map already treats as unranked.
func osvFindings(raw []byte) ([]vulnerabilityFinding, string) {
	report := struct {
		Results []struct {
			Packages []struct {
				Package struct {
					Name    string `json:"name"`
					Version string `json:"version"`
				} `json:"package"`
				Vulnerabilities []struct {
					ID               string `json:"id"`
					DatabaseSpecific struct {
						Severity string `json:"severity"`
					} `json:"database_specific"`
				} `json:"vulnerabilities"`
			} `json:"packages"`
		} `json:"results"`
	}{}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, ""
	}
	findings := []vulnerabilityFinding{}
	for _, result := range report.Results {
		for _, pkg := range result.Packages {
			for _, entry := range pkg.Vulnerabilities {
				findings = append(findings, vulnerabilityFinding{
					Vulnerability: entry.ID,
					Severity:      normalizeSeverity(entry.DatabaseSpecific.Severity),
					Package:       pkg.Package.Name,
					Version:       pkg.Package.Version,
				})
			}
		}
	}
	return sortFindings(findings), ""
}

// normalizeSeverity lowercases and trims, and calls an absent severity
// "unknown" — which the bundle ranks at zero. Anything else is passed through
// as the scanner wrote it: inventing a rank for a word nobody defined would be
// the platform deciding a threshold, which is the one thing gates and scans do
// not do.
func normalizeSeverity(severity string) string {
	trimmed := strings.ToLower(strings.TrimSpace(severity))
	if trimmed == "" {
		return "unknown"
	}
	return trimmed
}

// sortFindings orders findings by identifier then package, so that two scans
// of the same artifact against the same database produce the same predicate
// bytes. Without it the attestation would differ run to run for no reason a
// reader could see, and "attaching is idempotent by content" would quietly
// stop being true for this predicate.
func sortFindings(findings []vulnerabilityFinding) []vulnerabilityFinding {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Vulnerability != findings[j].Vulnerability {
			return findings[i].Vulnerability < findings[j].Vulnerability
		}
		if findings[i].Package != findings[j].Package {
			return findings[i].Package < findings[j].Package
		}
		return findings[i].Version < findings[j].Version
	})
	return findings
}
