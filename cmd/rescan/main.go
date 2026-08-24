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

// Command rescan carries a deployed artifact's bill of materials into a
// scanner pod and the scanner's findings back out of it.
//
// It is two halves of one pod's contract and neither of them is the scanner.
// The scanner is an image somebody else wrote — Grype, Trivy, OSV-Scanner —
// which runs between them as an init container, reads a file and writes a
// file.
//
//	rescan fetch    reads the artifact's SBOM attestation out of the registry
//	                with the credential the pod already holds, and writes the
//	                bill of materials to a volume the scanner reads.
//	rescan publish  reads the scanner's findings, stores them in the registry
//	                as a blob, and reports the digest through the pod's
//	                termination message.
//
// The indirection exists for the reason cmd/qualitygate exists: a scan report
// does not fit in a 4 KiB termination message, does not reliably fit in a
// ConfigMap, and cannot be read back out of a log without racing the Job
// finishing. See docs/COMPLIANCE.md §7.4.
//
// Why the bill of materials and not the image: rescanning must require no
// rebuild and no redeploy, and matching an SBOM the build already produced
// against today's vulnerability database is exactly that — the artifact is
// never pulled, never unpacked, never touched. It also means the scan is
// honest about what it covers: it covers what the SBOM says is in the image,
// and §11 says out loud what that misses.
//
// Nothing here is evidence yet. It is unsigned, and anything with push access
// could have written it — it becomes evidence in the operator, where the
// signing key is and where neither the scanner's image nor this pod ever
// reaches.
//
// It ships in the operator's image and is never run by a person. Its argv is
// fixed by the reconciler: one word, chosen from the two above, and nothing
// from any API request reaches it.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Bermos/Kitchen/internal/attestation"
)

// The pod's contract with the operator, in both directions.
const (
	// envArtifact is the deployed artifact, as repository@digest. Its
	// attestations are read from it and the findings are stored beside it.
	envArtifact = "KITCHEN_ARTIFACT"

	// envSBOM is the file the bill of materials is written to and the scanner
	// is pointed at.
	envSBOM = "KITCHEN_SBOM"

	// envFindings is the file the scanner was told to write.
	envFindings = "KITCHEN_FINDINGS"

	// envDataSnapshot is a file the scanner may write its vulnerability
	// database's identifier to. It is optional and most scanners will not:
	// a scan whose snapshot is not named here falls back to what the operator
	// can establish from the report itself, and then to the scanner's own
	// version and the day it ran.
	envDataSnapshot = "KITCHEN_DATA_SNAPSHOT"

	// envScanner is the scanner's name, echoed back so a report cannot be
	// mistaken for another scanner's.
	envScanner = "KITCHEN_SCANNER"

	// envTerminationLog is where to write the report the operator reads. It
	// is passed rather than compiled in so the two ends cannot disagree about
	// it silently.
	envTerminationLog = "KITCHEN_TERMINATION_LOG"

	// envDockerConfig is the directory holding the registry credential, in
	// docker's own environment variable so the scanner image beside this one
	// reads the same one without being told.
	envDockerConfig = "DOCKER_CONFIG"
)

// sbomSourceSuffix names the file `fetch` leaves beside the bill of materials
// saying which attestation it came from. It travels on the volume rather than
// through a termination message because an init container's message is a
// second place to look, and one place is better.
const sbomSourceSuffix = ".source.json"

// bill-of-materials predicate types the fetch half recognises. They are the
// two the default policy bundle already knows, and they are matched exactly:
// a predicate type nobody agreed on is not an SBOM because it looks like one.
var sbomPredicateTypes = []string{
	"https://spdx.dev/Document",
	"https://cyclonedx.org/bom",
}

// report is what the operator reads off the pod. It is small on purpose: the
// findings are in the registry and this says where.
//
// There is no size field. One was carried and claimed to make "the scanner
// wrote nothing" distinguishable from "the output went missing", and it never
// could: publish refuses an empty file outright, so a report is only ever
// written for findings that exist and the distinction is made by Error rather
// than by a number nobody read.
type report struct {
	// Scanner is the name the platform gave the scanner, echoed back so that a
	// person reading a termination message on the pod can see which one
	// produced it. The operator does not read it — it configured the scanner
	// and knows — and the failure report in main() carries it for the same
	// reason.
	Scanner string `json:"scanner"`
	// Blob is the digest the findings are stored under, in the artifact's own
	// repository.
	Blob string `json:"blob"`
	// DataSnapshot is what the scanner said about its vulnerability database,
	// where it said anything. Empty is ordinary and the operator fills it in
	// from what it can establish.
	DataSnapshot string `json:"dataSnapshot,omitempty"`
	// SBOM records which bill of materials was matched: the predicate type
	// and the envelope digest of the attestation it came out of. A scan is a
	// claim about a specific SBOM, and an artifact can carry more than one.
	SBOM       string `json:"sbom,omitempty"`
	SBOMDigest string `json:"sbomDigest,omitempty"`
	// FinishedAt is when the scanner's output was read, which is as close to
	// when the scan finished as this process can honestly claim.
	FinishedAt string `json:"finishedAt"`
	// Error explains a run whose findings could not be produced or stored.
	// The operator treats a report carrying one as a scan that did not
	// happen, which is a different thing from a scan that found nothing.
	Error string `json:"error,omitempty"`
}

// sbomSource is what `fetch` leaves for `publish`: which attestation the bill
// of materials came out of.
type sbomSource struct {
	PredicateType string `json:"predicateType"`
	Digest        string `json:"digest"`
}

func main() {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	var err error
	switch mode {
	case "fetch":
		err = fetch(context.Background())
	case "publish":
		err = publish(context.Background())
	default:
		err = fmt.Errorf("rescan takes one argument, fetch or publish (got %q)", mode)
	}
	if err == nil {
		return
	}
	// The failure goes to the termination message as well as to stderr,
	// because stderr is a log line the operator would have to go looking for
	// and the termination message is on the pod status it is already reading.
	_ = writeTermination(report{
		Scanner:    os.Getenv(envScanner),
		Error:      err.Error(),
		FinishedAt: time.Now().UTC().Format(time.RFC3339),
	})
	fmt.Fprintf(os.Stderr, "rescan %s failed: %v\n", mode, err)
	os.Exit(1)
}

// fetch writes the artifact's bill of materials where the scanner will look
// for it.
//
// It reads the attestation unverified, and that is not a gap: what the
// platform signs at the end of this is the *findings*, and the findings are a
// claim about the SBOM this pod actually matched — which is why the envelope's
// digest travels back with them. A pod holding the platform's public key would
// not make the scan more trustworthy; it would only move the check somewhere
// the operator cannot see it.
func fetch(ctx context.Context) error {
	artifact := os.Getenv(envArtifact)
	target := os.Getenv(envSBOM)
	if artifact == "" || target == "" {
		return fmt.Errorf("%s and %s must both be set", envArtifact, envSBOM)
	}
	repository, _, byDigest := strings.Cut(artifact, "@")
	if !byDigest {
		return fmt.Errorf(
			"the artifact must be given as repository@digest, and %q is not — "+
				"a scan of a tag is a scan of a moving target",
			artifact)
	}

	store, err := storeFor(repository)
	if err != nil {
		return err
	}
	set, err := store.Evidence(ctx, artifact)
	if err != nil {
		return fmt.Errorf("the artifact's evidence could not be read: %w", err)
	}

	for _, wanted := range sbomPredicateTypes {
		for _, found := range set.Attestations {
			if found.PredicateType != wanted || len(found.Statement.Predicate) == 0 {
				continue
			}
			if err := os.WriteFile(filepath.Clean(target), found.Statement.Predicate, 0o600); err != nil {
				return fmt.Errorf("the bill of materials could not be written to %s: %w", target, err)
			}
			source, err := json.Marshal(sbomSource{PredicateType: found.PredicateType, Digest: found.Digest})
			if err != nil {
				return err
			}
			return os.WriteFile(filepath.Clean(target+sbomSourceSuffix), source, 0o600)
		}
	}
	// An artifact with no bill of materials cannot be rescanned, and saying so
	// is the point. Silently scanning nothing would report a clean result for
	// an image nobody has ever looked inside.
	return fmt.Errorf(
		"the artifact carries no bill of materials attestation (%s), so there is nothing to match "+
			"against a vulnerability database — turn on compliance.attestation.build.sbom and rebuild",
		strings.Join(sbomPredicateTypes, " or "))
}

// publish carries the scanner's findings out of the pod.
func publish(ctx context.Context) error {
	artifact := os.Getenv(envArtifact)
	findingsPath := os.Getenv(envFindings)
	scanner := os.Getenv(envScanner)
	if artifact == "" || findingsPath == "" || scanner == "" {
		return fmt.Errorf("%s, %s and %s must all be set", envArtifact, envFindings, envScanner)
	}

	body, err := os.ReadFile(filepath.Clean(findingsPath))
	if err != nil {
		// A scanner that exited cleanly and wrote nothing has not run,
		// whatever its exit code said. Saying so is the point: a scanner
		// silently producing no findings reads exactly like a clean scan.
		return fmt.Errorf("the scanner wrote no findings to %s: %w", findingsPath, err)
	}
	if len(body) == 0 {
		return fmt.Errorf("the scanner wrote an empty file to %s", findingsPath)
	}

	repository, _, found := strings.Cut(artifact, "@")
	if !found {
		return fmt.Errorf(
			"the artifact must be given as repository@digest, and %q is not", artifact)
	}

	store, err := storeFor(repository)
	if err != nil {
		return err
	}
	digest, err := store.PutBlob(ctx, repository, body)
	if err != nil {
		return err
	}

	answer := report{
		Scanner:      scanner,
		Blob:         digest,
		DataSnapshot: readSnapshot(),
		FinishedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if source := readSBOMSource(); source != nil {
		answer.SBOM, answer.SBOMDigest = source.PredicateType, source.Digest
	}
	return writeTermination(answer)
}

// readSnapshot reads what the scanner said about its vulnerability database,
// if it was given a file and wrote to it. It is trimmed and bounded: this
// travels in a 4 KiB termination message, and a scanner that dumped its whole
// database index here would take the report down with it.
func readSnapshot() string {
	path := os.Getenv(envDataSnapshot)
	if path == "" {
		return ""
	}
	body, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return ""
	}
	snapshot := strings.TrimSpace(string(body))
	if len(snapshot) > 256 {
		snapshot = snapshot[:256]
	}
	return snapshot
}

// readSBOMSource reads what the fetch half left beside the bill of materials.
// Its absence is not an error here: the scan happened, and which SBOM it was
// of is recorded where it can be.
func readSBOMSource() *sbomSource {
	path := os.Getenv(envSBOM)
	if path == "" {
		return nil
	}
	body, err := os.ReadFile(filepath.Clean(path + sbomSourceSuffix))
	if err != nil {
		return nil
	}
	source := &sbomSource{}
	if err := json.Unmarshal(body, source); err != nil {
		return nil
	}
	return source
}

// storeFor builds a registry client from the credential mounted beside this
// process — the same one that pulls the application's own image.
func storeFor(repository string) (*attestation.Store, error) {
	directory := os.Getenv(envDockerConfig)
	if directory == "" {
		return nil, fmt.Errorf("%s is not set, so there is no credential to reach the registry with", envDockerConfig)
	}
	config, err := os.ReadFile(filepath.Clean(filepath.Join(directory, "config.json")))
	if err != nil {
		return nil, fmt.Errorf("the registry credential could not be read: %w", err)
	}
	server, _, _ := strings.Cut(repository, "/")
	auth, err := attestation.AuthFromDockerConfig(config, server)
	if err != nil {
		return nil, err
	}
	return &attestation.Store{Auth: auth}, nil
}

// writeTermination puts the report where the operator reads it.
func writeTermination(answer report) error {
	path := os.Getenv(envTerminationLog)
	if path == "" {
		path = "/dev/termination-log"
	}
	body, err := json.Marshal(answer)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Clean(path), body, 0o600)
}
